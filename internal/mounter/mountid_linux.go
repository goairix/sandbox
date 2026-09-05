//go:build linux

package mounter

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func kernelMountID(path string) (uint64, error) {
	var stat unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_NO_AUTOMOUNT, unix.STATX_MNT_ID, &stat); err != nil {
		return 0, fmt.Errorf("statx workspace mount: %w", err)
	}
	if stat.Mask&unix.STATX_MNT_ID == 0 || stat.Mnt_id == 0 {
		return 0, fmt.Errorf("statx workspace mount ID is unavailable")
	}
	return stat.Mnt_id, nil
}
