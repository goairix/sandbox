//go:build linux && (amd64 || arm64)

package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const drainCaseEnv = "SANDBOX_DRAIN_CASE"

// Fixed test modes only. User children never have a Cmd.Wait goroutine in the
// monitor; Start uses *os.File streams and Release closes only Go's process FD.
func nativeDrainMode(mode string) error {
	switch mode {
	case "drain-monitor":
		return nativeDrainMonitor(os.Getenv(drainCaseEnv))
	case "drain-user":
		return nativeDrainUser(os.Getenv(drainCaseEnv))
	case "drain-middle":
		child, err := startDrainCommand("drain-leaf", "", nil)
		if err != nil {
			return err
		}
		return child.Process.Release()
	case "drain-leaf":
		fmt.Printf("drain-leaf pid=%d ppid=%d\n", os.Getpid(), os.Getppid())
		for {
			time.Sleep(time.Hour)
		}
	case "drain-sender":
		target, err := strconv.Atoi(os.Getenv("SANDBOX_DRAIN_TARGET"))
		if err != nil || target <= 1 {
			return fmt.Errorf("invalid fixed signal target")
		}
		if err = unix.Kill(target, unix.SIGCONT); err != nil {
			return err
		}
		fmt.Printf("drain-sigcont delivered target=%d\n", target)
		fmt.Println("phase drain ready")
		for {
			if err = unix.Kill(target, unix.SIGCONT); err != nil && !errors.Is(err, unix.ESRCH) {
				return err
			}
			time.Sleep(time.Millisecond)
		}
	default:
		return fmt.Errorf("unknown drain mode")
	}
}

func startDrainCommand(mode, scenario string, configure func(*exec.Cmd)) (*exec.Cmd, error) {
	binary, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(binary)
	cmd.Env = []string{nativeModeEnv + "=" + mode, drainCaseEnv + "=" + scenario, "GOMAXPROCS=2"}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if configure != nil {
		configure(cmd)
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func nativeDrainUser(scenario string) error {
	if os.Getuid() != 1000 || os.Getgid() != 1000 {
		return fmt.Errorf("drain user identity")
	}
	switch scenario {
	case "clone-zero", "clone-parent":
		return unix.Exec("/drainclone", []string{"/drainclone", scenario}, []string{"GOMAXPROCS=2"})
	case "double", "subreaper":
		if scenario == "subreaper" {
			if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
				return err
			}
		}
		middle, err := startDrainCommand("drain-middle", "", func(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} })
		if err != nil {
			return err
		}
		if err = middle.Wait(); err != nil {
			return err
		}
		fmt.Printf("drain-middle joined pid=%d subreaper=%t\n", middle.Process.Pid, scenario == "subreaper")
		fmt.Println("phase drain ready")
		if scenario == "double" {
			return nil
		}
	case "forks":
		// Six user forks cap this adversary far below the unchanged 128 task limit.
		for i := 0; i < 6; i++ {
			c, err := startDrainCommand("drain-leaf", "", nil)
			if err != nil {
				return err
			}
			if err = c.Process.Release(); err != nil {
				return err
			}
			if i == 0 {
				fmt.Println("phase drain ready")
			}
			time.Sleep(5 * time.Millisecond)
		}
	case "sigcont":
		leaf, err := startDrainCommand("drain-leaf", "", nil)
		if err != nil {
			return err
		}
		target := leaf.Process.Pid
		if err = leaf.Process.Release(); err != nil {
			return err
		}
		sender, err := startDrainCommand("drain-sender", "", func(c *exec.Cmd) { c.Env = append(c.Env, "SANDBOX_DRAIN_TARGET="+strconv.Itoa(target)) })
		if err != nil {
			return err
		}
		if err = sender.Process.Release(); err != nil {
			return err
		}
	case "zombie", "natural":
		fmt.Println("phase drain ready")
		return nil
	case "live", "cancel":
		fmt.Println("phase drain ready")
	default:
		return fmt.Errorf("unknown fixed drain case %q", scenario)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func nativeDrainMonitor(scenario string) (result error) {
	k, err := PrepareMonitor()
	if err != nil {
		return err
	}
	m, err := ConfineMonitor(k, 1000, 1000)
	if err != nil {
		return err
	}
	// On success this proves no residual children. On failure this remains
	// diagnostic cleanup by the same sole wait owner; poison is never cleared.
	defer func() {
		n, e := nativeDrainCleanup(m)
		fmt.Printf("drain diagnostic cleanup reaped=%d poison=%t err=%v\n", n, m.poisoned, e)
		result = errors.Join(result, e)
	}()
	fmt.Println("phase drain prepared")
	if err = nativeContinue(); err != nil {
		return err
	}
	cmd, err := startDrainCommand("drain-user", scenario, func(c *exec.Cmd) {
		c.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000, Groups: []uint32{}}}
	})
	if err != nil {
		return err
	}
	root := cmd.Process.Pid
	fmt.Printf("drain-root pid=%d\n", root)
	if err = cmd.Process.Release(); err != nil {
		return err
	}
	if err = nativeContinue(); err != nil {
		return err
	}
	if scenario == "natural" {
		var status unix.WaitStatus
		got, e := unix.Wait4(root, &status, unix.WALL, nil)
		if e != nil || got != root || !status.Exited() {
			return fmt.Errorf("natural sole-owner wait: pid=%d status=%x err=%v", got, status, e)
		}
		fmt.Printf("natural wait pid=%d status=%x\n", got, status)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if scenario == "cancel" {
		before := runtime.NumGoroutine()
		o, e := m.drainWith(ctx, drainCalls{validate: func() error { e := m.validateCurrentLocked(); cancel(); return e }, wait: func(s *unix.WaitStatus) (int, error) { return unix.Wait4(-1, s, unix.WNOHANG|unix.WALL, nil) }, scan: m.killDirectChildren})
		if !errors.Is(e, context.Canceled) || o.MonitorPID != 0 || o.Reaped != nil || !m.poisoned {
			return fmt.Errorf("canceled drain did not preserve unknown: %+v %v", o, e)
		}
		time.Sleep(30 * time.Millisecond)
		data, e := readBounded(fmt.Sprintf("/proc/%d/status", root), maxStatusBytes)
		if e != nil {
			return e
		}
		s, e := parseDrainStatus(data, root)
		if e != nil || s.ppid != m.pid {
			return fmt.Errorf("canceled child disappeared: %v", e)
		}
		if runtime.NumGoroutine() != before {
			return fmt.Errorf("drain left goroutine: before=%d after=%d", before, runtime.NumGoroutine())
		}
		retryCtx, retryCancel := context.WithTimeout(context.Background(), time.Second)
		defer retryCancel()
		if o, e = m.Drain(retryCtx); e == nil || o.MonitorPID != 0 {
			return fmt.Errorf("poison retry returned success")
		}
		fmt.Println("drain canceled: zero observation, poison retained, child alive, no detached worker")
		return nil
	}
	observation, err := m.Drain(ctx)
	if err != nil {
		return err
	}
	// Independent test oracle, outside Drain: the same sole wait owner must see
	// real kernel ECHILD, and a repeated Drain must validate that state anew.
	var status unix.WaitStatus
	pid, e := unix.Wait4(-1, &status, unix.WNOHANG|unix.WALL, nil)
	if !errors.Is(e, unix.ECHILD) {
		return fmt.Errorf("post-drain actual wait pid=%d status=%x err=%v", pid, status, e)
	}
	fmt.Printf("drain oracle wait4(-1,WNOHANG|__WALL) pid=%d errno=%v\n", pid, e)
	again, err := m.Drain(ctx)
	if err != nil || len(again.Reaped) != len(observation.Reaped) {
		return fmt.Errorf("repeat drain: %+v %v", again, err)
	}
	raw, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	fmt.Printf("drain-observation %s\n", raw)
	return nil
}

// Test-only fallback uses fresh context without touching poison or returning a
// LocalDrainObservation. Call only as the sole owner, after all spawn completed.
func nativeDrainCleanup(m *MonitorBoundary) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	count := 0
	for {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		var status unix.WaitStatus
		pid, err := unix.Wait4(-1, &status, unix.WNOHANG|unix.WALL, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.ECHILD) {
			return count, nil
		}
		if err != nil {
			return count, err
		}
		if pid > 0 {
			count++
			fmt.Printf("diagnostic actual wait pid=%d status=%x\n", pid, status)
			continue
		}
		if err = m.killDirectChildren(ctx); err != nil {
			return count, err
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type drainNativeProcess struct {
	status          drainStatus
	state           string
	signal, session int
	start           string
}

// Trusted PID1 reads actual proc rather than trusting user-supplied PID text.
// The map is test evidence only; product Drain retains no transitive PID map.
func snapshotDrainUsers(t *testing.T) map[int]drainNativeProcess {
	t.Helper()
	dir, err := os.Open("/proc")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	out := map[int]drainNativeProcess{}
	group, err := readBounded("/proc/self/cgroup", maxCgroupBytes)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for {
		entries, readErr := dir.Readdirnames(128)
		for _, name := range entries {
			pid, e := strconv.Atoi(name)
			if e != nil || pid <= 1 {
				continue
			}
			count++
			if count > 4096 {
				t.Fatal("test proc evidence scan limit")
			}
			data, e := readBounded("/proc/"+name+"/status", maxStatusBytes)
			if vanishedDrainProcess(e) {
				continue
			}
			if e != nil {
				t.Fatal(e)
			}
			s, e := parseDrainStatus(data, pid)
			if e != nil {
				t.Fatal(e)
			}
			if s.uids[0] != 1000 {
				continue
			}
			if s.uids != [4]uint32{1000, 1000, 1000, 1000} || s.gids != [4]uint32{1000, 1000, 1000, 1000} || s.caps != [5]uint64{} || s.nnp != 1 || s.seccomp != 2 || s.filters < 2 {
				t.Fatalf("actual user seal: %+v", s)
			}
			cg, e := readBounded("/proc/"+name+"/cgroup", maxCgroupBytes)
			if e != nil {
				t.Fatal(e)
			}
			if !bytes.Equal(cg, group) {
				t.Fatal("actual user cgroup mismatch")
			}
			stat, e := readBounded("/proc/"+name+"/stat", 4096)
			if vanishedDrainProcess(e) {
				continue
			}
			if e != nil {
				t.Fatal(e)
			}
			fields := strings.Fields(string(stat)[strings.LastIndexByte(string(stat), ')')+1:])
			if len(fields) < 36 {
				t.Fatal("short proc stat")
			}
			sig, e := strconv.Atoi(fields[35])
			if e != nil {
				t.Fatal(e)
			}
			sid, e := strconv.Atoi(fields[3])
			if e != nil {
				t.Fatal(e)
			}
			out[pid] = drainNativeProcess{s, fields[0], sig, sid, fields[19]}
			t.Logf("parent actual proc pid=%d PPid=%d state=%s session=%d exit_signal=%d start=%s UID=%v GID=%v caps=%x NNP=%d Seccomp=%d filters=%d cgroup=%q", pid, s.ppid, fields[0], sid, sig, fields[19], s.uids, s.gids, s.caps, s.nnp, s.seccomp, s.filters, cg)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
	}
	return out
}

func startDrainMonitor(t *testing.T, scenario string, group *[]*nativeProcess) *nativeProcess {
	t.Helper()
	p, err := startNativeProcess("drain-monitor", func(c *exec.Cmd) { c.Env = append(c.Env, drainCaseEnv+"="+scenario) })
	if err != nil {
		t.Fatal(err)
	}
	*group = append(*group, p)
	if err = p.phase("phase drain prepared"); err != nil {
		t.Fatal(err)
	}
	if err = observeNativeRoot(p.cmd.Process.Pid, 0); err != nil {
		t.Fatal(err)
	}
	if _, err = nativeSeccompObservations(p.cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	return p
}
func releaseDrainSpawn(t *testing.T, p *nativeProcess) {
	t.Helper()
	if err := p.release(); err != nil {
		t.Fatal(err)
	}
	if err := p.phase("phase drain ready"); err != nil {
		t.Fatal(err)
	}
}
func finishDrainMonitor(t *testing.T, p *nativeProcess, observed map[int]drainNativeProcess, scenario string) {
	t.Helper()
	if err := p.release(); err != nil {
		t.Fatal(err)
	}
	if err := p.wait(); err != nil {
		t.Fatalf("drain monitor: %v\n%s", err, p.output.String())
	}
	t.Logf("monitor %d joined output:\n%s", p.cmd.Process.Pid, p.output.String())
	if scenario == "cancel" {
		if !strings.Contains(p.output.String(), "drain canceled: zero observation") {
			t.Fatal("missing cancellation proof")
		}
	} else {
		var observation LocalDrainObservation
		found := false
		for _, line := range strings.Split(p.output.String(), "\n") {
			if strings.HasPrefix(line, "drain-observation ") {
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "drain-observation ")), &observation); err != nil {
					t.Fatal(err)
				}
				found = true
			}
		}
		if !found || observation.MonitorPID != p.cmd.Process.Pid {
			t.Fatal("missing actual observation")
		}
		reaped := map[int]bool{}
		for _, exit := range observation.Reaped {
			status := unix.WaitStatus(exit.WaitStatus)
			if !status.Exited() && !status.Signaled() {
				t.Fatal("nonterminal observed status")
			}
			reaped[exit.PID] = true
		}
		for pid := range observed {
			if scenario != "natural" && !reaped[pid] {
				t.Fatalf("observed child %d omitted from actual reaps %+v", pid, observation)
			}
		}
		if !strings.Contains(p.output.String(), "drain oracle wait4(-1,WNOHANG|__WALL)") {
			t.Fatal("missing actual ECHILD oracle")
		}
	}
	for pid := range observed {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("child %d still exists after monitor joined: %v", pid, err)
		}
	}
}

func registerDrainCleanup(t *testing.T, group *[]*nativeProcess) {
	t.Helper()
	t.Cleanup(func() {
		// No PID1 wait is allowed until all management Cmd.Wait owners are joined.
		joined := true
		for _, p := range *group {
			if err := p.stop(); err != nil {
				t.Error(err)
				joined = false
			}
			t.Logf("cleanup management pid=%d wait=%s", p.cmd.Process.Pid, p.waitResult())
		}
		if !joined {
			t.Error("cannot safely reap adopted users: monitor owner not joined")
			return
		}
		membership, err := readBounded("/proc/self/cgroup", maxCgroupBytes)
		if err != nil {
			t.Error(err)
			return
		}
		fallback := &MonitorBoundary{pid: os.Getpid(), uid: 1000, gid: 1000, installedFilters: 2, cgroup: membership}
		count, err := nativeDrainCleanup(fallback)
		if count != 0 || err != nil {
			t.Errorf("unexpected adopted users (test-only cleanup): reaped=%d err=%v", count, err)
		}
		t.Logf("PID1 test-only fallback complete reaped=%d err=%v", count, err)
	})
}

func TestMonitorDrainNativeAdversaries(t *testing.T) {
	requireNativeBoundary(t)
	var uname unix.Utsname
	if err := unix.Uname(&uname); err != nil {
		t.Fatal(err)
	}
	t.Logf("actual native kernel=%s machine=%s", unix.ByteSliceToString(uname.Release[:]), unix.ByteSliceToString(uname.Machine[:]))
	for _, scenario := range []string{"double", "subreaper", "clone-parent", "clone-zero", "forks", "sigcont", "zombie", "natural", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			var group []*nativeProcess
			registerDrainCleanup(t, &group)
			p := startDrainMonitor(t, scenario, &group)
			releaseDrainSpawn(t, p)
			// Wait boundedly for actual root exit/adoption instead of mistaking the user's
			// ready line for evidence. Other cases snapshot live descendants directly.
			var users map[int]drainNativeProcess
			deadline := time.Now().Add(time.Second)
			for {
				users = snapshotDrainUsers(t)
				ready := len(users) > 0
				if scenario == "double" {
					zombie, adopted := false, false
					for _, s := range users {
						zombie = zombie || s.state == "Z"
						adopted = adopted || (s.state != "Z" && s.status.ppid == p.cmd.Process.Pid && s.session != p.cmd.Process.Pid)
					}
					ready = zombie && adopted
				}
				if scenario == "zombie" || scenario == "natural" {
					ready = false
					for _, s := range users {
						ready = ready || s.state == "Z"
					}
				}
				if scenario == "subreaper" {
					ready = false
					for _, s := range users {
						if _, ok := users[s.status.ppid]; ok {
							ready = true
						}
					}
				}
				if scenario == "clone-zero" {
					ready = false
					for _, s := range users {
						if _, ok := users[s.status.ppid]; ok && s.signal == 0 && s.state != "Z" {
							ready = true
						}
					}
				}
				if scenario == "forks" {
					ready = len(users) >= 4
				}
				if scenario == "clone-parent" {
					ready = len(users) == 2
					for _, s := range users {
						ready = ready && s.status.ppid == p.cmd.Process.Pid && s.signal == 17
					}
				}
				if ready {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("actual %s precondition missing: %+v", scenario, users)
				}
				time.Sleep(5 * time.Millisecond)
			}
			finishDrainMonitor(t, p, users, scenario)
		})
	}
	if !t.Failed() {
		t.Log("monitor drain verified: adversaries and sole-owner ECHILD")
	}
}

func TestMonitorDrainNativeIsolationAndStaleFD(t *testing.T) {
	requireNativeBoundary(t)
	for _, fresh := range []bool{false, true} {
		t.Run(fmt.Sprintf("fresh=%t", fresh), func(t *testing.T) {
			var group []*nativeProcess
			registerDrainCleanup(t, &group)
			a := startDrainMonitor(t, "live", &group)
			releaseDrainSpawn(t, a)
			usersA := snapshotDrainUsers(t)
			if len(usersA) != 1 {
				t.Fatal("expected one A user")
			}
			var oldPID int
			for pid := range usersA {
				oldPID = pid
			}
			fd, err := unix.Open(fmt.Sprintf("/proc/%d", oldPID), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fd)
			b := startDrainMonitor(t, "live", &group)
			var usersB map[int]drainNativeProcess
			if !fresh {
				releaseDrainSpawn(t, b)
				usersB = snapshotDrainUsers(t)
				delete(usersB, oldPID)
			}
			finishDrainMonitor(t, a, usersA, "live")
			if fresh {
				releaseDrainSpawn(t, b)
				usersB = snapshotDrainUsers(t)
			}
			if len(usersB) != 1 {
				t.Fatal("expected one B user")
			}
			err = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
			if !errors.Is(err, unix.ESRCH) {
				t.Fatalf("old pinned proc FD signal: %v", err)
			}
			actual := snapshotDrainUsers(t)
			for pid, s := range usersB {
				if current, ok := actual[pid]; !ok || current.start != s.start || current.state == "Z" {
					t.Fatalf("other monitor user changed: %d", pid)
				}
			}
			t.Logf("old procFD PID=%d returned ESRCH; fresh=%t; B users alive with unchanged starttime; PID-number reuse not forced", oldPID, fresh)
			finishDrainMonitor(t, b, usersB, "live")
		})
	}
}
