package controlrunner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

type diagnosticFD struct {
	Number int    `json:"number"`
	Target string `json:"target"`
	Flags  string `json:"flags"`
}
type diagnosticProcess struct {
	PID  int    `json:"pid"`
	PPID string `json:"ppid"`
	Comm string `json:"comm"`
}
type diagnosticSnapshot struct {
	Version    uint32              `json:"version"`
	PID        int                 `json:"pid"`
	BootID     string              `json:"boot_id"`
	Gate       string              `json:"gate"`
	Active     int                 `json:"active"`
	ObservedAt time.Time           `json:"observed_at"`
	Complete   bool                `json:"complete"`
	Status     map[string]string   `json:"status"`
	FDs        []diagnosticFD      `json:"fds"`
	Processes  []diagnosticProcess `json:"processes"`
	Cgroup     map[string]string   `json:"cgroup"`
}

func readDiagnosticFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil {
		return nil, err
	}
	if len(b) > 16384 {
		return nil, fmt.Errorf("diagnostic file exceeds bound")
	}
	return b, nil
}
func diagnosticField(b []byte, name string) string {
	for _, line := range strings.Split(string(b), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && key == name {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// Called only on a finite protected request by the actual production PID1. The
// sampling request itself contributes one bounded connection/handler and FDs.
func (s *Supervisor) diagnostics(ctx context.Context) ([]byte, error) {
	if err := s.validateReceiver(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	snap := diagnosticSnapshot{Version: 1, PID: os.Getpid(), BootID: s.birth.BootID, Gate: "closed", Active: len(s.active), ObservedAt: time.Now().UTC(), Status: map[string]string{}, Cgroup: map[string]string{}}
	if s.admission && !s.closed && !s.failed && !s.failurePending.Load() {
		snap.Gate = "open"
	}
	s.mu.Unlock()
	status, err := readDiagnosticFile("/proc/self/status")
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"Threads", "VmRSS", "VmHWM", "FDSize"} {
		value := diagnosticField(status, key)
		if value == "" {
			return nil, ErrUnavailable
		}
		snap.Status[key] = value
	}
	fds, err := os.Open("/proc/self/fd")
	if err != nil {
		return nil, err
	}
	defer fds.Close()
	entries, err := fds.ReadDir(129)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > 128 {
		return nil, ErrUnavailable
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fd, err := strconv.Atoi(entry.Name())
		if err != nil {
			return nil, err
		}
		target, err := os.Readlink("/proc/self/fd/" + entry.Name())
		if err != nil || len(target) > 512 {
			return nil, ErrUnavailable
		}
		info, err := readDiagnosticFile("/proc/self/fdinfo/" + entry.Name())
		if err != nil {
			return nil, err
		}
		flags := diagnosticField(info, "flags")
		if flags == "" || len(flags) > 512 {
			return nil, ErrUnavailable
		}
		snap.FDs = append(snap.FDs, diagnosticFD{fd, target, flags})
	}
	proc, err := os.Open("/proc")
	if err != nil {
		return nil, err
	}
	defer proc.Close()
	entries, err = proc.ReadDir(257)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > 256 {
		return nil, ErrUnavailable
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if len(snap.Processes) >= 128 {
			return nil, ErrUnavailable
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b, err := readDiagnosticFile("/proc/" + entry.Name() + "/status")
		if err != nil {
			return nil, err
		}
		comm := diagnosticField(b, "Name")
		ppid := diagnosticField(b, "PPid")
		if comm == "" || len(comm) > 512 || ppid == "" {
			return nil, ErrUnavailable
		}
		snap.Processes = append(snap.Processes, diagnosticProcess{pid, ppid, comm})
	}
	membership, err := readDiagnosticFile("/proc/self/cgroup")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(membership)) != "0::/" {
		return nil, fmt.Errorf("diagnostics requires current private cgroup v2 root")
	}
	for _, name := range []string{"memory.current", "memory.peak", "pids.current"} {
		b, err := readDiagnosticFile("/sys/fs/cgroup/" + name)
		if err != nil {
			return nil, err
		}
		value := strings.TrimSpace(string(b))
		if _, err = strconv.ParseUint(value, 10, 64); err != nil {
			return nil, err
		}
		snap.Cgroup[name] = value
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	snap.Complete = true
	wire, err := json.Marshal(snap)
	if err != nil {
		return nil, err
	}
	if len(wire) > 16384 {
		return nil, ErrUnavailable
	}
	return wire, nil
}
