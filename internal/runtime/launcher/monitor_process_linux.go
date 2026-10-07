//go:build linux && (amd64 || arm64)

package launcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type drainStatus struct {
	pid, tgid, ppid       int
	uids, gids            [4]uint32
	caps                  [5]uint64
	nnp, seccomp, filters uint32
}

// A child may be a zombie with zero Threads. Parse identity independently from
// the live-thread kernel parser; unrelated proc records need only valid identity.
func parseDrainStatus(data []byte, expected int) (drainStatus, error) {
	return parseDrainFields(data, expected, false)
}

func parseDrainFields(data []byte, expected int, identityOnly bool) (drainStatus, error) {
	var s drainStatus
	bad := func() (drainStatus, error) {
		return drainStatus{}, fmt.Errorf("%w: malformed child status", ErrUnsafeKernel)
	}
	if len(data) == 0 || len(data) > maxStatusBytes || data[len(data)-1] != '\n' || bytes.ContainsAny(data, "\r\x00") {
		return bad()
	}
	seen := uint16(0)
	for _, line := range strings.Split(string(data[:len(data)-1]), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		index := -1
		switch key {
		case "Pid":
			index = 0
		case "Tgid":
			index = 1
		case "PPid":
			index = 2
		case "Uid":
			index = 3
		case "Gid":
			index = 4
		case "CapInh":
			index = 5
		case "CapPrm":
			index = 6
		case "CapEff":
			index = 7
		case "CapBnd":
			index = 8
		case "CapAmb":
			index = 9
		case "NoNewPrivs":
			index = 10
		case "Seccomp":
			index = 11
		case "Seccomp_filters":
			index = 12
		default:
			continue
		}
		if identityOnly && index > 2 {
			continue
		}
		bit := uint16(1) << index
		if seen&bit != 0 {
			return bad()
		}
		seen |= bit
		fields := strings.Fields(value)
		want := 1
		if index == 3 || index == 4 {
			want = 4
		}
		if len(fields) != want {
			return bad()
		}
		for i, f := range fields {
			base, bits := 10, 32
			if index <= 2 {
				bits = 31
			}
			if index >= 5 && index <= 9 {
				base, bits = 16, 64
			}
			v, err := parseUnsigned(f, base, bits)
			if err != nil {
				return bad()
			}
			switch index {
			case 0:
				s.pid = int(v)
			case 1:
				s.tgid = int(v)
			case 2:
				s.ppid = int(v)
			case 3:
				s.uids[i] = uint32(v)
			case 4:
				s.gids[i] = uint32(v)
			case 5, 6, 7, 8, 9:
				s.caps[index-5] = v
			case 10:
				s.nnp = uint32(v)
			case 11:
				s.seccomp = uint32(v)
			case 12:
				s.filters = uint32(v)
			}
		}
	}
	required := uint16((1 << 13) - 1)
	if identityOnly {
		required = 7
	}
	if seen != required || s.pid != expected || s.pid <= 0 || s.tgid != s.pid {
		return bad()
	}
	return s, nil
}

func (m *MonitorBoundary) checkDrainChild(s drainStatus, cgroup []byte) error {
	if s.pid <= 1 || s.pid == m.pid || s.ppid != m.pid || s.uids != [4]uint32{m.uid, m.uid, m.uid, m.uid} || s.gids != [4]uint32{m.gid, m.gid, m.gid, m.gid} || s.caps != [5]uint64{} || s.nnp != 1 || s.seccomp != 2 || s.filters < m.installedFilters {
		return fmt.Errorf("%w: direct child confinement mismatch pid=%d", ErrUnsafeKernel, s.pid)
	}
	if err := validateCgroupMembership(cgroup); err != nil {
		return err
	}
	if !bytes.Equal(cgroup, m.cgroup) {
		return fmt.Errorf("%w: direct child cgroup mismatch", ErrUnsafeKernel)
	}
	return nil
}

func readDrainFile(dir int, name string, limit int64) (data []byte, result error) {
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() {
		if e := f.Close(); e != nil {
			result = unavailable("close child file", e)
		}
	}()
	data, err = io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: child %s byte limit", ErrUnsafeKernel, name)
	}
	return data, nil
}

func drainProcessEntry(name string, count *int) (int, error) {
	if name == "" {
		return 0, nil
	}
	for _, c := range name {
		if c < '0' || c > '9' {
			return 0, nil
		}
	}
	*count++
	if *count > maxDrainProcesses {
		return 0, fmt.Errorf("%w: drain process scan limit", ErrUnsafeKernel)
	}
	pid, err := strconv.ParseUint(name, 10, 31)
	if err != nil || pid == 0 || strconv.FormatUint(pid, 10) != name {
		return 0, fmt.Errorf("%w: invalid proc entry", ErrUnsafeKernel)
	}
	return int(pid), nil
}

func (m *MonitorBoundary) killDirectChildren(ctx context.Context) (result error) {
	fd, err := unix.Open("/proc", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return unavailable("drain proc", err)
	}
	dir := os.NewFile(uintptr(fd), "/proc")
	defer func() {
		if e := dir.Close(); e != nil {
			result = unavailable("close proc directory", e)
		}
	}()
	count := 0
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		names, readErr := dir.Readdirnames(128)
		for _, name := range names {
			if err = ctx.Err(); err != nil {
				return err
			}
			pid, e := drainProcessEntry(name, &count)
			if e != nil {
				return e
			}
			if pid == 0 || pid == 1 || pid == m.pid {
				continue
			}
			e = m.killPinnedChild(ctx, fd, name, pid)
			if e != nil && !vanishedDrainProcess(e) {
				return unavailable("drain pinned child", e)
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return unavailable("drain proc entries", readErr)
		}
	}
}

func (m *MonitorBoundary) killPinnedChild(ctx context.Context, proc int, name string, pid int) (result error) {
	fd, err := unix.Openat(proc, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() {
		if e := unix.Close(fd); e != nil {
			result = unavailable("close child directory", e)
		}
	}()
	data, err := readDrainFile(fd, "status", maxStatusBytes)
	if err != nil {
		return err
	}
	s, err := parseDrainFields(data, pid, true)
	if err != nil {
		return err
	}
	if s.ppid != m.pid {
		return nil
	}
	s, err = parseDrainStatus(data, pid)
	if err != nil {
		return err
	}
	group, err := readDrainFile(fd, "cgroup", maxCgroupBytes)
	if err != nil {
		return err
	}
	if err = m.checkDrainChild(s, group); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	// Re-read through the same directory FD immediately before signaling; no path
	// lookup can retarget this FD to a recycled PID. Recheck the complete seal too.
	data, err = readDrainFile(fd, "status", maxStatusBytes)
	if err != nil {
		return err
	}
	s, err = parseDrainStatus(data, pid)
	if err != nil {
		return err
	}
	if err = m.checkDrainChild(s, group); err != nil {
		return err
	}
	return unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
}
