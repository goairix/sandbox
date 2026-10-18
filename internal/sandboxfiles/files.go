package sandboxfiles

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goairix/sandbox/pkg/types"
	"golang.org/x/sys/unix"
)

func Run(req Request) Result {
	if err := Validate(req); err != nil {
		return failure(err)
	}
	base, _ := rootPath(req.Path)
	root, err := os.OpenRoot(base)
	if err != nil {
		return failure(err)
	}
	defer root.Close()
	return runAt(req, root)
}

func rootPath(name string) (string, string) {
	name = path.Clean(name)
	base := "/workspace"
	if name == "/tmp" || strings.HasPrefix(name, "/tmp/") {
		base = "/tmp"
	}
	rel := strings.TrimPrefix(strings.TrimPrefix(name, base), "/")
	if rel == "" {
		rel = "."
	}
	return base, rel
}

func failure(err error) Result {
	var code failureCode
	switch {
	case errors.As(err, &code):
		return failed(string(code))
	case errors.Is(err, os.ErrNotExist):
		return failed("not_found")
	case errors.Is(err, os.ErrPermission):
		return failed("permission_denied")
	case errors.Is(err, unix.ENOTDIR), errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENXIO):
		return failed("unsupported_type")
	default:
		return failed("io_error")
	}
}

// runAt is private so tests can supply a temporary root without adding a host
// root knob to the deployed helper's request, flags or environment.
func runAt(req Request, root *os.Root) Result {
	if err := Validate(req); err != nil {
		return failure(err)
	}
	_, rel := rootPath(req.Path)
	if req.Operation == "list" || req.Operation == "list-recursive" || req.Operation == "glob" {
		result, err := scan(req, root, rel)
		if err != nil {
			return failure(err)
		}
		return result
	}
	if req.Operation == "edit" || req.Operation == "edit-lines" {
		return edit(req, root, rel)
	}
	if req.Operation == "upload" {
		return upload(req, root, rel)
	}
	file, err := root.OpenFile(rel, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return failure(err)
	}
	defer file.Close()
	content, err := readRegular(file)
	if err != nil {
		return failure(err)
	}
	if req.Operation == "read-lines" {
		if !utf8.Valid(content) {
			return failed("unsupported_type")
		}
		return readLines(req, content)
	}
	return Result{Content: content}
}

func readRegular(file *os.File) ([]byte, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, failureCode("unsupported_type")
	}
	if info.Size() > MaxContentBytes {
		return nil, failureCode("limit_exceeded")
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxContentBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxContentBytes {
		return nil, failureCode("limit_exceeded")
	}
	return data, nil
}

func scan(req Request, root *os.Root, rel string) (Result, error) {
	entries := make([]types.FileInfo, 0)
	count := 0
	var walk func(*os.File, string, int) error
	walk = func(file *os.File, dir string, depth int) error {
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return failureCode("unsupported_type")
		}
		for {
			batch, readErr := file.ReadDir(1)
			for _, entry := range batch {
				count++
				if count > MaxEntries {
					return failureCode("limit_exceeded")
				}
				if !utf8.ValidString(entry.Name()) {
					return failureCode("unsupported_type")
				}
				name := path.Join(dir, entry.Name())
				var stat unix.Stat_t
				if err := unix.Fstatat(int(file.Fd()), entry.Name(), &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
					return err
				}
				isDir := stat.Mode&unix.S_IFMT == unix.S_IFDIR
				base := strings.TrimPrefix(strings.TrimPrefix(name, rel), "/")
				if rel == "." {
					base = name
				}
				match := true
				if req.Operation == "glob" {
					match, _ = path.Match(req.Pattern, base)
				}
				if match {
					entries = append(entries, types.FileInfo{Name: entry.Name(), Path: path.Join(path.Clean(req.Path), base), Size: stat.Size, IsDir: isDir, ModTime: time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec)})
				}
				if !isDir || req.Operation == "list" {
					continue
				}
				if req.MaxDepth > 0 && depth >= req.MaxDepth {
					continue
				}
				if depth >= MaxDepth {
					return failureCode("limit_exceeded")
				}
				// Raw openat keeps recursive resolution on this pinned parent
				// and refuses a replacement symlink, FIFO or ordinary file.
				child, err := openChildDirectory(file, entry.Name())
				if err != nil {
					return err
				}
				if err := walk(child, name, depth+1); err != nil {
					return err
				}
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				return readErr
			}
		}
		return nil
	}
	startDir, err := root.OpenFile(rel, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK, 0)
	if err != nil {
		return Result{}, err
	}
	if err := walk(startDir, rel, 1); err != nil {
		return Result{}, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	result := Result{Files: entries, TotalCount: len(entries)}
	if req.Operation == "list" {
		return result, nil
	}
	result.Page, result.PageSize = req.Page, req.PageSize
	if result.Page == 0 {
		result.Page = 1
	}
	if result.PageSize == 0 {
		result.PageSize = 100
	}
	if result.Page-1 > len(entries)/result.PageSize {
		result.Files = []types.FileInfo{}
		return result, nil
	}
	start := (result.Page - 1) * result.PageSize
	end := start + result.PageSize
	if end > len(entries) {
		end = len(entries)
	}
	result.Files = entries[start:end]
	return result, nil
}

func openChildDirectory(parent *os.File, name string) (*os.File, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return nil, failureCode("invalid_request")
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func lineStarts(data []byte) []int {
	starts := make([]int, 0)
	if len(data) == 0 {
		return starts
	}
	starts = append(starts, 0)
	for i, c := range data {
		if c == '\n' && i+1 < len(data) {
			starts = append(starts, i+1)
		}
	}
	return starts
}

func readLines(req Request, data []byte) Result {
	starts := lineStarts(data)
	start, end := req.StartLine, req.EndLine
	if start == 0 {
		start = 1
	}
	if end == 0 || end > len(starts) {
		end = len(starts)
	}
	result := Result{Lines: []string{}, StartLine: start, EndLine: end, TotalLines: len(starts)}
	for i := start - 1; i < end; i++ {
		stop := len(data)
		if i+1 < len(starts) {
			stop = starts[i+1]
		}
		line := data[starts[i]:stop]
		line = bytes.TrimSuffix(line, []byte{'\n'})
		result.Lines = append(result.Lines, string(line))
	}
	return result
}

func transformed(req Request, data []byte) ([]byte, error) {
	if req.Operation == "edit" {
		count := bytes.Count(data, []byte(req.OldStr))
		if count == 0 {
			return nil, failureCode("conflict")
		}
		if !req.ReplaceAll {
			count = 1
		}
		if growth := len(req.NewStr) - len(req.OldStr); growth > 0 && count > (MaxContentBytes-len(data))/growth {
			return nil, failureCode("limit_exceeded")
		}
		return bytes.Replace(data, []byte(req.OldStr), []byte(req.NewStr), count), nil
	}
	starts := lineStarts(data)
	if req.StartLine > len(starts)+1 || req.StartLine == len(starts)+1 && req.EndLine != 0 {
		return nil, failureCode("invalid_request")
	}
	begin, end := len(data), len(data)
	if req.StartLine <= len(starts) {
		begin = starts[req.StartLine-1]
	}
	if req.EndLine > 0 && req.EndLine < len(starts) {
		end = starts[req.EndLine]
	}
	if begin+len(req.NewContent)+len(data)-end > MaxContentBytes {
		return nil, failureCode("limit_exceeded")
	}
	output := make([]byte, 0, begin+len(req.NewContent)+len(data)-end)
	output = append(output, data[:begin]...)
	output = append(output, req.NewContent...)
	output = append(output, data[end:]...)
	return output, nil
}

func edit(req Request, root *os.Root, rel string) Result {
	if rel == "." {
		return failed("unsupported_type")
	}
	parent, err := root.OpenFile(path.Dir(rel), os.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK, 0)
	if err != nil {
		return failure(err)
	}
	defer parent.Close()
	base := path.Base(rel)
	fd, err := unix.Openat(int(parent.Fd()), base, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return failure(err)
	}
	file := os.NewFile(uintptr(fd), base)
	data, err := readRegular(file)
	if err != nil {
		file.Close()
		return failure(err)
	}
	info, err := file.Stat()
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return failure(errors.Join(err, closeErr))
	}
	output, err := transformed(req, data)
	if err != nil {
		return failure(err)
	}
	return publishFile(parent, base, output, info.Mode().Perm())
}

// upload creates or replaces without reading the prior file. Every parent is
// opened relative to the preceding pinned FD with no symlink traversal, and
// the final no-follow stat rejects directories/FIFOs without opening them.
// Missing parents are created as the same unprivileged helper user; no ambient
// path is reopened between staging and publication.
func upload(req Request, root *os.Root, rel string) Result {
	if rel == "." {
		return failed("unsupported_type")
	}
	parent, err := root.OpenFile(".", os.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK, 0)
	if err != nil {
		return failure(err)
	}
	defer func() { _ = parent.Close() }()
	for _, component := range strings.Split(path.Dir(rel), "/") {
		if component == "." {
			continue
		}
		if err := unix.Mkdirat(int(parent.Fd()), component, 0755); err != nil && !errors.Is(err, unix.EEXIST) {
			return failure(err)
		}
		next, err := openChildDirectory(parent, component)
		if err != nil {
			return failure(err)
		}
		previous := parent
		parent = next
		if err := previous.Close(); err != nil {
			return failure(err)
		}
	}

	base := path.Base(rel)
	var target unix.Stat_t
	err = unix.Fstatat(int(parent.Fd()), base, &target, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return failure(err)
	}
	if err == nil && target.Mode&unix.S_IFMT != unix.S_IFREG {
		return failed("unsupported_type")
	}
	return publishFile(parent, base, req.Content, 0644)
}

// publishFile publishes complete bytes through one already pinned parent FD.
// Edit supplies its original permissions; upload supplies 0644. Every failure
// before rename leaves the old inode untouched and removes the staging name.
// Once rename succeeds, a failed parent Sync is publication uncertainty, never
// an assertion that the old file survived or permission to retry automatically.
func publishFile(parent *os.File, base string, output []byte, mode os.FileMode) Result {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return failure(err)
	}
	tmp := ".sandbox-file-" + hex.EncodeToString(random[:])
	fd, err := unix.Openat(int(parent.Fd()), tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return failure(err)
	}
	defer unix.Unlinkat(int(parent.Fd()), tmp, 0)
	staged := os.NewFile(uintptr(fd), tmp)
	if err = staged.Chmod(mode.Perm()); err == nil {
		var n int
		n, err = staged.Write(output)
		if err == nil && n != len(output) {
			err = io.ErrShortWrite
		}
	}
	if err == nil {
		err = staged.Sync()
	}
	err = errors.Join(err, staged.Close())
	if err != nil {
		return failure(err)
	}
	if err = unix.Renameat(int(parent.Fd()), tmp, int(parent.Fd()), base); err != nil {
		return failure(err)
	}
	if err = parent.Sync(); err != nil {
		return failed("publish_uncertain")
	}
	return Result{}
}
