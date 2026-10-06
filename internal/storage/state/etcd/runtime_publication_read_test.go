package etcd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func publicationTestRecords(t *testing.T, b *Backend) (WorkspaceIdentity, RuntimePublicationRecord, RuntimePublicationProofRecord, []string, []string) {
	t.Helper()
	w, d, _, _, _, _ := dispatchTestRecords(t, b)
	r := publicationRecordFixture()
	r.ExpiresAt = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	r.WorkspaceHash = w.Hash()
	r.RestoreEpoch = b.restoreEpoch
	r.Attempt = d.Attempt
	r.Attempt.StageID = "runtime_publish"
	p := publicationProofFixture(r)
	root, _ := b.namespace.intentKey(w.Partition(), r.IntentID)
	rk, err := b.runtimeDispatchReceiptKey(r.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{root + "/runtime-publication", root + "/publication-proof", rk}
	s := stageReceipt{Version: 1, StageReference: r.Attempt.reference(domainHash), Outcome: OutcomeCommitted}
	return w, r, p, keys, []string{dispatchTestWire(t, r), dispatchTestWire(t, p), dispatchTestWire(t, s)}
}
func seedPublication(t *testing.T, raw *clientv3.Client, keys, values []string) {
	t.Helper()
	ops := make([]clientv3.Op, len(keys))
	for i, key := range keys {
		ops[i] = clientv3.OpPut(key, values[i])
	}
	if _, err := raw.Txn(context.Background()).Then(ops...).Commit(); err != nil {
		t.Fatal(err)
	}
}
func TestRuntimePublicationHistoricalRecovery(t *testing.T) {
	b, raw := integrationBackend(t)
	w, r, _, keys, values := publicationTestRecords(t, b)
	seedPublication(t, raw, keys, values)
	jk, pk, err := b.namespace.runtimePublicationKeys(w.Partition(), "intent")
	if err != nil || jk != keys[0] || pk != keys[1] {
		t.Fatalf("publication keys: %s %s %v", jk, pk, err)
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
	// No dispatch, current runtime index, configured trust/clock, or live claim is required.
	entry, err := fresh.LoadRuntimePublication(context.Background(), w, "intent")
	if err != nil || entry == nil || entry.Record != r || string(entry.Proof) != "{}" || entry.Reference != r.Attempt.reference(domainHash) {
		t.Fatalf("historical recovery: %+v %v", entry, err)
	}
	entry.Proof[0] = 'x'
	again, err := fresh.LoadRuntimePublication(context.Background(), w, "intent")
	if err != nil || again == nil || string(again.Proof) != "{}" {
		t.Fatalf("proof copy: %+v %v", again, err)
	}
	outcome, err := fresh.ResolveStage(context.Background(), entry.Reference)
	if err != nil || outcome != OutcomeCommitted {
		t.Fatalf("recovered ref: %v %v", outcome, err)
	}
	// A different boot with the same UID has a distinct historical record under the same fixed key.
	r.Runtime.BootID = "other-boot"
	values[0] = dispatchTestWire(t, r)
	ops := []clientv3.Op{}
	for _, k := range keys {
		ops = append(ops, clientv3.OpDelete(k))
	}
	if _, err := raw.Txn(context.Background()).Then(ops...).Commit(); err != nil {
		t.Fatal(err)
	}
	seedPublication(t, raw, keys, values)
	entry, err = fresh.LoadRuntimePublication(context.Background(), w, "intent")
	if err != nil || entry == nil || entry.Record.Runtime.BootID != "other-boot" {
		t.Fatalf("boot recovered incorrectly: %+v %v", entry, err)
	}
}
func TestRuntimePublicationSameRevisionCorruption(t *testing.T) {
	cases := map[string]func(*RuntimePublicationRecord, *RuntimePublicationProofRecord, *stageReceipt, []string){
		"journal workspace": func(r *RuntimePublicationRecord, p *RuntimePublicationProofRecord, s *stageReceipt, v []string) {
			r.WorkspaceHash = domainHash
			r.Attempt.Partition = 0xaa
			v[0] = dispatchTestWire(t, r)
		},
		"journal namespace": func(r *RuntimePublicationRecord, p *RuntimePublicationProofRecord, s *stageReceipt, v []string) {
			r.Attempt.Namespace = "/other/scope/cell/"
			v[0] = dispatchTestWire(t, r)
		},
		"stored restore": func(r *RuntimePublicationRecord, p *RuntimePublicationProofRecord, s *stageReceipt, v []string) {
			r.RestoreEpoch = "old"
			r.Attempt.RestoreEpoch = "old"
			v[0] = dispatchTestWire(t, r)
		},
		"proof restore": func(r *RuntimePublicationRecord, p *RuntimePublicationProofRecord, s *stageReceipt, v []string) {
			p.RestoreEpoch = "old"
			v[1] = dispatchTestWire(t, p)
		},
		"proof intent": func(r *RuntimePublicationRecord, p *RuntimePublicationProofRecord, s *stageReceipt, v []string) {
			p.IntentID = "other"
			v[1] = dispatchTestWire(t, p)
		},
		"proof sandbox": func(r *RuntimePublicationRecord, p *RuntimePublicationProofRecord, s *stageReceipt, v []string) {
			p.SandboxID = "other"
			v[1] = dispatchTestWire(t, p)
		},
		"proof workspace": func(r *RuntimePublicationRecord, p *RuntimePublicationProofRecord, s *stageReceipt, v []string) {
			p.WorkspaceHash = domainHash
			v[1] = dispatchTestWire(t, p)
		},
		"proof generation": func(r *RuntimePublicationRecord, p *RuntimePublicationProofRecord, s *stageReceipt, v []string) {
			p.Generation++
			v[1] = dispatchTestWire(t, p)
		},
		"proof digest": func(r *RuntimePublicationRecord, p *RuntimePublicationProofRecord, s *stageReceipt, v []string) {
			p.ProofDigest = domainHash
			v[1] = dispatchTestWire(t, p)
		},
		"receipt aborted": func(r *RuntimePublicationRecord, p *RuntimePublicationProofRecord, s *stageReceipt, v []string) {
			s.Outcome = OutcomeAborted
			v[2] = dispatchTestWire(t, s)
		},
		"receipt schema": func(r *RuntimePublicationRecord, p *RuntimePublicationProofRecord, s *stageReceipt, v []string) {
			s.Version = 2
			v[2] = dispatchTestWire(t, s)
		},
		"receipt digest": func(r *RuntimePublicationRecord, p *RuntimePublicationProofRecord, s *stageReceipt, v []string) {
			s.Digest = strings.Repeat("A", 64)
			v[2] = dispatchTestWire(t, s)
		},
		"receipt request": func(r *RuntimePublicationRecord, p *RuntimePublicationProofRecord, s *stageReceipt, v []string) {
			s.RequestID = "wrong"
			v[2] = dispatchTestWire(t, s)
		},
		"receipt namespace": func(r *RuntimePublicationRecord, p *RuntimePublicationProofRecord, s *stageReceipt, v []string) {
			s.Namespace = "/other/scope/cell/"
			v[2] = dispatchTestWire(t, s)
		},
		"receipt partition": func(r *RuntimePublicationRecord, p *RuntimePublicationProofRecord, s *stageReceipt, v []string) {
			s.Partition++
			v[2] = dispatchTestWire(t, s)
		},
		"receipt stage": func(r *RuntimePublicationRecord, p *RuntimePublicationProofRecord, s *stageReceipt, v []string) {
			s.StageID = "runtime_dispatch"
			v[2] = dispatchTestWire(t, s)
		},
		"receipt restore": func(r *RuntimePublicationRecord, p *RuntimePublicationProofRecord, s *stageReceipt, v []string) {
			s.RestoreEpoch = "old"
			v[2] = dispatchTestWire(t, s)
		},
		"receipt attempt": func(r *RuntimePublicationRecord, p *RuntimePublicationProofRecord, s *stageReceipt, v []string) {
			s.AttemptID = "11234567-89ab-4cde-8012-3456789abcde"
			v[2] = dispatchTestWire(t, s)
		},
		"receipt duplicate": func(r *RuntimePublicationRecord, p *RuntimePublicationProofRecord, s *stageReceipt, v []string) {
			v[2] = strings.Replace(v[2], `"version":1`, `"version":1,"version":1`, 1)
		},
		"receipt missing": func(r *RuntimePublicationRecord, p *RuntimePublicationProofRecord, s *stageReceipt, v []string) {
			v[2] = strings.Replace(v[2], `"version":1,`, ``, 1)
		},
		"receipt null": func(r *RuntimePublicationRecord, p *RuntimePublicationProofRecord, s *stageReceipt, v []string) {
			v[2] = strings.Replace(v[2], `"version":1`, `"version":null`, 1)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b, raw := integrationBackend(t)
			w, r, p, keys, v := publicationTestRecords(t, b)
			s := stageReceipt{Version: 1, StageReference: r.Attempt.reference(domainHash), Outcome: OutcomeCommitted}
			mutate(&r, &p, &s, v)
			seedPublication(t, raw, keys, v)
			kvs, err := b.readDomain(context.Background(), keys...)
			if err != nil {
				t.Fatal(err)
			}
			for _, kv := range kvs {
				if kv.CreateRevision != kv.ModRevision || kv.CreateRevision != kvs[0].CreateRevision {
					t.Fatal("fixture must share first revision")
				}
			}
			entry, err := b.LoadRuntimePublication(context.Background(), w, "intent")
			want := ErrCorruptRecord
			if strings.HasPrefix(name, "receipt") {
				want = ErrCorruptReceipt
			}
			if strings.Contains(name, "restore") && !strings.HasPrefix(name, "receipt") {
				want = ErrIdentityMismatch
			}
			if entry != nil || !errors.Is(err, want) {
				t.Fatalf("accepted same-revision corruption: %+v %v want %v", entry, err, want)
			}
		})
	}
}
func TestRuntimePublicationEnvelopesAndMissing(t *testing.T) {
	for _, name := range []string{"empty", "half journal", "half proof", "missing receipt", "journal lease", "proof lease", "receipt lease", "journal rewrite", "proof rewrite", "receipt rewrite", "separate receipt"} {
		t.Run(name, func(t *testing.T) {
			b, raw := integrationBackend(t)
			w, _, _, keys, v := publicationTestRecords(t, b)
			ops := []clientv3.Op{clientv3.OpPut(keys[0], v[0]), clientv3.OpPut(keys[1], v[1]), clientv3.OpPut(keys[2], v[2])}
			index := 0
			if strings.HasPrefix(name, "proof") {
				index = 1
			}
			if strings.HasPrefix(name, "receipt") {
				index = 2
			}
			switch name {
			case "empty":
				ops = nil
			case "half journal":
				ops = ops[:1]
			case "half proof":
				ops = ops[1:2]
			case "missing receipt", "separate receipt":
				ops = ops[:2]
			}
			if strings.HasSuffix(name, "lease") {
				l, err := raw.Grant(context.Background(), 30)
				if err != nil {
					t.Fatal(err)
				}
				defer raw.Revoke(context.Background(), l.ID)
				ops[index] = clientv3.OpPut(keys[index], v[index], clientv3.WithLease(l.ID))
			}
			if _, err := raw.Txn(context.Background()).Then(ops...).Commit(); err != nil {
				t.Fatal(err)
			}
			if strings.HasSuffix(name, "rewrite") || name == "separate receipt" {
				if name == "separate receipt" {
					index = 2
				}
				if _, err := raw.Put(context.Background(), keys[index], v[index]); err != nil {
					t.Fatal(err)
				}
			}
			entry, err := b.LoadRuntimePublication(context.Background(), w, "intent")
			if name == "empty" {
				if err != nil || entry != nil {
					t.Fatal(entry, err)
				}
				return
			}
			want := ErrCorruptRecord
			if name == "receipt lease" || name == "receipt rewrite" {
				want = ErrCorruptReceipt
			}
			if entry != nil || !errors.Is(err, want) {
				t.Fatalf("envelope: %+v %v want %v", entry, err, want)
			}
			if name == "missing receipt" {
				resp, err := raw.Get(context.Background(), keys[2])
				if err != nil || len(resp.Kvs) != 0 {
					t.Fatal("loader wrote receipt", err)
				}
			}
		})
	}
}
func TestRuntimePublicationReadContext(t *testing.T) {
	b, raw := integrationBackend(t)
	w, _, _, keys, v := publicationTestRecords(t, b)
	seedPublication(t, raw, keys, v)
	if _, err := b.LoadRuntimePublication(nil, w, "intent"); !errors.Is(err, ErrInvalidRecord) {
		t.Fatal(err)
	}
	if _, err := b.LoadRuntimePublication(context.Background(), WorkspaceIdentity{}, "intent"); err == nil {
		t.Fatal("zero workspace accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.LoadRuntimePublication(ctx, w, "intent"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	wrong, _ := NewWorkspaceIdentity("s3", "other-storage", "bucket", "workspace/")
	if _, err := b.LoadRuntimePublication(context.Background(), wrong, "intent"); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatal(err)
	}
	if _, err := b.LoadRuntimePublication(context.Background(), w, "../bad"); !errors.Is(err, ErrInvalidRecord) {
		t.Fatal(err)
	}
	if _, _, err := b.namespace.runtimePublicationKeys(w.Partition(), "../bad"); !errors.Is(err, ErrInvalidRecord) {
		t.Fatal(err)
	}
	if _, err := raw.Put(context.Background(), b.restoreKey, "changed"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.LoadRuntimePublication(context.Background(), w, "intent"); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatal(err)
	}
}
func TestRuntimePublicationSecondSnapshot(t *testing.T) {
	b, raw := integrationBackend(t)
	w, r, _, keys, v := publicationTestRecords(t, b)
	seedPublication(t, raw, keys, v)
	original, err := b.readDomain(context.Background(), keys...)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func([]*mvccpb.KeyValue){
		"changed locator": func(v []*mvccpb.KeyValue) {
			r.Attempt.AttemptID = "11234567-89ab-4cde-8012-3456789abcde"
			v[0].Value = []byte(dispatchTestWire(t, r))
		},
		"receipt survives": func(v []*mvccpb.KeyValue) { v[0] = nil; v[1] = nil },
		"all gone":         func(v []*mvccpb.KeyValue) { v[0] = nil; v[1] = nil; v[2] = nil },
	} {
		t.Run(name, func(t *testing.T) {
			got := make([]*mvccpb.KeyValue, 3)
			for i, kv := range original {
				copy := *kv
				got[i] = &copy
			}
			located := r.Attempt
			mutate(got)
			entry, err := b.decodeRuntimePublicationEntry(got, w, "intent", keys, located)
			if name == "all gone" {
				if entry != nil || err != nil {
					t.Fatal(entry, err)
				}
				return
			}
			if entry != nil || !errors.Is(err, ErrCorruptRecord) {
				t.Fatal(entry, err)
			}
		})
	}
}
func TestRuntimePublicationPointAttribution(t *testing.T) {
	for i := 0; i < 3; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			b, raw := integrationBackend(t)
			w, _, _, keys, v := publicationTestRecords(t, b)
			seedPublication(t, raw, keys, v)
			b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool { return len(ops) == 3 && string(ops[2].KeyBytes()) == keys[2] }, after: func(resp *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				if err != nil {
					t.Fatal(err)
				}
				resp.Responses[i].GetResponseRange().Kvs[0].Key = []byte("wrong")
				return resp, nil
			}}
			entry, err := b.LoadRuntimePublication(context.Background(), w, "intent")
			want, other := ErrCorruptRecord, ErrCorruptReceipt
			if i == 2 {
				want, other = other, want
			}
			if entry != nil || !errors.Is(err, want) || errors.Is(err, other) {
				t.Fatalf("point attribution: %+v %v", entry, err)
			}
		})
	}
}

func TestRuntimePublicationBoundedReadAndGCRace(t *testing.T) {
	for _, gc := range []bool{false, true} {
		t.Run(fmt.Sprint(gc), func(t *testing.T) {
			b, raw := integrationBackend(t)
			w, _, _, keys, v := publicationTestRecords(t, b)
			seedPublication(t, raw, keys, v)
			reads := 0
			b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
				reads++
				want := keys[:2]
				if reads == 2 {
					want = keys
				}
				if reads > 2 || len(ops) != len(want) {
					t.Fatalf("unbounded historical read: %d reads, %d ops", reads, len(ops))
				}
				for i, op := range ops {
					if !op.IsGet() || len(op.RangeBytes()) != 0 || string(op.KeyBytes()) != want[i] {
						t.Fatal("expected exact historical point reads")
					}
				}
				return gc && reads == 2
			}, before: func() {
				ops := []clientv3.Op{}
				for _, key := range keys {
					ops = append(ops, clientv3.OpDelete(key))
				}
				if _, err := raw.Txn(context.Background()).Then(ops...).Commit(); err != nil {
					t.Fatal(err)
				}
			}}
			entry, err := b.LoadRuntimePublication(context.Background(), w, "intent")
			if err != nil || reads != 2 || (gc && entry != nil) || (!gc && entry == nil) {
				t.Fatalf("GC/read outcome: entry=%+v err=%v reads=%d", entry, err, reads)
			}
		})
	}
}
