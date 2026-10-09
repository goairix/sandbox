//go:build linux

package etcd

import (
	"archive/tar"
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func workspaceSyncRecyclingNativeBody(t *testing.T, plan TaskWorkspaceSyncPlan, budget *TaskWorkspaceSyncBudgetOptions, setup taskDeliverySetupFactory) (*quiescenceNativeFixture, *PreparedTaskWorkspaceSyncSource, *taskWorkspaceSyncBody, *taskWorkspaceSyncLocalTarget) {
	return workspaceSyncRecyclingNativeBodyData(t, plan, budget, setup, nil, 0, 60*time.Second)
}

func workspaceSyncRecyclingNativeBodyData(t *testing.T, plan TaskWorkspaceSyncPlan, budget *TaskWorkspaceSyncBudgetOptions, setup taskDeliverySetupFactory, data []byte, entries uint64, ttl time.Duration) (*quiescenceNativeFixture, *PreparedTaskWorkspaceSyncSource, *taskWorkspaceSyncBody, *taskWorkspaceSyncLocalTarget) {
	t.Helper()
	if setup == nil {
		setup = func(t *testing.T) (*Backend, *clientv3.Client, AcquireIntentInput, *CreationClaim, []byte, ed25519.PrivateKey) {
			original, raw := integrationBackend(t)
			root := ed25519.NewKeyFromSeed(make([]byte, 32))
			options := integrationOptions(original, raw)
			options.PublicationTrust = &RuntimePublicationTrust{AuthorityID: options.Identity.RuntimeID, Target: "kubernetes", Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}
			options.Clock = goodAuthorityClock()
			base, err := New(context.Background(), options)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, base.Close()) })
			archiveTrackCleanup(base)
			input := acquisitionInput(t, "recycling-local-source")
			input.Workspace = plan.workspace
			result, err := base.AcquireIntent(context.Background(), input)
			require.NoError(t, err)
			require.Equal(t, OutcomeCommitted, result.Outcome)
			claim, err := base.ClaimCreation(context.Background(), input.Workspace, input.IntentID, "local-preparer", 30*time.Second)
			require.NoError(t, err)
			t.Cleanup(func() { _ = base.ReleaseCreationClaim(context.Background(), claim) })
			dispatch, err := base.DeclareRuntimeDispatch(context.Background(), claim, dispatchInputForDeclaration())
			require.NoError(t, err)
			certificate := preparationCertificate(t, base, dispatch.Entry.Record, root, "plain", RuntimeReference{ID: uuid.NewString(), UID: uuid.NewString(), BootID: uuid.NewString()})
			return base, raw, input, claim, certificate, root
		}
	}
	f, prepared, wire := deliveryNativeFixtureWithProfileData(t, 0, func(TaskRecord) TaskFinalSyncPlan {
		p, err := plan.FinalSyncPlan()
		require.NoError(t, err)
		return p
	}, budget, setup, data, entries, true, ttl)
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

func workspaceSyncRecyclingNativeDefault(t *testing.T) (*quiescenceNativeFixture, *PreparedTaskWorkspaceSyncSource, *taskWorkspaceSyncBody, *taskWorkspaceSyncLocalTarget) {
	t.Helper()
	plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspaceSyncIdentity(t, "local", "test-storage", "bucket", "team/project/")})
	require.NoError(t, err)
	return workspaceSyncRecyclingNativeBody(t, plan, &TaskWorkspaceSyncBudgetOptions{uuid.NewString(), 1, 4096}, nil)
}

func TestTaskWorkspaceSyncRecyclingNativeSettlement(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit owned Linux native fixture required")
	}
	f, s, body, target := workspaceSyncRecyclingNativeDefault(t)
	require.NoError(t, s.bindLocalPut(f.ctx, body, target))
	_, err := s.reservePutIntent(f.ctx, body)
	require.NoError(t, err)
	w, err := s.putLocal(f.ctx, body, target)
	require.NoError(t, err)
	consumer, ok := any(s).(interface {
		settleLocal(context.Context, *taskWorkspaceSyncLocalWitness) (*taskWorkspaceSyncLocalSettlement, error)
		recycleLocal(context.Context, *taskWorkspaceSyncLocalSettlement) (Outcome, error)
	})
	require.True(t, ok, "original successful local witness needs durable terminal and GC consumer")
	terminal, err := consumer.settleLocal(f.ctx, w)
	require.NoError(t, err)
	point, err := f.raw.Get(f.ctx, body.intent.slotKey)
	require.NoError(t, err)
	require.Len(t, point.Kvs, 1)
	slot, err := decodeWorkspaceSyncRecyclingSlot(point.Kvs[0])
	require.NoError(t, err)
	require.Equal(t, "terminal", slot.State)
	require.Equal(t, body.intent.revision, slot.IntentBirth)
	require.EqualValues(t, 3, point.Kvs[0].Version)
	outcome, err := consumer.recycleLocal(f.ctx, terminal)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	point, err = f.raw.Get(f.ctx, body.intent.slotKey)
	require.NoError(t, err)
	require.Len(t, point.Kvs, 1)
	slot, err = decodeWorkspaceSyncRecyclingSlot(point.Kvs[0])
	require.NoError(t, err)
	require.Equal(t, "free", slot.State)
	require.EqualValues(t, 4, point.Kvs[0].Version)
	intent, err := f.raw.Get(f.ctx, body.intent.intentKey)
	require.NoError(t, err)
	require.Empty(t, intent.Kvs)
	require.NoError(t, body.Close())
	entry, err := s.next(f.ctx)
	require.NoError(t, err)
	require.NotNil(t, entry, "same original source advances after known GC")
	actual, err := os.ReadFile(filepath.Join(target.pin.Root, body.entry.path))
	require.NoError(t, err)
	require.Equal(t, "hello", string(actual))
	second, err := s.openBody(f.ctx, entry)
	require.NoError(t, err)
	require.NoError(t, s.bindLocalPut(f.ctx, second, target))
	_, err = s.reservePutIntent(f.ctx, second)
	require.NoError(t, err)
	w2, err := s.putLocal(f.ctx, second, target)
	require.NoError(t, err)
	terminal2, err := consumer.settleLocal(f.ctx, w2)
	require.NoError(t, err)
	outcome, err = consumer.recycleLocal(f.ctx, terminal2)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	actual, err = os.ReadFile(filepath.Join(target.pin.Root, second.entry.path))
	require.NoError(t, err)
	require.Equal(t, "bye", string(actual))
	t.Logf("original terminal and GC acknowledged; permanent slot create=%d mod=%d version=%d; next=%s", point.Kvs[0].CreateRevision, point.Kvs[0].ModRevision, point.Kvs[0].Version, entry.path)
}

func recyclingTerminalMatch(ops []clientv3.Op) bool {
	return len(ops) == 1 && ops[0].IsPut() && strings.Contains(string(ops[0].KeyBytes()), "/workspace-sync-recycling-slots/")
}

func recyclingGCMatch(ops []clientv3.Op) bool {
	return len(ops) == 2 && ops[0].IsPut() && ops[1].IsDelete() && strings.Contains(string(ops[0].KeyBytes()), "/workspace-sync-recycling-slots/")
}

func workspaceSyncRecyclingNativeWritten(t *testing.T) (*quiescenceNativeFixture, *PreparedTaskWorkspaceSyncSource, *taskWorkspaceSyncBody, *taskWorkspaceSyncLocalTarget, *taskWorkspaceSyncLocalWitness) {
	t.Helper()
	f, s, body, target := workspaceSyncRecyclingNativeDefault(t)
	require.NoError(t, s.bindLocalPut(f.ctx, body, target))
	_, err := s.reservePutIntent(f.ctx, body)
	require.NoError(t, err)
	w, err := s.putLocal(f.ctx, body, target)
	require.NoError(t, err)
	require.Error(t, w.origin.ctx.Err(), "finished put ctx is canceled; settlement needs fresh ownership")
	return f, s, body, target, w
}

func TestTaskWorkspaceSyncRecyclingNativeSettlementAuthority(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit owned Linux native fixture required")
	}
	f, s, body, target, w := workspaceSyncRecyclingNativeWritten(t)
	var mutations atomic.Int32
	workspaceSyncIntentSetKV(s, &faultKV{KV: f.b.client.KV, match: recyclingTerminalMatch, before: func() { mutations.Add(1) }})
	for _, fault := range []string{"witness-copy", "joint-copy", "foreign-body", "unfinished-join", "missing-ack", "count", "hash", "dev", "ino"} {
		t.Run(fault, func(t *testing.T) {
			candidate := w
			restore := func() {}
			switch fault {
			case "witness-copy", "joint-copy":
				copy := *w
				copy.self = &copy
				candidate = &copy
				if fault == "joint-copy" {
					p := &taskWorkspaceSyncLocalPut{source: s, body: body, target: target, draft: body.intent, witness: &copy, sealed: true, ioDone: w.origin.ioDone}
					p.self, copy.origin = p, p
				}
			case "foreign-body":
				original := w.origin.body
				w.origin.body = &taskWorkspaceSyncBody{source: s, entry: body.entry, ctx: body.ctx}
				w.origin.body.self = w.origin.body
				restore = func() { w.origin.body = original }
			case "unfinished-join":
				original := w.origin.ioDone
				w.origin.ioDone = make(chan struct{})
				restore = func() { w.origin.ioDone = original }
			case "missing-ack":
				body.intent.acknowledged = false
				restore = func() { body.intent.acknowledged = true }
			case "count":
				w.bytes++
				restore = func() { w.bytes-- }
			case "hash":
				original := w.sha256
				w.sha256 = strings.Repeat("a", 64)
				restore = func() { w.sha256 = original }
			case "dev":
				w.dev++
				restore = func() { w.dev-- }
			case "ino":
				original := w.ino
				w.ino = 0
				restore = func() { w.ino = original }
			}
			defer restore()
			_, err := s.settleLocal(f.ctx, candidate)
			require.Error(t, err)
			require.Zero(t, mutations.Load(), "invalid authority cannot emit terminal mutation")
		})
	}
	q, err := s.settleLocal(f.ctx, w)
	require.NoError(t, err)
	require.EqualValues(t, 1, mutations.Load())
	var gcMutations atomic.Int32
	workspaceSyncIntentSetKV(s, &faultKV{KV: f.b.client.KV, match: recyclingGCMatch, before: func() { gcMutations.Add(1) }})
	for _, joint := range []bool{false, true} {
		t.Run(fmt.Sprintf("settlement-copy-joint-%t", joint), func(t *testing.T) {
			copy := *q
			copy.self = &copy
			if joint {
				wcopy := *w
				p := &taskWorkspaceSyncLocalPut{source: s, body: body, target: target, draft: body.intent, witness: &wcopy, sealed: true, ioDone: w.origin.ioDone}
				p.self, wcopy.self, wcopy.origin = p, &wcopy, p
				copy.witness = &wcopy
			}
			_, err := s.recycleLocal(f.ctx, &copy)
			require.Error(t, err)
			require.Zero(t, gcMutations.Load())
		})
	}
	outcome, err := s.recycleLocal(f.ctx, q)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	require.EqualValues(t, 1, gcMutations.Load())
	actual, err := os.ReadFile(filepath.Join(target.pin.Root, body.entry.path))
	require.NoError(t, err)
	require.Equal(t, "hello", string(actual))
}

func TestTaskWorkspaceSyncRecyclingNativeCursorGate(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit owned Linux native fixture required")
	}
	for _, state := range []string{"io-complete", "terminal", "unknown-gc", "failed-io"} {
		t.Run(state, func(t *testing.T) {
			f, s, body, target := workspaceSyncRecyclingNativeDefault(t)
			require.NoError(t, s.bindLocalPut(f.ctx, body, target))
			_, err := s.reservePutIntent(f.ctx, body)
			require.NoError(t, err)
			if state == "failed-io" {
				s.mu.Lock()
				s.localIOComplete = func(stage string) {
					if stage == "fsync" {
						require.NoError(t, os.Chmod(target.pin.Root, 0750))
					}
				}
				s.mu.Unlock()
			}
			w, err := s.putLocal(f.ctx, body, target)
			if state == "failed-io" {
				require.Error(t, err)
				require.Nil(t, w)
				_, err = s.settleLocal(f.ctx, w)
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.True(t, body.complete, "Read completed even when subsequent IO/settlement was not known successful")
			if state == "terminal" || state == "unknown-gc" {
				q, err := s.settleLocal(f.ctx, w)
				require.NoError(t, err)
				if state == "unknown-gc" {
					workspaceSyncIntentSetKV(s, &faultKV{KV: f.b.client.KV, match: recyclingGCMatch, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
						require.NoError(t, err)
						return nil, context.DeadlineExceeded
					}})
					_, err = s.recycleLocal(f.ctx, q)
					require.ErrorIs(t, err, ErrOutcomeUnknown)
					require.False(t, q.completed)
				}
			}
			current := s.current
			_, err = s.next(f.ctx)
			require.Error(t, err)
			require.Same(t, body, s.body, "GC not clearly completed: current original body retained")
			require.Same(t, current, s.current)
		})
	}
}

func TestTaskWorkspaceSyncRecyclingNativeSequential65(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit owned Linux native fixture required")
	}
	headers := []*tar.Header{{Name: "workspace", Typeflag: tar.TypeDir, Mode: 0700}, {Name: "workspace/z", Typeflag: tar.TypeDir, Mode: 0700}}
	bodies := []string{"", ""}
	for i := 0; i < 65; i++ {
		contents := fmt.Sprintf("original-file-%02d", i)
		headers = append(headers, &tar.Header{Name: fmt.Sprintf("workspace/z/file-%02d", i), Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(contents))})
		bodies = append(bodies, contents)
	}
	data := archiveTarBytes(t, headers, bodies)
	plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspaceSyncIdentity(t, "local", "test-storage", "bucket", "team/project/")})
	require.NoError(t, err)
	f, s, body, target := workspaceSyncRecyclingNativeBodyData(t, plan, &TaskWorkspaceSyncBudgetOptions{uuid.NewString(), 1, 4096}, nil, data, 67, 180*time.Second)
	started := time.Now()
	originalKV := f.b.client.KV
	var oldReserve, oldTerminal, oldGC clientv3.Txn
	var create, previous int64
	for i := 0; i < 65; i++ {
		require.NoError(t, s.bindLocalPut(f.ctx, body, target))
		if i == 0 {
			workspaceSyncIntentSetKV(s, &faultKV{KV: originalKV, match: recyclingPutMatch, before: func() {
				d := body.intent
				oldReserve = f.raw.Txn(f.ctx).If(s.recyclingReserveComparisons(d)...).Then(clientv3.OpPut(d.intentKey, d.value), clientv3.OpPut(d.slotKey, d.slotValue))
			}})
		}
		_, err := s.reservePutIntent(f.ctx, body)
		require.NoError(t, err)
		d := body.intent
		if i == 0 {
			create = d.recycling.create
		}
		inspect := func(state string, version, revision int64, count int) {
			t.Helper()
			all, err := f.raw.Get(f.ctx, fmt.Sprintf("%sp/%02x/workspace-sync-recycling-", f.b.namespace.Root(), plan.workspace.partition), clientv3.WithPrefix())
			require.NoError(t, err)
			require.Len(t, all.Kvs, count, "S1 active key bound")
			point, err := f.raw.Get(f.ctx, d.slotKey)
			require.NoError(t, err)
			require.Len(t, point.Kvs, 1)
			kv := point.Kvs[0]
			slot, err := decodeWorkspaceSyncRecyclingSlot(kv)
			require.NoError(t, err)
			require.Equal(t, state, slot.State)
			require.Equal(t, create, kv.CreateRevision)
			require.Equal(t, version, kv.Version)
			require.Equal(t, revision, kv.ModRevision)
			require.Greater(t, revision, previous)
			previous = revision
			t.Logf("file=%02d state=%s Create=%d MOD=%d Version=%d activeKeys=%d", i, state, create, revision, version, count)
		}
		inspect("occupied", int64(i*3+2), d.revision, 2)
		if i == 1 {
			for _, txn := range []clientv3.Txn{oldReserve, oldTerminal, oldGC} {
				r, err := txn.Commit()
				require.NoError(t, err)
				require.False(t, r.Succeeded, "captured original old Txn cannot modify the next occupied generation")
			}
			point, err := f.raw.Get(f.ctx, d.slotKey)
			require.NoError(t, err)
			require.Equal(t, d.revision, point.Kvs[0].ModRevision)
			t.Log("captured original reserve/terminal/GC actual late native Txns all rejected after next reserve")
		}
		w, err := s.putLocal(f.ctx, body, target)
		require.NoError(t, err)
		actual, err := os.ReadFile(filepath.Join(target.pin.Root, body.entry.path))
		require.NoError(t, err)
		require.Equal(t, bodies[i+2], string(actual))
		require.Equal(t, workspaceSyncIntentDigest(string(actual)), w.sha256)
		if i == 0 {
			workspaceSyncIntentSetKV(s, &faultKV{KV: originalKV, match: recyclingTerminalMatch, before: func() {
				q := d.localSettlement
				oldTerminal = f.raw.Txn(f.ctx).If(s.recyclingTerminalComparisons(d)...).Then(clientv3.OpPut(d.slotKey, q.value))
			}})
		}
		q, err := s.settleLocal(f.ctx, w)
		require.NoError(t, err)
		inspect("terminal", int64(i*3+3), q.terminal.mod, 2)
		if i == 0 {
			workspaceSyncIntentSetKV(s, &faultKV{KV: originalKV, match: recyclingGCMatch, before: func() {
				free, err := encodeWorkspaceSyncRecyclingSlot(taskWorkspaceSyncRecyclingSlotRecord{Version: 2, BudgetID: d.record.Reference.BudgetID, Partition: d.record.Reference.Task.Partition, Slot: 0, State: "free"})
				require.NoError(t, err)
				oldGC = f.raw.Txn(f.ctx).If(s.recyclingGCComparisons(q)...).Then(clientv3.OpPut(d.slotKey, free), clientv3.OpDelete(d.intentKey))
			}})
		}
		outcome, err := s.recycleLocal(f.ctx, q)
		require.NoError(t, err)
		require.Equal(t, OutcomeCommitted, outcome)
		point, err := f.raw.Get(f.ctx, d.slotKey)
		require.NoError(t, err)
		inspect("free", int64(i*3+4), point.Kvs[0].ModRevision, 1)
		if i == 0 {
			workspaceSyncIntentSetKV(s, originalKV)
		}
		require.NoError(t, body.Close())
		if i < 64 {
			e, err := s.next(f.ctx)
			require.NoError(t, err)
			body, err = s.openBody(f.ctx, e)
			require.NoError(t, err)
		}
	}
	_, err = s.next(f.ctx)
	require.ErrorIs(t, err, io.EOF)
	all, err := f.raw.Get(f.ctx, f.b.namespace.Root(), clientv3.WithPrefix())
	require.NoError(t, err)
	slots, intents := 0, 0
	for _, kv := range all.Kvs {
		if strings.Contains(string(kv.Key), "/workspace-sync-recycling-slots/") {
			slots++
		} else if strings.Contains(string(kv.Key), "/workspace-sync-recycling-intents/") {
			intents++
		}
	}
	require.Equal(t, 256, slots)
	require.Zero(t, intents)
	entries, err := os.ReadDir(filepath.Join(target.pin.Root, "z"))
	require.NoError(t, err)
	require.Len(t, entries, 65)
	t.Logf("65 actual local files completed sequentially S1; final Version196/Create%d; slots256/intents0; elapsed=%s; tmpfs and repeated archive guards, no power-loss/performance proof", create, time.Since(started))
}

func TestTaskWorkspaceSyncRecyclingNativeCompletionUnknown(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit owned Linux native fixture required")
	}
	for _, stage := range []string{"terminal", "gc"} {
		for _, fault := range []string{"lost-ack", "malformed-ack", "unknown-absent"} {
			t.Run(stage+"-"+fault, func(t *testing.T) {
				f, s, body, target, w := workspaceSyncRecyclingNativeWritten(t)
				var q *taskWorkspaceSyncLocalSettlement
				var err error
				match := recyclingTerminalMatch
				if stage == "gc" {
					q, err = s.settleLocal(f.ctx, w)
					require.NoError(t, err)
					match = recyclingGCMatch
				}
				var writes atomic.Int32
				var late *recyclingLateKV
				if fault == "unknown-absent" {
					late = &recyclingLateKV{KV: f.b.client.KV, ctx: f.ctx, gate: make(chan struct{}), done: make(chan error, 1), joined: make(chan struct{}), match: match}
					workspaceSyncIntentSetKV(s, late)
				} else {
					workspaceSyncIntentSetKV(s, &faultKV{KV: f.b.client.KV, match: match, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
						writes.Add(1)
						require.NoError(t, err)
						require.True(t, r.Succeeded)
						if fault == "lost-ack" {
							return nil, context.DeadlineExceeded
						}
						if stage == "terminal" {
							r.Responses = nil
						} else {
							r.Responses[1].GetResponseDeleteRange().Deleted = 0
						}
						return r, nil
					}})
				}
				call := func() error {
					if stage == "terminal" {
						q, err = s.settleLocal(f.ctx, w)
						return err
					}
					_, err = s.recycleLocal(f.ctx, q)
					return err
				}
				require.ErrorIs(t, call(), ErrOutcomeUnknown)
				require.NotNil(t, q)
				if stage == "terminal" {
					require.False(t, q.known)
					_, err := s.recycleLocal(f.ctx, q)
					require.Error(t, err, "unknown terminal is not GC authority")
				} else {
					require.False(t, q.completed)
				}
				copy := *q
				copy.self = &copy
				_, err = s.recycleLocal(f.ctx, &copy)
				require.Error(t, err, "copied unknown cannot gain completion authority")
				if late != nil {
					for i := 0; i < 2; i++ {
						require.ErrorIs(t, call(), ErrOutcomeUnknown, "same-point unchanged native state remains unknown")
					}
					require.EqualValues(t, 1, late.writes.Load())
					close(late.gate)
					require.NoError(t, <-late.done)
					<-late.joined
					t.Log("actual original escaped native Txn joined once; earlier diagnostics were readonly")
				}
				require.NoError(t, call(), "only original fixed guarded read can recognize exact next native generation")
				require.NoError(t, call(), "repeated original call does not resend mutation")
				if late == nil {
					require.EqualValues(t, 1, writes.Load())
				} else {
					require.EqualValues(t, 1, late.writes.Load())
				}
				point, err := f.raw.Get(f.ctx, body.intent.slotKey)
				require.NoError(t, err)
				require.Len(t, point.Kvs, 1)
				if stage == "terminal" {
					require.True(t, q.known)
					require.Equal(t, body.intent.recycling.version+2, point.Kvs[0].Version)
				} else {
					require.True(t, q.completed)
					require.Equal(t, q.terminal.version+1, point.Kvs[0].Version)
				}
				actual, err := os.ReadFile(filepath.Join(target.pin.Root, body.entry.path))
				require.NoError(t, err)
				require.Equal(t, "hello", string(actual), "readonly arbitration never repeats file IO")
			})
		}
	}
}

func TestTaskWorkspaceSyncRecyclingNativeCompletionFences(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit owned Linux native fixture required")
	}
	for _, stage := range []string{"terminal", "gc"} {
		for _, fault := range []string{"slot-generation", "intent-immutable", "restore", "claim-lost", "root", "deadline"} {
			t.Run(stage+"-"+fault, func(t *testing.T) {
				f, s, body, target, w := workspaceSyncRecyclingNativeWritten(t)
				d := body.intent
				var q *taskWorkspaceSyncLocalSettlement
				var err error
				if stage == "gc" {
					q, err = s.settleLocal(f.ctx, w)
					require.NoError(t, err)
				}
				ctx := f.ctx
				switch fault {
				case "slot-generation":
					point, e := f.raw.Get(f.ctx, d.slotKey)
					require.NoError(t, e)
					_, err = f.raw.Put(f.ctx, d.slotKey, string(point.Kvs[0].Value))
				case "intent-immutable":
					_, err = f.raw.Put(f.ctx, d.intentKey, d.value)
				case "restore":
					deferRestore(t, f)
					_, err = f.raw.Put(f.ctx, f.b.restoreKey, "different-epoch")
				case "claim-lost":
					_, err = f.raw.Revoke(f.ctx, clientv3.LeaseID(f.c.reference.LeaseID))
				case "root":
					err = os.Chmod(target.pin.Root, 0750)
				case "deadline":
					bounded, cancel := context.WithTimeout(f.ctx, time.Nanosecond)
					defer cancel()
					<-bounded.Done()
					ctx = bounded
				}
				require.NoError(t, err)
				if stage == "terminal" {
					q, err = s.settleLocal(ctx, w)
				} else {
					_, err = s.recycleLocal(ctx, q)
				}
				require.Error(t, err)
				if q != nil {
					require.False(t, q.completed)
				}
				point, err := f.raw.Get(f.ctx, d.slotKey)
				require.NoError(t, err)
				require.Len(t, point.Kvs, 1)
				slot, err := decodeWorkspaceSyncRecyclingSlot(point.Kvs[0])
				require.NoError(t, err)
				want := "occupied"
				if stage == "gc" {
					want = "terminal"
				}
				require.Equal(t, want, slot.State, "failed guard conservatively retains charged state")
				intent, err := f.raw.Get(f.ctx, d.intentKey)
				require.NoError(t, err)
				require.Len(t, intent.Kvs, 1)
				actual, err := os.ReadFile(filepath.Join(target.pin.Root, body.entry.path))
				require.NoError(t, err)
				require.Equal(t, "hello", string(actual))
			})
		}
	}
}

func TestTaskWorkspaceSyncRecyclingNativeCompletionClose(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit owned Linux native fixture required")
	}
	for _, stage := range []string{"terminal", "gc"} {
		for _, point := range []string{"rpc", "postguard"} {
			for _, owner := range []string{"source", "body", "target"} {
				t.Run(stage+"-"+point+"-"+owner, func(t *testing.T) {
					f, s, body, target, w := workspaceSyncRecyclingNativeWritten(t)
					var q *taskWorkspaceSyncLocalSettlement
					var err error
					match := recyclingTerminalMatch
					if stage == "gc" {
						q, err = s.settleLocal(f.ctx, w)
						require.NoError(t, err)
						match = recyclingGCMatch
					}
					entered, release := make(chan struct{}), make(chan struct{})
					var free atomic.Bool
					pause := func() {
						if point == "postguard" {
							available := s.archive.owner.nativeMu.TryLock()
							free.Store(available)
							if available {
								s.archive.owner.nativeMu.Unlock()
							}
						}
						close(entered)
						<-release
					}
					fault := &faultKV{KV: f.b.client.KV, match: match}
					if point == "rpc" {
						fault.before = pause
					} else {
						fault.after = func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
							body.intent.localSettlement.probeComplete = pause
							return r, err
						}
					}
					workspaceSyncIntentSetKV(s, fault)
					done := make(chan error, 1)
					go func() {
						if stage == "terminal" {
							_, err := s.settleLocal(f.ctx, w)
							done <- err
						} else {
							_, err := s.recycleLocal(f.ctx, q)
							done <- err
						}
					}()
					select {
					case <-entered:
					case <-time.After(5 * time.Second):
						close(release)
						t.Fatal("original completion did not reach controlled boundary")
					}
					closed := make(chan error, 1)
					go func() {
						switch owner {
						case "source":
							closed <- s.Close()
						case "body":
							closed <- body.Close()
						case "target":
							closed <- target.Close()
						}
					}()
					early := false
					select {
					case err := <-closed:
						early = true
						t.Errorf("%s.Close returned before original %s %s joined: %v", owner, stage, point, err)
					case <-time.After(30 * time.Millisecond):
					}
					close(release)
					require.Error(t, <-done, "Close cancels pending completion authority")
					if !early {
						require.NoError(t, <-closed)
					}
					if point == "postguard" {
						require.True(t, free.Load(), "nativeMu free at suspension after actual physical check")
					}
					if stage == "gc" {
						require.False(t, q.completed)
					} else {
						require.False(t, body.intent.localSettlement.known)
					}
					t.Logf("original %s.Close actually joined %s at %s; post-check hook does not prove lock freedom throughout syscall", owner, stage, point)
				})
			}
		}
	}
}

func TestTaskWorkspaceSyncRecyclingNativeGCReusedUnknown(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit owned Linux native fixture required")
	}
	f1, s1, body1, _, w := workspaceSyncRecyclingNativeWritten(t)
	q, err := s1.settleLocal(f1.ctx, w)
	require.NoError(t, err)
	var writes atomic.Int32
	workspaceSyncIntentSetKV(s1, &faultKV{KV: f1.b.client.KV, match: recyclingGCMatch, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		writes.Add(1)
		require.NoError(t, err)
		return nil, context.DeadlineExceeded
	}})
	_, err = s1.recycleLocal(f1.ctx, q)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	var workspace WorkspaceIdentity
	for i := 0; i < 10000; i++ {
		candidate := workspaceSyncIdentity(t, "local", "test-storage", "bucket", fmt.Sprintf("gc-reuse/%d/", i))
		if candidate.partition == s1.plan.workspace.partition {
			workspace = candidate
			break
		}
	}
	require.NotEmpty(t, workspace.hash)
	plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspace})
	require.NoError(t, err)
	budget := f1.b.workspaceSyncBudget.options
	f2, s2, body2, target2 := workspaceSyncRecyclingNativeBody(t, plan, &budget, workspaceSyncIntentSecondSetup(t, f1, workspace))
	require.NotSame(t, f1.b, f2.b)
	require.NotSame(t, f1.b.client, f2.b.client)
	require.NoError(t, s2.bindLocalPut(f2.ctx, body2, target2))
	_, err = s2.reservePutIntent(f2.ctx, body2)
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		outcome, err := s1.recycleLocal(f1.ctx, q)
		require.ErrorIs(t, err, ErrOutcomeUnknown, "already reused free cannot confirm old GC")
		require.Equal(t, OutcomeUnknown, outcome)
	}
	require.EqualValues(t, 1, writes.Load())
	require.False(t, q.completed)
	require.Same(t, body1, s1.body)
	point, err := f2.raw.Get(f2.ctx, body2.intent.slotKey)
	require.NoError(t, err)
	require.Len(t, point.Kvs, 1)
	require.Equal(t, body2.intent.revision, point.Kvs[0].ModRevision)
	require.Equal(t, q.terminal.create, point.Kvs[0].CreateRevision)
	require.Equal(t, q.terminal.version+2, point.Kvs[0].Version)
	t.Log("original old unknown GC remains readonly after independent Backend/client reserves next native generation; same physical worker only")
}

func TestTaskWorkspaceSyncRecyclingNativeAdmission(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit owned Linux native fixture required")
	}
	f, s, body, target := workspaceSyncRecyclingNativeDefault(t)
	all, err := f.raw.Get(f.ctx, f.b.namespace.Root(), clientv3.WithPrefix())
	require.NoError(t, err)
	freeCount := 0
	for _, kv := range all.Kvs {
		if _, err := decodeWorkspaceSyncRecyclingSlot(kv); err == nil {
			freeCount++
		}
	}
	require.Equal(t, 256, freeCount, "operator provisioned all permanent slots before traffic")
	require.NoError(t, s.bindLocalPut(f.ctx, body, target))
	workspaceSyncIntentSetKV(s, &faultKV{KV: f.b.client.KV, match: recyclingPutMatch, before: func() {
		d := body.intent
		cmps := s.recyclingReserveComparisons(d)
		keys := []string{f.b.identityKey, f.b.restoreKey, f.b.workspaceSyncBudget.key, d.intentKey, d.slotKey}
		request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: []*pb.RequestOp{taskProtoPut(d.intentKey, d.value, 0), taskProtoPut(d.slotKey, d.slotValue, 0)}, Failure: taskProtoPoints(keys)}
		values := []string{f.b.identityValue, f.b.restoreEpoch, f.b.workspaceSyncBudget.value, strings.Repeat("x", int(f.b.workspaceSyncBudget.options.MaxIntentBytes)), strings.Repeat("x", 512)}
		require.Len(t, cmps, 57)
		require.Equal(t, 64, len(cmps)+len(request.Success)+len(request.Failure))
		require.NoError(t, taskCheckWire(f.b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{taskProtoResponse(keys, values)}))
		t.Logf("original native reserve pins=50 nodes=64 request=%d successReply=%d failureReply=%d", request.Size(), taskProtoPutResponse(2).Size(), taskProtoResponse(keys, values).Size())
	}})
	result, err := s.reservePutIntent(f.ctx, body)
	require.NoError(t, err, "original bound unread local body reserves the recycling profile")
	require.Equal(t, OutcomeCommitted, result.Outcome)
	require.True(t, body.intent.acknowledged)
	entry, err := f.b.LoadTaskWorkspaceSyncIntent(f.ctx, result.Reference)
	require.NoError(t, err)
	require.Equal(t, uint32(2), entry.Record.Version)
	witness, err := s.putLocal(f.ctx, body, target)
	require.NoError(t, err)
	require.True(t, witness.original())
	actual, err := os.ReadFile(filepath.Join(target.pin.Root, body.entry.path))
	require.NoError(t, err)
	require.Equal(t, "hello", string(actual))
	_, err = s.putLocal(f.ctx, body, target)
	require.Error(t, err, "one-shot body cannot write twice")
	occupied, err := f.raw.Get(f.ctx, body.intent.slotKey)
	require.NoError(t, err)
	require.Len(t, occupied.Kvs, 1)
	slot, err := decodeWorkspaceSyncRecyclingSlot(occupied.Kvs[0])
	require.NoError(t, err)
	require.Equal(t, "occupied", slot.State, "Task2 keeps successful IO charged until Task3")
	require.Equal(t, result.Reference.Revision, occupied.Kvs[0].ModRevision)
	require.EqualValues(t, 2, occupied.Kvs[0].Version)
	t.Logf("original profile2 IO bytes=%d hash=%s slotCreate=%d ACKmod=%d nativeVersion=%d originalPins=%d", witness.bytes, witness.sha256, occupied.Kvs[0].CreateRevision, occupied.Kvs[0].ModRevision, occupied.Kvs[0].Version, len(s.archive.owner.comparisons))
}

func recyclingPutMatch(ops []clientv3.Op) bool {
	return len(ops) == 2 && ops[0].IsPut() && strings.Contains(string(ops[0].KeyBytes()), "/workspace-sync-recycling-intents/")
}

func TestTaskWorkspaceSyncRecyclingNativeCloseRPC(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit owned Linux native fixture required")
	}
	f, s, body, target := workspaceSyncRecyclingNativeDefault(t)
	require.NoError(t, s.bindLocalPut(f.ctx, body, target))
	entered, release := make(chan struct{}), make(chan struct{})
	workspaceSyncIntentSetKV(s, &faultKV{KV: f.b.client.KV, match: recyclingPutMatch, before: func() { close(entered); <-release }})
	done := make(chan error, 1)
	go func() { _, err := s.reservePutIntent(f.ctx, body); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("original reserve request did not reach controlled native transport")
	}
	closed := make(chan error, 1)
	go func() { closed <- target.Close() }()
	early := false
	select {
	case err := <-closed:
		early = true
		t.Errorf("target.Close returned before original reserve RPC joined: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	require.Error(t, <-done, "target cancellation cannot become ACK authority")
	if !early {
		require.NoError(t, <-closed)
	}
	require.False(t, body.intent.acknowledged)
	entries, err := os.ReadDir(target.pin.Root)
	require.NoError(t, err)
	require.Empty(t, entries)
	t.Log("original target.Close joined the outstanding original reserve call; no IO authority")
}

func TestTaskWorkspaceSyncRecyclingNativeCandidateFaults(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit owned Linux native fixture required")
	}
	for _, fault := range []string{"unbound", "foreign-body", "missing-free", "malformed-free", "foreign-budget", "known-occupied"} {
		t.Run(fault, func(t *testing.T) {
			f, s, body, target := workspaceSyncRecyclingNativeDefault(t)
			_, key, err := f.b.workspaceSyncRecyclingKeys(TaskWorkspaceSyncIntentReference{Task: f.c.reference.Task, BudgetID: f.b.workspaceSyncBudget.options.BudgetID, IntentID: uuid.NewString()})
			require.NoError(t, err)
			before, err := f.raw.Get(f.ctx, key)
			require.NoError(t, err)
			require.Len(t, before.Kvs, 1)
			candidate := body
			if fault != "unbound" {
				require.NoError(t, s.bindLocalPut(f.ctx, body, target))
			}
			switch fault {
			case "foreign-body":
				candidate = &taskWorkspaceSyncBody{source: s, entry: body.entry, ctx: body.ctx}
				candidate.self = candidate
			case "missing-free":
				_, err = f.raw.Delete(f.ctx, key) // operator-only negative fixture
			case "malformed-free":
				_, err = f.raw.Put(f.ctx, key, string(before.Kvs[0].Value)+" ")
			case "foreign-budget", "known-occupied":
				slot, e := decodeWorkspaceSyncRecyclingSlot(before.Kvs[0])
				require.NoError(t, e)
				if fault == "foreign-budget" {
					slot.BudgetID = uuid.NewString()
				} else {
					slot.State, slot.IntentID, slot.IntentDigest = "occupied", uuid.NewString(), strings.Repeat("a", 64)
				}
				value, e := encodeWorkspaceSyncRecyclingSlot(slot)
				require.NoError(t, e)
				_, err = f.raw.Put(f.ctx, key, value)
			}
			require.NoError(t, err)
			var writes atomic.Int32
			workspaceSyncIntentSetKV(s, &faultKV{KV: f.b.client.KV, match: recyclingPutMatch, before: func() { writes.Add(1) }})
			_, err = s.reservePutIntent(f.ctx, candidate)
			require.Error(t, err)
			if fault == "known-occupied" {
				require.ErrorIs(t, err, errTaskWorkspaceSyncIntentQuota)
			}
			require.Zero(t, writes.Load(), "bad diagnostics never emit reserve mutations")
			entries, err := os.ReadDir(target.pin.Root)
			require.NoError(t, err)
			require.Empty(t, entries)
			if fault == "missing-free" {
				after, err := f.raw.Get(f.ctx, key)
				require.NoError(t, err)
				require.Empty(t, after.Kvs, "consumer never repairs an operator's missing permanent slot")
			}
		})
	}
}

func TestTaskWorkspaceSyncRecyclingNativeUnknown(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit owned Linux native fixture required")
	}
	for _, fault := range []string{"lost-ack", "malformed-ack", "physical-postguard"} {
		t.Run(fault, func(t *testing.T) {
			f, s, body, target := workspaceSyncRecyclingNativeDefault(t)
			require.NoError(t, s.bindLocalPut(f.ctx, body, target))
			var writes atomic.Int32
			workspaceSyncIntentSetKV(s, &faultKV{KV: f.b.client.KV, match: recyclingPutMatch, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				writes.Add(1)
				require.NoError(t, err)
				require.True(t, r.Succeeded)
				switch fault {
				case "lost-ack":
					return nil, context.DeadlineExceeded
				case "malformed-ack":
					r.Responses = r.Responses[:1]
				case "physical-postguard":
					require.NoError(t, os.Chmod(target.pin.Root, 0750))
				}
				return r, nil
			}})
			unknown, err := s.reservePutIntent(f.ctx, body)
			require.Error(t, err)
			require.False(t, body.intent.acknowledged)
			if fault != "physical-postguard" {
				require.ErrorIs(t, err, ErrOutcomeUnknown)
				require.Equal(t, OutcomeUnknown, unknown.Outcome)
			}
			for i := 0; i < 2; i++ {
				read, err := s.reservePutIntent(f.ctx, body)
				require.NoError(t, err, "matching original fixed candidate is copied diagnostic metadata")
				require.Equal(t, OutcomeCommitted, read.Outcome)
				require.Equal(t, unknown.Reference.IntentID, read.Reference.IntentID)
				require.Equal(t, unknown.Reference.Slot, read.Reference.Slot)
				require.False(t, body.intent.acknowledged, "read arbitration cannot upgrade original authority")
				entry, err := f.b.LoadTaskWorkspaceSyncIntent(f.ctx, read.Reference)
				require.NoError(t, err)
				entry.Record.Local.Root = "/copied/diagnostic"
				again, err := f.b.LoadTaskWorkspaceSyncIntent(f.ctx, read.Reference)
				require.NoError(t, err)
				require.Equal(t, target.pin.Root, again.Record.Local.Root)
				cold := freshDispatchBackend(t, f.b, f.raw)
				_, err = cold.LoadTaskWorkspaceSyncIntent(f.ctx, read.Reference)
				require.Error(t, err, "nil budget remains a legacy lookup; cannot infer profile2")
			}
			require.EqualValues(t, 1, writes.Load())
			_, err = s.putLocal(f.ctx, body, target)
			require.Error(t, err)
			entries, err := os.ReadDir(target.pin.Root)
			require.NoError(t, err)
			require.Empty(t, entries)
			charged, err := f.raw.Get(f.ctx, body.intent.slotKey)
			require.NoError(t, err)
			require.Len(t, charged.Kvs, 1)
			slot, err := decodeWorkspaceSyncRecyclingSlot(charged.Kvs[0])
			require.NoError(t, err)
			require.Equal(t, "occupied", slot.State)
		})
	}
}

func TestTaskWorkspaceSyncRecyclingNativeGenerationABA(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit owned Linux native fixture required")
	}
	f, s, body, target := workspaceSyncRecyclingNativeDefault(t)
	require.NoError(t, s.bindLocalPut(f.ctx, body, target))
	var captured clientv3.Txn
	workspaceSyncIntentSetKV(s, &faultKV{KV: f.b.client.KV, match: recyclingPutMatch, before: func() {
		d := body.intent
		captured = f.raw.Txn(f.ctx).If(s.recyclingReserveComparisons(d)...).Then(clientv3.OpPut(d.intentKey, d.value), clientv3.OpPut(d.slotKey, d.slotValue))
		old, err := f.raw.Get(f.ctx, d.slotKey)
		require.NoError(t, err)
		require.Len(t, old.Kvs, 1)
		require.Equal(t, d.recycling.mod, old.Kvs[0].ModRevision)
		_, err = f.raw.Put(f.ctx, d.slotKey, string(old.Kvs[0].Value)) // operator-only same-value free generation advance
		require.NoError(t, err)
	}, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		require.NoError(t, err)
		require.False(t, r.Succeeded, "actual original native CAS rejects same free bytes with a new ModRevision")
		return r, err
	}})
	_, err := s.reservePutIntent(f.ctx, body)
	require.ErrorIs(t, err, errTaskWorkspaceSyncIntentQuota)
	require.NotNil(t, captured)
	replayed, err := captured.Commit()
	require.NoError(t, err)
	require.False(t, replayed.Succeeded, "captured old native transaction cannot reserve the operator's next generation")
	free, err := f.raw.Get(f.ctx, body.intent.slotKey)
	require.NoError(t, err)
	require.Len(t, free.Kvs, 1)
	require.Equal(t, body.intent.recycling.value, string(free.Kvs[0].Value))
	require.Greater(t, free.Kvs[0].ModRevision, body.intent.recycling.mod)
	require.Equal(t, body.intent.recycling.version+1, free.Kvs[0].Version)
	intent, err := f.raw.Get(f.ctx, body.intent.intentKey)
	require.NoError(t, err)
	require.Empty(t, intent.Kvs)
	t.Logf("operator-only free ABA oldMod=%d newMod=%d oldVersion=%d newVersion=%d; actual original CAS and captured replay both failed", body.intent.recycling.mod, free.Kvs[0].ModRevision, body.intent.recycling.version, free.Kvs[0].Version)
}

func TestTaskWorkspaceSyncRecyclingNativeCompetition(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit owned Linux native fixture required")
	}
	f1, s1, body1, target1 := workspaceSyncRecyclingNativeDefault(t)
	var workspace WorkspaceIdentity
	for i := 0; i < 10000; i++ {
		candidate, err := NewWorkspaceIdentity("local", "test-storage", "bucket", fmt.Sprintf("other/workspace-%d/", i))
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
	f2, s2, body2, target2 := workspaceSyncRecyclingNativeBody(t, plan, &budget, workspaceSyncIntentSecondSetup(t, f1, workspace))
	require.NotSame(t, f1.b, f2.b)
	require.NotSame(t, f1.b.client, f2.b.client)
	require.NotEqual(t, f1.c.reference.Task, f2.c.reference.Task)
	require.NotEqual(t, s1.plan.workspace.hash, s2.plan.workspace.hash)
	require.Equal(t, f1.b.namespace, f2.b.namespace)
	require.Equal(t, s1.plan.workspace.partition, s2.plan.workspace.partition)
	require.Equal(t, f1.b.workspaceSyncBudget.create, f2.b.workspaceSyncBudget.create)
	require.Same(t, target1.worker, target2.worker, "this proves native client CAS in one physical worker process")
	require.NoError(t, s1.bindLocalPut(f1.ctx, body1, target1))
	require.NoError(t, s2.bindLocalPut(f2.ctx, body2, target2))
	start := make(chan struct{})
	type answer struct {
		result taskWorkspaceSyncIntentResult
		err    error
		body   *taskWorkspaceSyncBody
	}
	done := make(chan answer, 2)
	for _, pair := range []struct {
		s    *PreparedTaskWorkspaceSyncSource
		body *taskWorkspaceSyncBody
	}{{s1, body1}, {s2, body2}} {
		go func(s *PreparedTaskWorkspaceSyncSource, body *taskWorkspaceSyncBody) {
			<-start
			r, err := s.reservePutIntent(context.Background(), body)
			done <- answer{r, err, body}
		}(pair.s, pair.body)
	}
	close(start)
	a, b := <-done, <-done
	if a.err != nil {
		a, b = b, a
	}
	require.NoError(t, a.err)
	require.True(t, a.body.intent.acknowledged)
	require.ErrorIs(t, b.err, errTaskWorkspaceSyncIntentQuota)
	require.False(t, b.body.intent.acknowledged)
	all, err := f1.raw.Get(f1.ctx, fmt.Sprintf("%sp/%02x/workspace-sync-recycling-", f1.b.namespace.Root(), workspace.partition), clientv3.WithPrefix())
	require.NoError(t, err)
	require.Len(t, all.Kvs, 2, "one permanent slot and one charged intent in the shared S1 partition")
	t.Logf("distinct Backend/client/claim/workspace same namespace+partition+BudgetID; one original ACK=%d, other rejected; same worker process only", a.result.Reference.Revision)
}

func TestTaskWorkspaceSyncRecyclingNativeGateFaults(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit owned Linux native fixture required")
	}
	for _, fault := range []string{"slot-same-value-generation", "slot-birth", "intent-immutable", "restore", "whole-prefix", "claim-lost", "root-after-ack", "caller-canceled"} {
		t.Run(fault, func(t *testing.T) {
			f, s, body, target := workspaceSyncRecyclingNativeDefault(t)
			require.NoError(t, s.bindLocalPut(f.ctx, body, target))
			_, err := s.reservePutIntent(f.ctx, body)
			require.NoError(t, err)
			d := body.intent
			putCtx := f.ctx
			switch fault {
			case "slot-same-value-generation":
				_, err = f.raw.Put(f.ctx, d.slotKey, d.slotValue)
			case "slot-birth":
				_, err = f.raw.Delete(f.ctx, d.slotKey) // operator-only ABA negative fixture
				require.NoError(t, err)
				_, err = f.raw.Put(f.ctx, d.slotKey, d.slotValue)
			case "intent-immutable":
				_, err = f.raw.Put(f.ctx, d.intentKey, d.value)
			case "restore":
				deferRestore(t, f)
				_, err = f.raw.Put(f.ctx, f.b.restoreKey, "different-epoch")
			case "whole-prefix":
				_, err = f.raw.Put(f.ctx, s.archive.owner.prefix+"zz/future", "present")
			case "claim-lost":
				_, err = f.raw.Revoke(f.ctx, clientv3.LeaseID(f.c.reference.LeaseID))
			case "root-after-ack":
				err = os.Chmod(target.pin.Root, 0750)
			case "caller-canceled":
				ctx, cancel := context.WithCancel(f.ctx)
				cancel()
				putCtx = ctx
			}
			require.NoError(t, err)
			_, err = s.putLocal(putCtx, body, target)
			require.Error(t, err)
			entries, err := os.ReadDir(target.pin.Root)
			require.NoError(t, err)
			require.Empty(t, entries, "no physical local file dispatch after failed original gate")
			slot, err := f.raw.Get(f.ctx, d.slotKey)
			require.NoError(t, err)
			require.Len(t, slot.Kvs, 1, "failure never frees or deletes the charged slot")
			intent, err := f.raw.Get(f.ctx, d.intentKey)
			require.NoError(t, err)
			require.Len(t, intent.Kvs, 1, "failure retains immutable charge")
		})
	}
}

func TestTaskWorkspaceSyncRecyclingNativePostguardClose(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit owned Linux native fixture required")
	}
	f, s, body, target := workspaceSyncRecyclingNativeDefault(t)
	require.NoError(t, s.bindLocalPut(f.ctx, body, target))
	_, err := s.reservePutIntent(f.ctx, body)
	require.NoError(t, err)
	p, err := s.prepareLocalPut(f.ctx, body, target)
	require.NoError(t, err)
	require.Equal(t, 59, p.nodes)
	t.Logf("profile2 native gate nodes=%d request=%d actualResponse=%d successReply=%d failureReply=%d", p.nodes, p.requestBytes, p.responseBytes, p.successBytes, p.failureBytes)
	observed, resume := make(chan struct{}), make(chan struct{})
	p.mu.Lock()
	p.probeComplete = func() { close(observed); <-resume }
	p.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- p.guard() }()
	select {
	case <-observed:
	case <-time.After(5 * time.Second):
		close(resume)
		t.Fatal("actual physical postguard was not observed")
	}
	nativeFree := s.archive.owner.nativeMu.TryLock()
	if nativeFree {
		s.archive.owner.nativeMu.Unlock()
	}
	joined := make(chan error, 3)
	go func() { joined <- p.Close() }()
	go func() { joined <- target.Close() }()
	go func() { joined <- s.Close() }()
	early := 0
	select {
	case err := <-joined:
		early++
		t.Errorf("Close returned while original physical postguard pending: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(resume)
	require.NoError(t, <-done)
	for i := early; i < 3; i++ {
		require.NoError(t, <-joined)
	}
	require.True(t, nativeFree, "nativeMu must be free throughout the actual physical postguard")
	t.Log("original owner/source/target Close waited actual physical postguard; nativeMu free")
}

// Controlled transport loss retains the exact original native Txn. It models
// an escaped request; it is not a replacement etcd implementation or partition.
type recyclingLateKV struct {
	clientv3.KV
	ctx    context.Context
	gate   chan struct{}
	done   chan error
	joined chan struct{}
	writes atomic.Int32
	match  func([]clientv3.Op) bool
}

func (kv *recyclingLateKV) Txn(ctx context.Context) clientv3.Txn {
	return &recyclingLateTxn{Txn: kv.KV.Txn(ctx), owner: kv}
}

type recyclingLateTxn struct {
	clientv3.Txn
	owner *recyclingLateKV
	cmps  []clientv3.Cmp
	late  bool
}

func (txn *recyclingLateTxn) If(cmps ...clientv3.Cmp) clientv3.Txn {
	txn.cmps = cmps
	txn.Txn = txn.Txn.If(cmps...)
	return txn
}

func (txn *recyclingLateTxn) Then(ops ...clientv3.Op) clientv3.Txn {
	txn.late = recyclingPutMatch(ops)
	if txn.owner.match != nil {
		txn.late = txn.owner.match(ops)
	}
	if txn.late {
		txn.Txn = txn.owner.KV.Txn(txn.owner.ctx).If(txn.cmps...).Then(ops...)
	} else {
		txn.Txn = txn.Txn.Then(ops...)
	}
	return txn
}

func (txn *recyclingLateTxn) Else(ops ...clientv3.Op) clientv3.Txn {
	txn.Txn = txn.Txn.Else(ops...)
	return txn
}

func (txn *recyclingLateTxn) Commit() (*clientv3.TxnResponse, error) {
	if !txn.late {
		return txn.Txn.Commit()
	}
	txn.owner.writes.Add(1)
	go func() {
		defer close(txn.owner.joined)
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

func TestTaskWorkspaceSyncRecyclingNativeUnknownAbsent(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit owned Linux native fixture required")
	}
	plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspaceSyncIdentity(t, "local", "test-storage", "bucket", "team/project/")})
	require.NoError(t, err)
	f, s, body, target := workspaceSyncRecyclingNativeBody(t, plan, &TaskWorkspaceSyncBudgetOptions{uuid.NewString(), 2, 4096}, nil)
	require.NoError(t, s.bindLocalPut(f.ctx, body, target))
	transportCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	late := &recyclingLateKV{KV: f.b.client.KV, ctx: transportCtx, gate: make(chan struct{}), done: make(chan error, 1), joined: make(chan struct{})}
	t.Cleanup(func() {
		cancel()
		if late.writes.Load() != 0 {
			select {
			case <-late.joined:
			case <-time.After(5 * time.Second):
				t.Error("actual original escaped native transaction did not join")
			}
		}
	})
	workspaceSyncIntentSetKV(s, late)
	unknown, err := s.reservePutIntent(f.ctx, body)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	d := body.intent
	for i := 0; i < 2; i++ {
		read, err := s.reservePutIntent(f.ctx, body)
		require.ErrorIs(t, err, ErrOutcomeUnknown)
		require.Equal(t, unknown, read, "absence never changes the escaped request's fixed candidate")
	}
	require.EqualValues(t, 1, late.writes.Load())
	otherRef := d.record.Reference
	otherRef.Slot = (otherRef.Slot + 1) % 2
	_, otherKey, err := f.b.workspaceSyncRecyclingKeys(otherRef)
	require.NoError(t, err)
	other, err := f.raw.Get(f.ctx, otherKey)
	require.NoError(t, err)
	require.Len(t, other.Kvs, 1)
	otherSlot, err := decodeWorkspaceSyncRecyclingSlot(other.Kvs[0])
	require.NoError(t, err)
	require.Equal(t, "free", otherSlot.State, "available alternate slot cannot cause an unknown original request to retarget")
	require.EqualValues(t, 1, other.Kvs[0].Version)
	close(late.gate)
	require.NoError(t, <-late.done, "only the actual originally escaped native transaction eventually commits")
	<-late.joined
	read, err := s.reservePutIntent(f.ctx, body)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, read.Outcome)
	require.False(t, d.acknowledged)
	require.EqualValues(t, 1, late.writes.Load())
	_, err = s.putLocal(f.ctx, body, target)
	require.Error(t, err)
	entries, err := os.ReadDir(target.pin.Root)
	require.NoError(t, err)
	require.Empty(t, entries)
	t.Log("S2 fixed unknown candidate remained readonly while another free generation was available; one actual original late Txn committed without IO authority")
}
