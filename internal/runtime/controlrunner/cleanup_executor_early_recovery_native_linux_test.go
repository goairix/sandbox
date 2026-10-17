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
	case "pending-close-recovery":
		return "claimed"
	case "pending-quiescence-recovery":
		return "closed"
	default:
		return ""
	}
}

func cleanupExecutorPendingProgress(scenario string) bool {
	return scenario == "pending-close-recovery" || scenario == "pending-quiescence-recovery"
}

func cleanupExecutorEarlyProposedPhase(scenario string) string {
	switch scenario {
	case "pending-close-recovery":
		return "closed"
	case "pending-quiescence-recovery":
		return "quiescent"
	default:
		return cleanupExecutorEarlyPhase(scenario)
	}
}

func cleanupExecutorEarlyHasQuiescence(scenario string) bool {
	return scenario == "quiescent-progress-recovery" || scenario == "pending-quiescence-recovery"
}

// Both fault kinds occur after the actual business reply. Pending cases do
// not submit progress upstream; the earlier cases lose its committed ACK.
// Neither is runtime ACK loss. Original Load/Query proves the real receipt.
func cleanupExecutorEarlyQuery(t *testing.T, ctx context.Context, s cleanupExecutorSetup, options state.Options, b *state.Backend, row cleanupExecutorRecoveryRow) *cleanupExecutorEarlyHistory {
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
	if s.Recovery == "pending-close-recovery" {
		cleanupExecutorPendingStage(t, row, "close", closeEntry.Reference.Stage, closeEntry.Record.Claim)
	}

	quiescence, err := b.LoadTaskQuiescence(bounded, s.Task)
	require.NoError(t, err)
	attempt, err := b.LoadTaskQuiescenceAttempt(bounded, s.Task)
	require.NoError(t, err)
	if !cleanupExecutorEarlyHasQuiescence(s.Recovery) {
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
	if s.Recovery == "pending-quiescence-recovery" {
		cleanupExecutorPendingStage(t, row, "quiescence", quiescence.Reference.Stage, quiescence.Record.Claim)
	}
	return history
}

// The original CAS value is copied from the request that never went upstream.
// Actual post-work join may change only Run.Joined, retaining every other E
// field, including the complete original pending Stage and Claim coordinates.
func cleanupExecutorPendingOriginal(t *testing.T, before string, joined []byte) {
	t.Helper()
	require.NotEmpty(t, before)
	require.LessOrEqual(t, len(before), 4096)
	var original map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(before), &original))
	var run map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(original["run"], &run))
	require.JSONEq(t, "false", string(run["joined"]))
	run["joined"] = json.RawMessage("true")
	wire, err := json.Marshal(run)
	require.NoError(t, err)
	original["run"] = wire
	wire, err = json.Marshal(original)
	require.NoError(t, err)
	require.JSONEq(t, string(wire), string(joined), "actual join must retain the complete original uncommitted-progress E value")
}

func cleanupExecutorPendingStage(t *testing.T, row cleanupExecutorRecoveryRow, purpose string, stage state.StageReference, claim state.TaskClaimReference) {
	t.Helper()
	require.NotNil(t, row.Pending)
	require.Equal(t, purpose, row.Pending.Purpose)
	require.Equal(t, stage.StageID, row.Pending.StageID)
	require.Equal(t, stage.RequestID, row.Pending.RequestID)
	require.Equal(t, stage.AttemptID, row.Pending.AttemptID)
	require.Equal(t, stage.Digest, row.Pending.Digest)
	require.Equal(t, claim.ClaimID, row.Pending.ClaimID)
	require.Equal(t, claim.LeaseID, row.Pending.LeaseID)
	t.Logf("early recovery original pending retained purpose=%s stage=%+v claim=%s", purpose, stage, claim.ClaimID)
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
		require.Contains(t, []string{"closed-progress-recovery", "pending-close-recovery"}, s.Recovery)
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
	if cleanupExecutorEarlyHasQuiescence(s.Recovery) {
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
