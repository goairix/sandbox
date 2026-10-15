//go:build linux && (amd64 || arm64)

package nodeobserver

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

func kernelPeer(conn *net.UnixConn) (int, error) {
	if conn == nil {
		return 0, fmt.Errorf("missing node enrollment socket")
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var peer *unix.Ucred
	var peerErr error
	err = raw.Control(func(fd uintptr) {
		peer, peerErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if err != nil {
		return 0, err
	}
	if peerErr != nil {
		return 0, peerErr
	}
	if peer == nil || peer.Uid != 0 || peer.Pid <= 1 {
		return 0, fmt.Errorf("enrollment requires host-view UID0 private PID1 peer")
	}
	return int(peer.Pid), nil
}
