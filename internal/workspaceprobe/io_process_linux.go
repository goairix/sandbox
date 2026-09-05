//go:build linux

package workspaceprobe

import "syscall"

func ioHelperProcessAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true, Pdeathsig: syscall.SIGKILL}
}
