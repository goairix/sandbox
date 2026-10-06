package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func preparationOperation(t *testing.T, b *Backend, c *CreationClaim, wire []byte, mount bool) (string, func() (PreparationResult, error)) {
	t.Helper()
	if mount {
		_, err := b.BindRuntime(context.Background(), c, wire)
		require.NoError(t, err)
		key, err := b.namespace.runtimeMountIntentKey(c.reference.Partition, c.reference.IntentID)
		require.NoError(t, err)
		return key, func() (PreparationResult, error) { return b.ConsumeRuntimeMount(context.Background(), c) }
	}
	key, _, err := b.namespace.runtimeBindingKeys(c.reference.Partition, c.reference.IntentID)
	require.NoError(t, err)
	return key, func() (PreparationResult, error) { return b.BindRuntime(context.Background(), c, wire) }
}
func TestRuntimePreparationLostReplies(t *testing.T) {
	for _, mount := range []bool{false, true} {
		for _, guard := range []bool{false, true} {
			t.Run(fmt.Sprintf("mount=%t/guard=%t", mount, guard), func(t *testing.T) {
				b, raw, input, c, wire, _ := preparationFixture(t, "fuse")
				key, run := preparationOperation(t, b, c, wire, mount)
				var captured StageReference
				b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
					isGuard := len(ops) == 1 && ops[0].IsPut() && strings.Contains(string(ops[0].KeyBytes()), "/attempts/")
					if isGuard {
						require.NoError(t, json.Unmarshal(ops[0].ValueBytes(), &captured))
					}
					if guard {
						return isGuard
					}
					return putsKey(key)(ops)
				}, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
					require.NoError(t, e)
					require.True(t, r.Succeeded)
					return nil, context.DeadlineExceeded
				}}
				result, err := run()
				require.ErrorIs(t, err, ErrOutcomeUnknown)
				require.Equal(t, OutcomeUnknown, result.Outcome)
				require.Nil(t, result.Binding)
				require.Nil(t, result.Mount)
				require.NotEmpty(t, result.Reference.Digest)
				require.Equal(t, captured, result.Reference)
				fresh := freshDispatchBackend(t, b, raw)
				outcome, err := fresh.ResolveStage(context.Background(), result.Reference)
				require.NoError(t, err)
				expected := OutcomeCommitted
				if guard {
					expected = OutcomeAborted
				}
				require.Equal(t, expected, outcome)
				if mount {
					entry, e := fresh.LoadRuntimeMountIntent(context.Background(), input.Workspace, input.IntentID)
					require.NoError(t, e)
					if guard {
						require.Nil(t, entry)
					} else {
						require.NotNil(t, entry)
						require.Equal(t, result.Reference, entry.Reference)
					}
				} else {
					entry, e := fresh.LoadRuntimeBinding(context.Background(), input.Workspace, input.IntentID)
					require.NoError(t, e)
					if guard {
						require.Nil(t, entry)
					} else {
						require.NotNil(t, entry)
						require.Equal(t, result.Reference, entry.Reference)
					}
				}
			})
		}
	}
}
func TestRuntimePreparationDelayedCompleteTxn(t *testing.T) {
	for _, mount := range []bool{false, true} {
		t.Run(fmt.Sprint(mount), func(t *testing.T) {
			b, raw, _, c, wire, _ := preparationFixture(t, "fuse")
			key, run := preparationOperation(t, b, c, wire, mount)
			arrived := make(chan StageReference, 1)
			release := make(chan struct{})
			var once sync.Once
			unlock := func() { once.Do(func() { close(release) }) }
			defer unlock()
			var reference StageReference
			b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
				if len(ops) == 1 && ops[0].IsPut() && strings.Contains(string(ops[0].KeyBytes()), "/attempts/") {
					require.NoError(t, json.Unmarshal(ops[0].ValueBytes(), &reference))
				}
				return putsKey(key)(ops)
			}, before: func() { arrived <- reference; <-release }}
			type answer struct {
				r PreparationResult
				e error
			}
			done := make(chan answer, 1)
			go func() { r, e := run(); done <- answer{r, e} }()
			var ref StageReference
			select {
			case ref = <-arrived:
			case <-time.After(5 * time.Second):
				t.Fatal("complete transaction did not arrive")
			}
			outcome, err := b.ResolveStage(context.Background(), ref)
			require.NoError(t, err)
			require.Equal(t, OutcomeAborted, outcome)
			unlock()
			select {
			case a := <-done:
				require.NoError(t, a.e)
				require.Equal(t, OutcomeAborted, a.r.Outcome)
				require.Nil(t, a.r.Binding)
				require.Nil(t, a.r.Mount)
			case <-time.After(5 * time.Second):
				t.Fatal("delayed transaction stuck")
			}
			got, err := raw.Get(context.Background(), key)
			require.NoError(t, err)
			require.Empty(t, got.Kvs)
		})
	}
}
func TestRuntimePreparationImmutableCAS(t *testing.T) {
	for _, fault := range []string{"dispatch", "binding", "certificate", "index", "receipt", "claim", "guard", "control", "restore"} {
		t.Run(fault, func(t *testing.T) {
			b, raw, input, c, wire, _ := preparationFixture(t, "fuse")
			ctx := context.Background()
			mount := fault != "dispatch"
			key, run := preparationOperation(t, b, c, wire, mount)
			bundle, err := b.loadRuntimePreparation(ctx, input.Workspace, input.IntentID, "")
			require.NoError(t, err)
			var changed string
			switch fault {
			case "dispatch":
				changed = string(bundle.DispatchKVs[0].Key)
			case "binding":
				changed = bundle.BindingKey
			case "certificate":
				changed = bundle.CertificateKey
			case "index":
				changed = bundle.IndexKey
			case "receipt":
				changed = string(bundle.BindingKVs[3].Key)
			case "claim":
				changed = c.claimKey
			case "guard":
				changed = c.guardKey
			case "control":
				changed, _, err = b.namespace.sandboxKeys(input.Workspace.Partition(), input.SandboxID, "read")
			case "restore":
				changed = b.restoreKey
			}
			require.NoError(t, err)
			b.client.Lease = &creationFaultLease{Lease: b.client.Lease, grant: func(r *clientv3.LeaseGrantResponse, e error) (*clientv3.LeaseGrantResponse, error) {
				require.NoError(t, e)
				got, e := raw.Get(ctx, changed)
				require.NoError(t, e)
				require.Len(t, got.Kvs, 1)
				if fault == "claim" || fault == "guard" {
					_, e = raw.Delete(ctx, changed)
					require.NoError(t, e)
					_, e = raw.Put(ctx, changed, string(got.Kvs[0].Value), clientv3.WithLease(clientv3.LeaseID(got.Kvs[0].Lease)))
				} else {
					value := string(got.Kvs[0].Value)
					if fault == "restore" {
						value = "changed-restore"
					}
					_, e = raw.Put(ctx, changed, value)
				}
				require.NoError(t, e)
				return r, nil
			}}
			result, err := run()
			require.Error(t, err)
			require.Nil(t, result.Binding)
			require.Nil(t, result.Mount)
			got, err := raw.Get(ctx, key)
			require.NoError(t, err)
			require.Empty(t, got.Kvs)
		})
	}
}
func TestRuntimePreparationClockRecheckAndCopies(t *testing.T) {
	for _, fault := range []string{"before grant", "after grant", "caller copy", "cleanup"} {
		t.Run(fault, func(t *testing.T) {
			var invalid atomic.Bool
			clock := testAuthorityClock(func(context.Context) (ClockObservation, error) {
				if invalid.Load() {
					return ClockObservation{}, errTestClock
				}
				return ClockObservation{UTC: time.Now().UTC()}, nil
			})
			b, raw, input, c, wire, _ := preparationFixture(t, "fuse", clock)
			original := append([]byte(nil), wire...)
			if fault == "before grant" {
				invalid.Store(true)
			}
			cleanupErr := errors.New("injected cleanup failure")
			var cleanupLease clientv3.LeaseID
			lease := &creationFaultLease{Lease: b.client.Lease, grant: func(r *clientv3.LeaseGrantResponse, e error) (*clientv3.LeaseGrantResponse, error) {
				if fault == "after grant" {
					invalid.Store(true)
				}
				if fault == "caller copy" {
					for i := range wire {
						wire[i] = '!'
					}
				}
				return r, e
			}}
			if fault == "cleanup" {
				lease.revoke = func(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
					cleanupLease = id
					require.NotEqual(t, clientv3.LeaseID(c.Reference().LeaseID), id)
					return nil, cleanupErr
				}
			}
			b.client.Lease = lease
			defer func() { b.client.Lease = lease.Lease }()
			result, err := b.BindRuntime(context.Background(), c, wire)
			if fault == "before grant" || fault == "after grant" {
				require.Error(t, err)
				require.Nil(t, result.Binding)
				if fault == "before grant" {
					require.Zero(t, lease.grants.Load())
					require.Empty(t, result.Reference.Digest)
				} else {
					require.NotEmpty(t, result.Reference.Digest)
					outcome, e := b.ResolveStage(context.Background(), result.Reference)
					require.NoError(t, e)
					require.Equal(t, OutcomeAborted, outcome)
				}
			} else {
				require.NoError(t, err)
				require.Equal(t, OutcomeCommitted, result.Outcome)
				require.Equal(t, original, []byte(result.Binding.Certificate))
				result.Binding.Certificate[0] = '!'
				loaded, e := b.LoadRuntimeBinding(context.Background(), input.Workspace, input.IntentID)
				require.NoError(t, e)
				require.Equal(t, original, []byte(loaded.Certificate))
				if fault == "cleanup" {
					require.ErrorIs(t, result.GuardCleanupError, cleanupErr)
					_, e = raw.Revoke(context.Background(), cleanupLease)
					require.NoError(t, e)
				}
			}
		})
	}
}
