//go:build linux

package mounter

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

func defaultVerifyRootPeer(connection *net.UnixConn) error {
	raw, err := connection.SyscallConn()
	if err != nil {
		return fmt.Errorf("inspect control peer")
	}
	var credential *unix.Ucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credential, socketErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || socketErr != nil || credential == nil || credential.Uid != 0 {
		return fmt.Errorf("control peer is not trusted root")
	}
	return nil
}
