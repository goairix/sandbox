//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This independent matcher loses only the post-cleanup progress request. The
// real Terminate result has already returned before this request can exist.
// It does not weaken the early matcher, whose archive must remain absent.
func (p *cleanupExecutorEtcdProxy) unforwardedOwnerProgress(request *pb.TxnRequest) (string, bool) {
	if len(request.Success) != 1 {
		return "", false
	}
	put := request.Success[0].GetRequestPut()
	if put == nil || string(put.Key) != p.prefix+"execution" || len(put.Value) > 4096 {
		return "", false
	}
	var proposed cleanupExecutorRecoveryRow
	if json.Unmarshal(put.Value, &proposed) != nil || proposed.Task != p.task || proposed.Phase != "occupied" || proposed.Confirmed != "terminated" || proposed.Run == nil || proposed.Run.Joined || proposed.Archive == nil || proposed.Pending == nil || proposed.Pending.Purpose != "owner-release" {
		return "", false
	}
	for _, comparison := range request.Compare {
		if comparison == nil || comparison.Target != pb.Compare_VALUE || string(comparison.Key) != p.prefix+"execution" {
			continue
		}
		wire := comparison.GetValue()
		var original cleanupExecutorRecoveryRow
		if len(wire) > 4096 || json.Unmarshal(wire, &original) != nil || original.Task != p.task || original.Phase != "occupied" || original.Confirmed != "local-complete" || original.Run == nil || original.Run.Joined || original.Run.ID != proposed.Run.ID || original.Archive == nil || original.Pending == nil || original.Pending.Purpose != "termination" {
			continue
		}
		pending := original.Pending
		if pending.ClaimID == "" || pending.LeaseID <= 0 || pending.StageID != "" || pending.RequestID != "" || pending.AttemptID != "" || pending.Digest != "" || pending.ClaimID != proposed.Pending.ClaimID || pending.LeaseID != proposed.Pending.LeaseID {
			continue
		}
		return string(wire), true
	}
	return "", false
}

// The original public loader authenticates the actual observer proof and
// completed provider digest. Decoding below only attributes those real bytes
// to the retained pending coordinates; it creates no proof or capability.
func cleanupExecutorOwnerCompleted(t *testing.T, ctx context.Context, s cleanupExecutorSetup, b *state.Backend, raw *clientv3.Client, prefix string, row cleanupExecutorRecoveryRow, wire []byte, stats cleanupExecutorProxyStats) {
	t.Helper()
	require.Equal(t, "local-complete", row.Confirmed)
	require.NotNil(t, row.Pending)
	require.Equal(t, "termination", row.Pending.Purpose)
	require.Empty(t, row.Pending.StageID)
	require.Empty(t, row.Pending.RequestID)
	require.Empty(t, row.Pending.AttemptID)
	require.Empty(t, row.Pending.Digest)
	cleanupExecutorPendingOriginal(t, stats.UnforwardedExecution, wire)

	bounded, cancel := context.WithTimeout(ctx, 6*time.Second)
	entry, err := b.LoadTaskRuntimeTermination(bounded, s.Task)
	cancel()
	require.NoError(t, err)
	require.NotNil(t, entry)
	require.Equal(t, "cleanup-complete", entry.Phase)
	termination := cleanupExecutorRecoveryPoint(t, ctx, raw, prefix+"runtime-termination")
	require.LessOrEqual(t, len(termination.Value), 16384)
	intent, err := p.DecodeTaskRuntimeTerminationIntent(termination.Value)
	require.NoError(t, err)
	require.Equal(t, "cleanup-complete", intent.Phase)
	require.NotEmpty(t, intent.TerminalProof)
	require.Equal(t, row.TaskBirth, intent.TaskBirth)
	require.Equal(t, row.Pending.ClaimID, intent.Current.ClaimID)
	require.Equal(t, row.Pending.LeaseID, intent.Current.LeaseID)
	require.Equal(t, termination.CreateRevision, entry.CreateRevision)
	require.Equal(t, termination.ModRevision, entry.ModRevision)
	require.Equal(t, fmt.Sprintf("%x", sha256.Sum256(termination.Value)), entry.SHA256)

	bounded, cancel = context.WithTimeout(ctx, 6*time.Second)
	release, err := b.LoadTaskOwnerRelease(bounded, s.Task)
	cancel()
	require.NoError(t, err)
	require.Nil(t, release, "actual completed cleanup must still hold its original owner")
	t.Logf("owner-held first authenticated cleanup-complete revision=%d pending_claim=%s pending_lease=%d release_absent=true", termination.ModRevision, row.Pending.ClaimID, row.Pending.LeaseID)
}

// Both workers are actual joined OS processes owned by the shared recovery
// parent. These exact points preserve the completed business result across
// the new worker; only owner release and execution bookkeeping may advance.
func cleanupExecutorOwnerWorkers(t *testing.T, ctx context.Context, s cleanupExecutorSetup, raw *clientv3.Client, api *cleanupExecutorAPI, first cleanupExecutorRecoveryReport, run func(string) cleanupExecutorRecoveryReport) {
	t.Helper()
	prefix := fmt.Sprintf("%sp/%02x/tasks/%s/", s.Binding.Namespace, s.Task.Partition, s.Task.TaskID)
	termination := cleanupExecutorRecoveryPoint(t, ctx, raw, prefix+"runtime-termination")
	require.LessOrEqual(t, len(termination.Value), 16384)
	intent, err := p.DecodeTaskRuntimeTerminationIntent(termination.Value)
	require.NoError(t, err)
	require.Equal(t, "cleanup-complete", intent.Phase)
	require.NotEmpty(t, intent.TerminalProof)
	require.Greater(t, termination.ModRevision, termination.CreateRevision)
	require.Greater(t, termination.Version, int64(1), "completed history is mutable, unlike the initial reservation")
	keys, err := p.TaskRuntimeTerminationKeysFor(intent)
	require.NoError(t, err)
	provider := cleanupExecutorRecoveryPoint(t, ctx, raw, prefix+"runtime-provider-cleanup")
	require.LessOrEqual(t, len(provider.Value), 16384)
	require.Equal(t, intent.CleanupDigest, fmt.Sprintf("%x", sha256.Sum256(provider.Value)))
	var cleanup struct {
		Slots []struct {
			State string `json:"state"`
		} `json:"slots"`
	}
	require.NoError(t, json.Unmarshal(provider.Value, &cleanup))
	require.Len(t, cleanup.Slots, 7)
	for index, slot := range cleanup.Slots {
		if index >= 2 && index <= 5 {
			require.Contains(t, []string{"complete", "skipped"}, slot.State)
		} else {
			require.Equal(t, "complete", slot.State)
		}
	}

	task := cleanupExecutorRecoveryPoint(t, ctx, raw, keys.Task)
	archive := cleanupExecutorRecoveryPoint(t, ctx, raw, keys.Archive)
	complete := cleanupExecutorRecoveryPoint(t, ctx, raw, keys.LocalComplete)
	require.Equal(t, first.Row.TaskBirth, task.CreateRevision)
	require.Equal(t, intent.Authority.LocalComplete, p.RecordStamp{CreateRevision: complete.CreateRevision, ModRevision: complete.ModRevision, SHA256: fmt.Sprintf("%x", sha256.Sum256(complete.Value))})
	immutable := []*mvccpb.KeyValue{termination, provider, archive, complete, task}
	owners := []string{keys.Owner, keys.Control, keys.RuntimeIndex}
	for _, key := range owners {
		point := cleanupExecutorRecoveryPoint(t, ctx, raw, key)
		require.Zero(t, point.Lease)
	}
	cleanupExecutorEarlyAbsent(t, ctx, raw, prefix+"owner-release")
	api.mu.Lock()
	patches, deletes, bridges, absent := api.patches, api.deletes, api.upgrades, api.pod == nil
	api.mu.Unlock()
	require.Equal(t, 1, patches)
	require.Equal(t, 1, deletes)
	require.True(t, absent)
	t.Logf("owner-held first actual Wait returned; completed_revision=%d provider_revision=%d owner_keys=3 release_absent=true", termination.ModRevision, provider.ModRevision)

	second := run("second")
	for _, original := range immutable {
		current := cleanupExecutorRecoveryPoint(t, ctx, raw, string(original.Key))
		require.Equal(t, original, current, "completed history bytes and full native revisions must remain exact")
	}
	for _, key := range owners {
		cleanupExecutorEarlyAbsent(t, ctx, raw, key)
	}
	release := cleanupExecutorRecoveryPoint(t, ctx, raw, prefix+"owner-release")
	require.Greater(t, release.CreateRevision, termination.ModRevision)
	require.Equal(t, first.Row.Task, second.Row.Task)
	require.Equal(t, first.Row.TaskBirth, second.Row.TaskBirth)
	require.Equal(t, first.Row.Incarnation, second.Row.Incarnation)
	require.Equal(t, first.Row.Slot, second.Row.Slot)
	require.Equal(t, first.Row.Archive, second.Row.Archive)
	require.NotEqual(t, first.Row.Run.ID, second.Row.Run.ID)
	require.NotEqual(t, first.Row.Run.Worker.Incarnation, second.Row.Run.Worker.Incarnation)
	require.NotEqual(t, first.SessionLease, second.SessionLease)
	require.Len(t, first.Claims, 2)
	require.Len(t, second.Claims, 1)
	require.NotContains(t, first.Claims, second.Claims[0])
	require.Zero(t, second.Stats.ArchivePuts)
	require.Zero(t, second.Stats.TerminationPuts)
	require.Zero(t, second.Stats.ProviderPuts)
	require.Len(t, second.Stats.GrantIDs, 2, "only original session and the single managed release Claim")
	api.mu.Lock()
	patchesAfter, deletesAfter, bridgesAfter := api.patches, api.deletes, api.upgrades
	api.mu.Unlock()
	require.Equal(t, patches, patchesAfter)
	require.Equal(t, deletes, deletesAfter)
	require.Equal(t, bridges, bridgesAfter, "owner-held release must not reconnect to the terminated runtime")
	t.Logf("owner-held recovery immutable_points=%d actual_managed_claims=1 archive_writes=0 termination_writes=0 provider_writes=0 bridge_delta=0 owner_keys_deleted=3", len(immutable))
}
