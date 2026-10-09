//go:build linux

package etcd

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime/launcher"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func recoveryNativeMemorySample(label string) {
	for _, name := range []string{"memory.current", "memory.peak", "memory.max", "memory.events"} {
		f, err := os.Open("/sys/fs/cgroup/" + name)
		if err != nil {
			fmt.Printf("ARCHIVE CGROUP %s %s unavailable=%v\n", label, name, err)
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, 4097))
		closeErr := f.Close()
		if len(data) > 4096 {
			fmt.Printf("ARCHIVE CGROUP %s %s oversized\n", label, name)
			continue
		}
		fmt.Printf("ARCHIVE CGROUP %s %s read=%v close=%v raw=%q\n", label, name, err, closeErr, data)
	}
}

func recoveryNativePrepared(t *testing.T) (*quiescenceNativeFixture, *PreparedTaskWorkspaceArchive, *deliveryNativeWire, TaskWorkspaceSyncPlan) {
	t.Helper()
	recoveryNativeMemorySample("before-native-ownership-check")
	t.Cleanup(func() { recoveryNativeMemorySample("after-native-operation-or-early-exit") })
	plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspaceSyncIdentity(t, "local", "test-storage", "bucket", "team/project/")})
	require.NoError(t, err)
	setup := func(t *testing.T) (*Backend, *clientv3.Client, AcquireIntentInput, *CreationClaim, []byte, ed25519.PrivateKey) {
		original, raw := integrationBackend(t)
		root := ed25519.NewKeyFromSeed(make([]byte, 32))
		options := integrationOptions(original, raw)
		options.PublicationTrust = &RuntimePublicationTrust{AuthorityID: options.Identity.RuntimeID, Target: "kubernetes", Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}
		options.Clock = goodAuthorityClock()
		base, err := New(context.Background(), options)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, base.Close()) })
		archiveTrackCleanup(base)
		input := acquisitionInput(t, "recovery-original-source")
		input.Workspace = plan.workspace
		result, err := base.AcquireIntent(context.Background(), input)
		require.NoError(t, err)
		require.Equal(t, OutcomeCommitted, result.Outcome)
		claim, err := base.ClaimCreation(context.Background(), input.Workspace, input.IntentID, "recovery-preparer", 30*time.Second)
		require.NoError(t, err)
		t.Cleanup(func() { _ = base.ReleaseCreationClaim(context.Background(), claim) })
		dispatch, err := base.DeclareRuntimeDispatch(context.Background(), claim, dispatchInputForDeclaration())
		require.NoError(t, err)
		certificate := preparationCertificate(t, base, dispatch.Entry.Record, root, "plain", RuntimeReference{ID: uuid.NewString(), UID: uuid.NewString(), BootID: uuid.NewString()})
		return base, raw, input, claim, certificate, root
	}
	f, prepared, wire := deliveryNativeFixtureWithRecoveryProfileData(t, 0, func(TaskRecord) TaskFinalSyncPlan {
		p, err := plan.FinalSyncPlan()
		require.NoError(t, err)
		return p
	}, &TaskWorkspaceSyncBudgetOptions{uuid.NewString(), 1, 4096}, setup, nil, 0, true, true, 60*time.Second)
	return f, prepared, wire, plan
}

func TestTaskArchiveRecoveryOriginalNative(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit original Linux worker fixture required")
	}
	f, prepared, wire, plan := recoveryNativePrepared(t)
	done := deliveryBegin(f, prepared)
	deliveryWaitSent(t, wire, done)
	close(wire.body)
	r := deliveryWait(t, done)
	require.NoError(t, r.err)
	require.NoError(t, <-wire.done)
	diagnostic, err := f.b.archiveStore.coldSnapshot(0)
	require.NoError(t, err)
	require.EqualValues(t, 2, diagnostic.State, "actual original Session.Close precedes durable received")
	s, err := f.b.PrepareTaskWorkspaceSyncSource(f.ctx, r.archive, plan)
	require.NoError(t, err)
	_, err = s.next(f.ctx)
	require.NoError(t, err)
	entry, err := s.next(f.ctx)
	require.NoError(t, err)
	body, err := s.openBody(f.ctx, entry)
	require.NoError(t, err)
	target, err := openTaskWorkspaceSyncLocalTarget(f.ctx, workspaceSyncLocalNativeWorker, plan, archiveTestDirectory(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	var requests atomic.Int32
	workspaceSyncIntentSetKV(s, &faultKV{KV: f.b.client.KV, match: recyclingPutMatch, before: func() {
		requests.Add(1)
		wire, err := os.ReadFile(filepath.Join(f.b.archiveStore.recovery.directory, "slot-00.json"))
		require.NoError(t, err)
		var out [taskArchiveRecoveryMetadataBytes]byte
		m, err := decodeTaskArchiveRecovery(wire, out[:])
		require.NoError(t, err)
		require.Equal(t, "bound", m.State, "bound is durable before reserve RPC")
		require.Equal(t, target.pin, m.Binding.Local)
		raw, err := encodeOperationMetadata(target.pin, 16384)
		require.NoError(t, err)
		require.Equal(t, workspaceSyncIntentDigest(raw), m.Binding.TargetDigest)
	}})
	for i, expected := range []string{"hello", "bye"} {
		require.NoError(t, s.bindLocalPut(f.ctx, body, target))
		_, err := s.reservePutIntent(f.ctx, body)
		require.NoError(t, err)
		w, err := s.putLocal(f.ctx, body, target)
		require.NoError(t, err)
		actual, err := os.ReadFile(filepath.Join(target.pin.Root, body.entry.path))
		require.NoError(t, err)
		require.Equal(t, expected, string(actual))
		settled, err := s.settleLocal(f.ctx, w)
		require.NoError(t, err)
		outcome, err := s.recycleLocal(f.ctx, settled)
		require.NoError(t, err)
		require.Equal(t, OutcomeCommitted, outcome)
		require.NoError(t, body.Close())
		if i == 0 {
			entry, err := s.next(f.ctx)
			require.NoError(t, err)
			body, err = s.openBody(f.ctx, entry)
			require.NoError(t, err)
		}
	}
	require.EqualValues(t, 2, requests.Load())
	directory, options := f.b.archiveStore.recovery.directory, recoveryTestOptions(f.b.archiveStore.recovery.directory, 2)
	// The fixture Entries can differ from the small store fixture; use the exact
	// constructor-owned limits so the cold configuration digest is unchanged.
	l := f.b.archiveStore.limits
	options.TaskWorkspaceArchiveSpoolOptions = TaskWorkspaceArchiveSpoolOptions{directory, l.Jobs, l.JobBytes, l.TotalBytes, l.Entries, l.JobIndexBytes, l.TotalIndexBytes}
	require.NoError(t, s.Close())
	require.NoError(t, f.b.Close())
	cold, err := newTaskArchiveRecoveryStore(options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cold.close()) })
	diagnostic, err = cold.coldSnapshot(0)
	require.NoError(t, err)
	require.False(t, diagnostic.Quarantined)
	require.EqualValues(t, 3, diagnostic.State)
	require.Equal(t, uint64(1), cold.jobs)
	t.Log("public recovery profile: actual original archive -> two local files; cold bound snapshot contains no executable handles")
}

func TestTaskArchiveRecoverySessionNative(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit original Linux worker fixture required")
	}
	taskArchiveRecoveryFixtureDuration = 31 * time.Second
	t.Cleanup(func() { taskArchiveRecoveryFixtureDuration = 0 })
	f, prepared, wire, plan := recoveryNativePrepared(t)
	done := deliveryBegin(f, prepared)
	deliveryWaitSent(t, wire, done)
	close(wire.body)
	r := deliveryWait(t, done)
	require.NoError(t, r.err)
	require.NoError(t, <-wire.done)
	s, err := f.b.PrepareTaskWorkspaceSyncSource(f.ctx, r.archive, plan)
	require.NoError(t, err)
	select {
	case <-r.archive.owner.spool.ctx.Done():
	case <-time.After(35 * time.Second):
		t.Fatal("actual signed session did not expire")
	}
	require.True(t, time.Now().After(prepared.draft.claims.SessionNotAfter))
	require.NoError(t, f.b.RenewTaskClaim(f.ctx, f.c), "current original native claim remains valid")
	_, err = s.next(f.ctx)
	require.NoError(t, err)
	entry, err := s.next(f.ctx)
	require.NoError(t, err)
	body, err := s.openBody(f.ctx, entry)
	require.NoError(t, err)
	target, err := openTaskWorkspaceSyncLocalTarget(f.ctx, workspaceSyncLocalNativeWorker, plan, archiveTestDirectory(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	require.NoError(t, s.bindLocalPut(f.ctx, body, target))
	_, err = s.reservePutIntent(f.ctx, body)
	require.NoError(t, err, "received original source binding must survive actual session expiry")
	w, err := s.putLocal(f.ctx, body, target)
	require.NoError(t, err)
	require.True(t, w.original())
	actual, err := os.ReadFile(filepath.Join(target.pin.Root, entry.path))
	require.NoError(t, err)
	require.Equal(t, "hello", string(actual))
	t.Log("actual signed 31s session expired; freshly renewed native claim still bound and wrote original source")
}

// Invoked only by the parent retaining this exact exec.Cmd. TestMain's native
// cluster mode is disabled in this child; the parent retains and Waits the
// three original etcd processes. The child bootstraps its own actual worker.
func TestTaskArchiveRecoveryChild(t *testing.T) {
	window := os.Getenv("SANDBOX_ARCHIVE_RECOVERY_WINDOW")
	if window == "" {
		return
	}
	directory := os.Getenv("SANDBOX_ARCHIVE_RECOVERY_DIRECTORY")
	if window == "held-lock" {
		s, err := newTaskArchiveRecoveryStore(recoveryTestOptions(directory, 2))
		require.NoError(t, err)
		defer s.close()
		fmt.Println("ARCHIVE CHILD READY held-lock")
		select {}
	}
	worker, err := launcher.BootstrapWorkspaceSyncWorker()
	require.NoError(t, err)
	workspaceSyncLocalNativeWorker = worker
	taskWorkspaceSyncLocalOwnershipVerifier = func(t *testing.T) {
		t.Log("parent retains actual three etcd exec.Cmd handles; child opens fresh unique test namespace")
	}
	taskArchiveRecoveryFixtureDirectory = directory
	f, prepared, wire, plan := recoveryNativePrepared(t)
	f.b.archiveStore.recovery.probe = func(stage string) {
		if stage == window {
			fmt.Printf("ARCHIVE CHILD READY %s\n", stage)
			select {}
		}
	}
	done := deliveryBegin(f, prepared)
	deliveryWaitSent(t, wire, done)
	close(wire.body)
	r := deliveryWait(t, done)
	require.NoError(t, r.err)
	require.NoError(t, <-wire.done)
	s, err := f.b.PrepareTaskWorkspaceSyncSource(f.ctx, r.archive, plan)
	require.NoError(t, err)
	_, err = s.next(f.ctx)
	require.NoError(t, err)
	entry, err := s.next(f.ctx)
	require.NoError(t, err)
	body, err := s.openBody(f.ctx, entry)
	require.NoError(t, err)
	target, err := openTaskWorkspaceSyncLocalTarget(f.ctx, worker, plan, archiveTestDirectory(t))
	require.NoError(t, err)
	defer target.Close()
	require.NoError(t, s.bindLocalPut(f.ctx, body, target))
	_, err = s.reservePutIntent(f.ctx, body)
	require.NoError(t, err)
	t.Fatalf("original operation passed required crash window %s", window)
}

func TestTaskArchiveRecoveryCrashNative(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit original Linux worker fixture required")
	}
	windows := []string{"held-lock", "pending-before", "pending-next-fsync", "pending-root-fsync", "artifact-created", "artifact-root-fsync", "tar-fsync", "received-rename", "received-root-fsync", "bound-before", "bound-rename", "bound-root-fsync", "bound-postguard"}
	for _, window := range windows {
		t.Run(window, func(t *testing.T) {
			directory := archiveTestDirectory(t)
			options := recoveryTestOptions(directory, 2)
			executable, err := os.Executable()
			require.NoError(t, err)
			logPath := filepath.Join(archiveTestDirectory(t), "child.raw")
			log, err := os.Create(logPath)
			require.NoError(t, err)
			cmd := exec.Command(executable, "-test.run", "^TestTaskArchiveRecoveryChild$", "-test.v", "-test.timeout", "40s")
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "SANDBOX_WORKSPACE_SYNC_LOCAL_NATIVE=") && !strings.HasPrefix(entry, "SANDBOX_ARCHIVE_RECOVERY_") {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			cmd.Env = append(cmd.Env, "SANDBOX_WORKSPACE_SYNC_LOCAL_NATIVE=0", "SANDBOX_ARCHIVE_RECOVERY_WINDOW="+window, "SANDBOX_ARCHIVE_RECOVERY_DIRECTORY="+directory)
			cmd.Stdout, cmd.Stderr = log, log
			recoveryNativeMemorySample("before-child-" + window)
			require.NoError(t, cmd.Start())
			waited := make(chan struct{})
			var waitErr error
			go func() { waitErr = cmd.Wait(); close(waited) }()
			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				<-waited // the original Cmd.Wait physically returned, on every path
				recoveryNativeMemorySample("after-actual-child-Wait-" + window)
				_ = log.Close()
				data, err := os.ReadFile(logPath)
				if err == nil {
					fmt.Printf("BEGIN RAW ARCHIVE CHILD %s\n%sEND RAW ARCHIVE CHILD %s\n", window, data, window)
				}
				t.Logf("archive child window=%s pid=%d actualCmdWait=true joined=true exit=%v", window, cmd.Process.Pid, waitErr)
			})
			deadline := time.Now().Add(20 * time.Second)
			for {
				data, err := os.ReadFile(logPath)
				require.NoError(t, err)
				if strings.Contains(string(data), "ARCHIVE CHILD READY "+window+"\n") {
					break
				}
				select {
				case <-waited:
					t.Fatalf("child exited before window: %v\n%s", waitErr, data)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatalf("child window timeout: %s\n%s", window, data)
				}
				time.Sleep(10 * time.Millisecond)
			}
			_, err = newTaskArchiveRecoveryStore(options)
			require.Error(t, err, "live original child holds real nonblocking flock")
			require.NoError(t, cmd.Process.Signal(syscall.SIGKILL))
			<-waited
			require.Error(t, waitErr)
			cold, err := newTaskArchiveRecoveryStore(options)
			require.NoError(t, err)
			defer cold.close()
			diagnostic, err := cold.coldSnapshot(0)
			require.NoError(t, err)
			if window == "held-lock" || window == "pending-before" {
				require.False(t, diagnostic.Occupied, "no filename exists in pre-persistence window")
				require.Zero(t, cold.disk)
			} else {
				require.True(t, diagnostic.Occupied)
				require.Equal(t, options.JobBytes, cold.disk)
				if strings.HasPrefix(window, "pending") || strings.HasPrefix(window, "artifact") || window == "tar-fsync" {
					require.True(t, diagnostic.Quarantined)
				}
			}
			t.Logf("SIGKILL reopen window=%s occupied=%v quarantine=%v state=%d charge=%d; fixed diagnostic only, next never promoted", window, diagnostic.Occupied, diagnostic.Quarantined, diagnostic.State, cold.disk)
		})
	}
}

func recoveryNativeFirstBody(t *testing.T) (*quiescenceNativeFixture, *PreparedTaskWorkspaceSyncSource, *taskWorkspaceSyncBody, *taskWorkspaceSyncLocalTarget) {
	t.Helper()
	f, prepared, wire, plan := recoveryNativePrepared(t)
	done := deliveryBegin(f, prepared)
	deliveryWaitSent(t, wire, done)
	close(wire.body)
	r := deliveryWait(t, done)
	require.NoError(t, r.err)
	require.NoError(t, <-wire.done)
	s, err := f.b.PrepareTaskWorkspaceSyncSource(f.ctx, r.archive, plan)
	require.NoError(t, err)
	_, err = s.next(f.ctx)
	require.NoError(t, err)
	entry, err := s.next(f.ctx)
	require.NoError(t, err)
	body, err := s.openBody(f.ctx, entry)
	require.NoError(t, err)
	target, err := openTaskWorkspaceSyncLocalTarget(f.ctx, workspaceSyncLocalNativeWorker, plan, archiveTestDirectory(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	return f, s, body, target
}

func TestTaskArchiveRecoveryFailureNative(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit original Linux worker fixture required")
	}
	t.Run("received-seal-failure", func(t *testing.T) {
		f, prepared, wire, _ := recoveryNativePrepared(t)
		f.b.archiveStore.recovery.probe = func(stage string) {
			if stage == "tar-fsync" {
				require.NoError(t, os.WriteFile(filepath.Join(f.b.archiveStore.recovery.directory, "slot-00.next"), []byte("preserved collision"), 0600))
			}
		}
		done := deliveryBegin(f, prepared)
		deliveryWaitSent(t, wire, done)
		close(wire.body)
		r := deliveryWait(t, done)
		require.Error(t, r.err)
		require.Nil(t, r.archive)
		require.NoError(t, <-wire.done)
		require.Equal(t, uint64(1), f.b.archiveStore.jobs)
	})
	t.Run("binding-seal-failure-zero-reserve-effects", func(t *testing.T) {
		f, s, body, target := recoveryNativeFirstBody(t)
		var requests atomic.Int32
		workspaceSyncIntentSetKV(s, &faultKV{KV: f.b.client.KV, match: recyclingPutMatch, before: func() { requests.Add(1) }})
		f.b.archiveStore.recovery.probe = func(stage string) {
			if stage == "bound-before" {
				require.NoError(t, os.WriteFile(filepath.Join(f.b.archiveStore.recovery.directory, "slot-00.next"), []byte("preserved collision"), 0600))
			}
		}
		require.NoError(t, s.bindLocalPut(f.ctx, body, target))
		_, err := s.reservePutIntent(f.ctx, body)
		require.Error(t, err)
		require.Zero(t, requests.Load())
		entries, err := os.ReadDir(target.pin.Root)
		require.NoError(t, err)
		require.Empty(t, entries)
	})
	t.Run("different-target-on-second-file", func(t *testing.T) {
		f, s, body, target := recoveryNativeFirstBody(t)
		require.NoError(t, s.bindLocalPut(f.ctx, body, target))
		_, err := s.reservePutIntent(f.ctx, body)
		require.NoError(t, err)
		w, err := s.putLocal(f.ctx, body, target)
		require.NoError(t, err)
		settled, err := s.settleLocal(f.ctx, w)
		require.NoError(t, err)
		_, err = s.recycleLocal(f.ctx, settled)
		require.NoError(t, err)
		require.NoError(t, body.Close())
		entry, err := s.next(f.ctx)
		require.NoError(t, err)
		second, err := s.openBody(f.ctx, entry)
		require.NoError(t, err)
		other, err := openTaskWorkspaceSyncLocalTarget(f.ctx, workspaceSyncLocalNativeWorker, s.plan, archiveTestDirectory(t))
		require.NoError(t, err)
		defer other.Close()
		require.NoError(t, s.bindLocalPut(f.ctx, second, other))
		var requests atomic.Int32
		workspaceSyncIntentSetKV(s, &faultKV{KV: f.b.client.KV, match: recyclingPutMatch, before: func() { requests.Add(1) }})
		_, err = s.reservePutIntent(f.ctx, second)
		require.ErrorIs(t, err, ErrIdentityMismatch)
		require.Zero(t, requests.Load())
		entries, err := os.ReadDir(other.pin.Root)
		require.NoError(t, err)
		require.Empty(t, entries)
	})
}

func recoveryHoldProbe(store *taskArchiveStore, stage string) (<-chan struct{}, func()) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	store.recovery.probe = func(actual string) {
		if actual == stage {
			once.Do(func() { close(entered) })
			<-release
		}
	}
	return entered, func() { close(release) }
}

func TestTaskArchiveRecoveryCloseNative(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit original Linux worker fixture required")
	}
	t.Run("received-metadata-after-receive-owner-done", func(t *testing.T) {
		f, prepared, wire, _ := recoveryNativePrepared(t)
		entered, release := recoveryHoldProbe(f.b.archiveStore, "received-next-fsync")
		done := deliveryBegin(f, prepared)
		deliveryWaitSent(t, wire, done)
		close(wire.body)
		<-entered
		s := f.b.archiveStore
		s.mu.Lock()
		for spool := range s.spools {
			require.False(t, spool.active)
			select {
			case <-spool.ownerDone:
			default:
				t.Fatal("receive owner has not actually returned")
			}
		}
		s.mu.Unlock()
		closed := make(chan error, 1)
		go func() { closed <- f.b.Close() }()
		select {
		case err := <-closed:
			t.Fatalf("Backend.Close passed actual metadata registration: %v", err)
		case <-time.After(80 * time.Millisecond):
		}
		release()
		require.Error(t, deliveryWait(t, done).err)
		require.NoError(t, <-wire.done)
		require.NoError(t, <-closed)
		t.Log("Backend Close waited extra metadata owner after original receive ownerDone; actual next-file fsync completed before fixture suspension")
	})
	for _, stage := range []string{"bound-next-fsync", "bound-postguard"} {
		for _, owner := range []string{"source", "target", "backend"} {
			t.Run(stage+"-"+owner, func(t *testing.T) {
				f, s, body, target := recoveryNativeFirstBody(t)
				require.NoError(t, s.bindLocalPut(f.ctx, body, target))
				var requests atomic.Int32
				workspaceSyncIntentSetKV(s, &faultKV{KV: f.b.client.KV, match: recyclingPutMatch, before: func() { requests.Add(1) }})
				entered, release := recoveryHoldProbe(f.b.archiveStore, stage)
				reserved := make(chan error, 1)
				go func() { _, err := s.reservePutIntent(f.ctx, body); reserved <- err }()
				<-entered
				closed := make(chan error, 1)
				go func() {
					switch owner {
					case "source":
						closed <- s.Close()
					case "target":
						closed <- target.Close()
					case "backend":
						closed <- f.b.Close()
					}
				}()
				select {
				case err := <-closed:
					t.Fatalf("%s Close passed registered %s: %v", owner, stage, err)
				case <-time.After(80 * time.Millisecond):
				}
				release()
				require.Error(t, <-reserved)
				require.NoError(t, <-closed)
				require.Zero(t, requests.Load())
				t.Logf("%s Close joined actual original source/body/target metadata or postguard registration; zero reserve RPCs", owner)
			})
		}
	}
}
