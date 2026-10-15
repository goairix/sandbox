package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestControlKeyLostAckArbitratesWithoutWriteReplay(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact-record", true: "operator-drift"}[drift], func(t *testing.T) {
			b, raw := integrationBackend(t)
			ctx := context.Background()
			name := b.namespace.Root() + "meta/workspace-control-seed-v1"
			writes, reads := 0, 0
			b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
				if putsKey(name)(ops) {
					writes++
					return true
				}
				reads++
				if len(ops) != 1 || !ops[0].IsGet() || string(ops[0].KeyBytes()) != name {
					t.Fatal("arbitration is not a fixed read-only point")
				}
				return false
			}, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				if err != nil || r == nil || !r.Succeeded {
					t.Fatal("expected actual successful server CAS")
				}
				if drift {
					if _, err := raw.Put(ctx, b.restoreKey, "changed-epoch"); err != nil {
						t.Fatal("could not inject operator drift")
					}
				}
				return nil, errors.New("private transport request details suppressed")
			}}
			seed := controlKeySeed()
			defer clear(seed)
			pub, err := b.ProvisionWorkspaceControlKey(ctx, seed)
			if drift {
				if pub != nil || !errors.Is(err, ErrOutcomeUnknown) || !errors.Is(err, ErrIdentityMismatch) {
					t.Fatal("unknown commit ignored current operator drift")
				}
			} else if err != nil || len(pub) != ed25519.PublicKeySize {
				t.Fatal("lost ACK failed exact-record arbitration")
			}
			if writes != 1 || reads != 1 {
				t.Fatal("application replayed CAS or arbitration")
			}
		})
	}
}

func TestControlKeyRejectsInvalidStoredRecords(t *testing.T) {
	for _, defect := range []string{"short", "long", "lease", "version"} {
		t.Run(defect, func(t *testing.T) {
			b, raw := integrationBackend(t)
			ctx := context.Background()
			name := b.namespace.Root() + "meta/workspace-control-seed-v1"
			seed := controlKeySeed()
			defer clear(seed)
			value := bytes.Clone(seed)
			defer clear(value)
			var opts []clientv3.OpOption
			switch defect {
			case "short":
				value = value[:31]
			case "long":
				value = append(value, 1)
			case "lease":
				lease, err := raw.Grant(ctx, 30)
				if err != nil {
					t.Fatal("lease fixture failed")
				}
				defer raw.Revoke(ctx, lease.ID)
				opts = append(opts, clientv3.WithLease(lease.ID))
			}
			if _, err := raw.Put(ctx, name, string(value), opts...); err != nil {
				t.Fatal("record fixture failed")
			}
			if defect == "version" {
				if _, err := raw.Put(ctx, name, string(value)); err != nil {
					t.Fatal("version fixture failed")
				}
			}
			before, err := raw.Get(ctx, name)
			if err != nil || len(before.Kvs) != 1 {
				t.Fatal("record fixture missing")
			}
			if key, err := b.LoadWorkspaceControlKey(ctx); err == nil || key != nil {
				t.Fatal("invalid stored record loaded")
			}
			if pub, err := b.ProvisionWorkspaceControlKey(ctx, seed); err == nil || pub != nil {
				t.Fatal("invalid stored record repaired or accepted")
			}
			after, err := raw.Get(ctx, name)
			if err != nil || len(after.Kvs) != 1 || after.Kvs[0].ModRevision != before.Kvs[0].ModRevision {
				t.Fatal("refusal changed invalid record")
			}
		})
	}
}

func TestControlKeyLoadRejectsWireDefects(t *testing.T) {
	defects := map[string]func(*clientv3.TxnResponse){
		"cluster":        func(r *clientv3.TxnResponse) { r.Header.ClusterId++ },
		"revision":       func(r *clientv3.TxnResponse) { r.Header.Revision = 0 },
		"top-unknown":    func(r *clientv3.TxnResponse) { r.XXX_unrecognized = []byte{1} },
		"header-unknown": func(r *clientv3.TxnResponse) { r.Header.XXX_unrecognized = []byte{1} },
		"count":          func(r *clientv3.TxnResponse) { r.Responses[0].GetResponseRange().Count++ },
		"more":           func(r *clientv3.TxnResponse) { r.Responses[0].GetResponseRange().More = true },
		"key":            func(r *clientv3.TxnResponse) { r.Responses[0].GetResponseRange().Kvs[0].Key = []byte("/foreign/key") },
		"mvcc":           func(r *clientv3.TxnResponse) { r.Responses[0].GetResponseRange().Kvs[0].ModRevision++ },
		"kv-unknown":     func(r *clientv3.TxnResponse) { r.Responses[0].GetResponseRange().Kvs[0].XXX_unrecognized = []byte{1} },
		"range-unknown":  func(r *clientv3.TxnResponse) { r.Responses[0].GetResponseRange().XXX_unrecognized = []byte{1} },
		"op-unknown":     func(r *clientv3.TxnResponse) { r.Responses[0].XXX_unrecognized = []byte{1} },
		"nested-header": func(r *clientv3.TxnResponse) {
			r.Responses[0].GetResponseRange().Header = &pb.ResponseHeader{ClusterId: r.Header.ClusterId + 1, Revision: r.Header.Revision}
		},
		"recursive-shape": func(r *clientv3.TxnResponse) {
			r.Responses[0].Response = &pb.ResponseOp_ResponseTxn{ResponseTxn: (*pb.TxnResponse)(r)}
		},
		"oversized-shape": func(r *clientv3.TxnResponse) { r.Responses = make([]*pb.ResponseOp, 65) },
		"oversized-value": func(r *clientv3.TxnResponse) {
			r.Responses[0].GetResponseRange().Kvs[0].Value = make([]byte, maxMutationBytes+1)
		},
	}
	b, _ := integrationBackend(t)
	ctx := context.Background()
	seed := controlKeySeed()
	defer clear(seed)
	if _, err := b.ProvisionWorkspaceControlKey(ctx, seed); err != nil {
		t.Fatal("wire fixture provision failed")
	}
	original := b.client.KV
	for name, defect := range defects {
		t.Run(name, func(t *testing.T) {
			b.client.KV = &faultKV{KV: original, match: func([]clientv3.Op) bool { return true }, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				if err != nil {
					t.Fatal("wire fixture RPC failed")
				}
				defect(r)
				return r, nil
			}}
			if key, err := b.LoadWorkspaceControlKey(ctx); err == nil || key != nil {
				t.Fatal("malformed wire returned private key")
			}
		})
	}
	b.client.KV = original
}

func TestControlKeyInputDriftCancellationAndClosed(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	seed := controlKeySeed()
	defer clear(seed)
	for _, invalid := range [][]byte{nil, make([]byte, 31), make([]byte, 33)} {
		if key, err := b.ProvisionWorkspaceControlKey(ctx, invalid); err == nil || key != nil {
			t.Fatal("invalid seed accepted")
		}
	}
	if key, err := b.LoadWorkspaceControlKey(nil); err == nil || key != nil {
		t.Fatal("nil context accepted")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if key, err := b.LoadWorkspaceControlKey(canceled); key != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled load accepted")
	}
	if pub, err := b.ProvisionWorkspaceControlKey(canceled, seed); pub != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled provision accepted")
	}
	if _, err := b.ProvisionWorkspaceControlKey(ctx, seed); err != nil {
		t.Fatal("drift fixture provision failed")
	}
	for _, metadata := range []string{b.identityKey, b.restoreKey} {
		before, err := raw.Get(ctx, metadata)
		if err != nil || len(before.Kvs) != 1 {
			t.Fatal("operator fixture missing")
		}
		if _, err := raw.Put(ctx, metadata, "drift"); err != nil {
			t.Fatal("operator drift fixture failed")
		}
		if key, err := b.LoadWorkspaceControlKey(ctx); err == nil || key != nil {
			t.Fatal("load ignored operator drift")
		}
		if key, err := b.ProvisionWorkspaceControlKey(ctx, seed); err == nil || key != nil {
			t.Fatal("provision ignored operator drift")
		}
		if _, err := raw.Put(ctx, metadata, string(before.Kvs[0].Value)); err != nil {
			t.Fatal("operator fixture restoration failed")
		}
	}
	if err := b.Close(); err != nil {
		t.Fatal("backend close failed")
	}
	if key, err := b.LoadWorkspaceControlKey(ctx); err == nil || key != nil {
		t.Fatal("closed backend loaded")
	}
	if key, err := b.ProvisionWorkspaceControlKey(ctx, seed); err == nil || key != nil {
		t.Fatal("closed backend provisioned")
	}
}

func TestControlKeyPreflightMaximumNamespace(t *testing.T) {
	n, err := NewNamespace("/"+strings.Repeat("p", 252), strings.Repeat("s", 128), strings.Repeat("c", 128))
	if err != nil || len(n.Root()) != maxNamespaceRootBytes {
		t.Fatal("maximum namespace fixture invalid")
	}
	b := &Backend{namespace: n, identityValue: strings.Repeat("i", maxIdentityJSONBytes), restoreEpoch: strings.Repeat("r", maxIdentityFieldBytes)}
	b.identityKey, _ = n.Key("meta", "identity")
	b.restoreKey, _ = n.Key("meta", "restore_epoch")
	name, _ := n.Key("meta", "workspace-control-seed-v1")
	for _, provision := range []bool{false, true} {
		if err := b.controlKeyPreflight(name, provision); err != nil {
			t.Fatal("maximum valid namespace rejected by wire accounting")
		}
	}
	if err := b.controlKeyPreflight("/foreign/key", true); err == nil {
		t.Fatal("foreign key passed request preflight")
	}
	if err := controlKeyRecord(&mvccpb.KeyValue{Value: make([]byte, 32), CreateRevision: 1, ModRevision: 2, Version: 1}); err == nil {
		t.Fatal("noninitial MVCC record accepted")
	}
}

func TestControlKeyLoadErrorClearsTemporarySeed(t *testing.T) {
	b, _ := integrationBackend(t)
	ctx := context.Background()
	seed := controlKeySeed()
	defer clear(seed)
	if _, err := b.ProvisionWorkspaceControlKey(ctx, seed); err != nil {
		t.Fatal("cleanup fixture provision failed")
	}
	var temporary []byte
	b.client.KV = &faultKV{KV: b.client.KV, match: func([]clientv3.Op) bool { return true }, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		if err != nil {
			t.Fatal("cleanup fixture RPC failed")
		}
		kv := r.Responses[0].GetResponseRange().Kvs[0]
		temporary = kv.Value
		kv.Lease = 1
		return r, nil
	}}
	if key, err := b.LoadWorkspaceControlKey(ctx); err == nil || key != nil {
		t.Fatal("leased response returned key")
	}
	if len(temporary) != ed25519.SeedSize || !bytes.Equal(temporary, make([]byte, ed25519.SeedSize)) {
		t.Fatal("error retained temporary seed")
	}
}

func TestControlKeyRPCFailureDoesNotLeakOrReplay(t *testing.T) {
	b, _ := integrationBackend(t)
	ctx := context.Background()
	seed := controlKeySeed()
	defer clear(seed)
	writes, reads := 0, 0
	b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
		if len(ops) == 1 && ops[0].IsPut() {
			writes++
		} else {
			reads++
		}
		return true
	}, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		if err != nil {
			t.Fatal("RPC-failure fixture request failed")
		}
		return nil, errors.New(string(seed))
	}}
	pub, err := b.ProvisionWorkspaceControlKey(ctx, seed)
	if pub != nil || !errors.Is(err, ErrOutcomeUnknown) || strings.Contains(err.Error(), string(seed)) {
		t.Fatal("unknown arbitration returned key or transport details")
	}
	if writes != 1 || reads != 1 {
		t.Fatal("unknown arbitration retried")
	}
	key, err := b.LoadWorkspaceControlKey(ctx)
	if key != nil || err == nil || strings.Contains(err.Error(), string(seed)) {
		t.Fatal("failed read returned key or transport details")
	}
}
