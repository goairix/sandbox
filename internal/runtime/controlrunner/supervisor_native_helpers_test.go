//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func waitExecutionReady(t *testing.T, e *Execution) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for {
		e.mu.Lock()
		ready := e.rootPID > 1
		e.mu.Unlock()
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("monitor root registration timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func drainExecution(t *testing.T, e *Execution) ([]byte, []byte) {
	t.Helper()
	var out, stderr bytes.Buffer
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	for {
		select {
		case frame, ok := <-e.output:
			if !ok {
				<-e.ownerDone
				return out.Bytes(), stderr.Bytes()
			}
			if out.Len()+stderr.Len()+len(frame.data) > 1048576 {
				t.Fatal("test transcript bound")
			}
			if frame.kind == monitorStdout {
				out.Write(frame.data)
			} else {
				stderr.Write(frame.data)
			}
		case <-timer.C:
			t.Fatal("execution did not complete")
		}
	}
}
func observeActualUser(t *testing.T, e *Execution) map[string]string {
	t.Helper()
	waitExecutionReady(t, e)
	e.mu.Lock()
	root, monitor, stats := e.rootPID, e.monitorPID, e.stats
	e.mu.Unlock()
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", root))
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok {
			fields[k] = strings.Join(strings.Fields(v), " ")
		}
	}
	expected := map[string]string{"Uid": "1000 1000 1000 1000", "Gid": "1000 1000 1000 1000", "Groups": "", "CapInh": "0000000000000000", "CapPrm": "0000000000000000", "CapEff": "0000000000000000", "CapBnd": "0000000000000000", "CapAmb": "0000000000000000", "NoNewPrivs": "1", "Seccomp": "2", "PPid": fmt.Sprint(monitor)}
	for k, want := range expected {
		if fields[k] != want {
			t.Fatalf("actual user %s=%q want%q", k, fields[k], want)
		}
	}
	group, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", root))
	if err != nil {
		t.Fatal(err)
	}
	own, err := os.ReadFile("/proc/self/cgroup")
	if err != nil || !bytes.Equal(group, own) {
		t.Fatalf("actual cgroup %q own%q err%v", group, own, err)
	}
	fields["cgroup"] = string(group)
	t.Logf("actual parent-observed user pid=%d status=%q cgroup=%q; actual production-monitor self-census=%+v (test PID1 orchestration remains part of fixture cgroup cost)", root, raw, group, stats)
	return fields
}

func procPath(pid int) string { return fmt.Sprintf("/proc/%d", pid) }
