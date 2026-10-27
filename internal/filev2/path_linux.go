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
