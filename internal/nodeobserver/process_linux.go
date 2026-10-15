//go:build linux && (amd64 || arm64)

package nodeobserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type descriptorIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

type processFacts struct {
	PID       int                `json:"pid"`
	Starttime uint64             `json:"starttime"`
	PIDNS     descriptorIdentity `json:"pid_namespace"`
	MountNS   descriptorIdentity `json:"mount_namespace"`
	Cgroup    cgroupFacts        `json:"cgroup"`
}

type processAnchor struct {
	facts                       processFacts
	proc, pidfd, pidns, mountns int
	cgroup                      *cgroupAnchor
}

func bootID() (string, error) {
	wire, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || len(wire) != 37 || wire[36] != '\n' {
		return "", fmt.Errorf("host boot ID unavailable")
	}
	return string(wire[:36]), nil
}

func descriptorStat(fd int) (descriptorIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return descriptorIdentity{}, err
	}
	return descriptorIdentity{Device: uint64(stat.Dev), Inode: stat.Ino}, nil
}

func readProcAt(fd int, name string, limit int64) ([]byte, error) {
	child, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(child), name)
	wire, readErr := io.ReadAll(io.LimitReader(f, limit+1))
	if err := errors.Join(readErr, f.Close()); err != nil {
		return nil, err
	}
	if int64(len(wire)) > limit {
		return nil, fmt.Errorf("kernel process record exceeds bound")
	}
	return wire, nil
}

func parseStarttime(wire []byte) (uint64, error) {
	end := bytes.LastIndexByte(wire, ')')
	if end < 1 {
		return 0, fmt.Errorf("invalid process stat")
	}
	fields := bytes.Fields(wire[end+1:])
	if len(fields) < 20 {
		return 0, fmt.Errorf("incomplete process stat")
	}
	start, err := strconv.ParseUint(string(fields[19]), 10, 64)
	if err != nil || start == 0 {
		return 0, fmt.Errorf("invalid process starttime")
	}
	return start, nil
}

func privatePID1(wire []byte) error {
	found := false
	for _, line := range strings.Split(string(wire), "\n") {
		if !strings.HasPrefix(line, "NSpid:") {
			continue
		}
		fields := strings.Fields(line)
		if found || len(fields) < 3 || fields[len(fields)-1] != "1" {
			return fmt.Errorf("peer must be PID1 of a private child namespace")
		}
		found = true
	}
	if !found {
		return fmt.Errorf("kernel NSpid missing")
	}
	return nil
}

func openProcess(pid int) (_ *processAnchor, result error) {
	if pid <= 1 {
		return nil, fmt.Errorf("invalid host-view peer PID")
	}
	a := &processAnchor{proc: -1, pidfd: -1, pidns: -1, mountns: -1}
	defer func() {
		if result != nil {
			a.close()
		}
	}()
	var err error
	a.pidfd, err = unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, err
	}
	a.proc, err = unix.Open(fmt.Sprintf("/proc/%d", pid), unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	var fs unix.Statfs_t
	if err = unix.Fstatfs(a.proc, &fs); err != nil || fs.Type != unix.PROC_SUPER_MAGIC {
		return nil, fmt.Errorf("peer proc anchor is not procfs")
	}
	wire, err := readProcAt(a.proc, "stat", 65536)
	if err != nil {
		return nil, err
	}
	a.facts.PID = pid
	a.facts.Starttime, err = parseStarttime(wire)
	if err != nil {
		return nil, err
	}
	status, err := readProcAt(a.proc, "status", 65536)
	if err != nil || privatePID1(status) != nil {
		return nil, fmt.Errorf("peer lacks private PID1 topology")
	}
	a.pidns, err = unix.Openat(a.proc, "ns/pid", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	a.mountns, err = unix.Openat(a.proc, "ns/mnt", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	a.facts.PIDNS, err = descriptorStat(a.pidns)
	if err != nil {
		return nil, err
	}
	a.facts.MountNS, err = descriptorStat(a.mountns)
	if err != nil {
		return nil, err
	}
	var self unix.Stat_t
	if err := unix.Stat("/proc/self/ns/pid", &self); err != nil {
		return nil, err
	}
	if self.Ino == a.facts.PIDNS.Inode && uint64(self.Dev) == a.facts.PIDNS.Device {
		return nil, fmt.Errorf("peer shares observer PID namespace")
	}
	membership, err := readProcAt(a.proc, "cgroup", 4096)
	if err != nil {
		return nil, err
	}
	a.cgroup, err = openCgroup(membership, a.facts.PIDNS)
	if err != nil {
		return nil, err
	}
	a.facts.Cgroup = a.cgroup.facts
	if err := a.revalidate(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *processAnchor) revalidate() error {
	if a == nil || a.proc < 0 || a.pidfd < 0 {
		return fmt.Errorf("process anchors unavailable")
	}
	poll := []unix.PollFd{{Fd: int32(a.pidfd), Events: unix.POLLIN}}
	if n, err := unix.Poll(poll, 0); err != nil || n != 0 {
		return fmt.Errorf("enrolled process already exited")
	}
	wire, err := readProcAt(a.proc, "stat", 65536)
	if err != nil {
		return err
	}
	start, err := parseStarttime(wire)
	if err != nil || start != a.facts.Starttime {
		return fmt.Errorf("enrolled process starttime changed")
	}
	for name, expected := range map[string]descriptorIdentity{"ns/pid": a.facts.PIDNS, "ns/mnt": a.facts.MountNS} {
		fd, err := unix.Openat(a.proc, name, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		actual, statErr := descriptorStat(fd)
		closeErr := unix.Close(fd)
		if statErr != nil || closeErr != nil || actual != expected {
			return fmt.Errorf("enrolled process namespace changed")
		}
	}
	membership, err := readProcAt(a.proc, "cgroup", 4096)
	if err != nil || string(membership) != a.cgroup.membership {
		return fmt.Errorf("enrolled process cgroup changed")
	}
	return a.cgroup.revalidate(a.facts.PIDNS)
}

func (a *processAnchor) kill() error {
	return unix.PidfdSendSignal(a.pidfd, unix.SIGKILL, nil, 0)
}

func (a *processAnchor) waitDeathAndEmpty(ctx context.Context) error {
	dead := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		poll := []unix.PollFd{{Fd: int32(a.pidfd), Events: unix.POLLIN}}
		n, err := unix.Poll(poll, 20)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil || (n > 0 && poll[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0) {
			return fmt.Errorf("pidfd death wait failed: %v", err)
		}
		dead = dead || (n > 0 && poll[0].Revents&unix.POLLIN != 0)
		empty, err := a.cgroup.empty()
		if err != nil {
			return err
		}
		if dead && empty {
			return nil
		}
		if dead {
			timer := time.NewTimer(20 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
}

func (a *processAnchor) close() error {
	if a == nil {
		return nil
	}
	var result error
	for _, fd := range []*int{&a.proc, &a.pidfd, &a.pidns, &a.mountns} {
		if *fd >= 0 {
			result = errors.Join(result, unix.Close(*fd))
			*fd = -1
		}
	}
	return errors.Join(result, a.cgroup.close())
}
