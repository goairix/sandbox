//go:build linux

package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func nativeWorkspaceSyncWorkerChild() error {
	b, err := BootstrapWorkspaceSyncWorker()
	if err != nil {
		return err
	}
	if err := b.ValidateCurrent(); err != nil {
		return err
	}
	r, err := b.WorkspaceSyncWorkerReference()
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(r)
}

type workspaceWorkerOutput struct{ bytes.Buffer }

func (b *workspaceWorkerOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 4096 {
		return 0, fmt.Errorf("child output exceeds 4096 bytes")
	}
	return b.Buffer.Write(p)
}

func TestWorkspaceSyncWorkerNativeOriginal(t *testing.T) {
	if os.Getenv(nativeModeEnv) != "workspace-worker-positive" {
		return
	}
	b, err := BootstrapWorkspaceSyncWorker()
	if err != nil {
		t.Fatal(err)
	}
	r, err := b.WorkspaceSyncWorkerReference()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("actual-kernel", func(t *testing.T) {
		if err := b.ValidateCurrent(); err != nil {
			t.Fatal(err)
		}
		if err := validateKernelSnapshot(b.Snapshot(), roleWorkspaceSyncWorker, false); err != nil {
			t.Fatal(err)
		}
		if r.PID != os.Getpid() || r.UID != uint32(os.Getuid()) || r.GID != uint32(os.Getgid()) {
			t.Fatal("reference disagrees with actual process")
		}
		t.Logf("original worker reference %+v", r)
	})
	t.Run("diagnostics-copy", func(t *testing.T) {
		copied := r
		copied.UID++
		s := b.Snapshot()
		s.UIDs[0]++
		got, err := b.WorkspaceSyncWorkerReference()
		if err != nil || got != r || b.Snapshot().UIDs[0] != r.UID {
			t.Fatal("diagnostics mutation affected original")
		}
	})
	t.Run("boundary-copy", func(t *testing.T) {
		copyBoundary := &KernelBoundary{self: b, role: b.role, pid: b.pid, verified: b.verified, workerSeal: b.workerSeal}
		if err := copyBoundary.ValidateCurrent(); !errors.Is(err, ErrUnsafeKernel) {
			t.Fatalf("copied boundary: %v", err)
		}
		if _, err := copyBoundary.WorkspaceSyncWorkerReference(); err == nil {
			t.Fatal("copied boundary reference")
		}
	})
	t.Run("original-pins", func(t *testing.T) {
		original := *b.workerSeal
		defer func() { *b.workerSeal = original }()
		// Corrupt each saved original expectation; fresh real observation must
		// reject rather than bless another valid identity or credential pair.
		typ := reflect.TypeOf(original.reference)
		for i := 0; i < typ.NumField(); i++ {
			if typ.Field(i).Name == "Incarnation" {
				continue
			}
			*b.workerSeal = original
			field := reflect.ValueOf(&b.workerSeal.reference).Elem().Field(i)
			switch field.Kind() {
			case reflect.String:
				field.SetString("86b3c998-f1b4-44b6-8797-dfba528adbb6")
			case reflect.Int:
				field.SetInt(field.Int() + 1)
			default:
				field.SetUint(field.Uint() + 1)
			}
			if err := b.ValidateCurrent(); !errors.Is(err, ErrUnsafeKernel) {
				t.Fatalf("pin %s: %v", typ.Field(i).Name, err)
			}
		}
		*b.workerSeal = original
		b.workerSeal.procIno++
		if err := b.ValidateCurrent(); !errors.Is(err, ErrUnsafeKernel) {
			t.Fatalf("proc pin: %v", err)
		}
		*b.workerSeal = original
		if err := b.ValidateCurrent(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("fixed-worker-child", func(t *testing.T) {
		// Separate at least one ordinary Linux start tick, including low HZ.
		time.Sleep(30 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/workspace-sync-worker.test")
		cmd.Env = []string{nativeModeEnv + "=workspace-worker-child", "GOMAXPROCS=2"}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		cmd.WaitDelay = 2 * time.Second
		var stdout, stderr workspaceWorkerOutput
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("child physically joined: %v: %s", err, stderr.String())
		}
		var child WorkspaceSyncWorkerReference
		decoder := json.NewDecoder(&stdout.Buffer)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&child); err != nil {
			t.Fatal(err)
		}
		if err := child.Validate(); err != nil {
			t.Fatal(err)
		}
		if child.PID == r.PID || child.StartTicks == r.StartTicks || child.Incarnation == r.Incarnation || child.BootID != r.BootID || child.UID != r.UID || child.GID != r.GID || child.ExecutableDev != r.ExecutableDev || child.ExecutableIno != r.ExecutableIno {
			t.Fatalf("child identity mismatch: %+v", child)
		}
		if err := b.ValidateCurrent(); err != nil {
			t.Fatal(err)
		}
		t.Logf("fixed binary child PID=%d start=%d incarnation=%s joined=true", child.PID, child.StartTicks, child.Incarnation)
	})
	t.Run("repeat-and-role", func(t *testing.T) {
		for _, bootstrap := range []func() (*KernelBoundary, error){BootstrapWorkspaceSyncWorker, BootstrapPID1, PrepareMonitor} {
			if other, err := bootstrap(); other != nil || !errors.Is(err, ErrUnsafeKernel) {
				t.Fatalf("repeat/role accepted: %v", err)
			}
		}
	})
	fmt.Println("workspace sync worker verified: original actual kernel and child identity")
}

func TestWorkspaceSyncWorkerNativeRejection(t *testing.T) {
	mode := os.Getenv(nativeModeEnv)
	if !strings.HasPrefix(mode, "workspace-worker-reject-") {
		return
	}
	want := map[string]string{"workspace-worker-reject-root": "worker PID/credentials", "workspace-worker-reject-gid": "worker PID/credentials", "workspace-worker-reject-nnp": "no_new_privs/securebits", "workspace-worker-reject-cap": "capability sets"}[mode]
	if want == "" {
		t.Fatal("unknown rejection fixture")
	}
	b, err := BootstrapWorkspaceSyncWorker()
	if b != nil || !errors.Is(err, ErrUnsafeKernel) || !strings.Contains(err.Error(), want) {
		t.Fatalf("actual %s unexpectedly accepted/wrong rejection: %v", mode, err)
	}
	if b, err := BootstrapWorkspaceSyncWorker(); b != nil || !errors.Is(err, ErrUnsafeKernel) || !strings.Contains(err.Error(), "already attempted") {
		t.Fatalf("failed bootstrap retry: %v", err)
	}
	fmt.Printf("workspace sync worker verified: %s rejected actual kernel; initialization consumed\n", mode)
}

func TestWorkspaceSyncWorkerProcStat(t *testing.T) {
	good := "17 (a ) b (c)) S " + strings.Repeat("0 ", 18) + "123 0 0\n"
	if n, err := parseWorkspaceSyncWorkerStat(good, 17); err != nil || n != 123 {
		t.Fatalf("comm parentheses: %d %v", n, err)
	}
	for name, raw := range map[string]string{
		"empty": "", "oversize": strings.Repeat("x", 16385), "missing-newline": strings.TrimSuffix(good, "\n"), "missing-comm": "17 S 123\n", "pid-zero": strings.Replace(good, "17 ", "0 ", 1), "pid-overflow": strings.Replace(good, "17 ", "2147483648 ", 1), "foreign-pid": strings.Replace(good, "17 ", "18 ", 1), "missing-start": "17 (x) S 0\n", "zero-start": strings.Replace(good, "123", "0", 1), "overflow-start": strings.Replace(good, "123", "18446744073709551616", 1), "signed-start": strings.Replace(good, "123", "+123", 1), "bad-state": strings.Replace(good, ") S ", ") ? ", 1), "nul": good + "\x00", "cr": good + "\r",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseWorkspaceSyncWorkerStat(raw, 17); err == nil {
				t.Fatal("malformed proc stat accepted")
			}
		})
	}
}
