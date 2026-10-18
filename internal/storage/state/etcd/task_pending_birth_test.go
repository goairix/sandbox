package etcd

import (
	"bytes"
	"context"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/launcher"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type cleanupBirthTestSeed struct {
	task    TaskRecord
	intent  CleanupIntentRecord
	stage   StageReference
	keys    [7]string
	wires   [4]string
	birth   int64
	missing []string
}

// Seed canonical history at one real model transaction revision. No surviving
// owner/control/runtime keys, TaskClaim, execution slot or physical proof exist.
func seedCleanupBirth(t *testing.T, f *backfillBootstrapFixture) cleanupBirthTestSeed {
	t.Helper()
	task, intent, link, _ := taskTestRecords()
	task.Reference.Namespace, task.Reference.RestoreEpoch = f.b.namespace.Root(), f.b.restoreEpoch
	task.Reference.Partition, task.Reference.SandboxID = 0xaa, "actual-intent-sandbox"
	task.WorkspaceHash = "aa" + strings.Repeat("1", 62)
	intent.Task = task
	intent.Attempt.Namespace, intent.Attempt.RestoreEpoch, intent.Attempt.Partition = task.Reference.Namespace, task.Reference.RestoreEpoch, task.Reference.Partition
	intent.Attempt.RequestID, intent.Attempt.StageID = "actual-initial-request", "destroy"
	link.Reference = task.Reference
	s := cleanupBirthTestSeed{task: task, intent: intent, stage: intent.Attempt.reference(strings.Repeat("d", 64))}
	var err error
	s.keys[0], err = f.b.namespace.taskKey(task.Reference)
	require.NoError(t, err)
	s.keys[1], err = f.b.namespace.cleanupIntentKey(task.Reference)
	require.NoError(t, err)
	s.keys[2], err = f.b.namespace.taskLinkKey(task.Reference)
	require.NoError(t, err)
	_, s.keys[3], err = f.b.stageKeys(s.stage)
	require.NoError(t, err)
	s.keys[4], err = f.b.namespace.cleanupExecutionKey(task.Reference)
	require.NoError(t, err)
	s.keys[5], err = f.b.namespace.cleanupPendingKey(task.Reference)
	require.NoError(t, err)
	_, _, s.keys[6], err = f.b.namespace.localRuntimeReleaseKeys(task.Reference)
	require.NoError(t, err)
	s.wires[0], err = encodeTaskRecord(task)
	require.NoError(t, err)
	s.wires[1], err = encodeCleanupIntentRecord(intent)
	require.NoError(t, err)
	s.wires[2], err = encodeTaskLinkRecord(link)
	require.NoError(t, err)
	s.wires[3], err = encodeReceipt(s.stage, OutcomeCommitted)
	require.NoError(t, err)
	var ops []clientv3.Op
	for i, wire := range s.wires {
		ops = append(ops, clientv3.OpPut(s.keys[i], wire))
	}
	response, err := f.b.client.Txn(context.Background()).Then(ops...).Commit()
	require.NoError(t, err)
	s.birth = response.Header.Revision
	control, _, err := f.b.namespace.sandboxKeys(0xaa, task.Reference.SandboxID, "read")
	require.NoError(t, err)
	owner, err := f.b.namespace.Key("p", "aa", "workspaces", task.WorkspaceHash, "owner")
	require.NoError(t, err)
	index, err := f.b.namespace.runtimeIndexKey(task.Runtime.UID)
	require.NoError(t, err)
	s.missing = []string{control, owner, index}
	for _, key := range s.missing {
		require.Nil(t, f.server.point(key))
	}
	return s
}

func cleanupBirthTestPoints(f *backfillBootstrapFixture, s cleanupBirthTestSeed) []*mvccpb.KeyValue {
	points := make([]*mvccpb.KeyValue, len(s.keys))
	for i, key := range s.keys {
		points[i] = f.server.point(key)
	}
	return points
}

func cleanupBirthTestRead(t *testing.T, f *backfillBootstrapFixture, taskID string) *cleanupBirthEvidence {
	t.Helper()
	backfillBootstrapCredit(f.b, f.ranges, 262144)
	evidence, err := f.b.readCleanupBirth(context.Background(), f.partition, f.ranges, taskID)
	require.NoError(t, err)
	require.NotNil(t, evidence)
	return evidence
}

// These E records test metadata attribution only. No occupied slot, joined
// kernel boundary or terminal proof is fabricated or supplied to settlement.
func cleanupBirthTestExecution(s cleanupBirthTestSeed) cleanupExecutionRecord {
	r := cleanupExecutionRecord{
		Version: 1, Domain: cleanupExecutionDomain, Task: s.task.Reference,
		TaskBirth: s.birth, InitialAttempt: s.intent.Attempt,
		TaskDigest: workerIdentityDigest(s.wires[0]), InitialMutationDigest: s.stage.Digest,
		InitialReceiptDigest: workerIdentityDigest(s.wires[3]), Phase: "selecting", Confirmed: "queued",
	}
	r.Slot = cleanupCandidate(r.TaskDigest, r.Selection)
	r.Incarnation = cleanupIncarnation(cleanupSlotRecordFromExecution(r))
	return r
}

// Codec-valid release metadata deliberately lacks every signed terminal
// artifact. A birth read may retain it only as a preliminary observation.
func cleanupBirthTestRelease(t *testing.T, s cleanupBirthTestSeed) string {
	t.Helper()
	stamp := p.RecordStamp{CreateRevision: s.birth, ModRevision: s.birth, SHA256: strings.Repeat("a", 64)}
	r := localOwnerReleaseRecord{
		Version: 1, Domain: "sandbox.task-owner-release.v1", Task: s.task.Reference, TaskBirth: s.birth,
		Claim:   taskCloseClaimWire{Task: s.task.Reference, ClaimID: taskClaimTestID, WorkerID: "metadata-only", CreateRevision: s.birth + 1, LeaseID: 1},
		Runtime: s.task.Runtime, WorkspaceHash: s.task.WorkspaceHash, Generation: s.task.Generation,
		NodeUID: "11111111-1111-4111-8111-111111111111", NodeBootID: "22222222-2222-4222-8222-222222222222",
		Complete: stamp, Termination: stamp, CleanupDigest: strings.Repeat("b", 64),
	}
	for i, key := range s.missing {
		r.Deleted[i] = localOwnerReleasedKey{Key: key, Stamp: stamp}
	}
	wire, err := encodeLocalOwnerRelease(r)
	require.NoError(t, err)
	return wire
}

// This is unsigned preliminary metadata only. Its synthetic historical stamps
// satisfy the seal codec but have no terminal/provider/source records behind
// them. Birth discovery may retain the same-cohort hint, never a signed proof.
func cleanupBirthTestDetachedRelease(t *testing.T, s cleanupBirthTestSeed) detachedOwnerReleaseRecord {
	t.Helper()
	stamp := p.RecordStamp{CreateRevision: s.birth, ModRevision: s.birth, SHA256: strings.Repeat("a", 64)}
	r := detachedOwnerReleaseRecord{
		Version: 2, Domain: "sandbox.task-owner-release.v2", Task: s.task.Reference, TaskBirth: s.birth,
		Claim:   taskCloseClaimWire{Task: s.task.Reference, ClaimID: taskClaimTestID, WorkerID: "metadata-hint-only", CreateRevision: s.birth + 1, LeaseID: 1},
		Runtime: s.task.Runtime, WorkspaceHash: s.task.WorkspaceHash, Generation: s.task.Generation,
		NodeUID: "11111111-1111-4111-8111-111111111111", NodeBootID: "22222222-2222-4222-8222-222222222222",
		Termination:   p.RecordStamp{CreateRevision: s.birth + 2, ModRevision: s.birth + 2, SHA256: strings.Repeat("b", 64)},
		CleanupDigest: strings.Repeat("c", 64),
		DetachedEffects: p.DetachedRuntimeEffectBindings{
			Version: 1, Profile: "detached_enrolled_plain", ContractDigest: strings.Repeat("d", 64), Settlement: stamp,
		},
	}
	for i, key := range detachedOwnerReleaseDeletedKeys(r) {
		r.Deleted[i] = localOwnerReleasedKey{Key: key, Stamp: stamp}
	}
	_, err := encodeDetachedOwnerRelease(r)
	require.NoError(t, err)
	return r
}

func TestCleanupBirthDetachedPreliminaryHint(t *testing.T) {
	f := newBackfillBootstrapFixture(t, 60*time.Second)
	s := seedCleanupBirth(t, f)
	r := cleanupBirthTestDetachedRelease(t, s)
	wire, err := encodeDetachedOwnerRelease(r)
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		f.server.put(f.b.namespace.Root()+"detached-hint-observation", "later", 0)
	}
	f.server.put(s.keys[6], wire, 0)
	pending, err := encodeCleanupPending(cleanupPendingRecord{Version: 1, Reference: s.task.Reference, TaskDigest: workerIdentityDigest(s.wires[0])})
	require.NoError(t, err)
	f.server.put(s.keys[5], pending, 0)
	seal := f.server.point(s.keys[6])
	_, err = decodeDetachedOwnerRelease(seal)
	require.NoError(t, err, "precondition is a canonical permanent v2 hint, not terminal authentication")
	before := len(backfillBootstrapRequests(f))
	grants, keeps, revokes := f.server.counts()
	backfillBootstrapCredit(f.b, f.ranges, 262144)
	birth, err := f.b.readCleanupBirth(context.Background(), f.partition, f.ranges, s.task.Reference.TaskID)
	require.NoError(t, err, "the original guarded birth cohort must accept a strict v2 preliminary hint")
	require.NotNil(t, birth)
	require.NoError(t, f.b.validateCleanupBirthEvidence(f.partition, birth))
	require.NotNil(t, birth.release)
	require.Equal(t, ownTaskFence(seal), *birth.release)
	require.Nil(t, birth.releaseRecord, "a detached hint cannot fabricate the old local Complete/archive record")
	require.Len(t, backfillBootstrapRequests(f)[before:], 4)

	// This fixture has no terminal trust or authenticated release history.
	// Neither discovery nor present P can admit a delete or consume its Claim.
	before = len(backfillBootstrapRequests(f))
	release, err := f.b.readCleanupBirthRelease(context.Background(), f.partition, f.ranges, birth)
	require.ErrorIs(t, err, ErrInvalidConfiguration)
	require.Nil(t, release)
	err = f.b.deleteCleanupPending(context.Background(), f.partition, f.claims, birth, nil)
	require.Error(t, err)
	require.Len(t, backfillBootstrapRequests(f), before)
	require.NotNil(t, f.server.point(s.keys[5]))
	afterGrants, afterKeeps, afterRevokes := f.server.counts()
	require.Equal(t, []int{grants, keeps, revokes}, []int{afterGrants, afterKeeps, afterRevokes})
}

// These are refused native reply models, not alternative valid histories.
// Every mandatory point still comes from the original same-birth transaction.
func TestCleanupBirthDetachedHintRefusals(t *testing.T) {
	f := newBackfillBootstrapFixture(t, 60*time.Second)
	s := seedCleanupBirth(t, f)
	r := cleanupBirthTestDetachedRelease(t, s)
	wire, err := encodeDetachedOwnerRelease(r)
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		f.server.put(f.b.namespace.Root()+"detached-hint-observation", "later", 0)
	}
	f.server.put(s.keys[6], wire, 0)
	f.server.put(f.b.namespace.Root()+"detached-hint-observation", "after-release", 0)
	read, err := f.b.client.Txn(context.Background()).Then(clientv3.OpGet(s.keys[6])).Commit()
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		change func([]*mvccpb.KeyValue)
	}{
		{"foreign-key", func(v []*mvccpb.KeyValue) {
			v[6].Key = []byte(f.b.namespace.Root() + "foreign-release")
		}},
		{"leased", func(v []*mvccpb.KeyValue) {
			v[6].Lease = 1
		}},
		{"mutable", func(v []*mvccpb.KeyValue) {
			v[6].Version = 2
			v[6].ModRevision++
		}},
		{"noncanonical", func(v []*mvccpb.KeyValue) {
			v[6].Value = append(v[6].Value, ' ')
		}},
		{"wrong-task-birth", func(v []*mvccpb.KeyValue) {
			other := r
			other.TaskBirth--
			value, encodeErr := encodeDetachedOwnerRelease(other)
			require.NoError(t, encodeErr)
			v[6].Value = []byte(value)
		}},
		{"foreign-task", func(v []*mvccpb.KeyValue) {
			other := r
			other.Task.TaskID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
			other.Claim.Task = other.Task
			value, encodeErr := encodeDetachedOwnerRelease(other)
			require.NoError(t, encodeErr)
			v[6].Value = []byte(value)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			points := cleanupBirthTestPoints(f, s)
			tc.change(points)
			birth, err := f.b.cleanupBirthFromPoints(f.partition, f.server.point(s.keys[1]), points, read.Header.Revision)
			require.Error(t, err)
			require.Nil(t, birth)
			require.Equal(t, wire, string(f.server.point(s.keys[6]).Value))
		})
	}
}

func cleanupBirthCohortOps(ops []clientv3.Op) bool {
	return len(ops) == 9 && ops[0].IsGet()
}

func TestCleanupBirth(t *testing.T) {
	t.Run("absent-intent-cannot-become-birth-evidence", func(t *testing.T) {
		f := newBackfillBootstrapFixture(t, 60*time.Second)
		backfillBootstrapCredit(f.b, f.ranges, 262144)
		evidence, err := f.b.readCleanupBirth(context.Background(), f.partition, f.ranges, taskTestID)
		require.Error(t, err)
		require.Nil(t, evidence)
		f.b.archiveLife.mu.Lock()
		tokens, liveErr := f.ranges.bucket.tokens, f.session.live()
		f.b.archiveLife.mu.Unlock()
		require.Zero(t, tokens)
		require.NoError(t, liveErr, "authenticated absence must not poison later candidates")
	})
	t.Run("actual-routing-common-birth-and-released-key-absence", func(t *testing.T) {
		f := newBackfillBootstrapFixture(t, 60*time.Second)
		s := seedCleanupBirth(t, f)
		f.server.put(f.b.namespace.Root()+"test-observation-revision", "later", 0)
		var revision int64
		var retained []*mvccpb.KeyValue
		f.b.client.KV = &faultKV{KV: f.b.client.KV, match: cleanupBirthCohortOps, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
			if err == nil {
				revision = r.Header.Revision
				for _, op := range r.Responses[2:] {
					retained = append(retained, op.GetResponseRange().Kvs...)
				}
			}
			return r, err
		}}
		before := len(backfillBootstrapRequests(f))
		evidence := cleanupBirthTestRead(t, f, s.task.Reference.TaskID)
		require.Same(t, evidence, evidence.self)
		require.Same(t, f.b, evidence.origin)
		require.Same(t, f.partition, evidence.partition)
		require.Equal(t, s.task, evidence.record)
		require.Equal(t, s.intent.Attempt, evidence.initial)
		require.Equal(t, s.birth, evidence.birth)
		require.Equal(t, revision, evidence.revision)
		require.Greater(t, evidence.revision, evidence.birth, "native observation revision is not inferred from immutable KV birth")
		for i, fence := range []taskFence{evidence.task, evidence.intent, evidence.link, evidence.receipt} {
			require.Equal(t, ownTaskFence(f.server.point(s.keys[i])), fence)
		}
		require.Nil(t, evidence.execution)
		require.Nil(t, evidence.executionFence)
		require.Nil(t, evidence.pending)
		require.Nil(t, evidence.release)
		require.Nil(t, evidence.releaseRecord)
		requests := backfillBootstrapRequests(f)[before:]
		require.Len(t, requests, 4)
		f.b.archiveLife.mu.Lock()
		cmps, err := f.b.workerComparisons(f.session, f.partition, f.ranges)
		f.b.archiveLife.mu.Unlock()
		require.NoError(t, err)
		for i, request := range requests {
			require.Equal(t, []int{25, 10, 22, 25}[i], len(request.Compare)+len(request.Success)+len(request.Failure))
			require.LessOrEqual(t, request.Size(), 128*1024)
			if i == 0 || i == 3 {
				require.Equal(t, taskProtoComparisons(cmps), request.Compare)
			} else {
				require.Equal(t, taskProtoComparisons(f.b.baseComparisons()), request.Compare)
				require.Equal(t, request.Success, request.Failure)
				keys := append([]string{f.b.identityKey, f.b.restoreKey}, s.keys[:]...)
				if i == 1 {
					keys = []string{f.b.identityKey, f.b.restoreKey, s.keys[1]}
				}
				for j, key := range keys {
					q := request.Success[j].GetRequestRange()
					require.Equal(t, key, string(q.Key))
					require.Empty(t, q.RangeEnd)
					require.Zero(t, q.Revision)
					require.False(t, q.Serializable)
				}
			}
			for _, op := range request.Success {
				require.NotContains(t, s.missing, string(op.GetRequestRange().Key))
			}
		}
		for _, kv := range retained {
			for i := range kv.Value {
				kv.Value[i] = 'x'
			}
		}
		require.Equal(t, s.wires[0], evidence.task.value, "returned fences own native bytes")
		require.Equal(t, s.wires[1], evidence.intent.value)
		// Both discovery routes supply only the UUID. A real pending row adds
		// a digest hint, which this same loader authenticates against actual T.
		pending := cleanupPendingRecord{Version: 1, Reference: s.task.Reference, TaskDigest: workerIdentityDigest(s.wires[0])}
		wire, err := encodeCleanupPending(pending)
		require.NoError(t, err)
		f.server.put(s.keys[5], wire, 0)
		fromPending := cleanupBirthTestRead(t, f, pending.Reference.TaskID)
		require.Equal(t, evidence.task, fromPending.task)
		require.Equal(t, evidence.receipt, fromPending.receipt)
		require.Equal(t, ownTaskFence(f.server.point(s.keys[5])), *fromPending.pending)
	})
	for _, phase := range []string{"selecting", "occupied", "uncertain", "settled"} {
		t.Run("execution-"+phase, func(t *testing.T) {
			f := newBackfillBootstrapFixture(t, 60*time.Second)
			s := seedCleanupBirth(t, f)
			r := cleanupBirthTestExecution(s)
			switch phase {
			case "occupied", "uncertain":
				r.Phase, r.Confirmed = "occupied", "reserved"
				if phase == "uncertain" {
					r.Run = &cleanupExecutionRun{ID: taskTestID, Worker: launcher.WorkspaceSyncWorkerReference{Incarnation: taskClaimTestID, BootID: taskTestID, PID: 7, StartTicks: 1, UID: 1, GID: 1, PIDNamespaceDev: 1, PIDNamespaceIno: 2, MountNamespaceDev: 1, MountNamespaceIno: 3, ExecutableDev: 1, ExecutableIno: 4}}
					r.Pending = &cleanupExecutionPending{Purpose: "claim", ClaimID: taskClaimTestID}
				}
			case "settled":
				r.Phase, r.Confirmed = "settled", "settled"
				r.Release = &cleanupExecutionRelease{CreateRevision: s.birth + 2, Digest: strings.Repeat("a", 64)}
			}
			wire, err := encodeCleanupExecution(r)
			require.NoError(t, err)
			f.server.put(s.keys[4], wire, 0)
			evidence := cleanupBirthTestRead(t, f, s.task.Reference.TaskID)
			require.Equal(t, r, *evidence.execution)
			require.Equal(t, ownTaskFence(f.server.point(s.keys[4])), *evidence.executionFence)
			require.Nil(t, evidence.release, "E diagnostics cannot manufacture the absent release proof")
		})
	}
	t.Run("release-is-preliminary-without-signed-history", func(t *testing.T) {
		f := newBackfillBootstrapFixture(t, 60*time.Second)
		s := seedCleanupBirth(t, f)
		wire := cleanupBirthTestRelease(t, s)
		f.server.put(f.b.namespace.Root()+"test-release-revision", "later", 0)
		f.server.put(s.keys[6], wire, 0)
		evidence := cleanupBirthTestRead(t, f, s.task.Reference.TaskID)
		require.Equal(t, ownTaskFence(f.server.point(s.keys[6])), *evidence.release)
		require.Equal(t, s.birth, evidence.releaseRecord.TaskBirth)
		require.Nil(t, f.b.enrolledPublication)
		require.Nil(t, f.b.enrolledVerifier)
		require.Nil(t, f.b.localRuntimeRelease)
		termination, provider, _, err := f.b.namespace.localRuntimeReleaseKeys(s.task.Reference)
		require.NoError(t, err)
		require.Nil(t, f.server.point(termination))
		require.Nil(t, f.server.point(provider), "this case has no signed proof and makes no terminal acceptance claim")
	})
	t.Run("strict-birth-and-cross-record-bindings", func(t *testing.T) {
		f := newBackfillBootstrapFixture(t, 60*time.Second)
		s := seedCleanupBirth(t, f)
		for name, change := range map[string]func([]*mvccpb.KeyValue){
			"missing-task": func(v []*mvccpb.KeyValue) {
				v[0] = nil
			},
			"leased-intent": func(v []*mvccpb.KeyValue) {
				v[1].Lease = 1
			},
			"mutable-link": func(v []*mvccpb.KeyValue) {
				v[2].Version = 2
			},
			"receipt-other-birth": func(v []*mvccpb.KeyValue) {
				v[3].CreateRevision, v[3].ModRevision = s.birth-1, s.birth-1
			},
			"task-noncanonical": func(v []*mvccpb.KeyValue) {
				v[0].Value = append(v[0].Value, ' ')
			},
			"intent-routing-change": func(v []*mvccpb.KeyValue) {
				r := s.intent
				r.Task.Reference.SandboxID = "other-sandbox"
				wire, err := encodeCleanupIntentRecord(r)
				require.NoError(t, err)
				v[1].Value = []byte(wire)
			},
			"foreign-epoch": func(v []*mvccpb.KeyValue) {
				r := s.task
				r.Reference.RestoreEpoch = "foreign-epoch"
				wire, err := encodeTaskRecord(r)
				require.NoError(t, err)
				v[0].Value = []byte(wire)
			},
			"receipt-aborted": func(v []*mvccpb.KeyValue) {
				wire, err := encodeReceipt(s.stage, OutcomeAborted)
				require.NoError(t, err)
				v[3].Value = []byte(wire)
			},
			"receipt-wrong-attempt": func(v []*mvccpb.KeyValue) {
				stage := s.stage
				stage.RequestID = "other-request"
				wire, err := encodeReceipt(stage, OutcomeCommitted)
				require.NoError(t, err)
				v[3].Value = []byte(wire)
			},
			"execution-digest": func(v []*mvccpb.KeyValue) {
				r := cleanupBirthTestExecution(s)
				r.TaskDigest = strings.Repeat("e", 64)
				r.Slot = cleanupCandidate(r.TaskDigest, 0)
				r.Incarnation = cleanupIncarnation(cleanupSlotRecordFromExecution(r))
				wire, err := encodeCleanupExecution(r)
				require.NoError(t, err)
				v[4] = &mvccpb.KeyValue{Key: []byte(s.keys[4]), Value: []byte(wire), CreateRevision: s.birth + 1, ModRevision: s.birth + 1, Version: 1}
			},
			"pending-digest": func(v []*mvccpb.KeyValue) {
				wire, err := encodeCleanupPending(cleanupPendingRecord{Version: 1, Reference: s.task.Reference, TaskDigest: strings.Repeat("e", 64)})
				require.NoError(t, err)
				v[5] = &mvccpb.KeyValue{Key: []byte(s.keys[5]), Value: []byte(wire), CreateRevision: s.birth + 1, ModRevision: s.birth + 1, Version: 1}
			},
			"release-other-task": func(v []*mvccpb.KeyValue) {
				other := s
				other.task.Reference.SandboxID = "other-sandbox"
				v[6] = &mvccpb.KeyValue{Key: []byte(s.keys[6]), Value: []byte(cleanupBirthTestRelease(t, other)), CreateRevision: s.birth + 2, ModRevision: s.birth + 2, Version: 1}
			},
		} {
			t.Run(name, func(t *testing.T) {
				values := cleanupBirthTestPoints(f, s)
				change(values)
				evidence, err := f.b.cleanupBirthFromPoints(f.partition, f.server.point(s.keys[1]), values, s.birth+3)
				require.Error(t, err)
				require.Nil(t, evidence)
			})
		}
	})
	for _, attack := range []string{"intent-aba", "intent-updated", "nil-cohort-point", "foreign-header", "oversize-release"} {
		t.Run(attack, func(t *testing.T) {
			f := newBackfillBootstrapFixture(t, 60*time.Second)
			s := seedCleanupBirth(t, f)
			if attack == "intent-aba" || attack == "intent-updated" {
				f.b.client.KV = &faultKV{KV: f.b.client.KV, match: cleanupBirthCohortOps, before: func() {
					if attack == "intent-aba" {
						_, err := f.b.client.Txn(context.Background()).Then(clientv3.OpDelete(s.keys[1])).Commit()
						require.NoError(t, err)
					}
					f.server.put(s.keys[1], s.wires[1], 0)
				}}
			} else {
				f.b.client.KV = &faultKV{KV: f.b.client.KV, match: cleanupBirthCohortOps, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
					if err == nil {
						switch attack {
						case "nil-cohort-point":
							r.Responses[2].GetResponseRange().Kvs[0] = nil
						case "foreign-header":
							r.Header.ClusterId++
						case "oversize-release":
							point := r.Responses[8].GetResponseRange()
							point.Kvs = []*mvccpb.KeyValue{{Key: []byte(s.keys[6]), Value: bytes.Repeat([]byte("x"), 8193), CreateRevision: s.birth, ModRevision: s.birth, Version: 1}}
							point.Count = 1
						}
					}
					return r, err
				}}
			}
			backfillBootstrapCredit(f.b, f.ranges, 262144)
			evidence, err := f.b.readCleanupBirth(context.Background(), f.partition, f.ranges, s.task.Reference.TaskID)
			require.Error(t, err)
			require.Nil(t, evidence)
		})
	}
}

func TestCleanupBirthOwnership(t *testing.T) {
	t.Run("local-refusals-preserve-credit", func(t *testing.T) {
		f := newBackfillBootstrapFixture(t, 60*time.Second)
		s := seedCleanupBirth(t, f)
		other := newBackfillBootstrapFixture(t, 60*time.Second)
		copied := *f.partition
		backfillBootstrapCredit(f.b, f.ranges, 262144)
		before := len(backfillBootstrapRequests(f))
		for _, attempt := range []func() (*cleanupBirthEvidence, error){
			func() (*cleanupBirthEvidence, error) {
				return f.b.readCleanupBirth(nil, f.partition, f.ranges, s.task.Reference.TaskID)
			},
			func() (*cleanupBirthEvidence, error) {
				return (*Backend)(nil).readCleanupBirth(context.Background(), f.partition, f.ranges, s.task.Reference.TaskID)
			},
			func() (*cleanupBirthEvidence, error) {
				return f.b.readCleanupBirth(context.Background(), &copied, f.ranges, s.task.Reference.TaskID)
			},
			func() (*cleanupBirthEvidence, error) {
				return f.b.readCleanupBirth(context.Background(), f.partition, other.ranges, s.task.Reference.TaskID)
			},
			func() (*cleanupBirthEvidence, error) {
				return f.b.readCleanupBirth(context.Background(), f.partition, f.claims, s.task.Reference.TaskID)
			},
			func() (*cleanupBirthEvidence, error) {
				return f.b.readCleanupBirth(context.Background(), f.partition, f.ranges, "../foreign")
			},
		} {
			evidence, err := attempt()
			require.Error(t, err)
			require.Nil(t, evidence)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := f.b.readCleanupBirth(ctx, f.partition, f.ranges, s.task.Reference.TaskID)
		require.ErrorIs(t, err, context.Canceled)
		require.Len(t, backfillBootstrapRequests(f), before)
		f.b.archiveLife.mu.Lock()
		tokens := f.ranges.bucket.tokens
		f.b.archiveLife.mu.Unlock()
		require.Equal(t, uint64(262144), tokens)
		backfillBootstrapCredit(f.b, f.ranges, 0)
		_, err = f.b.readCleanupBirth(context.Background(), f.partition, f.ranges, s.task.Reference.TaskID)
		require.ErrorIs(t, err, ErrConflict)
		require.Len(t, backfillBootstrapRequests(f), before)
		cleanupBirthTestRead(t, f, s.task.Reference.TaskID)
	})
	t.Run("genuine-short-ttl-refuses", func(t *testing.T) {
		f := newBackfillBootstrapFixture(t, 15*time.Second)
		s := seedCleanupBirth(t, f)
		backfillBootstrapCredit(f.b, f.ranges, 262144)
		before := len(backfillBootstrapRequests(f))
		evidence, err := f.b.readCleanupBirth(context.Background(), f.partition, f.ranges, s.task.Reference.TaskID)
		require.Error(t, err)
		require.Nil(t, evidence)
		require.Len(t, backfillBootstrapRequests(f), before)
	})
	for _, failure := range []string{"unknown-cohort", "partition-changed-after-cohort", "typed-invalid-with-live-guards"} {
		t.Run(failure, func(t *testing.T) {
			f := newBackfillBootstrapFixture(t, 60*time.Second)
			s := seedCleanupBirth(t, f)
			f.b.client.KV = &faultKV{KV: f.b.client.KV, match: cleanupBirthCohortOps, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				if err == nil {
					switch failure {
					case "unknown-cohort":
						return nil, context.DeadlineExceeded
					case "partition-changed-after-cohort":
						f.server.put(f.partition.fence.key, f.partition.fence.value, f.partition.fence.lease)
					case "typed-invalid-with-live-guards":
						r.Responses[2].GetResponseRange().Kvs[0].Value = append(r.Responses[2].GetResponseRange().Kvs[0].Value, ' ')
					}
				}
				return r, err
			}}
			before := len(backfillBootstrapRequests(f))
			backfillBootstrapCredit(f.b, f.ranges, 262144)
			evidence, err := f.b.readCleanupBirth(context.Background(), f.partition, f.ranges, s.task.Reference.TaskID)
			require.Error(t, err)
			require.Nil(t, evidence)
			f.b.archiveLife.mu.Lock()
			tokens, liveErr := f.ranges.bucket.tokens, f.session.live()
			f.b.archiveLife.mu.Unlock()
			require.Zero(t, tokens)
			if failure == "typed-invalid-with-live-guards" {
				require.NoError(t, liveErr, "postguard-authenticated bad history must not poison fair later candidates")
				require.Len(t, backfillBootstrapRequests(f)[before:], 4)
			} else {
				require.Error(t, liveErr)
			}
		})
	}
	for _, action := range []string{"Close", "caller-deadline"} {
		t.Run("actual-return-join-"+action, func(t *testing.T) {
			f := newBackfillBootstrapFixture(t, 60*time.Second)
			s := seedCleanupBirth(t, f)
			relist, err := f.b.AcquireWorkerBudget(context.Background(), f.session, WorkerBudgetRelist, 0)
			require.NoError(t, err)
			entered, release := make(chan struct{}, 1), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			f.b.client.KV = &faultKV{KV: f.b.client.KV, match: cleanupBirthCohortOps, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				entered <- struct{}{}
				<-release
				return r, err
			}}
			ctx := context.Background()
			if action == "caller-deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 250*time.Millisecond)
				t.Cleanup(cancel)
			}
			backfillBootstrapCredit(f.b, f.ranges, 262144)
			done := make(chan error, 1)
			started := time.Now()
			go func() {
				done <- f.b.RunWorkerRelist(ctx, f.partition, relist, func(owned context.Context) error {
					_, err := f.b.readCleanupBirth(owned, f.partition, f.ranges, s.task.Reference.TaskID)
					return err
				})
			}()
			workerTestReceive(t, entered)
			f.b.archiveLife.mu.Lock()
			phaseContext := f.ranges.dueOwner.call.ctx
			deadline, bounded := phaseContext.Deadline()
			f.b.archiveLife.mu.Unlock()
			require.True(t, bounded)
			require.LessOrEqual(t, deadline.Sub(started), 4*f.b.requestTimeout+time.Second+100*time.Millisecond)
			closed := make(chan error, 1)
			if action == "Close" {
				go func() { closed <- f.b.Close() }()
			}
			workerTestReceive(t, phaseContext.Done())
			f.b.archiveLife.mu.Lock()
			pageHeld, relistHeld := f.ranges.dueOwner != nil, relist.owner != nil
			f.b.archiveLife.mu.Unlock()
			require.True(t, pageHeld)
			require.True(t, relistHeld)
			select {
			case err := <-done:
				t.Fatalf("metadata phase returned before its client call: %v", err)
			default:
			}
			if action == "Close" {
				select {
				case err := <-closed:
					t.Fatalf("Close returned before metadata joined: %v", err)
				case <-time.After(20 * time.Millisecond):
				}
			}
			unblock()
			require.Error(t, workerTestReceive(t, done))
			if action == "Close" {
				require.NoError(t, workerTestReceive(t, closed))
			}
		})
	}
}

func TestCleanupBirthWire(t *testing.T) {
	t.Run("phase-and-renewal-margin-arithmetic", func(t *testing.T) {
		for _, r := range []time.Duration{time.Nanosecond, time.Second, 3 * time.Second} {
			phase, margin, err := cleanupBirthPhaseTiming(r, false)
			require.NoError(t, err)
			require.Equal(t, 4*r+time.Second, phase)
			require.Equal(t, 7*r+2*time.Second, margin)
			phase, margin, err = cleanupBirthPhaseTiming(r, true)
			require.NoError(t, err)
			require.Equal(t, 5*r+time.Second, phase)
			require.Equal(t, 8*r+2*time.Second, margin)
		}
		for _, r := range []time.Duration{0, -time.Second, 3*time.Second + time.Nanosecond, time.Duration(math.MaxInt64)} {
			for _, terminal := range []bool{false, true} {
				_, _, err := cleanupBirthPhaseTiming(r, terminal)
				require.Error(t, err)
			}
		}
	})
	t.Run("maximal-native-profiles-and-aggregate-stopping-branches", func(t *testing.T) {
		f := newBackfillBootstrapFixture(t, 60*time.Second)
		s := seedCleanupBirth(t, f)
		f.b.archiveLife.mu.Lock()
		cmps, err := f.b.workerComparisons(f.session, f.partition, f.ranges)
		f.b.archiveLife.mu.Unlock()
		require.NoError(t, err)
		for _, terminal := range []bool{false, true} {
			profiles, err := f.b.cleanupBirthWireProjections(cmps, s.task.Reference, s.intent.Attempt, f.session, terminal)
			require.NoError(t, err)
			counts := []int{25, 10, 22, 25}
			caps := [][]int{{4096}, {4096, 128, 4096}, {4096, 128, 4096, 4096, 2048, 2048, 4096, 2048, 8192}, {4096}}
			bound := 87552
			if terminal {
				counts = []int{25, 10, 12, 26, 25}
				caps = [][]int{{4096}, {4096, 128, 8192}, {4096, 128, 16384, 16384}, {4096, 128, 4096, 8192, 32768, 4096, 8192, 8192, 8192, 65536, 8192}, {4096}}
				bound = 260480
			}
			require.Len(t, profiles, len(counts))
			prefix := 0
			for i, profile := range profiles {
				require.NoError(t, validateCleanupWire(profile))
				require.Equal(t, counts[i], len(profile.request.Compare)+len(profile.request.Success)+len(profile.request.Failure))
				require.LessOrEqual(t, profile.request.Size(), 128*1024)
				for _, cmp := range profile.request.Compare {
					switch cmp.Target {
					case pb.Compare_VALUE:
						valueBytes := 2048
						if string(cmp.Key) == f.b.identityKey {
							valueBytes = 4096
						} else if string(cmp.Key) == f.b.restoreKey {
							valueBytes = 128
						}
						require.Len(t, cmp.GetValue(), valueBytes)
					case pb.Compare_CREATE:
						require.Equal(t, int64(math.MaxInt64), cmp.GetCreateRevision())
					case pb.Compare_MOD:
						require.Equal(t, int64(math.MaxInt64), cmp.GetModRevision())
					case pb.Compare_LEASE:
						if cmp.GetLease() > 0 {
							require.Equal(t, int64(math.MaxInt64), cmp.GetLease())
						}
					}
				}
				for branch, response := range []*pb.TxnResponse{profile.success, profile.failure} {
					wantCaps := caps[i]
					if branch == 1 && (i == 0 || i == len(profiles)-1) {
						wantCaps = []int{4096, 128, 2048, 2048}
					}
					require.Equal(t, uint64(math.MaxUint64), response.Header.ClusterId)
					require.Equal(t, int64(math.MaxInt64), response.Header.Revision)
					require.Len(t, response.Responses, len(wantCaps))
					for j, op := range response.Responses {
						point := op.GetResponseRange()
						require.NotNil(t, point)
						require.Len(t, point.Kvs, 1)
						kv := point.Kvs[0]
						require.Len(t, kv.Key, 1024, "maximal key placeholders are accounting only")
						require.Len(t, kv.Value, wantCaps[j])
						require.Equal(t, int64(math.MaxInt64), kv.CreateRevision)
						require.Equal(t, int64(math.MaxInt64), kv.ModRevision)
						require.Equal(t, int64(math.MaxInt64), kv.Version)
						require.Equal(t, int64(math.MaxInt64), kv.Lease)
					}
					require.LessOrEqual(t, response.Size(), 262144)
				}
				// Every stopping branch includes the successful prefix and the
				// current failure, rather than summing successful replies alone.
				require.LessOrEqual(t, prefix+profile.failure.Size(), bound)
				prefix += profile.success.Size()
			}
			require.LessOrEqual(t, prefix, bound)
			require.LessOrEqual(t, bound, 262144)
		}
		oversize := append([]clientv3.Cmp(nil), cmps...)
		oversize[0] = clientv3.Compare(clientv3.Value(f.b.identityKey), "=", strings.Repeat("x", 128*1024))
		_, err = f.b.cleanupBirthWireProjections(oversize, s.task.Reference, s.intent.Attempt, f.session, false)
		require.Error(t, err, "an invalid actual request must not be hidden by replacing it with smaller padding")
	})
}
