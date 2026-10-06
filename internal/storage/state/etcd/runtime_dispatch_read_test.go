package etcd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func dispatchTestWire(t *testing.T, r any) string {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}
func dispatchTestRecords(t *testing.T, b *Backend) (WorkspaceIdentity, RuntimeDispatchRecord, RuntimeDispatchInputRecord, string, string, string) {
	t.Helper()
	w, err := NewWorkspaceIdentity("s3", "test-storage", "bucket", "workspace/")
	if err != nil {
		t.Fatal(err)
	}
	r := dispatchRecord()
	r.WorkspaceHash = w.Hash()
	r.RestoreEpoch = b.restoreEpoch
	r.Attempt.Namespace = b.namespace.Root()
	r.Attempt.Partition = w.Partition()
	r.Attempt.RestoreEpoch = b.restoreEpoch
	input := dispatchInput()
	input.WorkspaceHash = w.Hash()
	input.RestoreEpoch = b.restoreEpoch
	r.PayloadDigest = input.PayloadDigest
	root, _ := b.namespace.intentKey(w.Partition(), r.IntentID)
	dk, ik := root+"/runtime-dispatch", root+"/runtime-input"
	_, rk, err := b.stageKeys(r.Attempt.reference(domainHash))
	if err != nil {
		t.Fatal(err)
	}
	return w, r, input, dk, ik, rk
}
func TestRuntimeDispatchLoadRecovery(t *testing.T) {
	b, raw := integrationBackend(t)
	w, r, input, dk, ik, _ := dispatchTestRecords(t, b)
	stage, err := b.beginStageWithBuilder(context.Background(), w.Partition(), "request", "runtime_dispatch", 30*time.Second, func(l StageAttemptLocator) (Mutation, error) {
		r.Attempt = l
		return Mutation{Writes: []Write{{Key: dk, Value: []byte(dispatchTestWire(t, r))}, {Key: ik, Value: []byte(dispatchTestWire(t, input))}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := b.CommitStage(context.Background(), stage)
	if err != nil || outcome != OutcomeCommitted {
		t.Fatalf("commit %s %v", outcome, err)
	}
	identity, err := decodeIdentity(b.identityValue, b.namespace)
	if err != nil {
		t.Fatal(err)
	}
	identity.RestoreEpoch = b.restoreEpoch
	fresh, err := New(context.Background(), Options{Endpoints: raw.Endpoints(), Namespace: b.namespace, Identity: identity, AllowInsecureLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	entry, err := fresh.LoadRuntimeDispatch(context.Background(), w, "intent")
	if err != nil || entry == nil {
		t.Fatalf("durable dispatch missing: %+v %v", entry, err)
	}
	if entry.Record != r || entry.Reference != stage.Reference() || string(entry.Payload) != "{}" {
		t.Fatalf("recovered entry changed: %+v", entry)
	}
	entry.Payload[0] = 'x'
	again, err := fresh.LoadRuntimeDispatch(context.Background(), w, "intent")
	if err != nil || again == nil || string(again.Payload) != "{}" {
		t.Fatalf("caller mutated persisted payload: %+v %v", again, err)
	}
	outcome, err = fresh.ResolveStage(context.Background(), entry.Reference)
	if err != nil || outcome != OutcomeCommitted {
		t.Fatalf("recovered reference arbitration: %s %v", outcome, err)
	}
}
func TestRuntimeDispatchLoadCorruption(t *testing.T) {
	cases := map[string]func(*RuntimeDispatchRecord, *RuntimeDispatchInputRecord, *stageReceipt, []clientv3.Op, string, string, string) []clientv3.Op{
		"half declaration": func(r *RuntimeDispatchRecord, i *RuntimeDispatchInputRecord, s *stageReceipt, ops []clientv3.Op, dk, ik, rk string) []clientv3.Op {
			return ops[:1]
		},
		"half input": func(r *RuntimeDispatchRecord, i *RuntimeDispatchInputRecord, s *stageReceipt, ops []clientv3.Op, dk, ik, rk string) []clientv3.Op {
			return ops[1:2]
		},
		"missing receipt": func(r *RuntimeDispatchRecord, i *RuntimeDispatchInputRecord, s *stageReceipt, ops []clientv3.Op, dk, ik, rk string) []clientv3.Op {
			return ops[:2]
		},
		"aborted": func(r *RuntimeDispatchRecord, i *RuntimeDispatchInputRecord, s *stageReceipt, ops []clientv3.Op, dk, ik, rk string) []clientv3.Op {
			s.Outcome = OutcomeAborted
			ops[2] = clientv3.OpPut(rk, dispatchTestWire(t, s))
			return ops
		},
		"wrong receipt locator": func(r *RuntimeDispatchRecord, i *RuntimeDispatchInputRecord, s *stageReceipt, ops []clientv3.Op, dk, ik, rk string) []clientv3.Op {
			s.RequestID = "wrong"
			ops[2] = clientv3.OpPut(rk, dispatchTestWire(t, s))
			return ops
		},
		"receipt digest": func(r *RuntimeDispatchRecord, i *RuntimeDispatchInputRecord, s *stageReceipt, ops []clientv3.Op, dk, ik, rk string) []clientv3.Op {
			s.Digest = strings.Repeat("A", 64)
			ops[2] = clientv3.OpPut(rk, dispatchTestWire(t, s))
			return ops
		},
		"receipt schema": func(r *RuntimeDispatchRecord, i *RuntimeDispatchInputRecord, s *stageReceipt, ops []clientv3.Op, dk, ik, rk string) []clientv3.Op {
			s.Version = 2
			ops[2] = clientv3.OpPut(rk, dispatchTestWire(t, s))
			return ops
		},
		"receipt unknown": func(r *RuntimeDispatchRecord, i *RuntimeDispatchInputRecord, s *stageReceipt, ops []clientv3.Op, dk, ik, rk string) []clientv3.Op {
			ops[2] = clientv3.OpPut(rk, strings.TrimSuffix(dispatchTestWire(t, s), "}")+`,"extra":1}`)
			return ops
		},
		"receipt trailing": func(r *RuntimeDispatchRecord, i *RuntimeDispatchInputRecord, s *stageReceipt, ops []clientv3.Op, dk, ik, rk string) []clientv3.Op {
			ops[2] = clientv3.OpPut(rk, dispatchTestWire(t, s)+` {}`)
			return ops
		},
		"receipt UTF8": func(r *RuntimeDispatchRecord, i *RuntimeDispatchInputRecord, s *stageReceipt, ops []clientv3.Op, dk, ik, rk string) []clientv3.Op {
			ops[2] = clientv3.OpPut(rk, strings.TrimSuffix(dispatchTestWire(t, s), "}")+",\"extra\":\"\xff\"}")
			return ops
		},
		"input operation": func(r *RuntimeDispatchRecord, i *RuntimeDispatchInputRecord, s *stageReceipt, ops []clientv3.Op, dk, ik, rk string) []clientv3.Op {
			i.OperationID = "11234567-89ab-4cde-8012-3456789abcde"
			ops[1] = clientv3.OpPut(ik, dispatchTestWire(t, i))
			return ops
		},
		"input generation": func(r *RuntimeDispatchRecord, i *RuntimeDispatchInputRecord, s *stageReceipt, ops []clientv3.Op, dk, ik, rk string) []clientv3.Op {
			i.Generation = 2
			ops[1] = clientv3.OpPut(ik, dispatchTestWire(t, i))
			return ops
		},
		"input digest": func(r *RuntimeDispatchRecord, i *RuntimeDispatchInputRecord, s *stageReceipt, ops []clientv3.Op, dk, ik, rk string) []clientv3.Op {
			i.PayloadDigest = domainHash
			ops[1] = clientv3.OpPut(ik, dispatchTestWire(t, i))
			return ops
		},
		"declaration intent": func(r *RuntimeDispatchRecord, i *RuntimeDispatchInputRecord, s *stageReceipt, ops []clientv3.Op, dk, ik, rk string) []clientv3.Op {
			r.IntentID = "wrong"
			ops[0] = clientv3.OpPut(dk, dispatchTestWire(t, r))
			return ops
		},
		"declaration workspace": func(r *RuntimeDispatchRecord, i *RuntimeDispatchInputRecord, s *stageReceipt, ops []clientv3.Op, dk, ik, rk string) []clientv3.Op {
			r.WorkspaceHash = domainHash
			r.Attempt.Partition = 0xaa
			ops[0] = clientv3.OpPut(dk, dispatchTestWire(t, r))
			return ops
		},
		"declaration namespace": func(r *RuntimeDispatchRecord, i *RuntimeDispatchInputRecord, s *stageReceipt, ops []clientv3.Op, dk, ik, rk string) []clientv3.Op {
			r.Attempt.Namespace = "/other/scope/cell/"
			ops[0] = clientv3.OpPut(dk, dispatchTestWire(t, r))
			return ops
		},
		"stored restore": func(r *RuntimeDispatchRecord, i *RuntimeDispatchInputRecord, s *stageReceipt, ops []clientv3.Op, dk, ik, rk string) []clientv3.Op {
			r.RestoreEpoch = "old"
			r.Attempt.RestoreEpoch = "old"
			ops[0] = clientv3.OpPut(dk, dispatchTestWire(t, r))
			return ops
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b, raw := integrationBackend(t)
			w, r, input, dk, ik, rk := dispatchTestRecords(t, b)
			receipt := stageReceipt{Version: 1, StageReference: r.Attempt.reference(domainHash), Outcome: OutcomeCommitted}
			ops := []clientv3.Op{clientv3.OpPut(dk, dispatchTestWire(t, r)), clientv3.OpPut(ik, dispatchTestWire(t, input)), clientv3.OpPut(rk, dispatchTestWire(t, receipt))}
			ops = mutate(&r, &input, &receipt, ops, dk, ik, rk)
			if _, err := raw.Txn(context.Background()).Then(ops...).Commit(); err != nil {
				t.Fatal(err)
			}
			entry, err := b.LoadRuntimeDispatch(context.Background(), w, "intent")
			want := ErrCorruptRecord
			if strings.HasPrefix(name, "receipt") || name == "aborted" || name == "wrong receipt locator" {
				want = ErrCorruptReceipt
			}
			if name == "stored restore" {
				want = ErrIdentityMismatch
			}
			if entry != nil || !errors.Is(err, want) {
				t.Fatalf("corruption accepted: entry=%+v err=%v want=%v", entry, err, want)
			}
			if name == "missing receipt" {
				resp, err := raw.Get(context.Background(), rk)
				if err != nil || len(resp.Kvs) != 0 {
					t.Fatal("loader wrote abort receipt")
				}
			}
		})
	}
}
func TestRuntimeDispatchLoadMissing(t *testing.T) {
	b, raw := integrationBackend(t)
	w, r, _, _, _, rk := dispatchTestRecords(t, b)
	if _, err := raw.Put(context.Background(), rk, dispatchTestWire(t, stageReceipt{Version: 1, StageReference: r.Attempt.reference(domainHash), Outcome: OutcomeCommitted})); err != nil {
		t.Fatal(err)
	}
	entry, err := b.LoadRuntimeDispatch(context.Background(), w, "intent")
	if err != nil || entry != nil {
		t.Fatalf("absent declaration: %+v %v", entry, err)
	}
}

func TestRuntimeDispatchLoadEnvelopes(t *testing.T) {
	for _, name := range []string{"declaration lease", "input lease", "receipt lease", "declaration overwrite", "input overwrite", "receipt overwrite", "receipt separate revision"} {
		t.Run(name, func(t *testing.T) {
			b, raw := integrationBackend(t)
			w, r, input, dk, ik, rk := dispatchTestRecords(t, b)
			receipt := stageReceipt{Version: 1, StageReference: r.Attempt.reference(domainHash), Outcome: OutcomeCommitted}
			keys := []string{dk, ik, rk}
			values := []string{dispatchTestWire(t, r), dispatchTestWire(t, input), dispatchTestWire(t, receipt)}
			ops := []clientv3.Op{clientv3.OpPut(dk, values[0]), clientv3.OpPut(ik, values[1]), clientv3.OpPut(rk, values[2])}
			index := 0
			if strings.HasPrefix(name, "input") {
				index = 1
			}
			if strings.HasPrefix(name, "receipt") {
				index = 2
			}
			if strings.HasSuffix(name, "lease") {
				lease, err := raw.Grant(context.Background(), 30)
				if err != nil {
					t.Fatal(err)
				}
				defer raw.Revoke(context.Background(), lease.ID)
				ops[index] = clientv3.OpPut(keys[index], values[index], clientv3.WithLease(lease.ID))
			}
			if name == "receipt separate revision" {
				ops = ops[:2]
			}
			if _, err := raw.Txn(context.Background()).Then(ops...).Commit(); err != nil {
				t.Fatal(err)
			}
			if strings.HasSuffix(name, "overwrite") || name == "receipt separate revision" {
				if _, err := raw.Put(context.Background(), keys[index], values[index]); err != nil {
					t.Fatal(err)
				}
			}
			want := ErrCorruptRecord
			if name == "receipt lease" || name == "receipt overwrite" {
				want = ErrCorruptReceipt
			}
			entry, err := b.LoadRuntimeDispatch(context.Background(), w, "intent")
			if entry != nil || !errors.Is(err, want) {
				t.Fatalf("invalid envelope accepted: %+v %v want %v", entry, err, want)
			}
		})
	}
}

func TestRuntimeDispatchSecondSnapshot(t *testing.T) {
	// Directly exercise the second snapshot decoder with complete real etcd KVs;
	// invalid response metadata cannot be manufactured through a normal etcd Get.
	b, raw := integrationBackend(t)
	w, r, input, dk, ik, rk := dispatchTestRecords(t, b)
	receipt := stageReceipt{Version: 1, StageReference: r.Attempt.reference(domainHash), Outcome: OutcomeCommitted}
	if _, err := raw.Txn(context.Background()).Then(clientv3.OpPut(dk, dispatchTestWire(t, r)), clientv3.OpPut(ik, dispatchTestWire(t, input)), clientv3.OpPut(rk, dispatchTestWire(t, receipt))).Commit(); err != nil {
		t.Fatal(err)
	}
	original, err := b.readDomain(context.Background(), dk, ik, rk)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func([]*mvccpb.KeyValue){
		"declaration key":           func(v []*mvccpb.KeyValue) { v[0].Key = []byte("wrong") },
		"input key":                 func(v []*mvccpb.KeyValue) { v[1].Key = []byte("wrong") },
		"receipt key":               func(v []*mvccpb.KeyValue) { v[2].Key = []byte("wrong") },
		"zero declaration revision": func(v []*mvccpb.KeyValue) { v[0].CreateRevision = 0; v[0].ModRevision = 0 },
		"zero receipt revision":     func(v []*mvccpb.KeyValue) { v[2].CreateRevision = 0; v[2].ModRevision = 0 },
		"input revision mismatch":   func(v []*mvccpb.KeyValue) { v[1].CreateRevision++; v[1].ModRevision++ },
		"receipt survives GC":       func(v []*mvccpb.KeyValue) { v[0] = nil; v[1] = nil },
		"new locator": func(v []*mvccpb.KeyValue) {
			copy := r
			copy.Attempt.AttemptID = "11234567-89ab-4cde-8012-3456789abcde"
			v[0].Value = []byte(dispatchTestWire(t, copy))
		},
	} {
		t.Run(name, func(t *testing.T) {
			v := make([]*mvccpb.KeyValue, 3)
			for j, kv := range original {
				copy := *kv
				v[j] = &copy
			}
			mutate(v)
			want := ErrCorruptRecord
			if strings.Contains(name, "receipt") && name != "receipt survives GC" {
				want = ErrCorruptReceipt
			}
			entry, err := b.decodeRuntimeDispatchEntry(v, w, "intent", dk, ik, rk, r.Attempt)
			if entry != nil || !errors.Is(err, want) {
				t.Fatalf("bad second snapshot accepted: %+v %v want %v", entry, err, want)
			}
		})
	}
	entry, err := b.decodeRuntimeDispatchEntry([]*mvccpb.KeyValue{nil, nil, nil}, w, "intent", dk, ik, rk, r.Attempt)
	if entry != nil || err != nil {
		t.Fatalf("complete GC disappearance: %+v %v", entry, err)
	}
	if _, err := raw.Delete(context.Background(), b.restoreKey); err != nil {
		t.Fatal(err)
	}
	if _, err := b.LoadRuntimeDispatch(context.Background(), w, "intent"); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("base metadata missing accepted: %v", err)
	}
}
