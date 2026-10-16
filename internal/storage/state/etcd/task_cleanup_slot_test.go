package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This fixture creates a real published runtime record and original destroy
// Stage on the current three-member endpoint fixture. It performs no runtime
// effects and never enters the old Docker/global-fault fixture helper.
type cleanupSlotTestFixture struct {
	b        *Backend
	raw      *clientv3.Client
	ctx      context.Context
	ref      TaskReference
	session  *WorkerSession
	part     *WorkerPartitionClaim
	share    *WorkerBudgetShare
	evidence *cleanupSlotEvidence
}

func newCleanupSlotTestFixture(t *testing.T) *cleanupSlotTestFixture {
	t.Helper()
	require.NotEmpty(t, os.Getenv("TEST_ETCD_ENDPOINTS"), "this selector requires the current real three-member fixture; absence is not a skipped PASS")
	base, raw, in, keys := operationControlFixture(t, "plain")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	control, err := raw.Get(ctx, keys[1])
	require.NoError(t, err)
	require.Len(t, control.Kvs, 1)
	stage, ref := taskDestroyPrepare(t, base, ctx, BeginDestroyInput{SandboxID: in.SandboxID, RequestID: "cleanup-slot-destroy", ExpectedControlRevision: control.Kvs[0].ModRevision})
	outcome, err := base.CommitStage(ctx, stage)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)

	options := integrationOptions(base, raw)
	options.RequestTimeout = time.Second
	options.WorkerControl = &WorkerControlOptions{SessionTTL: 60 * time.Second}
	profileKey, err := base.namespace.Key("meta", "worker_control_budget")
	require.NoError(t, err)
	_, err = raw.Put(ctx, profileKey, workerTestProfile(options))
	require.NoError(t, err)
	b, err := New(ctx, options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	session, err := b.OpenWorkerSession(ctx, "cleanup-slot-test")
	require.NoError(t, err)
	part, err := b.ClaimWorkerPartition(ctx, session, ref.Partition)
	require.NoError(t, err)
	share, err := b.AcquireWorkerBudget(ctx, session, WorkerBudgetClaim, 0)
	require.NoError(t, err)
	f := &cleanupSlotTestFixture{b: b, raw: raw, ctx: ctx, ref: ref, session: session, part: part, share: share}

	var bundle *taskClaimBundle
	f.turn(t, func(owner *workerDueOwner) {
		bundle, err = b.loadCleanupClaimBundle(owner, ref)
		require.NoError(t, err)
	})
	f.turn(t, func(owner *workerDueOwner) {
		f.evidence, err = b.loadCleanupSlotEvidence(owner, bundle)
		require.NoError(t, err)
	})
	return f
}

// Only the original scheduling bucket's elapsed clock is advanced. Session,
// partition, share membership and every native fence are real and unchanged.
// Each call still consumes one nonrefundable original Claim credit.
func (f *cleanupSlotTestFixture) turn(t *testing.T, work func(*workerDueOwner)) {
	t.Helper()
	f.b.archiveLife.mu.Lock()
	f.share.bucket.last = time.Now().Add(-2 * time.Second)
	f.b.archiveLife.mu.Unlock()
	owner, stop, err := f.b.beginExpiredTaskOwner(f.ctx, f.part, f.share)
	require.NoError(t, err)
	defer stop()
	defer owner.finish()
	work(owner)
}

func (f *cleanupSlotTestFixture) observe(t *testing.T) *cleanupSlotState {
	t.Helper()
	var state *cleanupSlotState
	f.turn(t, func(owner *workerDueOwner) {
		var err error
		state, err = f.b.readCleanupSlot(owner, f.ref)
		require.NoError(t, err)
	})
	return state
}

func (f *cleanupSlotTestFixture) initialize(t *testing.T) *cleanupSlotState {
	t.Helper()
	f.turn(t, func(owner *workerDueOwner) {
		require.NoError(t, f.b.initializeCleanupSlot(owner, f.evidence))
	})
	state := f.observe(t)
	require.Equal(t, "selecting", state.record.Phase)
	return state
}

func (f *cleanupSlotTestFixture) occupy(t *testing.T) *cleanupSlotState {
	t.Helper()
	state := f.initialize(t)
	f.turn(t, func(owner *workerDueOwner) {
		require.NoError(t, f.b.reserveCleanupSlot(owner, f.evidence, state))
	})
	state = f.observe(t)
	require.Equal(t, "occupied", state.record.Phase)
	require.NotNil(t, state.slot)
	return state
}

func cleanupSlotSchemaRecord() cleanupExecutionRecord {
	task, intent, _, _ := taskTestRecords()
	record := cleanupExecutionRecord{
		Version: 1, Domain: "sandbox.cleanup-execution.v1", Task: task.Reference,
		TaskBirth: 11, InitialAttempt: intent.Attempt,
		TaskDigest: strings.Repeat("0", 64), InitialMutationDigest: strings.Repeat("1", 64), InitialReceiptDigest: strings.Repeat("2", 64),
		Selection: 0, Slot: 0,
		Phase: "selecting", Confirmed: "queued",
	}
	record.Incarnation = cleanupSlotTestIncarnation(record)
	return record
}

// Compute the frozen framing independently of the production record builder.
// This helper creates metadata fixtures only; it never mints runtime authority.
func cleanupSlotTestIncarnation(record cleanupExecutionRecord) string {
	task, _ := json.Marshal(record.Task)
	attempt, _ := json.Marshal(record.InitialAttempt)
	digest := framedDigest("sandbox.cleanup-execution-incarnation.v1", string(task), strconv.FormatInt(record.TaskBirth, 10), string(attempt), record.TaskDigest, record.InitialMutationDigest, record.InitialReceiptDigest)
	return uuid.NewSHA1(uuid.NameSpaceOID, digest[:]).String()
}

func TestCleanupSlotsSchema(t *testing.T) {
	record := cleanupSlotSchemaRecord()
	namespace, err := NewNamespace("/codex-test", "tasks", "cell")
	require.NoError(t, err)
	for slot := uint32(0); slot < 32; slot++ {
		key, err := namespace.cleanupExecutionSlotKey(slot)
		require.NoError(t, err)
		require.Equal(t, namespace.Root()+fmt.Sprintf("cleanup-execution-slots/%02d", slot), key)
	}
	_, err = namespace.cleanupExecutionSlotKey(32)
	require.Error(t, err)
	wire, err := encodeCleanupExecution(record)
	require.NoError(t, err)
	key := record.Task.Namespace + fmt.Sprintf("p/%02x/tasks/%s/execution", record.Task.Partition, record.Task.TaskID)
	kv := &mvccpb.KeyValue{Key: []byte(key), Value: []byte(wire), CreateRevision: 12, ModRevision: 12, Version: 1}
	var decoded cleanupExecutionRecord
	require.NoError(t, decodeCleanupExecution(kv, 12, record.Task, &decoded))
	require.Equal(t, record, decoded)

	for name, change := range map[string]func(*cleanupExecutionRecord){
		"slot-32":         func(r *cleanupExecutionRecord) { r.Slot = 32 },
		"phase":           func(r *cleanupExecutionRecord) { r.Phase = "released" },
		"progress":        func(r *cleanupExecutionRecord) { r.Confirmed = "404" },
		"incarnation-v4":  func(r *cleanupExecutionRecord) { r.Incarnation = uuid.NewString() },
		"wrong-candidate": func(r *cleanupExecutionRecord) { r.Slot = 1 },
		"birth-zero":      func(r *cleanupExecutionRecord) { r.TaskBirth = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := record
			change(&bad)
			_, err := encodeCleanupExecution(bad)
			require.Error(t, err)
		})
	}
	for _, wire := range []string{
		`{"version":1,` + wire[1:],
		`{"unknown":1,` + wire[1:],
		strings.Replace(wire, `"selection":0`, `"selection":null`, 1),
		strings.Replace(wire, `"selection":0,`, "", 1),
		strings.TrimSuffix(wire, "}") + `,"run":null}`,
		" " + wire,
		strings.Repeat("x", 4097),
	} {
		bad := *kv
		bad.Value = []byte(wire)
		unchanged := record
		require.Error(t, decodeCleanupExecution(&bad, 12, record.Task, &unchanged))
		require.Equal(t, record, unchanged, "decode must not partially replace destination")
	}
	for _, change := range []func(*mvccpb.KeyValue){
		func(v *mvccpb.KeyValue) { v.Lease = 1 },
		func(v *mvccpb.KeyValue) { v.Version = 0 },
		func(v *mvccpb.KeyValue) { v.CreateRevision = 11 },
		func(v *mvccpb.KeyValue) { v.ModRevision = 13 },
		func(v *mvccpb.KeyValue) { v.Key = []byte(key + "-wrong") },
	} {
		bad := *kv
		change(&bad)
		require.Error(t, decodeCleanupExecution(&bad, 12, record.Task, &decoded))
	}

	slot := cleanupSlotRecordFromExecution(record)
	slotWire, err := encodeCleanupExecutionSlot(slot)
	require.NoError(t, err)
	slotKey := record.Task.Namespace + "cleanup-execution-slots/00"
	slotKV := &mvccpb.KeyValue{Key: []byte(slotKey), Value: []byte(slotWire), Version: 1, CreateRevision: 12, ModRevision: 12}
	var decodedSlot cleanupExecutionSlotRecord
	require.NoError(t, decodeCleanupExecutionSlot(slotKV, 12, record.Task.Namespace, 0, &decodedSlot))
	require.Equal(t, slot, decodedSlot)
	for _, change := range []func(*mvccpb.KeyValue){
		func(v *mvccpb.KeyValue) { v.Lease = 1 },
		func(v *mvccpb.KeyValue) { v.Version = 2 },
		func(v *mvccpb.KeyValue) { v.ModRevision++ },
		func(v *mvccpb.KeyValue) { v.Key = []byte(record.Task.Namespace + "cleanup-execution-slots/0") },
		func(v *mvccpb.KeyValue) { v.Value = []byte(strings.TrimSuffix(slotWire, "}") + `,"run":null}`) },
	} {
		bad := *slotKV
		change(&bad)
		require.Error(t, decodeCleanupExecutionSlot(&bad, 13, record.Task.Namespace, 0, &decodedSlot))
	}
}

func TestCleanupSlotsBirthReceipt(t *testing.T) {
	for _, corruption := range []string{"missing", "rewritten-identical", "aborted"} {
		t.Run(corruption, func(t *testing.T) {
			f := newCleanupSlotTestFixture(t)
			receipt := f.evidence.receipt
			switch corruption {
			case "missing":
				_, err := f.raw.Delete(f.ctx, receipt.key)
				require.NoError(t, err)
			case "rewritten-identical":
				_, err := f.raw.Put(f.ctx, receipt.key, receipt.value)
				require.NoError(t, err)
			case "aborted":
				var value map[string]any
				require.NoError(t, json.Unmarshal([]byte(receipt.value), &value))
				value["outcome"] = "aborted"
				wire, err := json.Marshal(value)
				require.NoError(t, err)
				_, err = f.raw.Put(f.ctx, receipt.key, string(wire))
				require.NoError(t, err)
			}
			f.turn(t, func(owner *workerDueOwner) {
				_, err := f.b.loadCleanupSlotEvidence(owner, f.evidence.bundle)
				require.Error(t, err)
				require.Error(t, f.b.initializeCleanupSlot(owner, f.evidence), "stale copied evidence must be CAS-pinned")
			})
			key, err := f.b.namespace.cleanupExecutionKey(f.ref)
			require.NoError(t, err)
			got, err := f.raw.Get(f.ctx, key)
			require.NoError(t, err)
			require.Empty(t, got.Kvs)
		})
	}
}

func TestCleanupSlotsPersistent32(t *testing.T) {
	f := newCleanupSlotTestFixture(t)
	state := f.initialize(t)
	// These are explicitly metadata occupation fixtures, not 32 physical jobs.
	// Every numeric permanent key is populated; the real admission must refuse
	// all candidates without deleting or leasing any of those occupations.
	for slot := uint32(0); slot < 32; slot++ {
		r := cleanupSlotRecordFromExecution(state.record)
		r.Task.TaskID = uuid.NewString()
		r.InitialAttempt.RequestID = "other-task"
		attribution := state.record
		attribution.Task = r.Task
		attribution.InitialAttempt = r.InitialAttempt
		r.Incarnation = cleanupSlotTestIncarnation(attribution)
		r.Slot = slot
		r.Selection = uint64((slot + 32 - state.record.Slot) % 32)
		wire, err := encodeCleanupExecutionSlot(r)
		require.NoError(t, err)
		key, err := f.b.namespace.cleanupExecutionSlotKey(slot)
		require.NoError(t, err)
		_, err = f.raw.Put(f.ctx, key, wire)
		require.NoError(t, err)
	}
	lease := &creationFaultLease{Lease: f.b.client.Lease}
	f.b.client.Lease = lease
	for candidate := 0; candidate < 32; candidate++ {
		f.turn(t, func(owner *workerDueOwner) {
			require.Error(t, f.b.reserveCleanupSlot(owner, f.evidence, state))
		})
		f.turn(t, func(owner *workerDueOwner) {
			require.NoError(t, f.b.rotateCleanupSlot(owner, state))
		})
		state = f.observe(t)
	}
	require.Equal(t, uint64(32), state.record.Selection)
	require.Equal(t, "selecting", state.record.Phase)
	require.Zero(t, lease.grants.Load())
	require.NoError(t, f.b.ReleaseWorkerSession(f.ctx, f.session))
	got, err := f.raw.Get(f.ctx, f.b.namespace.Root()+"cleanup-execution-slots/", clientv3.WithPrefix())
	require.NoError(t, err)
	require.Len(t, got.Kvs, 32)
	for _, kv := range got.Kvs {
		require.Zero(t, kv.Lease)
		require.Equal(t, kv.CreateRevision, kv.ModRevision)
	}
}

func TestCleanupSlotsSameTask(t *testing.T) {
	f := newCleanupSlotTestFixture(t)
	before := f.initialize(t)
	f.turn(t, func(owner *workerDueOwner) {
		require.NoError(t, f.b.reserveCleanupSlot(owner, f.evidence, before))
	})
	first := f.observe(t)
	f.turn(t, func(owner *workerDueOwner) {
		// An old selecting CAS cannot reserve twice or rotate an occupied Task.
		require.Error(t, f.b.reserveCleanupSlot(owner, f.evidence, before))
		require.Error(t, f.b.rotateCleanupSlot(owner, before))
	})
	second := f.observe(t)
	require.Equal(t, first, second)
	got, err := f.raw.Get(f.ctx, f.b.namespace.Root()+"cleanup-execution-slots/", clientv3.WithPrefix())
	require.NoError(t, err)
	require.Len(t, got.Kvs, 1)

	// Seed terminal metadata only, without invoking or asserting physical
	// release. This tests the permanent no-restart rule after settlement.
	terminal := second.record
	terminal.Phase, terminal.Confirmed = "settled", "settled"
	terminal.Release = &cleanupExecutionRelease{CreateRevision: second.execution.mod, Digest: strings.Repeat("a", 64)}
	wire, err := encodeCleanupExecution(terminal)
	require.NoError(t, err)
	_, err = f.raw.Txn(f.ctx).Then(clientv3.OpDelete(second.slot.key), clientv3.OpPut(second.execution.key, wire)).Commit()
	require.NoError(t, err)
	f.turn(t, func(owner *workerDueOwner) {
		require.Error(t, f.b.initializeCleanupSlot(owner, f.evidence))
		require.Error(t, f.b.reserveCleanupSlot(owner, f.evidence, before))
	})
	settled := f.observe(t)
	require.Equal(t, terminal, settled.record)
	require.Nil(t, settled.slot)
	got, err = f.raw.Get(f.ctx, f.b.namespace.Root()+"cleanup-execution-slots/", clientv3.WithPrefix())
	require.NoError(t, err)
	require.Empty(t, got.Kvs)
}

func TestCleanupSlotsUnknownReserve(t *testing.T) {
	f := newCleanupSlotTestFixture(t)
	state := f.initialize(t)
	key, err := f.b.namespace.cleanupExecutionSlotKey(state.record.Slot)
	require.NoError(t, err)
	native := f.b.client.KV
	f.b.client.KV = &faultKV{KV: native, match: putsKey(key), after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		require.NoError(t, err)
		require.True(t, r.Succeeded)
		return nil, errors.New("test: committed reserve acknowledgement lost")
	}}
	f.turn(t, func(owner *workerDueOwner) {
		require.ErrorIs(t, f.b.reserveCleanupSlot(owner, f.evidence, state), ErrOutcomeUnknown)
	})
	f.b.client.KV = native
	confirmed := f.observe(t)
	require.Equal(t, "occupied", confirmed.record.Phase)
	require.Equal(t, state.record.Incarnation, confirmed.record.Incarnation)
	require.Equal(t, cleanupSlotTestIncarnation(confirmed.record), confirmed.record.Incarnation)
	require.Equal(t, state.record.Selection, confirmed.record.Selection)
	require.Equal(t, confirmed.execution.mod, confirmed.slot.mod)
	f.turn(t, func(owner *workerDueOwner) {
		require.Error(t, f.b.rotateCleanupSlot(owner, state))
	})
	require.Equal(t, confirmed, f.observe(t))
}

func TestCleanupSlotsUnknownRotation(t *testing.T) {
	f := newCleanupSlotTestFixture(t)
	old := f.initialize(t)
	native := f.b.client.KV
	f.b.client.KV = &faultKV{KV: native, match: putsKey(old.execution.key), after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		require.NoError(t, err)
		require.True(t, r.Succeeded)
		return nil, errors.New("test: committed rotation acknowledgement lost")
	}}
	f.turn(t, func(owner *workerDueOwner) {
		require.ErrorIs(t, f.b.rotateCleanupSlot(owner, old), ErrOutcomeUnknown)
	})
	f.b.client.KV = native
	rotated := f.observe(t)
	require.Equal(t, old.record.Selection+1, rotated.record.Selection)
	require.Equal(t, old.record.Incarnation, rotated.record.Incarnation)
	f.turn(t, func(owner *workerDueOwner) {
		// The exact old revision arbitrates a reserve that arrives after rotation.
		require.Error(t, f.b.reserveCleanupSlot(owner, f.evidence, old))
		require.Error(t, f.b.rotateCleanupSlot(owner, old))
	})
	require.Equal(t, rotated, f.observe(t), "unknown rotation cannot silently skip another candidate")
}

func TestCleanupSlotsABASettlement(t *testing.T) {
	f := newCleanupSlotTestFixture(t)
	occupied := f.occupy(t)
	// This is a native CAS primitive fixture only. It does not manufacture an
	// owner-release proof or claim successful physical settlement/join.
	_, err := f.raw.Delete(f.ctx, occupied.slot.key)
	require.NoError(t, err)
	_, err = f.raw.Put(f.ctx, occupied.slot.key, occupied.slot.value)
	require.NoError(t, err)
	stale, err := f.raw.Txn(f.ctx).If(
		clientv3.Compare(clientv3.ModRevision(occupied.execution.key), "=", occupied.execution.mod),
		clientv3.Compare(clientv3.ModRevision(occupied.slot.key), "=", occupied.slot.mod),
	).Then(clientv3.OpDelete(occupied.slot.key)).Commit()
	require.NoError(t, err)
	require.False(t, stale.Succeeded, "identical bytes after reuse must not satisfy the old occupation pins")
	f.turn(t, func(owner *workerDueOwner) {
		_, err := f.b.readCleanupSlot(owner, f.ref)
		require.Error(t, err, "recreated S has a different reservation birth from E attribution")
	})
	got, err := f.raw.Get(f.ctx, occupied.slot.key)
	require.NoError(t, err)
	require.Len(t, got.Kvs, 1)
}

func TestCleanupSlotsLeaseLoss(t *testing.T) {
	f := newCleanupSlotTestFixture(t)
	occupied := f.occupy(t)
	_, err := f.raw.Revoke(f.ctx, clientv3.LeaseID(f.session.Reference().LeaseID))
	require.NoError(t, err)
	for _, fence := range []taskFence{occupied.execution, *occupied.slot} {
		got, err := f.raw.Get(f.ctx, fence.key)
		require.NoError(t, err)
		require.Len(t, got.Kvs, 1)
		require.Equal(t, fence.value, string(got.Kvs[0].Value))
		require.Equal(t, fence.mod, got.Kvs[0].ModRevision)
		require.Zero(t, got.Kvs[0].Lease)
	}
	f.turn(t, func(owner *workerDueOwner) {
		require.Error(t, f.b.rotateCleanupSlot(owner, occupied))
	})
}

func TestCleanupSlotsWire(t *testing.T) {
	f := newCleanupSlotTestFixture(t)
	state := f.initialize(t)
	f.turn(t, func(owner *workerDueOwner) {
		inventory, err := f.b.cleanupSlotWireInventory(owner, f.evidence, state)
		require.NoError(t, err)
		want := map[string]int{"point": 7, "bundle": 16, "observe": 8, "claim-observe": 9, "terminal-history": 10, "initialize": 36, "rotate": 27, "reserve": 41, "progress": 31, "recovery-run": 43, "claim": 64, "claim-postcheck": 46, "joined": 15, "settle": 48}
		for _, projection := range inventory {
			nodes, ok := want[projection.name]
			require.True(t, ok, "unexpected or duplicate projection %s", projection.name)
			delete(want, projection.name)
			require.Equal(t, nodes, len(projection.request.Compare)+len(projection.request.Success)+len(projection.request.Failure))
			require.LessOrEqual(t, projection.request.Size(), 128<<10)
			require.LessOrEqual(t, projection.success.Size(), 256<<10)
			require.LessOrEqual(t, projection.failure.Size(), 256<<10)
			require.NoError(t, validateCleanupWire(projection))
			t.Logf("%s nodes=%d request=%d success=%d failure=%d", projection.name, nodes, projection.request.Size(), projection.success.Size(), projection.failure.Size())
		}
		require.Empty(t, want, "missing new-protocol inventory entries")
	})

	for _, projection := range []cleanupWireProjection{
		{name: "65-nodes", request: &pb.TxnRequest{Compare: make([]*pb.Compare, 65)}, success: &pb.TxnResponse{}, failure: &pb.TxnResponse{}},
		{name: "request-overflow", request: &pb.TxnRequest{Success: []*pb.RequestOp{{Request: &pb.RequestOp_RequestPut{RequestPut: &pb.PutRequest{Key: []byte("k"), Value: make([]byte, (128<<10)+1)}}}}}, success: &pb.TxnResponse{}, failure: &pb.TxnResponse{}},
		{name: "success-overflow", request: &pb.TxnRequest{}, success: &pb.TxnResponse{Responses: []*pb.ResponseOp{{Response: &pb.ResponseOp_ResponseRange{ResponseRange: &pb.RangeResponse{Kvs: []*mvccpb.KeyValue{{Value: make([]byte, (256<<10)+1), ModRevision: math.MaxInt64}}}}}}}, failure: &pb.TxnResponse{}},
		{name: "failure-overflow", request: &pb.TxnRequest{}, success: &pb.TxnResponse{}, failure: &pb.TxnResponse{Responses: []*pb.ResponseOp{{Response: &pb.ResponseOp_ResponseRange{ResponseRange: &pb.RangeResponse{Kvs: []*mvccpb.KeyValue{{Value: make([]byte, (256<<10)+1)}}}}}}}},
	} {
		require.Error(t, validateCleanupWire(projection), projection.name)
	}
}
