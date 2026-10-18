//go:build linux && (amd64 || arm64)

package nodeobserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/fuseprotocol"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// These deliberately invalid anchors model only ownership handoff. The test
// cannot mint a kernel identity or enrollment: it proves that a concurrent
// terminal owner can remove its pointer under mu before any FD is inspected.
func TestNodeObserverMounterLauncherRevalidationSerializesRetirement(t *testing.T) {
	for _, registering := range []bool{true, false} {
		t.Run(fmt.Sprintf("registering-%t", registering), func(t *testing.T) {
			launcher := &processAnchor{proc: -1, pidfd: -1, pidns: -1, mountns: -1}
			observer := &Observer{
				slots:   []observerSlot{{anchor: launcher}},
				journal: &journal{records: []journalRecord{{State: "enrolled"}}},
			}
			var owner *mounterHealthChannel
			if !registering {
				owner = &mounterHealthChannel{launcher: launcher}
				observer.slots[0].mounter = owner
			}
			observer.mu.Lock()
			started := make(chan struct{})
			joined := make(chan error, 1)
			go func() {
				close(started)
				joined <- observer.revalidateMounterLauncher(0, launcher, owner)
			}()
			<-started
			select {
			case err := <-joined:
				observer.mu.Unlock()
				t.Fatalf("launcher FD inspection escaped original ownership mutex: %v", err)
			case <-time.After(50 * time.Millisecond):
			}

			// This is the original terminal-close ownership transition; no real
			// or fake kernel descriptors are installed in this refusal model.
			observer.slots[0].anchor = nil
			observer.slots[0].running = true
			observer.mu.Unlock()
			select {
			case err := <-joined:
				require.EqualError(t, err, "mounter original launcher changed")
			case <-time.After(time.Second):
				t.Fatal("launcher revalidation did not join after terminal ownership returned")
			}
		})
	}
}

func TestNodeObserverMounterConnectionClosePreservesOriginalResult(t *testing.T) {
	for _, alreadyClosed := range []bool{false, true} {
		t.Run(fmt.Sprintf("already-closed-%t", alreadyClosed), func(t *testing.T) {
			dir, err := os.MkdirTemp("/tmp", "observer-close-")
			require.NoError(t, err)
			defer os.RemoveAll(dir)
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "health.sock"), Net: "unix"})
			require.NoError(t, err)
			defer listener.Close()
			require.NoError(t, listener.SetDeadline(time.Now().Add(time.Second)))
			client, err := net.DialUnix("unix", nil, listener.Addr().(*net.UnixAddr))
			require.NoError(t, err)
			defer client.Close()
			server, err := listener.AcceptUnix()
			require.NoError(t, err)
			defer server.Close()
			if alreadyClosed {
				require.NoError(t, server.Close())
			}
			conn := &mounterConnection{UnixConn: server}
			var joins sync.WaitGroup
			results := make(chan error, 8)
			for range 8 {
				joins.Add(1)
				go func() {
					defer joins.Done()
					results <- conn.Close()
				}()
			}
			joins.Wait()
			close(results)
			for err := range results {
				require.NoError(t, err, "stop/timeout/retirement must share the original close result")
			}
			require.NoError(t, client.SetReadDeadline(time.Now().Add(time.Second)))
			var one [1]byte
			_, err = client.Read(one[:])
			require.Error(t, err, "the real original Unix connection was not closed")
		})
	}

	// A first-close failure state is a bookkeeping model, not a forged socket
	// failure or native authority. No raw descriptor is closed behind netpoll.
	t.Run("completed-first-error-is-not-overwritten", func(t *testing.T) {
		want := errors.New("original connection close was not certified")
		conn := &mounterConnection{}
		conn.closeOnce.Do(func() { conn.closeErr = want })
		require.ErrorIs(t, conn.Close(), want)
		require.ErrorIs(t, conn.Close(), want)
	})
}

// Retirement bookkeeping carries no process or effect authority. Close errors
// cannot make this model a reusable slot, even though its original loop joined.
func TestNodeObserverMounterRetirementRetainsFailedClose(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed-%t", failed), func(t *testing.T) {
			owner := &mounterHealthChannel{done: make(chan struct{})}
			observer := &Observer{slots: []observerSlot{{mounter: owner}}}
			var closeErr error
			if failed {
				closeErr = errors.New("original channel close failed")
			}
			observer.finishMounterRetirement(0, owner, closeErr)
			select {
			case <-owner.done:
			default:
				t.Fatal("retirement returned without joining the original owner")
			}
			if failed {
				require.Same(t, owner, observer.slots[0].mounter)
				require.ErrorIs(t, observer.closeErr, closeErr)
				require.True(t, observer.stopping)
			} else {
				require.Nil(t, observer.slots[0].mounter)
				require.NoError(t, observer.closeErr)
				require.False(t, observer.stopping)
			}
		})
	}
}

// This tests the finite consumer's monotonic budget, not peer authentication.
// UTC diagnostic times deliberately do not track the local monotonic anchor.
// The actual channel must call this same window only after binding its original
// slot and kernel anchor; a window value cannot supply enrollment authority.
func TestNodeObserverMounterWindowCannotRenewSameSample(t *testing.T) {
	anchor := time.Now()
	sample := fuseprotocol.MounterReadiness{
		Version: 2, RuntimeUID: "uid-a", PoolKey: strings.Repeat("a", 64),
		Generation: 1, MountAttempt: 1, MountID: 42, SampleSequence: 7,
		SampledAt: time.Unix(100, 0).UTC(), CacheObservedAt: time.Unix(90, 0).UTC(),
		ValidForNanoseconds: int64(time.Second),
	}
	var window mounterSampleWindow
	first, err := window.accept(sample, anchor, anchor.Add(100*time.Millisecond))
	require.NoError(t, err)
	require.Equal(t, int64(900*time.Millisecond), first.ValidForNanoseconds)
	require.Equal(t, sample.SampledAt, first.SampledAt, "UTC times are diagnostic, not freshness authority")

	// A second hop that reports a larger remaining duration for the exact
	// same sequence cannot move the first local monotonic deadline forward.
	repeated := sample
	repeated.ValidForNanoseconds = int64(3 * time.Second)
	second, err := window.accept(repeated, anchor.Add(200*time.Millisecond), anchor.Add(300*time.Millisecond))
	require.NoError(t, err)
	require.Equal(t, int64(700*time.Millisecond), second.ValidForNanoseconds)
	_, err = window.accept(repeated, anchor.Add(time.Second), anchor.Add(time.Second))
	require.Error(t, err, "even immediate delivery cannot revive an expired unchanged sequence")

	var late mounterSampleWindow
	_, err = late.accept(sample, anchor, anchor.Add(time.Second))
	require.Error(t, err, "the request anchor precedes IO; receiving a reply is not a new sample")
}

func TestNodeObserverMounterWindowRejectsTupleAndSequenceMutation(t *testing.T) {
	anchor := time.Now()
	sample := fuseprotocol.MounterReadiness{
		Version: 2, RuntimeUID: "uid-a", PoolKey: strings.Repeat("a", 64),
		Generation: 1, MountAttempt: 1, MountID: 42, SampleSequence: 7,
		SampledAt: anchor.UTC(), CacheObservedAt: anchor.UTC(), ValidForNanoseconds: int64(time.Second),
	}
	for name, mutate := range map[string]func(*fuseprotocol.MounterReadiness){
		"runtime":                       func(s *fuseprotocol.MounterReadiness) { s.RuntimeUID = "uid-b" },
		"pool":                          func(s *fuseprotocol.MounterReadiness) { s.PoolKey = strings.Repeat("b", 64) },
		"generation":                    func(s *fuseprotocol.MounterReadiness) { s.Generation++ },
		"attempt":                       func(s *fuseprotocol.MounterReadiness) { s.MountAttempt++ },
		"mount":                         func(s *fuseprotocol.MounterReadiness) { s.MountID++ },
		"sequence-rollback":             func(s *fuseprotocol.MounterReadiness) { s.SampleSequence-- },
		"same-sequence-new-sample-time": func(s *fuseprotocol.MounterReadiness) { s.SampledAt = s.SampledAt.Add(time.Millisecond) },
		"same-sequence-new-cache-time":  func(s *fuseprotocol.MounterReadiness) { s.CacheObservedAt = s.CacheObservedAt.Add(time.Millisecond) },
		"zero-validity":                 func(s *fuseprotocol.MounterReadiness) { s.ValidForNanoseconds = 0 },
		"over-budget":                   func(s *fuseprotocol.MounterReadiness) { s.ValidForNanoseconds = int64(3*time.Second) + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			var window mounterSampleWindow
			_, err := window.accept(sample, anchor, anchor)
			require.NoError(t, err)
			changed := sample
			mutate(&changed)
			_, err = window.accept(changed, anchor.Add(time.Millisecond), anchor.Add(2*time.Millisecond))
			require.Error(t, err)
		})
	}
}

// Only exact terminal cgroup components may be associated with a CRI ID.
// These are parser models, not claims that such a container exists locally.
func TestNodeObserverMounterContainerIDMatchesExactCgroup(t *testing.T) {
	id := strings.Repeat("a", 64)
	for _, test := range []struct {
		container string
		group     string
		want      bool
	}{
		{"containerd://" + id, "/kubepods/burstable/pod-uid/" + id, true},
		{"containerd://" + id, "/kubepods.slice/pod.slice/cri-containerd-" + id + ".scope", true},
		{"cri-o://" + id, "/kubepods.slice/pod.slice/crio-" + id + ".scope", true},
		{"docker://" + id, "/kubepods/pod-uid/docker-" + id + ".scope", true},
		{"containerd://" + id, "/kubepods/" + id + "/child", false},
		{"containerd://" + id, "/kubepods/prefix-" + id, false},
		{"containerd://" + id, "/kubepods/" + id + "0", false},
		{"unknown://" + id, "/kubepods/" + id, false},
		{"containerd://" + id, "/kubepods/crio-" + id + ".scope", false},
		{"containerd://" + id, "/kubepods/../" + id, false},
		{"containerd://" + id, "/", false},
	} {
		t.Run(test.container+test.group, func(t *testing.T) {
			require.Equal(t, test.want, mounterContainerIDMatches(test.container, test.group))
		})
	}
}

// A genuine local Unix connection and public locator fields cannot create an
// enrollment slot. This intentionally supplies no fake anchor or signed slot.
func TestNodeObserverMounterChannelRejectsUnregisteredPeer(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "observer-health-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "roothealth.sock"), Net: "unix"})
	require.NoError(t, err)
	defer listener.Close()
	require.NoError(t, listener.SetDeadline(time.Now().Add(time.Second)))
	client, err := net.DialUnix("unix", nil, listener.Addr().(*net.UnixAddr))
	require.NoError(t, err)
	defer client.Close()
	server, err := listener.AcceptUnix()
	require.NoError(t, err)
	defer server.Close()
	require.NoError(t, client.SetDeadline(time.Now().Add(time.Second)))
	require.NoError(t, server.SetDeadline(time.Now().Add(time.Second)))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	observer := &Observer{ctx: ctx, options: Options{RequestTimeout: time.Second}}
	joined := make(chan error, 1)
	go func() { joined <- observer.handleMounterHealth(ctx, server) }()
	registration, err := json.Marshal(fuseprotocol.SocketRequest{
		Version: fuseprotocol.Version, Command: "register-readiness",
		Input: json.RawMessage(`{"version":2,"runtime_uid":"uid-a","pod_name":"pod-a","node_uid":"22222222-2222-4222-8222-222222222222","node_boot_id":"33333333-3333-4333-8333-333333333333","contract_digest":"` + strings.Repeat("a", 64) + `"}`),
	})
	require.NoError(t, err)
	// The genuine host peer may be rejected even before reading this locator.
	_ = ct.WriteNodeObserverFrame(client, registration)
	select {
	case err := <-joined:
		require.Error(t, err)
	case <-ctx.Done():
		_ = server.Close()
		<-joined
		t.Fatal("unregistered peer retained channel IO")
	}
	require.Empty(t, observer.slots)
	result, err := observer.ReadMounterReadiness(ctx, p.RuntimeIdentityContext{
		SandboxID: "pod-a", Generation: 1,
		Runtime: p.RuntimeReference{ID: "pod-a", UID: "uid-a", BootID: "33333333-3333-4333-8333-333333333333"},
	}, strings.Repeat("a", 64), 1, 1)
	require.Error(t, err)
	require.Zero(t, result, "public identity fields cannot authorize a readiness result")
}

func TestNodeObserverEnrollmentKernelParsers(t *testing.T) {
	for _, text := range []string{"0::/\n", "0::/../x\n", "0::/x\n0::/y\n", "1:cpu:/x\n"} {
		if _, err := cgroupMembership([]byte(text)); err == nil {
			t.Fatalf("accepted invalid membership %q", text)
		}
	}
	for _, text := range []string{"NSpid:\t1\n", "NSpid:\t2\t3\n", "NSpid:\t2\t1\nNSpid:\t2\t1\n"} {
		if privatePID1([]byte(text)) == nil {
			t.Fatalf("accepted invalid private PID1 %q", text)
		}
	}
	if err := privatePID1([]byte("NSpid:\t7\t1\n")); err != nil {
		t.Fatal(err)
	}
}

func TestNodeObserverOptionsRejectMissingAuthority(t *testing.T) {
	if observer, err := New(Options{}); err == nil || observer != nil {
		t.Fatal("observer accepted missing concrete authority")
	}
	var empty Observer
	if err := empty.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNodeObserverEnrollmentBoundary(t *testing.T) {
	actual := []byte("NoNewPrivs:\t1\nSeccomp:\t2\nCapEff:\t00000000000000e0\nCapPrm:\t00000000000000e0\nCapBnd:\t0000000000000000\nCapAmb:\t0000000000000000\nCapInh:\t00000000000000e0\n")
	if err := validateProcessStatus(actual); err != nil {
		t.Fatalf("genuine Supervisor fixed inheritable set rejected: %v", err)
	}
	for _, invalid := range [][]byte{
		[]byte(strings.Replace(string(actual), "CapInh:\t00000000000000e0", "CapInh:\t00000000002000e0", 1)),
		[]byte(strings.Replace(string(actual), "CapInh:\t00000000000000e0", "CapInh:\t00000000000001e0", 1)),
		[]byte(strings.Replace(string(actual), "CapEff:\t00000000000000e0", "CapEff:\t00000000000800e0", 1)),
		[]byte(strings.Replace(string(actual), "CapBnd:\t0000000000000000", "CapBnd:\t00000000000000e0", 1)),
		[]byte(strings.Replace(string(actual), "CapAmb:\t0000000000000000", "CapAmb:\t00000000000000e0", 1)),
		[]byte(strings.Replace(string(actual), "NoNewPrivs:\t1", "NoNewPrivs:\t0", 1)),
	} {
		if err := validateProcessStatus(invalid); err == nil {
			t.Fatal("escape capability or missing NNP accepted")
		}
	}
}

func TestNodeObserverPhysicalKernelChild(t *testing.T) {
	if os.Getenv("SANDBOX_NODE_OBSERVER_KERNEL_CHILD") != "1" {
		return
	}
	if os.Getpid() != 1 {
		t.Fatal("kernel child must be genuine namespace PID1")
	}
	child := exec.Command("/bin/sleep", "600")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: os.Getenv("SANDBOX_NODE_OBSERVER_KERNEL_SOCKET"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("descendant-ready")); err != nil {
		t.Fatal(err)
	}
	// No success callback or voluntary exit: the parent uses its held pidfd.
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	t.Fatal("descendant exited before observer signal")
}

func TestNodeObserverPhysicalKernel(t *testing.T) {
	if os.Getenv("SANDBOX_NODE_OBSERVER_KERNEL_NATIVE") != "1" {
		t.Skip("requires exact owned native kernel fixture")
	}
	if os.Getuid() != 0 {
		t.Fatal("kernel fixture requires root")
	}
	group, err := os.ReadFile("/proc/self/cgroup")
	if err != nil || string(group) != "0::/\n" {
		t.Fatal("fixture must have private delegated cgroup namespace")
	}
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	mount := filepath.Join(dir, "cgroup")
	if err := os.Mkdir(mount, 0700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount("none", mount, "cgroup2", 0, ""); err != nil {
		t.Fatalf("private cgroup delegation unavailable: %v", err)
	}
	groupName := fmt.Sprintf("observer-owned-%d", os.Getpid())
	owned := filepath.Join(mount, groupName)
	if err := os.Mkdir(owned, 0700); err != nil {
		unix.Unmount(mount, 0)
		t.Fatal(err)
	}
	cgroupFD, err := unix.Open(owned, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cgroupFD >= 0 {
			_ = unix.Close(cgroupFD)
		}
	}()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "enroll.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	child := exec.Command(os.Args[0], "-test.run=^TestNodeObserverPhysicalKernelChild$", "-test.v", "-test.timeout=35s")
	child.Env = append(os.Environ(), "SANDBOX_NODE_OBSERVER_KERNEL_CHILD=1", "SANDBOX_NODE_OBSERVER_KERNEL_SOCKET="+listener.Addr().String())
	child.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWPID | unix.CLONE_NEWNS, UseCgroupFD: true, CgroupFD: cgroupFD}
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	if err := unix.Close(cgroupFD); err != nil {
		t.Fatal(err)
	}
	cgroupFD = -1
	waited := false
	defer func() {
		if !waited {
			_ = child.Process.Kill()
			err := child.Wait()
			t.Logf("original child Cmd.Wait cleanup: %v", err)
		}
		events, err := os.ReadFile(filepath.Join(owned, "cgroup.events"))
		if err != nil || !strings.Contains(string(events), "populated 0\n") {
			t.Errorf("owned cgroup cleanup not empty: %q %v", events, err)
			return
		}
		if err := os.Remove(owned); err != nil {
			t.Error(err)
			return
		}
		if err := unix.Unmount(mount, 0); err != nil {
			t.Error(err)
			return
		}
		t.Log("owned child cgroup empty→rmdir→private unmount joined")
	}()
	if err := listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	conn, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var ready [16]byte
	n, err := conn.Read(ready[:])
	if err != nil || string(ready[:n]) != "descendant-ready" {
		t.Fatalf("actual descendant readiness: %q %v", ready[:n], err)
	}
	pid, err := kernelPeer(conn)
	if err != nil || pid != child.Process.Pid {
		t.Fatalf("actual kernel peer pid=%d child=%d: %v", pid, child.Process.Pid, err)
	}
	anchor, err := openProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	defer anchor.close()
	t.Logf("actual enrollment anchors: %+v", anchor.facts)
	if err := anchor.revalidate(); err != nil {
		t.Fatal(err)
	}
	if err := anchor.kill(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := anchor.waitDeathAndEmpty(ctx); err != nil {
		t.Fatal(err)
	}
	err = child.Wait()
	waited = true
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("actual original child Cmd.Wait expected SIGKILL: %v", err)
	}
	t.Logf("original child Cmd.Wait=%v; pidfd death + original cgroup populated0 observed", err)
}
