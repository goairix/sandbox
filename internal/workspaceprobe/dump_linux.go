//go:build linux

package workspaceprobe

import "golang.org/x/sys/unix"

func disableProcessDump() error {
	return unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)
}
