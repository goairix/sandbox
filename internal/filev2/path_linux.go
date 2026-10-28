//go:build linux

package filev2

import (
	"errors"
	"strings"

	"golang.org/x/sys/unix"
)

// openBeneath resolves and opens in one kernel operation against a pinned
// directory. A preflight filepath check alone cannot protect a swapped link.
func openBeneath(rootFD int, relative string, flags int, mode uint32) (int, error) {
	if relative == "" || strings.HasPrefix(relative, "/") || strings.IndexByte(relative, 0) >= 0 {
		return -1, errors.New("invalid relative file path")
	}
	for _, part := range strings.Split(relative, "/") {
		if part == ".." {
			return -1, errors.New("parent traversal is forbidden")
		}
	}
	return unix.Openat2(rootFD, relative, &unix.OpenHow{
		Flags:   uint64(flags | unix.O_CLOEXEC),
		Mode:    uint64(mode),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS,
	})
}

// openWriteWithParents walks only pinned directory descriptors. A concurrent
// symlink swap cannot redirect a new parent or the final file outside root.
func openWriteWithParents(rootFD int, relative string) (int, error) {
	if relative == "" || relative == "." || strings.HasPrefix(relative, "/") || strings.IndexByte(relative, 0) >= 0 {
		return -1, errors.New("invalid write path")
	}
	parts := strings.Split(relative, "/")
	current := rootFD
	defer func() {
		if current != rootFD {
			_ = unix.Close(current)
		}
	}()
	for _, part := range parts[:len(parts)-1] {
		if part == "" || part == "." || part == ".." {
			return -1, errors.New("invalid parent directory")
		}
		next, err := unix.Openat(current, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) {
			if mkdirErr := unix.Mkdirat(current, part, 0755); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				return -1, mkdirErr
			}
			next, err = unix.Openat(current, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if err != nil {
			return -1, err
		}
		if current != rootFD {
			_ = unix.Close(current)
		}
		current = next
	}
	base := parts[len(parts)-1]
	if base == "" || base == "." || base == ".." {
		return -1, errors.New("invalid file basename")
	}
	return unix.Openat(current, base, unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0644)
}
