package etcd

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func workspaceSyncIntentNativeBody(t *testing.T, slots uint32) (*quiescenceNativeFixture, *PreparedTaskWorkspaceSyncSource, *taskWorkspaceSyncBody) {
	t.Helper()
	plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspaceSyncIdentity(t, "s3", "test-storage", "bucket", "team/project/")})
	require.NoError(t, err)
	return workspaceSyncIntentBodyWithSetup(t, plan, &TaskWorkspaceSyncBudgetOptions{uuid.NewString(), slots, 4096}, nil)
}

func workspaceSyncIntentBodyWithSetup(t *testing.T, plan TaskWorkspaceSyncPlan, budget *TaskWorkspaceSyncBudgetOptions, setup taskDeliverySetupFactory, ttl ...time.Duration) (*quiescenceNativeFixture, *PreparedTaskWorkspaceSyncSource, *taskWorkspaceSyncBody) {
	t.Helper()
	lifetime := 30 * time.Second
	if len(ttl) > 0 {
		lifetime = ttl[0]
	}
	f, prepared, wire := deliveryNativeFixtureWithBudgetSetup(t, 0, func(TaskRecord) TaskFinalSyncPlan {
		p, e := plan.FinalSyncPlan()
		require.NoError(t, e)
		return p
	}, budget, setup, lifetime)
	done := deliveryBegin(f, prepared)
	deliveryWaitSent(t, wire, done)
	close(wire.body)
	r := deliveryWait(t, done)
	require.NoError(t, r.err)
	require.NoError(t, <-wire.done)
	source, err := f.b.PrepareTaskWorkspaceSyncSource(f.ctx, r.archive, plan)
	require.NoError(t, err)
	_, err = source.next(f.ctx)
	require.NoError(t, err)
	entry, err := source.next(f.ctx)
	require.NoError(t, err)
	body, err := source.openBody(f.ctx, entry)
	require.NoError(t, err)
	return f, source, body
}

func TestTaskWorkspaceSyncIntentNativeAtomicRegistration(t *testing.T) {
	f, source, body := workspaceSyncIntentNativeBody(t, 1)
	t.Logf("preserved archive comparisons=%d", len(source.archive.owner.comparisons))
	result, err := source.reservePutIntent(f.ctx, body)
	require.NoError(t, err, "original unread body must register native intent and slot")
	require.Equal(t, OutcomeCommitted, result.Outcome)
	draft := body.intent
	keys := []string{f.b.identityKey, f.b.restoreKey, f.b.workspaceSyncBudget.key, draft.intentKey, draft.slotKey}
	cmps := append([]clientv3.Cmp(nil), source.archive.owner.comparisons...)
	cmps = append(cmps, clientv3.Compare(clientv3.Version(source.archive.owner.prefix), "=", 0).WithRange(clientv3.GetPrefixRangeEnd(source.archive.owner.prefix)))
	cmps = append(cmps, f.b.workspaceSyncBudget.comparisons()...)
	cmps = append(cmps, clientv3.Compare(clientv3.Version(draft.intentKey), "=", 0), clientv3.Compare(clientv3.Version(draft.slotKey), "=", 0))
	request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: []*pb.RequestOp{taskProtoPut(draft.intentKey, draft.value, 0), taskProtoPut(draft.slotKey, draft.slotValue, 0)}, Failure: taskProtoPoints(keys)}
	values := []string{f.b.identityValue, f.b.restoreEpoch, f.b.workspaceSyncBudget.value, strings.Repeat("x", int(f.b.workspaceSyncBudget.options.MaxIntentBytes)), strings.Repeat("x", 512)}
	t.Logf("actual preserved native request nodes=%d bytes=%d successReplyBound=%d failureReplyBound=%d", len(request.Compare)+len(request.Success)+len(request.Failure), request.Size(), taskProtoPutResponse(2).Size(), taskProtoResponse(keys, values).Size())
	intent, slot, err := f.b.workspaceSyncIntentKeys(result.Reference)
	require.NoError(t, err)
	got, err := f.raw.Txn(f.ctx).Then(clientv3.OpGet(intent), clientv3.OpGet(slot)).Commit()
	require.NoError(t, err)
	for _, response := range got.Responses {
		require.Len(t, response.GetResponseRange().Kvs, 1)
		kv := response.GetResponseRange().Kvs[0]
		require.True(t, immutableWorkspaceSyncKV(kv))
		require.Equal(t, result.Reference.Revision, kv.CreateRevision)
	}
	entry, err := f.b.LoadTaskWorkspaceSyncIntent(f.ctx, result.Reference)
	require.NoError(t, err)
	require.Equal(t, "put_object", entry.Record.Kind)
	require.Equal(t, body.entry.path, entry.Record.RelativePath)
	require.Equal(t, f.c.reference, entry.Record.Claim)
	again, err := source.reservePutIntent(f.ctx, body)
	require.NoError(t, err)
	require.Equal(t, result, again)
	data, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, "hello", string(data))
	require.NoError(t, body.Close())
	_, err = source.next(f.ctx)
	require.NoError(t, err)
	body2, err := source.openBody(f.ctx, source.current)
	require.NoError(t, err)
	_, err = source.reservePutIntent(f.ctx, body2)
	require.ErrorIs(t, err, errTaskWorkspaceSyncIntentQuota)
	require.NoError(t, source.Close())
	cold := freshDispatchBackend(t, f.b, f.raw)
	copy, err := cold.LoadTaskWorkspaceSyncIntent(f.ctx, result.Reference)
	require.NoError(t, err)
	require.Equal(t, entry, copy)
	copy.Record.Claim.WorkerID = "tampered"
	copy, err = cold.LoadTaskWorkspaceSyncIntent(f.ctx, result.Reference)
	require.NoError(t, err)
	require.Equal(t, entry, copy)
}

func workspaceSyncIntentSecondSetup(t *testing.T, first *quiescenceNativeFixture, workspace WorkspaceIdentity) taskDeliverySetupFactory {
	t.Helper()
	return func(t *testing.T) (*Backend, *clientv3.Client, AcquireIntentInput, *CreationClaim, []byte, ed25519.PrivateKey) {
		base := first.b
		input := acquisitionInput(t, "sync-intent-second")
		input.Workspace = workspace
		ctx := context.Background()
		result, err := base.AcquireIntent(ctx, input)
		require.NoError(t, err)
		require.Equal(t, OutcomeCommitted, result.Outcome)
		claim, err := base.ClaimCreation(ctx, workspace, input.IntentID, "second-preparer", 30*time.Second)
		require.NoError(t, err)
		t.Cleanup(func() { _ = base.ReleaseCreationClaim(context.Background(), claim) })
		dispatch, err := base.DeclareRuntimeDispatch(ctx, claim, dispatchInputForDeclaration())
		require.NoError(t, err)
		root := ed25519.NewKeyFromSeed(make([]byte, 32))
		certificate := preparationCertificate(t, base, dispatch.Entry.Record, root, "plain", RuntimeReference{ID: uuid.NewString(), UID: uuid.NewString(), BootID: uuid.NewString()})
		return base, first.raw, input, claim, certificate, root
	}
}

func TestTaskWorkspaceSyncIntentNativeSharedPartitionCompetition(t *testing.T) {
	f1, s1, body1 := workspaceSyncIntentNativeBody(t, 1)
	var workspace WorkspaceIdentity
	for i := 0; i < 10000; i++ {
		candidate, err := NewWorkspaceIdentity("s3", "test-storage", "bucket", fmt.Sprintf("other/workspace-%d/", i))
		require.NoError(t, err)
		if candidate.partition == s1.plan.workspace.partition {
			workspace = candidate
			break
		}
	}
	require.NotEmpty(t, workspace.hash)
	plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspace})
	require.NoError(t, err)
	budget := f1.b.workspaceSyncBudget.options
	f2, s2, body2 := workspaceSyncIntentBodyWithSetup(t, plan, &budget, workspaceSyncIntentSecondSetup(t, f1, workspace))
	require.NotSame(t, f1.b, f2.b)
	require.NotEqual(t, f1.c.reference.Task, f2.c.reference.Task)
	require.Equal(t, f1.b.namespace, f2.b.namespace)
	require.Equal(t, s1.plan.workspace.partition, s2.plan.workspace.partition)
	require.Equal(t, f1.b.workspaceSyncBudget.create, f2.b.workspaceSyncBudget.create)
	start := make(chan struct{})
	type answer struct {
		r   taskWorkspaceSyncIntentResult
		err error
	}
	done := make(chan answer, 2)
	for _, pair := range []struct {
		s *PreparedTaskWorkspaceSyncSource
		b *taskWorkspaceSyncBody
	}{{s1, body1}, {s2, body2}} {
		go func(s *PreparedTaskWorkspaceSyncSource, body *taskWorkspaceSyncBody) {
			<-start
			r, err := s.reservePutIntent(context.Background(), body)
			done <- answer{r, err}
		}(pair.s, pair.b)
	}
	close(start)
	a, b := <-done, <-done
	if a.err != nil {
		a, b = b, a
	}
	require.NoError(t, a.err)
	require.Equal(t, OutcomeCommitted, a.r.Outcome)
	require.ErrorIs(t, b.err, errTaskWorkspaceSyncIntentQuota)
	keys, err := f1.raw.Get(f1.ctx, fmt.Sprintf("%sp/%02x/workspace-sync-slots/", f1.b.namespace.Root(), workspace.partition), clientv3.WithPrefix())
	require.NoError(t, err)
	require.Len(t, keys.Kvs, 1)
	require.NoError(t, s1.Close())
	require.NoError(t, s2.Close())
	keys, err = f1.raw.Get(f1.ctx, string(keys.Kvs[0].Key))
	require.NoError(t, err)
	require.Len(t, keys.Kvs, 1)
	require.Zero(t, keys.Kvs[0].Lease)
}

func workspaceSyncIntentPutMatch(ops []clientv3.Op) bool {
	return len(ops) == 2 && ops[0].IsPut() && strings.Contains(string(ops[0].KeyBytes()), "/workspace-sync-intents/")
}

func workspaceSyncIntentSetKV(source *PreparedTaskWorkspaceSyncSource, kv clientv3.KV) {
	source.archive.owner.nativeMu.Lock()
	source.archive.owner.backend.client.KV = kv
	source.archive.owner.nativeMu.Unlock()
}

func TestTaskWorkspaceSyncIntentNativeUnknownCommitted(t *testing.T) {
	f, source, body := workspaceSyncIntentNativeBody(t, 2)
	var writes atomic.Int32
	workspaceSyncIntentSetKV(source, &faultKV{KV: f.b.client.KV, match: workspaceSyncIntentPutMatch, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
		writes.Add(1)
		require.NoError(t, e)
		require.True(t, r.Succeeded)
		return nil, context.DeadlineExceeded
	}})
	unknown, err := source.reservePutIntent(f.ctx, body)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.Equal(t, OutcomeUnknown, unknown.Outcome)
	require.Zero(t, unknown.Reference.Revision)
	_, err = f.b.LoadTaskWorkspaceSyncIntent(f.ctx, unknown.Reference)
	require.ErrorIs(t, err, ErrInvalidRecord)
	result, err := source.reservePutIntent(f.ctx, body)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	require.Equal(t, unknown.Reference.IntentID, result.Reference.IntentID)
	require.Equal(t, unknown.Reference.Slot, result.Reference.Slot)
	require.EqualValues(t, 1, writes.Load())
	_, err = source.reservePutIntent(f.ctx, body)
	require.NoError(t, err)
	require.EqualValues(t, 1, writes.Load())
}

func TestTaskWorkspaceSyncIntentNativeSameTxnCurrentFences(t *testing.T) {
	for _, fault := range []string{"whole-prefix", "restore", "command-aba", "budget-rewrite", "budget-aba"} {
		t.Run(fault, func(t *testing.T) {
			f, source, body := workspaceSyncIntentNativeBody(t, 1)
			var writes atomic.Int32
			var once sync.Once
			workspaceSyncIntentSetKV(source, &faultKV{KV: f.b.client.KV, match: workspaceSyncIntentPutMatch, before: func() {
				writes.Add(1)
				once.Do(func() {
					var err error
					key, value := f.b.workspaceSyncBudget.key, f.b.workspaceSyncBudget.value
					switch fault {
					case "whole-prefix":
						key, value = source.archive.owner.prefix+"zz/future", "inserted-after-preflight"
					case "restore":
						key, value = f.b.restoreKey, "new-restore"
					case "command-aba":
						key, err = f.b.namespace.taskWorkspaceArchiveKey(f.c.reference.Task)
						require.NoError(t, err)
						value = source.archive.owner.prepared.draft.value
						_, err = f.raw.Delete(context.Background(), key)
					case "budget-aba":
						_, err = f.raw.Delete(context.Background(), key)
					}
					require.NoError(t, err)
					_, err = f.raw.Put(context.Background(), key, value)
					require.NoError(t, err)
				})
			}})
			result, err := source.reservePutIntent(f.ctx, body)
			require.Error(t, err)
			require.NotErrorIs(t, err, errTaskWorkspaceSyncIntentQuota)
			require.Equal(t, OutcomeUnknown, result.Outcome)
			require.EqualValues(t, 1, writes.Load())
			got, err := f.raw.Get(f.ctx, body.intent.intentKey)
			require.NoError(t, err)
			require.Empty(t, got.Kvs)
			got, err = f.raw.Get(f.ctx, body.intent.slotKey)
			require.NoError(t, err)
			require.Empty(t, got.Kvs)
			require.NoError(t, source.Close())
		})
	}
}

func TestTaskWorkspaceSyncIntentNativeDiagnosticABA(t *testing.T) {
	f, source, body := workspaceSyncIntentNativeBody(t, 1)
	result, err := source.reservePutIntent(f.ctx, body)
	require.NoError(t, err)
	draft := body.intent
	_, err = f.raw.Txn(f.ctx).Then(clientv3.OpDelete(draft.intentKey), clientv3.OpDelete(draft.slotKey)).Commit()
	require.NoError(t, err)
	_, err = f.raw.Txn(f.ctx).Then(clientv3.OpPut(draft.intentKey, draft.value), clientv3.OpPut(draft.slotKey, draft.slotValue)).Commit()
	require.NoError(t, err)
	_, err = source.reservePutIntent(f.ctx, body)
	require.ErrorIs(t, err, ErrCorruptRecord)
	cold := freshDispatchBackend(t, f.b, f.raw)
	_, err = cold.LoadTaskWorkspaceSyncIntent(f.ctx, result.Reference)
	require.ErrorIs(t, err, ErrCorruptRecord)
	wrong := result.Reference
	wrong.Task.RestoreEpoch = "other"
	_, err = cold.LoadTaskWorkspaceSyncIntent(f.ctx, wrong)
	require.ErrorIs(t, err, ErrIdentityMismatch)
	wrong = result.Reference
	wrong.Task.Namespace = "/another/scope/cell/"
	_, err = cold.LoadTaskWorkspaceSyncIntent(f.ctx, wrong)
	require.ErrorIs(t, err, ErrIdentityMismatch)
}

func TestTaskWorkspaceSyncIntentNativeColdBudgetABA(t *testing.T) {
	f, source, body := workspaceSyncIntentNativeBody(t, 1)
	result, err := source.reservePutIntent(f.ctx, body)
	require.NoError(t, err)
	pin := f.b.workspaceSyncBudget
	_, err = f.raw.Delete(f.ctx, pin.key)
	require.NoError(t, err)
	put, err := f.raw.Put(f.ctx, pin.key, pin.value)
	require.NoError(t, err)
	require.Greater(t, put.Header.Revision, pin.create)
	cold := freshDispatchBackend(t, f.b, f.raw)
	_, err = cold.LoadTaskWorkspaceSyncIntent(f.ctx, result.Reference)
	require.ErrorIs(t, err, ErrIdentityMismatch)
	options := integrationOptions(f.b, f.raw)
	budget := pin.options
	options.TaskWorkspaceSyncBudget = &budget
	reopened, err := New(f.ctx, options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	_, err = reopened.LoadTaskWorkspaceSyncIntent(f.ctx, result.Reference)
	require.ErrorIs(t, err, ErrIdentityMismatch)
}

func TestTaskWorkspaceSyncIntentNativeFixed64CandidateBound(t *testing.T) {
	f, source, body := workspaceSyncIntentNativeBody(t, 64)
	pin := f.b.workspaceSyncBudget
	for i := uint32(0); i < 64; i++ {
		ref := TaskWorkspaceSyncIntentReference{Task: f.c.reference.Task, BudgetID: pin.options.BudgetID, IntentID: uuid.NewString(), Slot: i}
		_, key, err := f.b.workspaceSyncIntentKeys(ref)
		require.NoError(t, err)
		value, err := encodeOperationMetadata(taskWorkspaceSyncSlotRecord{1, ref.BudgetID, ref.Task.Partition, i, ref.IntentID, strings.Repeat("a", 64)}, 512)
		require.NoError(t, err)
		// Operator-injected immutable occupied slots model conservative unknown
		// charges. They do not grant or reconstruct any original source.
		_, err = f.raw.Put(f.ctx, key, value)
		require.NoError(t, err)
	}
	var attempts atomic.Int32
	workspaceSyncIntentSetKV(source, &faultKV{KV: f.b.client.KV, match: workspaceSyncIntentPutMatch, before: func() { attempts.Add(1) }})
	result, err := source.reservePutIntent(f.ctx, body)
	require.ErrorIs(t, err, errTaskWorkspaceSyncIntentQuota)
	require.Equal(t, OutcomeUnknown, result.Outcome)
	require.EqualValues(t, 64, attempts.Load())
	require.False(t, body.intent.writeAttempted, "explicit known failures may retry current original source")
	got, err := f.raw.Get(f.ctx, body.intent.intentKey)
	require.NoError(t, err)
	require.Empty(t, got.Kvs)
	_, err = f.raw.Put(f.ctx, f.b.restoreKey, "restored")
	require.NoError(t, err)
	_, err = source.reservePutIntent(f.ctx, body)
	require.Error(t, err)
	require.NotErrorIs(t, err, errTaskWorkspaceSyncIntentQuota)
	require.LessOrEqual(t, attempts.Load(), int32(65), "failed permission must not keep probing slots")
}

func TestTaskWorkspaceSyncIntentNativeLeaseExpiryRetainsSlots(t *testing.T) {
	plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspaceSyncIdentity(t, "s3", "test-storage", "bucket", "team/project/")})
	require.NoError(t, err)
	f, source, body := workspaceSyncIntentBodyWithSetup(t, plan, &TaskWorkspaceSyncBudgetOptions{uuid.NewString(), 1, 4096}, nil, 3*time.Second)
	result, err := source.reservePutIntent(f.ctx, body)
	require.NoError(t, err)
	id := clientv3.LeaseID(f.c.reference.LeaseID)
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		lease, err := f.raw.TimeToLive(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, lease)
		require.Equal(t, id, lease.ID)
		return lease.TTL < 0
	}, 8*time.Second, 50*time.Millisecond)
	_, err = source.reservePutIntent(f.ctx, body)
	require.Error(t, err)
	require.NoError(t, source.Close())
	cold := freshDispatchBackend(t, f.b, f.raw)
	entry, err := cold.LoadTaskWorkspaceSyncIntent(f.ctx, result.Reference)
	require.NoError(t, err)
	require.Equal(t, result.Reference.Revision, entry.Revision)
	for _, key := range []string{body.intent.intentKey, body.intent.slotKey} {
		got, err := f.raw.Get(f.ctx, key)
		require.NoError(t, err)
		require.Len(t, got.Kvs, 1)
		require.True(t, immutableWorkspaceSyncKV(got.Kvs[0]))
	}
	t.Logf("original claim lease %d naturally expired; intent/slot revision=%d retained permanently", id, entry.Revision)
}

func TestTaskWorkspaceSyncIntentNativeBudgetConstructor(t *testing.T) {
	f, source, body := workspaceSyncIntentNativeBody(t, 1)
	options := integrationOptions(f.b, f.raw)
	budget := f.b.workspaceSyncBudget.options
	options.TaskWorkspaceSyncBudget = &budget
	opened, err := New(f.ctx, options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, opened.Close()) })
	budget.BudgetID, budget.SlotsPerPartition, budget.MaxIntentBytes = uuid.NewString(), 64, 16384
	require.Equal(t, f.b.workspaceSyncBudget.options, opened.workspaceSyncBudget.options)
	_, err = New(f.ctx, options)
	require.ErrorIs(t, err, ErrIdentityMismatch)
	budget = f.b.workspaceSyncBudget.options
	_, err = f.raw.Put(f.ctx, f.b.workspaceSyncBudget.key, f.b.workspaceSyncBudget.value)
	require.NoError(t, err)
	_, err = New(f.ctx, options)
	require.ErrorIs(t, err, ErrIdentityMismatch)
	_, err = source.reservePutIntent(f.ctx, body)
	require.ErrorIs(t, err, ErrIdentityMismatch)
	_, err = f.raw.Delete(f.ctx, f.b.workspaceSyncBudget.key)
	require.NoError(t, err)
	_, err = New(f.ctx, options)
	require.ErrorIs(t, err, ErrIdentityMismatch)
	lease, err := f.raw.Grant(f.ctx, 10)
	require.NoError(t, err)
	_, err = f.raw.Put(f.ctx, f.b.workspaceSyncBudget.key, f.b.workspaceSyncBudget.value, clientv3.WithLease(lease.ID))
	require.NoError(t, err)
	_, err = New(f.ctx, options)
	require.ErrorIs(t, err, ErrIdentityMismatch)
	_, err = f.raw.Revoke(f.ctx, lease.ID)
	require.NoError(t, err)
}

func TestTaskWorkspaceSyncIntentNativeOriginalBodyOnly(t *testing.T) {
	for _, fault := range []string{"source-copy", "entry-copy", "body-copy", "partial", "closed", "disabled"} {
		t.Run(fault, func(t *testing.T) {
			var f *quiescenceNativeFixture
			var source *PreparedTaskWorkspaceSyncSource
			var body *taskWorkspaceSyncBody
			if fault == "disabled" {
				plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspaceSyncIdentity(t, "s3", "test-storage", "bucket", "team/project/")})
				require.NoError(t, err)
				f, source, body = workspaceSyncIntentBodyWithSetup(t, plan, nil, nil)
			} else {
				f, source, body = workspaceSyncIntentNativeBody(t, 1)
			}
			var writes atomic.Int32
			workspaceSyncIntentSetKV(source, &faultKV{KV: f.b.client.KV, match: workspaceSyncIntentPutMatch, before: func() { writes.Add(1) }})
			s, b := source, body
			switch fault {
			case "source-copy":
				s = &PreparedTaskWorkspaceSyncSource{self: source.self}
			case "entry-copy":
				copied := *body.entry
				b = &taskWorkspaceSyncBody{self: body, source: source, entry: &copied}
			case "body-copy":
				b = &taskWorkspaceSyncBody{self: body, source: source, entry: body.entry}
			case "partial":
				var data [1]byte
				_, err := body.Read(data[:])
				require.NoError(t, err)
			case "closed":
				require.Error(t, body.Close())
			}
			_, err := s.reservePutIntent(f.ctx, b)
			require.Error(t, err)
			require.Zero(t, writes.Load())
			require.Nil(t, body.intent)
			require.NoError(t, source.Close())
		})
	}
}

// The controlled transport boundary retains exactly the originally built
// native transaction and models a request surviving its caller's lost reply.
// This is not a Raft partition or a kernel/socket cancellation experiment.
type workspaceSyncIntentLateKV struct {
	clientv3.KV
	ctx    context.Context
	gate   chan struct{}
	done   chan error
	writes atomic.Int32
}

func (kv *workspaceSyncIntentLateKV) Txn(ctx context.Context) clientv3.Txn {
	return &workspaceSyncIntentLateTxn{Txn: kv.KV.Txn(ctx), owner: kv}
}

type workspaceSyncIntentLateTxn struct {
	clientv3.Txn
	owner *workspaceSyncIntentLateKV
	cmps  []clientv3.Cmp
	late  bool
}

func (txn *workspaceSyncIntentLateTxn) If(cmps ...clientv3.Cmp) clientv3.Txn {
	txn.cmps = cmps
	txn.Txn = txn.Txn.If(cmps...)
	return txn
}

func (txn *workspaceSyncIntentLateTxn) Then(ops ...clientv3.Op) clientv3.Txn {
	if workspaceSyncIntentPutMatch(ops) {
		txn.late = true
		txn.Txn = txn.owner.KV.Txn(txn.owner.ctx).If(txn.cmps...).Then(ops...)
	} else {
		txn.Txn = txn.Txn.Then(ops...)
	}
	return txn
}

func (txn *workspaceSyncIntentLateTxn) Else(ops ...clientv3.Op) clientv3.Txn {
	txn.Txn = txn.Txn.Else(ops...)
	return txn
}

func (txn *workspaceSyncIntentLateTxn) Commit() (*clientv3.TxnResponse, error) {
	if !txn.late {
		return txn.Txn.Commit()
	}
	txn.owner.writes.Add(1)
	go func() {
		select {
		case <-txn.owner.gate:
		case <-txn.owner.ctx.Done():
			txn.owner.done <- txn.owner.ctx.Err()
			return
		}
		r, err := txn.Txn.Commit()
		if err == nil && !r.Succeeded {
			err = ErrConflict
		}
		txn.owner.done <- err
	}()
	return nil, context.DeadlineExceeded
}

func TestTaskWorkspaceSyncIntentNativeUnknownAbsentAndLateOriginalTxn(t *testing.T) {
	f, source, body := workspaceSyncIntentNativeBody(t, 2)
	transportCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	late := &workspaceSyncIntentLateKV{KV: f.b.client.KV, ctx: transportCtx, gate: make(chan struct{}), done: make(chan error, 1)}
	workspaceSyncIntentSetKV(source, late)
	unknown, err := source.reservePutIntent(f.ctx, body)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.Equal(t, OutcomeUnknown, unknown.Outcome)
	draft := body.intent
	for i := 0; i < 2; i++ {
		again, err := source.reservePutIntent(f.ctx, body)
		require.ErrorIs(t, err, ErrOutcomeUnknown)
		require.Equal(t, unknown, again)
	}
	require.EqualValues(t, 1, late.writes.Load())
	got, err := f.raw.Get(f.ctx, draft.intentKey)
	require.NoError(t, err)
	require.Empty(t, got.Kvs, "absence cannot prove the original request will never commit")
	close(late.gate)
	require.NoError(t, <-late.done)
	result, err := source.reservePutIntent(f.ctx, body)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	require.Equal(t, unknown.Reference.IntentID, result.Reference.IntentID)
	require.Equal(t, unknown.Reference.Slot, result.Reference.Slot)
	require.EqualValues(t, 1, late.writes.Load())
	require.Positive(t, result.Reference.Revision)
}

func TestTaskWorkspaceSyncIntentNativeCloseJoinsOriginalRPC(t *testing.T) {
	for _, route := range []string{"source", "backend", "body"} {
		t.Run(route, func(t *testing.T) {
			f, source, body := workspaceSyncIntentNativeBody(t, 1)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unlock := func() { once.Do(func() { close(release) }) }
			defer unlock()
			workspaceSyncIntentSetKV(source, &faultKV{KV: f.b.client.KV, match: workspaceSyncIntentPutMatch, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				require.NoError(t, err)
				require.True(t, r.Succeeded)
				close(entered)
				<-release
				return r, nil
			}})
			type answer struct {
				r   taskWorkspaceSyncIntentResult
				err error
			}
			reserved := make(chan answer, 1)
			go func() { r, err := source.reservePutIntent(f.ctx, body); reserved <- answer{r, err} }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("native commit did not arrive")
			}
			closed := make(chan error, 1)
			go func() {
				var err error
				switch route {
				case "source":
					err = source.Close()
				case "backend":
					err = f.b.Close()
				case "body":
					err = body.Close()
				}
				closed <- err
			}()
			require.Eventually(t, func() bool {
				if route == "body" {
					return body.ctx.Err() != nil
				}
				return source.ctx.Err() != nil
			}, 2*time.Second, time.Millisecond)
			select {
			case <-closed:
				t.Fatal("Close returned before the original RPC completion")
			default:
			}
			store := source.archive.owner.spool.store
			store.mu.Lock()
			require.EqualValues(t, 1, store.jobs)
			store.mu.Unlock()
			_, err := source.archive.owner.spool.file.Stat()
			require.NoError(t, err)
			select {
			case <-f.b.client.Ctx().Done():
				t.Fatal("client closed before original RPC joined")
			default:
			}
			unlock()
			completed := <-reserved
			require.Error(t, completed.err)
			// Commit was real; post-RPC local invalidation never refunds metadata.
			got, err := f.raw.Get(context.Background(), body.intent.slotKey)
			require.NoError(t, err)
			require.Len(t, got.Kvs, 1)
			require.Zero(t, got.Kvs[0].Lease)
			if route == "body" {
				require.Error(t, <-closed)
				require.NoError(t, source.Close())
			} else {
				require.NoError(t, <-closed)
			}
		})
	}
}
