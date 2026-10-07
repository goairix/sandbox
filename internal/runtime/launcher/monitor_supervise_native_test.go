//go:build linux && (amd64 || arm64)

package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func nativeRootMonitor(scenario string) (result error) {
	k, err := PrepareMonitor()
	if err != nil {
		return err
	}
	m, err := ConfineMonitor(k, 1000, 1000)
	if err != nil {
		return err
	}
	defer func() {
		n, e := nativeDrainCleanup(m)
		fmt.Printf("root test-only fallback reaped=%d err=%v\n", n, e)
		result = errors.Join(result, e)
	}()
	fmt.Println("phase root prepared")
	if err = nativeContinue(); err != nil {
		return err
	}
	userCase := scenario
	if scenario == "cancel_before_entry" || scenario == "renew_expiry" {
		userCase = "live"
	}
	cmd, err := startDrainCommand("drain-user", userCase, func(c *exec.Cmd) {
		c.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000, Groups: []uint32{}}}
	})
	if err != nil {
		return err
	}
	root := cmd.Process.Pid
	if err = cmd.Process.Release(); err != nil {
		return err
	}
	fmt.Printf("root registered pid=%d\n", root)
	if err = nativeContinue(); err != nil {
		return err
	}
	budget := time.Second
	if scenario == "renew_expiry" {
		budget = 4 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	if scenario == "cancel_before_entry" {
		cancel()
	}
	var o RootExitObservation
	if scenario == "renew_expiry" {
		now, e := monotonicNS()
		if e != nil {
			return e
		}
		life, e := NewRootLifecycle(now + int64(1500*time.Millisecond))
		if e != nil {
			return e
		}
		renewed := make(chan error, 1)
		go func() {
			time.Sleep(100 * time.Millisecond)
			c, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			renewed <- life.Renew(c, now+int64(2500*time.Millisecond))
		}()
		defer func() {
			select {
			case e := <-renewed:
				result = errors.Join(result, e)
			case <-time.After(1500 * time.Millisecond):
				result = errors.Join(result, fmt.Errorf("renew owner did not join"))
			}
		}()
		o, err = m.SuperviseRootWithLifecycle(ctx, root, life)
		if err == nil && (o.Reason != "authority_expired") {
			return fmt.Errorf("renew expiry reason %s", o.Reason)
		}
		fmt.Println("root renewal accepted then actual authority expiry cleanup")
	} else {
		o, err = m.SuperviseRoot(ctx, root)
	}
	if err != nil {
		return err
	}
	if o.RootPID != root || o.Drain.MonitorPID != os.Getpid() {
		return fmt.Errorf("wrong root attribution %+v", o)
	}
	raw := unix.WaitStatus(o.RootWaitStatus)
	if scenario == "natural" || scenario == "double" {
		if !raw.Exited() || raw.ExitStatus() != 0 {
			return fmt.Errorf("natural root raw status %x", raw)
		}
	} else if !raw.Signaled() || raw.Signal() != unix.SIGKILL {
		return fmt.Errorf("terminated root raw status %x", raw)
	}
	var status unix.WaitStatus
	if pid, e := unix.Wait4(-1, &status, unix.WNOHANG|unix.WALL, nil); !errors.Is(e, unix.ECHILD) {
		return fmt.Errorf("independent root ECHILD oracle pid=%d err=%v", pid, e)
	}
	fmt.Printf("root observation pid=%d raw=%d reason=%s reaped=%+v actual-ECHILD=true\n", o.RootPID, o.RootWaitStatus, o.Reason, o.Drain.Reaped)
	return nil
}

func TestMonitorSuperviseNative(t *testing.T) {
	requireNativeBoundary(t)
	for _, scenario := range []string{"natural", "double", "live", "cancel_before_entry", "renew_expiry"} {
		t.Run(scenario, func(t *testing.T) {
			var group []*nativeProcess
			registerDrainCleanup(t, &group)
			p, err := startNativeProcess("root-monitor", func(c *exec.Cmd) { c.Env = append(c.Env, drainCaseEnv+"="+scenario) })
			if err != nil {
				t.Fatal(err)
			}
			group = append(group, p)
			if err = p.phase("phase root prepared"); err != nil {
				t.Fatal(err)
			}
			if err = p.release(); err != nil {
				t.Fatal(err)
			}
			if err = p.phase("phase drain ready"); err != nil {
				t.Fatal(err)
			}
			users := snapshotDrainUsers(t)
			if len(users) == 0 {
				t.Fatal("no actual user")
			}
			if err = p.release(); err != nil {
				t.Fatal(err)
			}
			if err = p.wait(); err != nil {
				t.Fatalf("supervision failed: %v\n%s", err, p.output.String())
			}
			t.Logf("actual root supervision %s\n%s", scenario, p.output.String())
			for pid := range users {
				if _, err = os.Stat(fmt.Sprintf("/proc/%d", pid)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("user remains %d %v", pid, err)
				}
			}
		})
	}
}
