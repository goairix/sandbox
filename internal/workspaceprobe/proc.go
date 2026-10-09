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
		snapshot, err := p.stableGroupSnapshot(pid)
		if err != nil {
			// Only disappearance of the entire group can be skipped. Missing
			// task evidence inside a still-existing group fails closed.
			if _, statErr := os.Stat(filepath.Join(p.root, strconv.Itoa(pid))); os.IsNotExist(statErr) {
				continue
			}
			return nil, err
		}
		sameUID, live := false, false
		for _, task := range snapshot.tasks {
			sameUID = sameUID || task.UID == uid
			live = live || !isExitedTask(task.State)
		}
		if sameUID && live {
			result = append(result, snapshot.identity())
		}
	}
	return result, nil
}

func (p procFS) signal(pid int, signal syscall.Signal) error {
	return syscall.Kill(pid, signal)
}

// procGroupSnapshot keeps task evidence local: the broker wire identity is
// still the leader PID, UID and starttime.
type procGroupSnapshot struct {
	leader  processIdentity
	tasks   []processIdentity
	stopped bool
}

func isExitedTask(state byte) bool { return state == 'Z' || state == 'X' || state == 'x' }

func (s procGroupSnapshot) identity() processIdentity {
	identity := s.leader
	if s.stopped {
		identity.State = 'T'
	} else if isStopped(identity.State) || isExitedTask(identity.State) {
		// The leader may be stopped or dead while a sibling can still execute.
		for _, task := range s.tasks {
			if !isExitedTask(task.State) && !isStopped(task.State) {
				identity.State = task.State
				break
			}
		}
	}
	return identity
}

func (p procFS) identity(pid int) (processIdentity, error) {
	snapshot, err := p.stableGroupSnapshot(pid)
	if err != nil {
		return processIdentity{}, err
	}
	return snapshot.identity(), nil
}

func readProcIdentity(dir string, pid int) (processIdentity, error) {
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return processIdentity{}, err
	}
	identity, err := parseProcStat(pid, stat)
	if err != nil {
		return processIdentity{}, err
	}
	status, err := os.ReadFile(filepath.Join(dir, "status"))
	if err != nil {
		return processIdentity{}, err
	}
	identity.UID, err = parseEffectiveUID(status)
	return identity, err
}

func readTaskIDs(dir string) ([]int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	ids := make([]int, 0, len(entries))
	for _, entry := range entries {
		tid, err := strconv.Atoi(entry.Name())
		if err != nil || tid <= 0 || !entry.IsDir() {
			return nil, fmt.Errorf("unexpected proc task entry")
		}
		ids = append(ids, tid)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("empty proc task set")
	}
	return ids, nil
}

func (p procFS) groupSnapshot(pid int) (procGroupSnapshot, error) {
	dir := filepath.Join(p.root, strconv.Itoa(pid))
	leader, err := readProcIdentity(dir, pid)
	if err != nil {
		return procGroupSnapshot{}, err
	}
	taskDir := filepath.Join(dir, "task")
	tids, err := readTaskIDs(taskDir)
	if err != nil {
		return procGroupSnapshot{}, err
	}
	if uint64(len(tids)) != leader.threads {
		return procGroupSnapshot{}, fmt.Errorf("proc task count changed")
	}
	snapshot := procGroupSnapshot{leader: leader, stopped: true}
	foundLeader, live := false, false
	for _, tid := range tids {
		task, err := readProcIdentity(filepath.Join(taskDir, strconv.Itoa(tid)), tid)
		if err != nil {
			return procGroupSnapshot{}, err
		}
		if task.threads != leader.threads {
			return procGroupSnapshot{}, fmt.Errorf("proc task count changed")
		}
		if tid == pid {
			if task.UID != leader.UID || task.StartTime != leader.StartTime {
				return procGroupSnapshot{}, fmt.Errorf("proc leader identity changed")
			}
			snapshot.leader = task
			foundLeader = true
		}
		if !isExitedTask(task.State) {
			live = true
			snapshot.stopped = snapshot.stopped && isStopped(task.State)
		}
		snapshot.tasks = append(snapshot.tasks, task)
	}
	if !foundLeader {
		return procGroupSnapshot{}, fmt.Errorf("proc leader task is missing")
	}
	snapshot.stopped = snapshot.stopped && live
	after, err := readTaskIDs(taskDir)
	if err != nil {
		return procGroupSnapshot{}, err
	}
	if len(tids) != len(after) {
		return procGroupSnapshot{}, fmt.Errorf("proc task set changed")
	}
	for i, tid := range tids {
		if tid != after[i] {
			return procGroupSnapshot{}, fmt.Errorf("proc task set changed")
		}
	}
	return snapshot, nil
}

func sameTaskIdentities(before, after procGroupSnapshot, requireSameState bool) bool {
	if len(before.tasks) != len(after.tasks) {
		return false
	}
	for i, task := range before.tasks {
		current := after.tasks[i]
		if task.PID != current.PID || task.UID != current.UID || task.StartTime != current.StartTime || task.threads != current.threads ||
			(requireSameState && task.State != current.State) || isExitedTask(task.State) != isExitedTask(current.State) {
			return false
		}
	}
	return true
}

func (p procFS) stableGroupSnapshot(pid int) (procGroupSnapshot, error) {
	before, err := p.groupSnapshot(pid)
	if err != nil {
		return procGroupSnapshot{}, err
	}
	after, err := p.groupSnapshot(pid)
	if err != nil {
		return procGroupSnapshot{}, err
	}
	if !sameTaskIdentities(before, after, false) {
		return procGroupSnapshot{}, fmt.Errorf("proc task identities changed")
	}
	// A running group can transition through S/R while SIGSTOP is pending.
	// It only becomes stopped evidence when both complete passes prove it.
	after.stopped = before.stopped && after.stopped
	if !after.stopped && isStopped(after.leader.State) {
		after.leader.State = 'R'
	}
	return after, nil
}

func (p procFS) descriptors(pid int) ([]descriptor, error) {
	before, err := p.stableGroupSnapshot(pid)
	if err != nil {
		return nil, err
	}
	if !before.stopped {
		return nil, fmt.Errorf("proc thread group is not stopped")
	}
	result := make([]descriptor, 0)
	for _, task := range before.tasks {
		// Dead tasks have released their files; in particular, /proc/PID/fd
		// may be unavailable once the group leader has exited.
		if isExitedTask(task.State) {
			continue
		}
		taskDir := filepath.Join(p.root, strconv.Itoa(pid), "task", strconv.Itoa(task.PID))
		fds, err := readTaskDescriptors(taskDir)
		if err != nil {
			return nil, err
		}
		result = append(result, fds...)
	}
	after, err := p.stableGroupSnapshot(pid)
	if err != nil {
		return nil, err
	}
	if !after.stopped || !sameTaskIdentities(before, after, true) {
		return nil, fmt.Errorf("proc task evidence changed during descriptor scan")
	}
	return result, nil
}

func readTaskDescriptors(taskDir string) ([]descriptor, error) {
	fdDir := filepath.Join(taskDir, "fd")
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return nil, err
	}
	result := make([]descriptor, 0, len(entries))
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil || fd < 0 {
			return nil, fmt.Errorf("unexpected descriptor name")
		}
		target, err := os.Readlink(filepath.Join(fdDir, entry.Name()))
		if err != nil {
			return nil, err
		}
		info, err := os.ReadFile(filepath.Join(taskDir, "fdinfo", entry.Name()))
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
	// num_threads is field 20, read from the same stat snapshot as state.
	threads, err := strconv.ParseUint(string(fields[17]), 10, 64)
	if err != nil || threads == 0 {
		return processIdentity{}, fmt.Errorf("malformed proc thread count")
	}
	return processIdentity{PID: pid, State: fields[0][0], StartTime: startTime, threads: threads}, nil
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
