//go:build linux && (amd64 || arm64)

package etcd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func snapshotNativeInput(t *testing.T, b *Backend, suffix string) AcquireIntentInput {
	t.Helper()
	in := acquisitionInput(t, suffix)
	var err error
	in.Workspace, err = NewWorkspaceIdentity("s3", "test-storage", "bucket", "snapshot/"+suffix+"/")
	require.NoError(t, err)
	in.Payload = json.RawMessage(`{"hello":"world"}`)
	result, err := b.AcquireIntent(context.Background(), in)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	return in
}

func snapshotNativeMatch(ops []clientv3.Op) bool {
	return len(ops) == 2 && ops[0].IsGet() && strings.Contains(string(ops[0].KeyBytes()), "/snapshots/")
}

func TestSnapshotReadCacheNative(t *testing.T) {
	backends, _ := coldPreparationNativeBackends(t)
	b := backends[0]
	ctx := context.Background()
	in := snapshotNativeInput(t, b, "cache")
	var snapshots, metadata, bytesRead atomic.Int64
	var invalidOperation atomic.Bool
	base := b.client.KV
	b.client.KV = &faultKV{KV: base, match: func(ops []clientv3.Op) bool {
		for _, op := range ops {
			if !op.IsGet() || len(op.RangeBytes()) != 0 {
				invalidOperation.Store(true)
				t.Errorf("snapshot read dispatched a non-point-read operation")
			}
		}
		if snapshotNativeMatch(ops) {
			snapshots.Add(1)
		} else {
			metadata.Add(1)
		}
		return true
	}, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		if r != nil {
			bytesRead.Add(int64((*pb.TxnResponse)(r).Size()))
		}
		return r, err
	}}
	for _, entry := range []string{"workspace", "sandbox"} {
		for i := 0; i < 2; i++ {
			var result *SandboxSnapshotReadResult
			var err error
			if entry == "workspace" {
				result, err = b.ReadWorkspaceSnapshot(ctx, in.Workspace)
			} else {
				result, err = b.ReadSandboxSnapshot(ctx, in.SandboxID)
			}
			require.NoError(t, err)
			require.NotNil(t, result)
			require.NotNil(t, result.View)
			require.Equal(t, `{"hello":"world"}`, string(result.Snapshot.Payload))
			result.Snapshot.Payload[2] = 'X'
			result.Snapshot.SandboxID = "caller-mutated"
			result.View.Records().Control.Snapshot.Version = "caller-mutated"
		}
	}
	require.Equal(t, int64(1), snapshots.Load(), "four calls share one immutable snapshot fill")
	require.Equal(t, int64(12), metadata.Load(), "every call keeps discovery, coherent view and final confirmation")
	require.False(t, invalidOperation.Load())
	t.Logf("fixed four-read scenario: snapshot RPCs=%d metadata RPCs=%d actual response bytes=%d; no latency/throughput/capacity claim", snapshots.Load(), metadata.Load(), bytesRead.Load())
	b.client.KV = base
	otherBase := backends[1].client.KV
	var otherReads atomic.Int64
	backends[1].client.KV = &faultKV{KV: otherBase, match: func(ops []clientv3.Op) bool {
		if snapshotNativeMatch(ops) {
			otherReads.Add(1)
		}
		return false
	}}
	result, err := backends[1].ReadWorkspaceSnapshot(ctx, in.Workspace)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, int64(1), otherReads.Load(), "another Backend must fill its own cache")
	backends[1].client.KV = otherBase
	w, err := NewWorkspaceIdentity("s3", "test-storage", "bucket", "snapshot/absent/")
	require.NoError(t, err)
	result, err = b.ReadWorkspaceSnapshot(ctx, w)
	require.NoError(t, err)
	require.Nil(t, result)
	result, err = b.ReadSandboxSnapshot(ctx, "absent-sandbox")
	require.NoError(t, err)
	require.Nil(t, result)
}

func TestSnapshotReadMalformedNative(t *testing.T) {
	backends, _ := coldPreparationNativeBackends(t)
	b := backends[0]
	in := snapshotNativeInput(t, b, "malformed")
	base := b.client.KV
	var invalidFixture atomic.Bool
	cases := []struct {
		name   string
		mutate func(*clientv3.TxnResponse)
	}{
		{"digest", func(r *clientv3.TxnResponse) {
			r.Responses[0].GetResponseRange().Kvs[0].Value = bytes.ReplaceAll(r.Responses[0].GetResponseRange().Kvs[0].Value, []byte("world"), []byte("other"))
		}},
		{"canonical", func(r *clientv3.TxnResponse) {
			r.Responses[0].GetResponseRange().Kvs[0].Value = append([]byte(" "), r.Responses[0].GetResponseRange().Kvs[0].Value...)
		}},
		{"oversize", func(r *clientv3.TxnResponse) {
			r.Responses[0].GetResponseRange().Kvs[0].Value = bytes.Repeat([]byte("x"), 65537)
		}},
		{"immutable", func(r *clientv3.TxnResponse) { r.Responses[0].GetResponseRange().Kvs[0].Version = 2 }},
		{"lease", func(r *clientv3.TxnResponse) { r.Responses[0].GetResponseRange().Kvs[0].Lease = 1 }},
		{"snapshot-unknown", func(r *clientv3.TxnResponse) {
			r.Responses[0].GetResponseRange().Kvs[0].XXX_unrecognized = []byte{8, 1}
		}},
		{"placement-association", func(r *clientv3.TxnResponse) {
			kv := r.Responses[1].GetResponseRange().Kvs[0]
			var placement SandboxPlacementRecord
			if err := json.Unmarshal(kv.Value, &placement); err != nil {
				invalidFixture.Store(true)
				t.Errorf("decode original placement fixture: %v", err)
				return
			}
			placement.IntentID = "replacement-intent"
			value, err := encodeDomainRecord(placement)
			if err != nil {
				invalidFixture.Store(true)
				t.Errorf("encode replacement placement fixture: %v", err)
				return
			}
			kv.Value = []byte(value)
		}},
		{"placement-canonical", func(r *clientv3.TxnResponse) {
			r.Responses[1].GetResponseRange().Kvs[0].Value = append([]byte(" "), r.Responses[1].GetResponseRange().Kvs[0].Value...)
		}},
		{"placement-missing", func(r *clientv3.TxnResponse) {
			r.Responses[1].GetResponseRange().Kvs = nil
			r.Responses[1].GetResponseRange().Count = 0
		}},
		{"snapshot-missing", func(r *clientv3.TxnResponse) {
			r.Responses[0].GetResponseRange().Kvs = nil
			r.Responses[0].GetResponseRange().Count = 0
		}},
		{"recursive-response", func(r *clientv3.TxnResponse) {
			r.Responses[0] = &pb.ResponseOp{Response: &pb.ResponseOp_ResponseTxn{ResponseTxn: (*pb.TxnResponse)(r)}}
		}},
		{"top-unknown", func(r *clientv3.TxnResponse) { r.XXX_unrecognized = []byte{8, 1} }},
		{"header-unknown", func(r *clientv3.TxnResponse) { r.Header.XXX_unrecognized = []byte{8, 1} }},
		{"nested-header", func(r *clientv3.TxnResponse) {
			r.Responses[0].GetResponseRange().Header = &pb.ResponseHeader{Revision: r.Header.Revision + 1}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b.client.KV = &faultKV{KV: base, match: snapshotNativeMatch, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				if err == nil {
					tc.mutate(r)
				}
				return r, err
			}}
			result, err := b.ReadWorkspaceSnapshot(context.Background(), in.Workspace)
			require.Nil(t, result)
			require.ErrorIs(t, err, ErrCorruptRecord)
			b.archiveLife.mu.Lock()
			entries := len(b.snapshotReads.entries)
			b.archiveLife.mu.Unlock()
			require.Zero(t, entries, "malformed results must not enter cache")
			require.False(t, invalidFixture.Load())
		})
	}
	b.client.KV = base
	result, err := b.ReadWorkspaceSnapshot(context.Background(), in.Workspace)
	require.NoError(t, err)
	require.NotNil(t, result)
}

func TestSnapshotReadFencesNative(t *testing.T) {
	for _, change := range []string{"control-miss", "restore-miss", "control-hit", "restore-hit", "owner-hit"} {
		t.Run(change, func(t *testing.T) {
			backends, raw := coldPreparationNativeBackends(t)
			b := backends[0]
			ctx := context.Background()
			in := snapshotNativeInput(t, b, change)
			hit := strings.HasSuffix(change, "hit")
			if hit {
				_, err := b.ReadWorkspaceSnapshot(ctx, in.Workspace)
				require.NoError(t, err)
			}
			controlKey, _, err := b.namespace.sandboxKeys(in.Workspace.Partition(), in.SandboxID, "read")
			require.NoError(t, err)
			ownerKey, _, err := b.namespace.workspaceKeys(in.Workspace)
			require.NoError(t, err)
			var changed atomic.Bool
			mutationErrors := make(chan error, 1)
			b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
				return (hit && len(ops) == 0 || !hit && snapshotNativeMatch(ops)) && changed.CompareAndSwap(false, true)
			}, before: func() {
				mutate := func() error {
					key := controlKey
					if strings.HasPrefix(change, "restore") {
						_, err := raw.Put(ctx, b.restoreKey, "replacement-epoch")
						return err
					}
					if strings.HasPrefix(change, "owner") {
						key = ownerKey
					}
					value, err := raw.Get(ctx, key)
					if err != nil {
						return err
					}
					if len(value.Kvs) != 1 {
						return fmt.Errorf("original metadata mutation needs one record, got %d", len(value.Kvs))
					}
					_, err = raw.Put(ctx, key, string(value.Kvs[0].Value))
					return err
				}
				err := mutate()
				if err != nil {
					t.Errorf("replace original metadata during snapshot read: %v", err)
				}
				mutationErrors <- err
			}}
			result, err := b.ReadWorkspaceSnapshot(ctx, in.Workspace)
			require.Nil(t, result)
			if strings.HasPrefix(change, "restore") {
				require.ErrorIs(t, err, ErrIdentityMismatch)
			} else {
				require.ErrorIs(t, err, ErrConflict)
			}
			require.True(t, changed.Load(), "replacement must occur on the intercepted original read")
			require.NoError(t, <-mutationErrors)
		})
	}
}

func TestSnapshotReadCancelAndCloseNative(t *testing.T) {
	for _, closing := range []bool{false, true} {
		t.Run(fmt.Sprintf("close-%t", closing), func(t *testing.T) {
			backends, _ := coldPreparationNativeBackends(t)
			b := backends[0]
			in := snapshotNativeInput(t, b, fmt.Sprintf("cancel-%t", closing))
			arrived, release := make(chan struct{}), make(chan struct{})
			var released atomic.Bool
			unlock := func() {
				if released.CompareAndSwap(false, true) {
					close(release)
				}
			}
			t.Cleanup(unlock)
			var reads atomic.Int64
			b.client.KV = &faultKV{KV: b.client.KV, match: snapshotNativeMatch, before: func() {
				reads.Add(1)
				close(arrived)
				<-release
			}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			first := make(chan error, 1)
			go func() { _, err := b.ReadWorkspaceSnapshot(ctx, in.Workspace); first <- err }()
			select {
			case <-arrived:
			case <-time.After(2 * time.Second):
				t.Fatal("actual miss transaction did not reach interception")
			}
			if closing {
				closed := make(chan error, 1)
				go func() { closed <- b.Close() }()
				require.ErrorIs(t, <-first, context.Canceled)
				select {
				case <-closed:
					t.Fatal("Close returned before original intercepted worker joined")
				default:
				}
				unlock()
				require.NoError(t, <-closed)
				result, err := b.ReadWorkspaceSnapshot(context.Background(), in.Workspace)
				require.Nil(t, result)
				require.ErrorIs(t, err, context.Canceled)
				require.Empty(t, b.snapshotReads.entries)
			} else {
				cancel()
				require.ErrorIs(t, <-first, context.Canceled)
				second := make(chan error, 1)
				go func() {
					result, err := b.ReadWorkspaceSnapshot(context.Background(), in.Workspace)
					if err == nil && result == nil {
						err = ErrCorruptRecord
					}
					second <- err
				}()
				unlock()
				require.NoError(t, <-second)
				require.Equal(t, int64(1), reads.Load())
			}
		})
	}
}

func TestSnapshotReadViewDispatchNative(t *testing.T) {
	backends, _ := coldPreparationNativeBackends(t)
	b := backends[0]
	in := snapshotNativeInput(t, b, "view-dispatch")
	base := b.client.KV
	cases := []struct {
		name   string
		mutate func(*clientv3.TxnResponse)
	}{
		{"recursive", func(r *clientv3.TxnResponse) {
			r.Responses[0] = &pb.ResponseOp{Response: &pb.ResponseOp_ResponseTxn{ResponseTxn: (*pb.TxnResponse)(r)}}
		}},
		{"top-unknown", func(r *clientv3.TxnResponse) { r.XXX_unrecognized = []byte{8, 1} }},
		{"header-unknown", func(r *clientv3.TxnResponse) { r.Header.XXX_unrecognized = []byte{8, 1} }},
		{"value-cap", func(r *clientv3.TxnResponse) {
			r.Responses[0].GetResponseRange().Kvs[0].Value = bytes.Repeat([]byte("x"), 4097)
		}},
		{"member", func(r *clientv3.TxnResponse) {
			r.Responses[0].GetResponseRange().Header = &pb.ResponseHeader{Revision: r.Header.Revision, MemberId: r.Header.MemberId + 1}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b.client.KV = &faultKV{KV: base, match: func(ops []clientv3.Op) bool { return len(ops) == 4 }, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				if err == nil {
					tc.mutate(r)
				}
				return r, err
			}}
			view, err := b.ReadWorkspaceView(context.Background(), in.Workspace)
			require.Nil(t, view)
			require.ErrorIs(t, err, ErrCorruptRecord)
		})
	}
	b.client.KV = base
	view, err := b.ReadWorkspaceView(context.Background(), in.Workspace)
	require.NoError(t, err)
	require.NoError(t, b.ConfirmSandboxView(context.Background(), view))
	b.client.KV = &faultKV{KV: base, match: func(ops []clientv3.Op) bool { return len(ops) == 0 }, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		if err == nil {
			r.XXX_unrecognized = []byte{8, 1}
		}
		return r, err
	}}
	require.ErrorIs(t, b.ConfirmSandboxView(context.Background(), view), ErrCorruptRecord)
	b.client.KV = base
}
