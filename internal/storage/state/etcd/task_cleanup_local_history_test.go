package etcd

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This is deliberately only codec-valid release metadata. It carries no
// termination seal/history and must never become a positive private proof.
// The actual Linux release-settlement-recovery case supplies genuine proof.
func cleanupUnsealedRelease(t *testing.T, f *cleanupActorFixture, wrongTask bool) string {
	t.Helper()
	task := f.evidence.bundle.task
	birth := f.evidence.bundle.birth
	ref := f.ref
	if wrongTask {
		ref.TaskID = uuid.NewString()
	}
	stamp := p.RecordStamp{CreateRevision: birth, ModRevision: birth, SHA256: strings.Repeat("a", 64)}
	r := localOwnerReleaseRecord{
		Version: 1, Domain: "sandbox.task-owner-release.v1", Task: ref, TaskBirth: birth,
		Claim:   taskCloseClaimWire{Task: ref, ClaimID: uuid.NewString(), WorkerID: "metadata-only", CreateRevision: birth + 1, LeaseID: 1},
		Runtime: task.Runtime, WorkspaceHash: task.WorkspaceHash, Generation: task.Generation,
		NodeUID: uuid.NewString(), NodeBootID: uuid.NewString(),
		Complete: stamp, Termination: stamp, CleanupDigest: strings.Repeat("b", 64),
	}
	for i := range r.Deleted {
		r.Deleted[i] = localOwnerReleasedKey{Key: f.evidence.bundle.fences[i+3].key, Stamp: stamp}
	}
	wire, err := encodeLocalOwnerRelease(r)
	require.NoError(t, err)
	return wire
}

// Genuine four-point metadata can authenticate permanent history without a
// release seal. These tests never turn that subset into physical permission or
// call settlement with a fabricated positive proof.
func TestCleanupLocalTerminalHistory(t *testing.T) {
	f := newCleanupSlotTestFixture(t)
	state := f.occupy(t)
	_, input := cleanupLocalWireFixture(t)
	run := *input.state.record.Run
	run.Joined = true
	r := state.record
	r.Run, r.Confirmed = &run, "terminated"
	wire, err := encodeCleanupExecution(r)
	require.NoError(t, err)
	_, err = f.raw.Put(f.ctx, state.execution.key, wire)
	require.NoError(t, err)
	state = f.observe(t)
	taskKey, err := f.b.namespace.taskKey(f.ref)
	require.NoError(t, err)
	intentKey, err := f.b.namespace.cleanupIntentKey(f.ref)
	require.NoError(t, err)
	linkKey, err := f.b.namespace.taskLinkKey(f.ref)
	require.NoError(t, err)
	response, err := f.raw.Txn(f.ctx).Then(clientv3.OpGet(taskKey), clientv3.OpGet(intentKey), clientv3.OpGet(linkKey), clientv3.OpGet(f.evidence.receipt.key)).Commit()
	require.NoError(t, err)
	points := make([]*mvccpb.KeyValue, 4)
	for i, op := range response.Responses {
		require.Len(t, op.GetResponseRange().Kvs, 1)
		points[i] = op.GetResponseRange().Kvs[0]
	}
	history, err := f.b.cleanupTerminalEvidenceFromPoints(state, points, response.Header.Revision)
	require.NoError(t, err)
	require.Equal(t, cleanupSlotRecordFromExecution(state.record), history.record)
	require.Equal(t, ownTaskFence(points[0]), history.task)
	require.Equal(t, ownTaskFence(points[1]), history.intent)
	require.Equal(t, ownTaskFence(points[3]), history.receipt)
	f.turn(t, func(owner *workerDueOwner) {
		loaded, loadErr := f.b.loadCleanupTerminalEvidence(owner, state)
		require.NoError(t, loadErr)
		require.Equal(t, history, loaded)
		cmps, compareErr := f.b.cleanupTerminalComparisons(owner, loaded)
		require.NoError(t, compareErr)
		require.Len(t, cmps, 32, "original W20 + permanent Task/intent/receipt12")
	})

	clone := func() (*cleanupSlotState, []*mvccpb.KeyValue) {
		owned := *state
		slot := *state.slot
		owned.slot = &slot
		run := *state.record.Run
		owned.record.Run = &run
		values := make([]*mvccpb.KeyValue, len(points))
		for i, point := range points {
			kv := *point
			kv.Key = append([]byte(nil), point.Key...)
			kv.Value = append([]byte(nil), point.Value...)
			values[i] = &kv
		}
		return &owned, values
	}
	reject := func(t *testing.T, altered *cleanupSlotState, values []*mvccpb.KeyValue, revision int64) {
		t.Helper()
		got, validateErr := f.b.cleanupTerminalEvidenceFromPoints(altered, values, revision)
		require.Error(t, validateErr)
		require.Nil(t, got)
	}
	for i, name := range []string{"task", "intent", "link", "receipt"} {
		for field, mutate := range map[string]func(*mvccpb.KeyValue){
			"lease":   func(kv *mvccpb.KeyValue) { kv.Lease = 1 },
			"version": func(kv *mvccpb.KeyValue) { kv.Version = 2 },
			"create":  func(kv *mvccpb.KeyValue) { kv.CreateRevision-- },
			"mod":     func(kv *mvccpb.KeyValue) { kv.ModRevision++ },
			"birth": func(kv *mvccpb.KeyValue) {
				kv.CreateRevision++
				kv.ModRevision++
			},
			"key": func(kv *mvccpb.KeyValue) { kv.Key = append(kv.Key, 'x') },
		} {
			t.Run(name+"/"+field, func(t *testing.T) {
				altered, values := clone()
				mutate(values[i])
				reject(t, altered, values, response.Header.Revision)
			})
		}
	}
	for _, scenario := range []string{"nil-state", "nil-points", "missing-point", "nil-point", "future-revision", "intent-locator", "intent-task", "link-reference", "receipt-canonical", "receipt-digest", "receipt-aborted", "execution-digest", "execution-mutation-digest", "execution-receipt-digest", "execution-incarnation", "unjoined"} {
		t.Run(scenario, func(t *testing.T) {
			altered, values := clone()
			revision := response.Header.Revision
			switch scenario {
			case "nil-state":
				altered = nil
			case "nil-points":
				values = nil
			case "missing-point":
				values = values[:3]
			case "nil-point":
				values[3] = nil
			case "future-revision":
				revision = state.execution.mod - 1
			case "intent-locator", "intent-task":
				var intent CleanupIntentRecord
				require.NoError(t, decodeCleanupIntentRecord(values[1], &intent))
				if scenario == "intent-locator" {
					intent.Attempt.AttemptID = uuid.NewString()
				} else {
					intent.Task.Runtime.ID = "another-runtime"
				}
				encoded, encodeErr := encodeCleanupIntentRecord(intent)
				require.NoError(t, encodeErr)
				values[1].Value = []byte(encoded)
			case "link-reference":
				var link TaskLinkRecord
				require.NoError(t, decodeTaskLinkRecord(values[2], &link))
				link.Reference.TaskID = uuid.NewString()
				encoded, encodeErr := encodeTaskLinkRecord(link)
				require.NoError(t, encodeErr)
				values[2].Value = []byte(encoded)
			case "receipt-canonical":
				values[3].Value = append([]byte(" "), values[3].Value...)
			case "receipt-digest", "receipt-aborted":
				var receipt stageReceipt
				require.NoError(t, json.Unmarshal(values[3].Value, &receipt))
				if scenario == "receipt-digest" {
					receipt.Digest = strings.Repeat("d", 64)
				} else {
					receipt.Outcome = OutcomeAborted
				}
				encoded, encodeErr := encodeReceipt(receipt.StageReference, receipt.Outcome)
				require.NoError(t, encodeErr)
				values[3].Value = []byte(encoded)
			case "execution-digest", "execution-mutation-digest", "execution-receipt-digest":
				// Preserve the candidate's first digest byte, then rebuild a
				// schema-valid E/S identity. Only real history can reject this.
				switch scenario {
				case "execution-digest":
					altered.record.TaskDigest = altered.record.TaskDigest[:2] + strings.Repeat("d", 62)
				case "execution-mutation-digest":
					altered.record.InitialMutationDigest = strings.Repeat("d", 64)
				case "execution-receipt-digest":
					altered.record.InitialReceiptDigest = strings.Repeat("d", 64)
				}
				altered.record.Incarnation = cleanupIncarnation(cleanupSlotRecordFromExecution(altered.record))
				encoded, encodeErr := encodeCleanupExecution(altered.record)
				require.NoError(t, encodeErr)
				altered.execution.value = encoded
				encoded, encodeErr = encodeCleanupExecutionSlot(cleanupSlotRecordFromExecution(altered.record))
				require.NoError(t, encodeErr)
				altered.slot.value = encoded
			case "execution-incarnation":
				altered.record.Incarnation = uuid.NewString()
			case "unjoined":
				altered.record.Run.Joined = false
				encoded, encodeErr := encodeCleanupExecution(altered.record)
				require.NoError(t, encodeErr)
				altered.execution.value = encoded
			}
			reject(t, altered, values, revision)
		})
	}
	require.Equal(t, ownTaskFence(points[0]), history.task, "negative caller mutations cannot change owned authenticated history")
}

func TestCleanupLocalRecoveryJoinedRelease(t *testing.T) {
	for _, scenario := range []string{"cold-unjoined", "missing-proof", "malformed-proof", "unsealed-release", "wrong-task-proof", "slot-aba"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCleanupActorFixture(t)
			e, s, record := cleanupLocalRetainedRows(t, f, "owner-release")
			if scenario != "cold-unjoined" {
				// Joined is only persisted diagnostic input here, not a claim of
				// host physical work. Positive settlement is exclusively native.
				run := *record.Run
				run.Joined = true
				record.Run = &run
				record.Confirmed = "terminated"
				wire, err := encodeCleanupExecution(record)
				require.NoError(t, err)
				_, err = f.raw.Put(f.ctx, e, wire)
				require.NoError(t, err)
			}
			_, _, releaseKey, err := f.b.namespace.localRuntimeReleaseKeys(f.ref)
			require.NoError(t, err)
			switch scenario {
			case "cold-unjoined", "malformed-proof":
				_, err = f.raw.Put(f.ctx, releaseKey, `{}`)
				require.NoError(t, err)
			case "unsealed-release", "wrong-task-proof":
				wire := cleanupUnsealedRelease(t, f, scenario == "wrong-task-proof")
				_, err = f.raw.Put(f.ctx, releaseKey, wire)
				require.NoError(t, err)
				point, readErr := f.raw.Get(f.ctx, releaseKey)
				require.NoError(t, readErr)
				require.Len(t, point.Kvs, 1)
				_, decodeErr := decodeLocalOwnerRelease(point.Kvs[0])
				require.NoError(t, decodeErr, "this negative must reach task/history authentication, not fail release codec")
			case "slot-aba":
				point, readErr := f.raw.Get(f.ctx, s)
				require.NoError(t, readErr)
				require.Len(t, point.Kvs, 1)
				_, err = f.raw.Delete(f.ctx, s)
				require.NoError(t, err)
				_, err = f.raw.Put(f.ctx, s, string(point.Kvs[0].Value))
				require.NoError(t, err)
			}

			// Independently establish the existing private reader's rejection.
			// The actor must use this authentication for a cold joined Run;
			// unjoined work must remain untouched even with malformed release.
			if scenario == "malformed-proof" || scenario == "unsealed-release" || scenario == "wrong-task-proof" {
				proof, readErr := f.b.readLocalOwnerReleaseProof(f.ctx, f.ref)
				require.ErrorIs(t, readErr, ErrCorruptRecord)
				require.Nil(t, proof)
			}
			before, err := f.raw.Txn(f.ctx).Then(clientv3.OpGet(e), clientv3.OpGet(s), clientv3.OpGet(releaseKey)).Commit()
			require.NoError(t, err)
			job := f.queue(t, f.ctx, f.ref)
			_ = workerTestReceive(t, f.probe.entered)
			f.probe.unblock()
			// Observe and private proof are separate charged turns. Leave a
			// finite allowance for original credit waits and native RPCs.
			deadline := time.NewTimer(12 * time.Second)
			defer deadline.Stop()
			select {
			case <-job.done:
			case <-deadline.C:
				t.Fatal("cold release diagnosis did not join its bounded turns")
			}
			require.NotEqual(t, OutcomeCommitted, job.result.Outcome)
			require.Nil(t, job.result.Release)
			if scenario != "slot-aba" {
				require.True(t, job.result.Retained)
				require.NotNil(t, job.result.Slot)
				require.Equal(t, record.Incarnation, job.result.Slot.Incarnation)
			}
			if scenario == "malformed-proof" || scenario == "unsealed-release" || scenario == "wrong-task-proof" {
				require.ErrorIs(t, job.err, ErrCorruptRecord, "joined recovery must authenticate original proof, not silently return retained-run")
			}
			if scenario == "cold-unjoined" {
				require.NoError(t, job.err)
				require.Equal(t, "retained-run", job.result.Reason)
			}
			after, err := f.raw.Txn(f.ctx).Then(clientv3.OpGet(e), clientv3.OpGet(s), clientv3.OpGet(releaseKey)).Commit()
			require.NoError(t, err)
			for i := range before.Responses {
				require.Equal(t, before.Responses[i].GetResponseRange().Kvs, after.Responses[i].GetResponseRange().Kvs, "negative recovery cannot alter Run, occupation or release history")
			}
			f.assertNoPhysicalGrant(t)
		})
	}
}
