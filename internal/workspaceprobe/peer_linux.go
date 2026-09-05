//go:build linux

package workspaceprobe

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

func unixPeerIdentity(connection net.Conn) (pid int, uid int, err error) {
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return 0, 0, fmt.Errorf("not a unix connection")
	}
	raw, err := unixConnection.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var credentials *unix.Ucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, socketErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, 0, err
	}
	if socketErr != nil || credentials == nil {
		if socketErr != nil {
			return 0, 0, socketErr
		}
		return 0, 0, fmt.Errorf("unix peer credentials are missing")
	}
	return int(credentials.Pid), int(credentials.Uid), nil
}
