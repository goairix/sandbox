package apparmorloader

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type readiness struct {
	PID         int       `json:"pid"`
	StartTime   string    `json:"startTime"`
	CheckedAt   time.Time `json:"checkedAt"`
	ProfileName string    `json:"profileName"`
}

func processIdentity(procRoot string, pid int) (string, error) {
	if pid <= 0 {
		return "", errors.New("invalid readiness PID")
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return "", errors.New("loader process is not alive")
	}
	stat, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", errors.New("cannot verify loader process identity")
	}
	closing := strings.LastIndex(string(stat), ") ")
	if closing < 0 {
		return "", errors.New("invalid process stat")
	}
	// The final ')' closes comm, which itself can contain ')' or spaces.
	tail := string(stat)[closing+2:]
	fields := strings.Fields(tail)
	if len(fields) < 20 || fields[0] == "Z" || fields[0] == "X" || fields[0] == "T" || fields[0] == "t" {
		return "", errors.New("loader process is stopped")
	}
	if _, err = strconv.ParseUint(fields[19], 10, 64); err != nil {
		return "", errors.New("invalid process start identity")
	}
	return fields[19], nil
}

func writeReadiness(path, procRoot, profile string) error {
	pid := os.Getpid()
	start, err := processIdentity(procRoot, pid)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(readiness{PID: pid, StartTime: start, CheckedAt: time.Now().UTC(), ProfileName: profile})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".ready-*")
	if err != nil {
		return errors.New("cannot create readiness marker")
	}
	defer func() { _ = os.Remove(f.Name()) }() // rename already removes a published temporary path
	if _, err = f.Write(payload); err != nil {
		_ = f.Close() // preserve the original write failure
		return errors.New("cannot write readiness marker")
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return errors.New("cannot publish readiness marker")
	}
	return nil
}

// Ready rejects missing, stale, future or dead/reused-process success markers.
// ProcRoot is /proc in production; it must not refer to host /proc.
func Ready(path, procRoot string, maxAge time.Duration) error {
	if maxAge <= 0 {
		return errors.New("readiness max age must be positive")
	}
	if procRoot == "" {
		procRoot = "/proc"
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return errors.New("loader is NotReady")
	}
	var r readiness
	if err = json.Unmarshal(payload, &r); err != nil {
		return errors.New("invalid readiness marker")
	}
	age := time.Since(r.CheckedAt)
	if r.CheckedAt.IsZero() || age < 0 || age > maxAge {
		return errors.New("readiness check is not recent")
	}
	start, err := processIdentity(procRoot, r.PID)
	if err != nil {
		return err
	}
	if start != r.StartTime || r.ProfileName == "" {
		return fmt.Errorf("readiness process identity mismatch")
	}
	return nil
}
