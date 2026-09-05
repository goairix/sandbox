//go:build !linux

package workspaceprobe

import "syscall"

func ioHelperProcessAttributes() *syscall.SysProcAttr {
	return detachedProcessAttributes()
}
