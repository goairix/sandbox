package filev2

import (
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/goairix/sandbox/internal/fuseprotocol"
	"golang.org/x/sys/unix"
)

const (
	maxSearchEntries = 10000
	maxSearchBytes   = 64 << 20
	maxSearchResults = 1000
	maxSearchDepth   = 128
	searchDeadline   = 5 * time.Second
)

type fileSearchPage struct {
	Files     []fileInfo `json:"files"`
	Truncated bool       `json:"truncated"`
}

// searchFilesMaxHeap retains the first result paths even when directory
// iteration order changes. A partial page is always marked as such.
type searchFilesMaxHeap []fileInfo

func (h searchFilesMaxHeap) Len() int           { return len(h) }
func (h searchFilesMaxHeap) Less(i, j int) bool { return h[i].Path > h[j].Path }
func (h searchFilesMaxHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *searchFilesMaxHeap) Push(v any)        { *h = append(*h, v.(fileInfo)) }
func (h *searchFilesMaxHeap) Pop() any {
	last := len(*h) - 1
	v := (*h)[last]
	*h = (*h)[:last]
	return v
}

func validateGlobPattern(pattern string) error {
	if pattern == "" || len(pattern) > 4096 || strings.HasPrefix(pattern, "/") || strings.IndexByte(pattern, 0) >= 0 {
		return ErrTextInvalid
	}
	for _, part := range strings.Split(pattern, "/") {
		if part == "" || part == "." || part == ".." {
			return ErrTextInvalid
		}
		if part != "**" {
			if _, err := path.Match(part, "probe"); err != nil {
				return ErrTextInvalid
			}
		}
	}
	return nil
}

// matchGlob matches complete path components. A standalone ** consumes zero
// or more components; other patterns use path.Match's *, ? and class rules.
func matchGlob(pattern, relative string) bool {
	parts := strings.Split(pattern, "/")
	names := strings.Split(relative, "/")
	type state struct{ pattern, name int }
	memo := make(map[state]bool, len(parts)*len(names))
	visited := make(map[state]bool, len(parts)*len(names))
	var match func(int, int) bool
	match = func(p, n int) bool {
		key := state{p, n}
		if visited[key] {
			return memo[key]
		}
		visited[key] = true
		if p == len(parts) {
			memo[key] = n == len(names)
			return memo[key]
		}
		if parts[p] == "**" {
			memo[key] = match(p+1, n) || (n < len(names) && match(p, n+1))
			return memo[key]
		}
		if n < len(names) {
			matched, err := path.Match(parts[p], names[n])
			memo[key] = err == nil && matched && match(p+1, n+1)
		}
		return memo[key]
	}
	return match(0, 0)
}

type globScanner struct {
	ctx          context.Context
	deadline     time.Time
	pattern      string
	hideReserved bool
	seen         int
	bytes        int
	truncated    bool
	files        searchFilesMaxHeap
}

func (s *globScanner) stop() (bool, error) {
	if err := s.ctx.Err(); err != nil {
		return true, err
	}
	if time.Now().After(s.deadline) {
		s.truncated = true
		return true, nil
	}
	return false, nil
}

// walk uses a pinned directory descriptor and opens each child with
// O_NOFOLLOW. Even a concurrent symlink swap cannot escape the search root.
func (s *globScanner) walk(dir *os.File, absolute, relative string, depth int) error {
	if depth >= maxSearchDepth {
		s.truncated = true
		return nil
	}
	for {
		stop, err := s.stop()
		if stop || err != nil {
			return err
		}
		entries, readErr := dir.ReadDir(256)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		for _, entry := range entries {
			stop, err := s.stop()
			if stop || err != nil {
				return err
			}
			s.seen++
			s.bytes += len(entry.Name())
			if s.seen > maxSearchEntries || s.bytes > maxSearchBytes {
				s.truncated = true
				return nil
			}
			if s.hideReserved && fuseprotocol.IsReservedProbeObjectName(entry.Name()) {
				continue
			}
			fd, err := unix.Openat(int(dir.Fd()), entry.Name(), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
			if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOENT) {
				continue // A link or a concurrently removed entry is not searchable.
			}
			if err != nil {
				return err
			}
			childPath := path.Join(absolute, entry.Name())
			childRelative := entry.Name()
			if relative != "" {
				childRelative = relative + "/" + childRelative
			}
			child := os.NewFile(uintptr(fd), childPath)
			info, statErr := child.Stat()
			if statErr != nil {
				_ = child.Close()
				return statErr
			}
			if info.IsDir() {
				err = s.walk(child, childPath, childRelative, depth+1)
			} else if info.Mode().IsRegular() && matchGlob(s.pattern, childRelative) {
				candidate := toFileInfo(childPath, info)
				if s.files.Len() < maxSearchResults {
					heap.Push(&s.files, candidate)
				} else {
					s.truncated = true
					if candidate.Path < s.files[0].Path {
						s.files[0] = candidate
						heap.Fix(&s.files, 0)
					}
				}
			}
			closeErr := child.Close()
			if err != nil || closeErr != nil {
				return errors.Join(err, closeErr)
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
	}
}

func runGlob(ctx context.Context, rootFD int, relative, absolute, pattern string, hideReserved bool, output io.Writer) error {
	if err := validateGlobPattern(pattern); err != nil {
		return err
	}
	fd, err := openBeneath(rootFD, relative, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(fd), absolute)
	defer dir.Close()
	scanner := globScanner{ctx: ctx, deadline: time.Now().Add(searchDeadline), pattern: pattern, hideReserved: hideReserved, files: make(searchFilesMaxHeap, 0)}
	if err := scanner.walk(dir, absolute, "", 0); err != nil {
		return err
	}
	sort.Slice(scanner.files, func(i, j int) bool { return scanner.files[i].Path < scanner.files[j].Path })
	return json.NewEncoder(output).Encode(fileSearchPage{Files: scanner.files, Truncated: scanner.truncated})
}
