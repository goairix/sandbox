//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime/launcher"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// These decoded records are test observations only. The consumer receives no
// record/claim/capability from this file, only the original explicit TaskRef.
type cleanupExecutorRecoveryRow struct {
	Task        state.TaskReference `json:"task"`
	TaskBirth   int64               `json:"task_birth"`
	Incarnation string              `json:"incarnation"`
	Slot        uint32              `json:"slot"`
	Phase       string              `json:"phase"`
	Confirmed   string              `json:"confirmed"`
	Run         *struct {
		ID     string                                `json:"id"`
		Worker launcher.WorkspaceSyncWorkerReference `json:"worker"`
		Joined bool                                  `json:"joined"`
	} `json:"run"`
	Archive *struct {
		StoreID       string `json:"store_id"`
		Slot          uint32 `json:"slot"`
		BindingDigest string `json:"binding_digest"`
	} `json:"archive"`
	Release *struct {
		CreateRevision int64  `json:"create_revision"`
		Digest         string `json:"digest"`
	} `json:"release"`
}

type cleanupExecutorRecoveryReport struct {
	Result       state.CleanupLocalResult
	Row          cleanupExecutorRecoveryRow
	Stats        cleanupExecutorProxyStats
	Claims       []string
	SessionLease int64
}

func cleanupExecutorRecoveryPoint(t *testing.T, ctx context.Context, raw *clientv3.Client, key string) *mvccpb.KeyValue {
	t.Helper()
	rpc, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	response, err := raw.Get(rpc, key)
	require.NoError(t, err)
	require.Len(t, response.Kvs, 1)
	return response.Kvs[0]
}

// Each invocation is one actual zero-cap OS worker with a fresh Backend and
// DueWorker. Only its target/spool directory survives between the two workers.
func cleanupExecutorRecoveryExecute(t *testing.T, ctx context.Context, s cleanupExecutorSetup, options state.Options, dir string, tlsConfig *tls.Config) {
	t.Helper()
	fault := ""
	if s.RecoveryStage == "first" {
		fault = s.Recovery
	}
	proxy := newCleanupExecutorEtcdProxy(t, tlsConfig, s.Task, fault)
	options.Endpoints = []string{proxy.endpoint()}
	b, err := state.New(ctx, options)
	require.NoError(t, err)
	defer b.Close()
	shares := make([]uint16, 32)
	for i := range shares {
		shares[i] = uint16(i)
	}
	w, err := b.StartDueWorker(ctx, state.DueWorkerOptions{WorkerID: "cleanup-native-" + s.RecoveryStage, Partitions: []uint8{s.Task.Partition}, RangeSlots: []uint16{0}, ClaimSlots: shares, RelistSlot: 0, StageTTL: 30 * time.Second})
	require.NoError(t, err)
	defer w.Stop()
	sessionLease := w.Status().Session.LeaseID
	raw, err := clientv3.New(clientv3.Config{Endpoints: options.Endpoints, TLS: tlsConfig, DialTimeout: 2 * time.Second})
	require.NoError(t, err)
	defer raw.Close()
	prefix := fmt.Sprintf("%sp/%02x/tasks/%s/", s.Binding.Namespace, s.Task.Partition, s.Task.TaskID)
	watchCtx, stopWatch := context.WithCancel(ctx)
	watch := raw.Watch(watchCtx, prefix, clientv3.WithPrefix(), clientv3.WithCreatedNotify())
	select {
	case created := <-watch:
		require.NoError(t, created.Err())
		require.True(t, created.Created)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	traceDone := make(chan cleanupExecutorClaimTrace, 1)
	go func() { traceDone <- cleanupExecutorObserveClaims(watch, prefix, make(chan struct{})) }()
	traceJoined := false
	defer func() {
		stopWatch()
		if !traceJoined {
			<-traceDone
		}
	}()

	result, executeErr := w.ExecuteCleanupLocal(ctx, s.Task)
	t.Logf("recovery %s/%s actual Execute result=%+v error=%v", s.Recovery, s.RecoveryStage, result, executeErr)
	t.Logf("recovery %s/%s proxy immediately after Execute: %+v", s.Recovery, s.RecoveryStage, proxy.snapshot())
	rowKV := cleanupExecutorRecoveryPoint(t, ctx, raw, prefix+"execution")
	require.LessOrEqual(t, len(rowKV.Value), 4096)
	var row cleanupExecutorRecoveryRow
	require.NoError(t, json.Unmarshal(rowKV.Value, &row))
	t.Logf("recovery %s/%s actual E create=%d mod=%d wire=%s", s.Recovery, s.RecoveryStage, rowKV.CreateRevision, rowKV.ModRevision, rowKV.Value)
	require.NotNil(t, row.Run)
	require.Equal(t, s.Task, row.Task)
	if s.RecoveryStage == "first" {
		require.ErrorIs(t, executeErr, state.ErrOutcomeUnknown)
		require.True(t, result.Retained)
		require.NotEqual(t, state.OutcomeCommitted, result.Outcome)
		require.Equal(t, "occupied", row.Phase)
		require.True(t, row.Run.Joined, "original job must reconcile its unknown progress ACK and publish its own actual join; cold workers may not repair this prerequisite")
		require.NotNil(t, row.Archive)
		if s.Recovery == "bound-recovery" {
			require.Equal(t, "archive-bound", row.Confirmed)
		} else {
			require.Equal(t, "terminated", row.Confirmed)
			release, readErr := b.LoadTaskOwnerRelease(ctx, s.Task)
			require.NoError(t, readErr)
			require.NotNil(t, release, "the actual original release committed before its ACK was withheld")
		}
		slot := cleanupExecutorRecoveryPoint(t, ctx, raw, fmt.Sprintf("%scleanup-execution-slots/%02d", s.Binding.Namespace, row.Slot))
		require.Zero(t, slot.Lease)
		require.Greater(t, slot.CreateRevision, rowKV.CreateRevision)
	} else {
		require.NoError(t, executeErr)
		require.Equal(t, state.OutcomeCommitted, result.Outcome)
		require.False(t, result.Retained)
		require.Equal(t, "settled", row.Phase)
		require.True(t, row.Run.Joined)
		require.NotNil(t, result.Release)
		require.NotNil(t, row.Release)
		repeat, repeatErr := w.ExecuteCleanupLocal(ctx, s.Task)
		require.NoError(t, repeatErr)
		require.Equal(t, result.Slot, repeat.Slot)
		require.Equal(t, result.Release, repeat.Release)
		content, readErr := os.ReadFile(filepath.Join(dir, "target", "cleanup-executor", "nested", "data"))
		require.NoError(t, readErr)
		require.Equal(t, cleanupExecutorPayload, string(content))
		for path, mode := range map[string]os.FileMode{"nested": 0750, "nested/data": 0640} {
			info, statErr := os.Stat(filepath.Join(dir, "target", "cleanup-executor", path))
			require.NoError(t, statErr)
			require.Equal(t, mode, info.Mode().Perm())
		}
	}

	_, err = raw.Put(ctx, prefix+"test-observation-end", s.RecoveryStage)
	require.NoError(t, err)
	var trace cleanupExecutorClaimTrace
	select {
	case trace = <-traceDone:
		traceJoined = true
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	stopWatch()
	require.NoError(t, trace.err)
	require.Empty(t, trace.active)
	wantClaims := 1
	if s.Recovery == "release-settlement-recovery" {
		wantClaims = 2
		if s.RecoveryStage == "second" {
			wantClaims = 0
		}
	}
	require.Len(t, trace.ids, wantClaims)
	wantEvents := make([]string, 0, wantClaims*2)
	for i := 0; i < wantClaims; i++ {
		wantEvents = append(wantEvents, "put", "delete")
	}
	require.Equal(t, wantEvents, append([]string{}, trace.events...))
	stopErr := w.Stop()
	t.Logf("recovery %s/%s actual Stop=%v", s.Recovery, s.RecoveryStage, stopErr)
	if stopErr != nil {
		require.ErrorIs(t, stopErr, state.ErrOutcomeUnknown)
		require.ErrorIs(t, stopErr, context.Canceled)
	}
	<-w.Done()
	require.False(t, w.Status().Running)
	require.False(t, w.Status().Synchronized)
	require.NoError(t, b.Close())
	require.NoError(t, raw.Close())
	proxy.close(t)
	stats := proxy.snapshot()
	t.Logf("recovery %s/%s actual proxy joined stats=%+v claims=%v", s.Recovery, s.RecoveryStage, stats, trace.ids)
	require.Positive(t, stats.WatchStreams)
	require.Equal(t, wantClaims, stats.ClaimPuts)
	if s.RecoveryStage == "first" {
		require.Equal(t, 1, stats.FaultMatched)
		require.Equal(t, 1, stats.FaultCommitted)
		require.Equal(t, 1, stats.FaultJoined)
		require.Positive(t, stats.FaultRevision)
		require.Equal(t, 1, stats.ArchivePuts)
	} else {
		require.Zero(t, stats.FaultMatched)
		require.Zero(t, stats.ArchivePuts, "cold recovery cannot prepare/send a replacement archive command")
		require.Len(t, stats.GrantIDs, 1+wantClaims, "only original worker session and the required managed Claim may Grant")
		require.Contains(t, stats.GrantIDs, sessionLease)
	}
	report := cleanupExecutorRecoveryReport{Result: result, Row: row, Stats: stats, Claims: trace.ids, SessionLease: sessionLease}

	// Only bounded, non-secret observations cross the UID boundary. The report
	// sits beside the private tree; spool/target permissions remain unchanged.
	wire := observerNativeJSON(t, report)
	require.LessOrEqual(t, len(wire), 65536)
	file, err := os.OpenFile(dir+"-"+s.RecoveryStage+".json", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	require.NoError(t, err)
	defer file.Close()
	require.NoError(t, file.Chmod(0644))
	_, err = file.Write(wire)
	require.NoError(t, err)
	require.NoError(t, file.Close())
}

// Cleanup runs as the same ordinary UID after both execution processes have
// actually exited, including a failed first worker. Parent cannot traverse the
// private tree and never changes its ownership or permissions to remove it.
func cleanupExecutorRecoveryRemove(t *testing.T, s cleanupExecutorSetup) {
	t.Helper()
	require.Equal(t, 1000, os.Geteuid())
	require.Equal(t, "/tmp", filepath.Dir(s.WorkerDirectory))
	require.True(t, strings.HasPrefix(filepath.Base(s.WorkerDirectory), "cleanup-executor-recovery-"))
	require.NoError(t, os.RemoveAll(s.WorkerDirectory))
	for _, stage := range []string{"first", "second"} {
		err := os.Remove(s.WorkerDirectory + "-" + stage + ".json")
		if !os.IsNotExist(err) {
			require.NoError(t, err)
		}
	}
}

func cleanupExecutorRunRecoveryWorkers(t *testing.T, ctx context.Context, s cleanupExecutorSetup, admin *clientv3.Client, api *cleanupExecutorAPI) {
	t.Helper()
	dir := filepath.Join("/tmp", "cleanup-executor-recovery-"+uuid.NewString())
	s.WorkerDirectory = dir
	defer func() {
		// A separate finite context also joins cleanup after the case deadline
		// or a Fatal assertion. This role performs no backend/runtime work.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		s.RecoveryStage = "cleanup"
		setup := filepath.Join(s.Directory, "worker-cleanup.json")
		require.NoError(t, os.WriteFile(setup, observerNativeJSON(t, s), 0600))
		child := exec.CommandContext(cleanupCtx, os.Args[0], "-test.run=^TestCleanupExecutorWorkerChild$", "-test.v", "-test.timeout=12s")
		child.Env = append(os.Environ(), "CLEANUP_EXECUTOR_CHILD_SETUP="+setup)
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		require.NoError(t, child.Start())
		waitErr := child.Wait()
		t.Logf("recovery %s/cleanup original worker actual Wait=%v", s.Recovery, waitErr)
		require.NoError(t, waitErr)
	}()
	run := func(stage string) cleanupExecutorRecoveryReport {
		s.RecoveryStage = stage
		setup := filepath.Join(s.Directory, "worker-"+stage+".json")
		require.NoError(t, os.WriteFile(setup, observerNativeJSON(t, s), 0600))
		worker := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCleanupExecutorWorkerChild$", "-test.v", "-test.timeout=110s")
		worker.Env = append(os.Environ(), "CLEANUP_EXECUTOR_CHILD_SETUP="+setup)
		worker.Stdout, worker.Stderr = os.Stdout, os.Stderr
		require.NoError(t, worker.Start())
		waitErr := worker.Wait()
		t.Logf("recovery %s/%s original worker actual Wait=%v", s.Recovery, stage, waitErr)
		require.NoError(t, waitErr)
		file, readErr := os.Open(dir + "-" + stage + ".json")
		require.NoError(t, readErr)
		defer file.Close()
		wire, readErr := io.ReadAll(io.LimitReader(file, 65537))
		require.NoError(t, readErr)
		require.NoError(t, file.Close())
		require.LessOrEqual(t, len(wire), 65536)
		var report cleanupExecutorRecoveryReport
		require.NoError(t, json.Unmarshal(wire, &report))
		return report
	}
	first := run("first")
	prefix := fmt.Sprintf("%sp/%02x/tasks/%s/", s.Binding.Namespace, s.Task.Partition, s.Task.TaskID)
	archiveBefore := cleanupExecutorRecoveryPoint(t, ctx, admin, prefix+"workspace-archive-command")
	if s.Recovery == "release-settlement-recovery" {
		wire := cleanupExecutorRecoveryPoint(t, ctx, admin, prefix+"owner-release")
		var release struct {
			Deleted [3]struct {
				Key string `json:"key"`
			} `json:"deleted"`
		}
		require.NoError(t, json.Unmarshal(wire.Value, &release))
		for _, deleted := range release.Deleted {
			require.NotEmpty(t, deleted.Key)
			point, readErr := admin.Get(ctx, deleted.Key)
			require.NoError(t, readErr)
			require.Empty(t, point.Kvs, "cold settlement cannot require the original deleted owner/control/index")
		}
	}
	api.mu.Lock()
	patchesBefore, deletesBefore, bridgesBefore := api.patches, api.deletes, api.upgrades
	api.mu.Unlock()
	second := run("second")
	archiveAfter := cleanupExecutorRecoveryPoint(t, ctx, admin, prefix+"workspace-archive-command")
	require.Equal(t, archiveBefore, archiveAfter, "original archive command bytes and immutable revisions survive recovery")
	require.Equal(t, first.Row.Incarnation, second.Row.Incarnation)
	require.Equal(t, first.Row.Slot, second.Row.Slot)
	require.Equal(t, first.Row.Archive, second.Row.Archive)
	require.NotEqual(t, first.SessionLease, second.SessionLease)
	if s.Recovery == "bound-recovery" {
		require.NotEqual(t, first.Row.Run.ID, second.Row.Run.ID)
		require.NotEqual(t, first.Row.Run.Worker.Incarnation, second.Row.Run.Worker.Incarnation)
		require.NotEqual(t, first.Claims[0], second.Claims[0])
	} else {
		require.Equal(t, first.Row.Run, second.Row.Run, "terminal settlement must not invent another physical run")
		api.mu.Lock()
		patchesAfter, deletesAfter, bridgesAfter := api.patches, api.deletes, api.upgrades
		api.mu.Unlock()
		require.Equal(t, patchesBefore, patchesAfter)
		require.Equal(t, deletesBefore, deletesAfter)
		require.Equal(t, bridgesBefore, bridgesAfter, "release-only recovery cannot reconnect to the terminated runtime")
	}
	// Parent case continues with the original actual supervisor Wait, terminal
	// E/Run/release, S-empty, provider and cgroup assertions used by complete.
}
