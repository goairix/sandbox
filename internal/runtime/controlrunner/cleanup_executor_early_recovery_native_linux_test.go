//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	k "github.com/goairix/sandbox/internal/runtime/kubernetes"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Early history is a diagnostic report of the original authenticated loaders
// and Query replies. It is never passed back to the consumer as authority.
type cleanupExecutorEarlyHistory struct {
	Close      state.TaskCloseDataReference
	Quiescence *state.TaskQuiescenceReference
}

func cleanupExecutorEarlyPhase(scenario string) string {
	switch scenario {
	case "closed-progress-recovery":
		return "closed"
	case "quiescent-progress-recovery":
		return "quiescent"
	default:
		return ""
	}
}

// The fault is a committed etcd progress ACK loss, not a runtime command ACK
// loss. After its own job joined, the first worker uses the original public
// Load/Query methods to prove the actual runtime receipt already exists.
func cleanupExecutorEarlyQuery(t *testing.T, ctx context.Context, s cleanupExecutorSetup, options state.Options, b *state.Backend) *cleanupExecutorEarlyHistory {
	t.Helper()
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	workspace, err := state.NewWorkspaceIdentity("local", s.WorkspaceStorage, s.WorkspaceBucket, s.WorkspacePrefix)
	require.NoError(t, err)
	verifier, err := p.NewManagementVerifier(s.Binding, []ed25519.PublicKey{s.Root})
	require.NoError(t, err)
	target := k.EnrolledBridgeTarget{
		PodName: s.Runtime.ID, PodUID: s.Runtime.UID,
		NodeName: "node-a", NodeUID: s.NodeUID, NodeBootID: s.NodeBootID,
		LogicalID: s.SandboxID, CreationIntentID: s.IntentID, Generation: s.Generation,
		Authority: s.Binding, ContractDigest: s.Contract, UID: 1000, GID: 1000,
	}
	destination, err := ct.NewDestination(ct.DestinationOptions{
		Identity:           p.RuntimeIdentityContext{SandboxID: s.SandboxID, WorkspaceHash: workspace.Hash(), Generation: s.Generation, Runtime: s.Runtime},
		RuntimeCertificate: s.RuntimeCertificate, Credential: options.LocalCleanup.Credential,
		Verifier: verifier, Clock: options.Clock,
		Dial: func(ctx context.Context) (net.Conn, error) {
			return options.LocalCleanup.Bridge.Dial(ctx, target)
		},
	})
	require.NoError(t, err)

	closeEntry, err := b.LoadTaskCloseData(bounded, s.Task)
	require.NoError(t, err)
	require.NotNil(t, closeEntry)
	require.NotNil(t, closeEntry.Record)
	require.Equal(t, state.OutcomeCommitted, closeEntry.Outcome)
	require.Equal(t, s.Task, closeEntry.Record.Task.Reference)
	closed, err := b.QueryTaskCloseData(bounded, closeEntry.Reference, destination)
	require.NoError(t, err)
	require.NotNil(t, closed.Receipt)
	require.Equal(t, closeEntry.Record.Context, closed.Receipt.Context())
	require.Equal(t, closeEntry.Record.TicketDigest, closed.Receipt.TicketDigest())
	history := &cleanupExecutorEarlyHistory{Close: closeEntry.Reference}
	t.Logf("early recovery %s original close Query authenticated command=%s revision=%d receipt_bytes=%d", s.Recovery, closeEntry.Reference.CommandID, closeEntry.Revision, len(closed.Receipt.Wire()))

	quiescence, err := b.LoadTaskQuiescence(bounded, s.Task)
	require.NoError(t, err)
	attempt, err := b.LoadTaskQuiescenceAttempt(bounded, s.Task)
	require.NoError(t, err)
	if s.Recovery == "closed-progress-recovery" {
		require.Nil(t, quiescence)
		require.Nil(t, attempt, "the closed-only case has no unresolved quiescence reservation")
		return history
	}

	require.NotNil(t, quiescence)
	require.NotNil(t, quiescence.Record)
	require.NotNil(t, attempt)
	require.Equal(t, state.OutcomeCommitted, quiescence.Outcome)
	require.Equal(t, closeEntry.Record.Task, quiescence.Record.Task)
	quiesced, err := b.QueryTaskQuiescence(bounded, s.Task, destination)
	require.NoError(t, err)
	require.NotNil(t, quiesced.Receipt, "progress was confirmed only after the original terminal quiescence receipt")
	require.Equal(t, quiescence.Reference, quiesced.Reference)
	history.Quiescence = &quiescence.Reference
	t.Logf("early recovery %s original quiescence Query authenticated command=%s revision=%d", s.Recovery, quiescence.Reference.CommandID, quiescence.Revision)
	return history
}

// Capture only exact immutable points. The Stage key is the original public
// reference's location; reading it here confers no dispatch or runtime rights.
func cleanupExecutorEarlyCommand(t *testing.T, ctx context.Context, raw *clientv3.Client, key string, stage state.StageReference, task *mvccpb.KeyValue) []*mvccpb.KeyValue {
	t.Helper()
	command := cleanupExecutorRecoveryPoint(t, ctx, raw, key)
	require.LessOrEqual(t, len(command.Value), 16384)
	require.Zero(t, command.Lease)
	require.EqualValues(t, 1, command.Version)
	require.Equal(t, command.CreateRevision, command.ModRevision)
	var decoded struct {
		Task json.RawMessage `json:"task"`
	}
	require.NoError(t, json.Unmarshal(command.Value, &decoded))
	require.JSONEq(t, string(task.Value), string(decoded.Task), "complete original Task, not just its ID, remains bound to the command")
	receiptKey := fmt.Sprintf("%sp/%02x/stages/%s/%s/%s/receipt", stage.Namespace, stage.Partition, stage.RequestID, stage.StageID, stage.AttemptID)
	receipt := cleanupExecutorRecoveryPoint(t, ctx, raw, receiptKey)
	require.LessOrEqual(t, len(receipt.Value), 4096)
	require.Zero(t, receipt.Lease)
	require.EqualValues(t, 1, receipt.Version)
	require.Equal(t, command.CreateRevision, receipt.CreateRevision)
	require.Equal(t, receipt.CreateRevision, receipt.ModRevision)
	return []*mvccpb.KeyValue{command, receipt}
}

func cleanupExecutorEarlyAbsent(t *testing.T, ctx context.Context, raw *clientv3.Client, key string) {
	t.Helper()
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	response, err := raw.Get(bounded, key)
	require.NoError(t, err)
	require.Empty(t, response.Kvs, "unexpected early history at %s", key)
}

// This branch owns no worker process itself: run always joins the actual OS
// worker before returning. The existing parent owns the shared cleanup child,
// supervisor, observer and final settled/provider/cgroup assertions.
func cleanupExecutorEarlyWorkers(t *testing.T, ctx context.Context, s cleanupExecutorSetup, raw *clientv3.Client, api *cleanupExecutorAPI, first cleanupExecutorRecoveryReport, run func(string) cleanupExecutorRecoveryReport) {
	t.Helper()
	require.NotNil(t, first.Early)
	require.Nil(t, first.Row.Archive)
	prefix := fmt.Sprintf("%sp/%02x/tasks/%s/", s.Binding.Namespace, s.Task.Partition, s.Task.TaskID)
	task := cleanupExecutorRecoveryPoint(t, ctx, raw, strings.TrimSuffix(prefix, "/"))
	require.Zero(t, task.Lease)
	require.EqualValues(t, 1, task.Version)
	require.Equal(t, task.CreateRevision, task.ModRevision)
	require.Equal(t, task.CreateRevision, first.Row.TaskBirth)
	immutable := []*mvccpb.KeyValue{task}
	immutable = append(immutable, cleanupExecutorEarlyCommand(t, ctx, raw, prefix+"close-data", first.Early.Close.Stage, task)...)
	if first.Early.Quiescence != nil {
		immutable = append(immutable, cleanupExecutorEarlyCommand(t, ctx, raw, prefix+"quiesce-users", first.Early.Quiescence.Stage, task)...)
		immutable = append(immutable, cleanupExecutorRecoveryPoint(t, ctx, raw, prefix+"quiesce-users-attempt"))
	} else {
		require.Equal(t, "closed-progress-recovery", s.Recovery)
		cleanupExecutorEarlyAbsent(t, ctx, raw, prefix+"quiesce-users")
		cleanupExecutorEarlyAbsent(t, ctx, raw, prefix+"quiesce-users-attempt")
	}
	for _, suffix := range []string{"workspace-archive-command", "final-sync-attempt", "runtime-termination", "runtime-provider-cleanup", "owner-release"} {
		cleanupExecutorEarlyAbsent(t, ctx, raw, prefix+suffix)
	}
	api.mu.Lock()
	patches, deletes := api.patches, api.deletes
	api.mu.Unlock()
	require.Zero(t, patches)
	require.Zero(t, deletes)
	t.Logf("early recovery %s first worker joined; immutable_points=%d original_task_birth=%d archive_absent=true", s.Recovery, len(immutable), task.CreateRevision)

	second := run("second")
	for _, original := range immutable {
		current := cleanupExecutorRecoveryPoint(t, ctx, raw, string(original.Key))
		require.Equal(t, original, current, "original command/receipt/task bytes and revisions cannot change")
	}
	require.Equal(t, first.Row.Task, second.Row.Task)
	require.Equal(t, first.Row.TaskBirth, second.Row.TaskBirth)
	require.Equal(t, first.Row.Incarnation, second.Row.Incarnation)
	require.Equal(t, first.Row.Slot, second.Row.Slot)
	require.NotEqual(t, first.Row.Run.ID, second.Row.Run.ID)
	require.NotEqual(t, first.Row.Run.Worker.Incarnation, second.Row.Run.Worker.Incarnation)
	require.NotEqual(t, first.SessionLease, second.SessionLease)
	require.Len(t, first.Claims, 1)
	require.Len(t, second.Claims, 2, "resume Claim and mandatory post-bind rollover are both original managed Claims")
	for _, claim := range second.Claims {
		require.NotEqual(t, first.Claims[0], claim)
	}
	require.NotNil(t, second.Row.Archive)
	require.Zero(t, second.Stats.ClosePuts)
	if s.Recovery == "quiescent-progress-recovery" {
		require.Zero(t, second.Stats.QuiescencePuts)
	} else {
		require.Len(t, second.Stats.QuiescenceGuards, 1)
		guard := second.Stats.QuiescenceGuards[0]
		command := cleanupExecutorRecoveryPoint(t, ctx, raw, prefix+"quiesce-users")
		require.LessOrEqual(t, len(command.Value), 32768)
		var record struct {
			Attempt state.StageAttemptLocator `json:"attempt"`
		}
		require.NoError(t, json.Unmarshal(command.Value, &record))
		expected := state.StageAttemptLocator{
			Namespace: guard.Stage.Namespace, Partition: guard.Stage.Partition, RestoreEpoch: guard.Stage.RestoreEpoch,
			RequestID: guard.Stage.RequestID, StageID: guard.Stage.StageID, AttemptID: guard.Stage.AttemptID,
		}
		require.Equal(t, expected, record.Attempt)
	}
	t.Logf("early recovery %s immutable history preserved; fresh_run=true actual_managed_claims=2 original_close_writes=0", s.Recovery)
}

func cleanupExecutorEarlyStageGrant(t *testing.T, task state.TaskReference, sessionLease int64, claims []string, stats cleanupExecutorProxyStats) {
	t.Helper()
	require.False(t, stats.ObservationOverflow)
	require.Len(t, claims, 2)
	require.Len(t, stats.ClaimLeases, 2)
	require.Len(t, stats.QuiescenceGuards, 1)
	require.Len(t, stats.GrantIDs, 4, "session + two managed Claims + original quiescence Stage guard")
	guard := stats.QuiescenceGuards[0]
	require.Equal(t, claims[0], guard.Stage.RequestID)
	require.Equal(t, "task_quiesce_users", guard.Stage.StageID)
	require.Equal(t, task.Namespace, guard.Stage.Namespace)
	require.Equal(t, task.Partition, guard.Stage.Partition)
	require.Equal(t, task.RestoreEpoch, guard.Stage.RestoreEpoch)
	require.Equal(t, fmt.Sprintf("%sp/%02x/attempts/%s/guard", task.Namespace, task.Partition, guard.Stage.AttemptID), guard.Key)
	require.Positive(t, guard.LeaseID)
	require.NotEqual(t, sessionLease, guard.LeaseID)
	require.Contains(t, stats.GrantIDs, guard.LeaseID)
	for i, claim := range stats.ClaimLeases {
		require.Equal(t, claims[i], claim.ClaimID)
		require.Contains(t, stats.GrantIDs, claim.LeaseID)
		require.NotEqual(t, guard.LeaseID, claim.LeaseID)
		require.NotEqual(t, sessionLease, claim.LeaseID)
	}
	require.NotEqual(t, stats.ClaimLeases[0].LeaseID, stats.ClaimLeases[1].LeaseID)
	t.Logf("early closed recovery actual quiescence Stage guard=%s lease=%d original_claim=%s", guard.Key, guard.LeaseID, guard.Stage.RequestID)
}
