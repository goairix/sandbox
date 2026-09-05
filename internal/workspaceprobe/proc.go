package workspaceprobe

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type procFS struct{ root string }

func (p procFS) listSameUID(uid, selfPID int) ([]processIdentity, error) {
	entries, err := os.ReadDir(p.root)
	if err != nil {
		return nil, err
	}
	result := make([]processIdentity, 0)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || pid == selfPID || !entry.IsDir() {
			continue
		}
		identity, err := p.identity(pid)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		if identity.UID == uid {
			result = append(result, identity)
		}
	}
	return result, nil
}

func (p procFS) signal(pid int, signal syscall.Signal) error {
	return syscall.Kill(pid, signal)
}

func (p procFS) identity(pid int) (processIdentity, error) {
	stat, err := os.ReadFile(filepath.Join(p.root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return processIdentity{}, err
	}
	identity, err := parseProcStat(pid, stat)
	if err != nil {
		return processIdentity{}, err
	}
	status, err := os.ReadFile(filepath.Join(p.root, strconv.Itoa(pid), "status"))
	if err != nil {
		return processIdentity{}, err
	}
	uid, err := parseEffectiveUID(status)
	if err != nil {
		return processIdentity{}, err
	}
	identity.UID = uid
	return identity, nil
}

func (p procFS) descriptors(pid int) ([]descriptor, error) {
	fdDir := filepath.Join(p.root, strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return nil, err
	}
	result := make([]descriptor, 0, len(entries))
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			return nil, fmt.Errorf("unexpected descriptor name")
		}
		target, err := os.Readlink(filepath.Join(fdDir, entry.Name()))
		if err != nil {
			return nil, err
		}
		info, err := os.ReadFile(filepath.Join(p.root, strconv.Itoa(pid), "fdinfo", entry.Name()))
		if err != nil {
			return nil, err
		}
		flags, err := parseFDFlags(info)
		if err != nil {
			return nil, err
		}
		result = append(result, descriptor{Target: target, Flags: flags})
	}
	return result, nil
}

func parseProcStat(pid int, stat []byte) (processIdentity, error) {
	closeParen := bytes.LastIndexByte(bytes.TrimSpace(stat), ')')
	if closeParen < 0 || closeParen+2 >= len(stat) {
		return processIdentity{}, fmt.Errorf("malformed proc stat")
	}
	fields := bytes.Fields(stat[closeParen+1:])
	// fields starts at the process state (field 3); starttime is field 22.
	if len(fields) <= 19 || len(fields[0]) != 1 {
		return processIdentity{}, fmt.Errorf("malformed proc stat")
	}
	startTime, err := strconv.ParseUint(string(fields[19]), 10, 64)
	if err != nil {
		return processIdentity{}, fmt.Errorf("malformed proc start time")
	}
	return processIdentity{PID: pid, State: fields[0][0], StartTime: startTime}, nil
}

func parseEffectiveUID(status []byte) (int, error) {
	for _, line := range strings.Split(string(status), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "Uid:" {
			uid, err := strconv.Atoi(fields[2])
			if err != nil || uid < 0 {
				return 0, fmt.Errorf("malformed process uid")
			}
			return uid, nil
		}
	}
	return 0, fmt.Errorf("process uid is missing")
}

func parseFDFlags(info []byte) (int, error) {
	for _, line := range strings.Split(string(info), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "flags:" {
			flags, err := strconv.ParseUint(fields[1], 8, 32)
			if err != nil {
				return 0, fmt.Errorf("malformed descriptor flags")
			}
			return int(flags), nil
		}
	}
	return 0, fmt.Errorf("descriptor flags are missing")
}
