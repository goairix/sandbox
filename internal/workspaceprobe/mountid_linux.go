//go:build linux

package workspaceprobe

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func effectiveMountID(path string) (uint64, error) {
	var stat unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_MNT_ID, &stat); err != nil {
		return 0, err
	}
	if stat.Mask&unix.STATX_MNT_ID == 0 || stat.Mnt_id == 0 {
		return 0, fmt.Errorf("statx did not return a mount id")
	}
	return stat.Mnt_id, nil
}
