//go:build linux || darwin

package controltarget

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type journalFiles struct{ uid uint32 }

// Pin every component without following aliases. Only the immediate parent is
// part of the management-owned 0700 boundary; ancestors can be system dirs.
func openJournalParent(path string, uid uint32) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, fmt.Errorf("%w: canonical absolute directory required", ErrInvalidConfiguration)
	}
	parent := filepath.Dir(path)
	canonical, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return nil, fmt.Errorf("%w: parent: %w", ErrInvalidConfiguration, err)
	}
	if canonical != parent {
		return nil, fmt.Errorf("%w: parent alias", ErrInvalidConfiguration)
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, name := range strings.Split(strings.TrimPrefix(parent, "/"), "/") {
		if name == "" {
			continue
		}
		next, e := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return nil, e
		}
		fd = next
	}
	f := os.NewFile(uintptr(fd), parent)
	if err = validateJournalFD(f, uid, true); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func journalName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00")
}

func validateJournalFD(f *os.File, uid uint32, directory bool) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return err
	}
	mode := uint32(0600)
	kind := uint32(unix.S_IFREG)
	if directory {
		mode = 0700
		kind = unix.S_IFDIR
	}
	if st.Uid != uid || uint32(st.Mode)&unix.S_IFMT != kind || uint32(st.Mode)&07777 != mode || (!directory && st.Nlink != 1) {
		return fmt.Errorf("%w: unsafe owner, mode, type or links: %s", ErrInvalidRecord, f.Name())
	}
	return nil
}

func (f *journalFiles) openChild(dir *os.File, name string, directory bool) (*os.File, error) {
	if !journalName(name) {
		return nil, fmt.Errorf("%w: invalid child name", ErrInvalidRecord)
	}
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	if directory {
		flags |= unix.O_DIRECTORY
	}
	fd, err := unix.Openat(int(dir.Fd()), name, flags, 0)
	if err != nil {
		return nil, err
	}
	child := os.NewFile(uintptr(fd), name)
	if err = validateJournalFD(child, f.uid, directory); err != nil {
		child.Close()
		return nil, err
	}
	return child, nil
}

func (f *journalFiles) readFile(ctx context.Context, dir *os.File, name string) ([]byte, error) {
	if ctx == nil {
		return nil, ErrInvalidConfiguration
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	child, err := f.openChild(dir, name, false)
	if err != nil {
		return nil, err
	}
	defer child.Close()
	st, err := child.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() > maxJournalWireBytes {
		return nil, fmt.Errorf("%w: oversized file", ErrInvalidRecord)
	}
	b, err := io.ReadAll(io.LimitReader(child, maxJournalWireBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxJournalWireBytes || int64(len(b)) != st.Size() {
		return nil, fmt.Errorf("%w: file size changed", ErrInvalidRecord)
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return b, nil
}
