//go:build linux

package mounter

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func dockerReaperPeerIdentity(connection net.Conn) (int, int, error) {
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return 0, 0, fmt.Errorf("not a Unix connection")
	}
	raw, err := unixConnection.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var credentials *unix.Ucred
	var socketErr error
	err = raw.Control(func(fd uintptr) {
		credentials, socketErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if err != nil || socketErr != nil || credentials == nil {
		return 0, 0, fmt.Errorf("Docker reaper peer credentials unavailable")
	}
	return int(credentials.Pid), int(credentials.Uid), nil
}

func dockerReaperProcessIdentity(pid int) (reaperProcessIdentity, error) {
	statRaw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return reaperProcessIdentity{}, err
	}
	end := strings.LastIndexByte(string(statRaw), ')')
	if end < 0 {
		return reaperProcessIdentity{}, fmt.Errorf("invalid proc stat")
	}
	fields := strings.Fields(string(statRaw[end+1:]))
	if len(fields) < 20 {
		return reaperProcessIdentity{}, fmt.Errorf("short proc stat")
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return reaperProcessIdentity{}, err
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return reaperProcessIdentity{}, err
	}
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return reaperProcessIdentity{}, err
	}
	uid := -1
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "Uid:") {
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				uid, _ = strconv.Atoi(parts[2])
			}
			break
		}
	}
	if uid < 0 {
		return reaperProcessIdentity{}, fmt.Errorf("process uid unavailable")
	}
	return reaperProcessIdentity{PID: pid, PPID: ppid, UID: uid, StartTime: start}, nil
}

func dockerWaitExactChild(ctx context.Context, expected reaperProcessIdentity) error {
	return waitDockerReaperChild(ctx, expected, dockerReaperProcessIdentity, func(pid int) (bool, error) {
		var status syscall.WaitStatus
		waited, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
		return waited == pid, err
	})
}
