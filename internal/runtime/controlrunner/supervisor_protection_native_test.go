//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"encoding/json"
	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"golang.org/x/sys/unix"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTask3NativeSignedPATHAndProtectedResources(t *testing.T) {
	f := newNativeFixture(t)
	if err := os.WriteFile("/journal/protected/key-marker", []byte("harmless test marker, real key remains private memory"), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", "/journal/protected/control.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	req := controlprotocol.ExecutionRequest{Argv: []string{"user-helper", "binary", "literal $() a b"}, Env: map[string]string{"GOMAXPROCS": "2", "PATH": "/task3", "PROBE_PROTECTED": "1"}, UID: 1000, GID: 1000, WorkDir: "/tmp", TimeoutSeconds: 5}
	e, _ := f.start(t, req, 10*time.Second)
	observeActualUser(t, e)
	out, _ := drainExecution(t, e)
	var got struct {
		FDInfo    []monitorFD `json:"fd_info"`
		Denied    map[string]string
		Env, Argv []string
	}
	if err = json.Unmarshal(out, &got); err != nil {
		t.Fatalf("framed helper output %q: %v", out, err)
	}
	if !reflect.DeepEqual(got.Argv, req.Argv) || !reflect.DeepEqual(got.Env, []string{"GOMAXPROCS=2", "PATH=/task3", "PROBE_PROTECTED=1"}) {
		t.Fatalf("signed argv/env %+v", got)
	}
	for _, fd := range got.FDInfo {
		if fd.FD <= 2 {
			if !strings.HasPrefix(fd.Target, "pipe:[") {
				t.Fatalf("standard pipe %+v", fd)
			}
			continue
		}
		if fd.Flags&unix.FD_CLOEXEC == 0 {
			t.Fatalf("user nonstandard FD lacks CLOEXEC %+v", fd)
		}
		if fd.Target != "/sys/fs/cgroup/cpu.max" && fd.Target != "anon_inode:[eventpoll]" && fd.Target != "anon_inode:[eventfd]" {
			t.Fatalf("unexpected user inherited FD %+v", fd)
		}
	}
	if len(got.Denied) != 6 {
		t.Fatalf("missing actual protected probes %+v", got.Denied)
	}
	for path, err := range got.Denied {
		if err == "" || err == "ACCESS_GRANTED" {
			t.Fatalf("protected path granted %s", path)
		}
	}
	e.mu.Lock()
	stats := e.stats
	e.mu.Unlock()
	seen := map[int]bool{}
	for _, fd := range stats.FDs {
		seen[fd.FD] = true
		if fd.FD >= 3 && fd.Flags&unix.FD_CLOEXEC == 0 {
			t.Fatalf("management FD unsealed %+v", fd)
		}
	}
	if !seen[4] || !seen[5] {
		t.Fatalf("management control/result census unavailable %+v", stats)
	}
	t.Logf("actual trusted helper self-FDs %+v protected-denials %+v; production monitor self-FDs %+v", got.FDInfo, got.Denied, stats.FDs)
}
