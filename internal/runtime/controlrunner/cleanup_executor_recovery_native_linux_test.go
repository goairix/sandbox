//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"crypto/sha256"
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

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
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
	Pending     *struct {
		Purpose string `json:"purpose"`
	} `json:"pending"`
	Run *struct {
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
	Early        *cleanupExecutorEarlyHistory
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
	if s.RecoveryStage == "first" && s.Recovery != "observer-ack-recovery" {
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
	var early *cleanupExecutorEarlyHistory
	if s.RecoveryStage == "first" {
		require.ErrorIs(t, executeErr, state.ErrOutcomeUnknown)
		require.True(t, result.Retained)
		require.NotEqual(t, state.OutcomeCommitted, result.Outcome)
		require.Equal(t, "occupied", row.Phase)
		require.True(t, row.Run.Joined, "original job must reconcile its unknown progress ACK and publish its own actual join; cold workers may not repair this prerequisite")
		if cleanupExecutorEarlyPhase(s.Recovery) != "" {
			require.Nil(t, row.Archive, "early progress ACK loss precedes all archive preparation")
			require.Equal(t, cleanupExecutorEarlyPhase(s.Recovery), row.Confirmed)
			early = cleanupExecutorEarlyQuery(t, ctx, s, options, b)
		} else {
			require.NotNil(t, row.Archive)
			if s.Recovery == "bound-recovery" {
				require.Equal(t, "archive-bound", row.Confirmed)
			} else if s.Recovery == "observer-ack-recovery" {
				require.Equal(t, "local-complete", row.Confirmed)
				require.NotNil(t, row.Pending)
				require.Equal(t, "termination", row.Pending.Purpose)
			} else {
				require.Equal(t, "terminated", row.Confirmed)
				release, readErr := b.LoadTaskOwnerRelease(ctx, s.Task)
				require.NoError(t, readErr)
				require.NotNil(t, release, "the actual original release committed before its ACK was withheld")
			}
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
	if s.Recovery == "observer-ack-recovery" && s.RecoveryStage == "first" {
		wantClaims = 2
	}
	if cleanupExecutorEarlyPhase(s.Recovery) != "" && s.RecoveryStage == "second" {
		wantClaims = 2
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
		if s.Recovery == "observer-ack-recovery" {
			// This case faults the actual observer response, never an etcd ACK.
			require.Zero(t, stats.FaultMatched)
			require.Zero(t, stats.FaultCommitted)
			require.Zero(t, stats.FaultJoined)
			require.Zero(t, stats.FaultRevision)
		} else {
			require.Equal(t, 1, stats.FaultMatched)
			require.Equal(t, 1, stats.FaultCommitted)
			require.Equal(t, 1, stats.FaultJoined)
			require.Positive(t, stats.FaultRevision)
		}
		if cleanupExecutorEarlyPhase(s.Recovery) != "" {
			require.Zero(t, stats.ArchivePuts)
			require.Equal(t, 1, stats.ClosePuts)
			wantQuiescence := 0
			if s.Recovery == "quiescent-progress-recovery" {
				wantQuiescence = 1
			}
			require.Equal(t, wantQuiescence, stats.QuiescencePuts)
		} else {
			require.Equal(t, 1, stats.ArchivePuts)
		}
	} else {
		require.Zero(t, stats.FaultMatched)
		if cleanupExecutorEarlyPhase(s.Recovery) != "" {
			require.Equal(t, 1, stats.ArchivePuts, "early recovery creates its first archive only after fresh dual-drain")
			require.Zero(t, stats.ClosePuts, "history cannot reconstruct close dispatch")
			wantQuiescence := 0
			if s.Recovery == "closed-progress-recovery" {
				wantQuiescence = 1
			}
			require.Equal(t, wantQuiescence, stats.QuiescencePuts)
		} else {
			require.Zero(t, stats.ArchivePuts, "cold recovery cannot prepare/send a replacement archive command")
		}
		if s.Recovery == "closed-progress-recovery" {
			// This branch creates its first quiescence command. Its original
			// Stage guard has one Lease in addition to session and Task Claims.
			cleanupExecutorEarlyStageGrant(t, s.Task, sessionLease, trace.ids, stats)
		} else {
			require.Len(t, stats.GrantIDs, 1+wantClaims, "only original worker session and the required managed Claim may Grant")
		}
		require.Contains(t, stats.GrantIDs, sessionLease)
	}
	report := cleanupExecutorRecoveryReport{Result: result, Row: row, Stats: stats, Claims: trace.ids, SessionLease: sessionLease, Early: early}

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

func cleanupExecutorRunRecoveryWorkers(t *testing.T, ctx context.Context, s cleanupExecutorSetup, admin *clientv3.Client, api *cleanupExecutorAPI, observer *cleanupExecutorObserverProxy) {
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
	if cleanupExecutorEarlyPhase(s.Recovery) != "" {
		cleanupExecutorEarlyWorkers(t, ctx, s, admin, api, first, run)
		return
	}
	archiveBefore := cleanupExecutorRecoveryPoint(t, ctx, admin, prefix+"workspace-archive-command")
	var observerHistory *cleanupExecutorObserverHistory
	if s.Recovery == "observer-ack-recovery" {
		require.NotNil(t, observer)
		observerHistory = cleanupExecutorObserverBefore(t, ctx, admin, prefix, observer)
	}
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
	if s.Recovery == "observer-ack-recovery" {
		require.Zero(t, patchesBefore)
		require.Equal(t, 1, deletesBefore, "original provider step 1 already deleted the retained Pod")
	}
	second := run("second")
	archiveAfter := cleanupExecutorRecoveryPoint(t, ctx, admin, prefix+"workspace-archive-command")
	require.Equal(t, archiveBefore, archiveAfter, "original archive command bytes and immutable revisions survive recovery")
	require.Equal(t, first.Row.Incarnation, second.Row.Incarnation)
	require.Equal(t, first.Row.Slot, second.Row.Slot)
	require.Equal(t, first.Row.Archive, second.Row.Archive)
	require.NotEqual(t, first.SessionLease, second.SessionLease)
	if s.Recovery == "bound-recovery" || s.Recovery == "observer-ack-recovery" {
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
	if s.Recovery == "observer-ack-recovery" {
		cleanupExecutorObserverAfter(t, ctx, admin, prefix, observer, observerHistory)
		api.mu.Lock()
		patchesAfter, deletesAfter, bridgesAfter := api.patches, api.deletes, api.upgrades
		api.mu.Unlock()
		require.Zero(t, patchesBefore)
		require.Equal(t, 1, deletesBefore)
		require.Equal(t, 1, patchesAfter)
		require.Equal(t, 1, deletesAfter)
		require.Equal(t, bridgesBefore, bridgesAfter, "observer recovery must not reconnect to the already terminated runtime")
	}
	// Parent case continues with the original actual supervisor Wait, terminal
	// E/Run/release, S-empty, provider and cgroup assertions used by complete.
}

// These observations authenticate no new runtime capability. They retain the
// exact original metadata and signed reply so recovery cannot silently replace
// the reservation, finalized files, or already completed provider effects.
type cleanupExecutorObserverHistory struct {
	intent        p.TaskRuntimeTerminationIntent
	termination   *mvccpb.KeyValue
	completion    *mvccpb.KeyValue
	providerSlots []json.RawMessage
}

func cleanupExecutorObserverBefore(t *testing.T, ctx context.Context, raw *clientv3.Client, prefix string, proxy *cleanupExecutorObserverProxy) *cleanupExecutorObserverHistory {
	t.Helper()
	stats := proxy.snapshot()
	t.Logf("observer recovery first worker actual Wait returned; TLS stats=%+v", stats)
	require.Empty(t, stats.Error)
	require.Equal(t, 1, stats.Terminate)
	require.Zero(t, stats.Query)
	require.Equal(t, 1, stats.TerminalReplies)
	require.Equal(t, 1, stats.Withheld)
	require.Equal(t, 1, stats.PeerClosed)
	require.Equal(t, 1, stats.Joined)
	require.GreaterOrEqual(t, stats.ConnectionLifetime, 4*time.Second, "original five-second caller deadline must close the connection")
	require.Less(t, stats.ConnectionLifetime, 10*time.Second)

	termination := cleanupExecutorRecoveryPoint(t, ctx, raw, prefix+"runtime-termination")
	require.LessOrEqual(t, len(termination.Value), 16384)
	intent, err := p.DecodeTaskRuntimeTerminationIntent(termination.Value)
	require.NoError(t, err)
	require.Equal(t, "reserved", intent.Phase)
	require.Empty(t, intent.TerminalProof)
	require.Equal(t, intent.Current.CommandID, stats.Original.CommandID)
	require.Equal(t, intent.Current.TaskID, stats.Original.TaskID)
	require.Equal(t, intent.EnrollmentDigest, stats.Original.EnrollmentDigest)
	require.Equal(t, fmt.Sprintf("%x", sha256.Sum256(termination.Value)), stats.Original.IntentDigest)
	require.Equal(t, termination.CreateRevision, stats.Original.IntentCreateRevision)
	require.Equal(t, termination.CreateRevision, termination.ModRevision)
	keys, err := p.TaskRuntimeTerminationKeysFor(intent)
	require.NoError(t, err)
	complete := cleanupExecutorRecoveryPoint(t, ctx, raw, keys.LocalComplete)
	require.LessOrEqual(t, len(complete.Value), 8192)
	require.Equal(t, intent.Authority.LocalComplete, p.RecordStamp{CreateRevision: complete.CreateRevision, ModRevision: complete.ModRevision, SHA256: fmt.Sprintf("%x", sha256.Sum256(complete.Value))})
	provider := cleanupExecutorRecoveryPoint(t, ctx, raw, prefix+"runtime-provider-cleanup")
	require.LessOrEqual(t, len(provider.Value), 16384)
	var record struct {
		Slots []json.RawMessage `json:"slots"`
	}
	require.NoError(t, json.Unmarshal(provider.Value, &record))
	require.Len(t, record.Slots, 7)
	for index, wire := range record.Slots {
		var slot struct {
			State string `json:"state"`
		}
		require.NoError(t, json.Unmarshal(wire, &slot))
		if index < 2 {
			require.Equal(t, "complete", slot.State)
		} else {
			require.Equal(t, "unstarted", slot.State)
		}
	}
	t.Logf("observer recovery actual reserved revision=%d; provider steps 0/1 complete; immutable local completion revision=%d", termination.ModRevision, complete.ModRevision)
	return &cleanupExecutorObserverHistory{intent: intent, termination: termination, completion: complete, providerSlots: record.Slots}
}

func cleanupExecutorObserverAfter(t *testing.T, ctx context.Context, raw *clientv3.Client, prefix string, proxy *cleanupExecutorObserverProxy, before *cleanupExecutorObserverHistory) {
	t.Helper()
	proxy.close(t)
	stats := proxy.snapshot()
	require.Empty(t, stats.Error)
	require.Equal(t, 1, stats.Terminate, "recovered reservation is Query-only")
	require.Equal(t, 1, stats.Query)
	require.Equal(t, 2, stats.TerminalReplies)
	require.Equal(t, 1, stats.Withheld)
	require.Equal(t, 1, stats.PeerClosed)
	require.Equal(t, 1, stats.Joined)

	termination := cleanupExecutorRecoveryPoint(t, ctx, raw, prefix+"runtime-termination")
	require.LessOrEqual(t, len(termination.Value), 16384)
	intent, err := p.DecodeTaskRuntimeTerminationIntent(termination.Value)
	require.NoError(t, err)
	require.Equal(t, "cleanup-complete", intent.Phase)
	require.Equal(t, before.termination.CreateRevision, termination.CreateRevision)
	require.Greater(t, termination.ModRevision, before.termination.ModRevision)
	intent.Phase, intent.TerminalProof, intent.CleanupDigest = "reserved", nil, ""
	require.Equal(t, before.intent, intent, "only original phase/proof/cleanup digest may advance")
	complete := cleanupExecutorRecoveryPoint(t, ctx, raw, string(before.completion.Key))
	require.Equal(t, before.completion, complete, "observer recovery never repeats local Finalize")
	provider := cleanupExecutorRecoveryPoint(t, ctx, raw, prefix+"runtime-provider-cleanup")
	require.LessOrEqual(t, len(provider.Value), 16384)
	var record struct {
		Slots []json.RawMessage `json:"slots"`
	}
	require.NoError(t, json.Unmarshal(provider.Value, &record))
	require.Len(t, record.Slots, 7)
	require.Equal(t, before.providerSlots[:2], record.Slots[:2], "completed retain/delete steps cannot be rewritten or replayed")
}
