package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestRuntimePublicationLostRealReplies(t *testing.T) {
	for _, guard := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "guard"}[guard], func(t *testing.T) {
			b, raw, in, c, wire := publicationFixture(t, "fuse")
			key := publicationKeys(t, b, c)[4]
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
			result, err := b.PublishRuntime(context.Background(), c, wire)
			require.ErrorIs(t, err, ErrOutcomeUnknown)
			require.Equal(t, OutcomeUnknown, result.Outcome)
			require.Nil(t, result.Entry)
			require.NotEmpty(t, result.Reference.Digest)
			require.Equal(t, captured, result.Reference)
			fresh := freshDispatchBackend(t, b, raw)
			outcome, err := fresh.ResolveStage(context.Background(), result.Reference)
			require.NoError(t, err)
			want := OutcomeCommitted
			if guard {
				want = OutcomeAborted
			}
			require.Equal(t, want, outcome)
			entry, err := fresh.LoadRuntimePublication(context.Background(), in.Workspace, in.IntentID)
			require.NoError(t, err)
			if guard {
				require.Nil(t, entry)
			} else {
				require.NotNil(t, entry)
				require.Equal(t, result.Reference, entry.Reference)
			}
		})
	}
}
func TestRuntimePublicationDelayedCompleteTxn(t *testing.T) {
	b, raw, _, c, wire := publicationFixture(t, "fuse")
	ctx := context.Background()
	keys := publicationKeys(t, b, c)
	index, err := b.namespace.runtimeIndexKey("uid")
	require.NoError(t, err)
	keys = append(keys, index)
	before, err := b.readDomain(ctx, keys...)
	require.NoError(t, err)
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
		return putsKey(keys[4])(ops)
	}, before: func() { arrived <- reference; <-release }}
	type answer struct {
		r RuntimePublishResult
		e error
	}
	done := make(chan answer, 1)
	go func() { r, e := b.PublishRuntime(ctx, c, wire); done <- answer{r, e} }()
	var ref StageReference
	select {
	case ref = <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("complete transaction did not arrive")
	}
	fresh := freshDispatchBackend(t, b, raw)
	outcome, err := fresh.ResolveStage(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, OutcomeAborted, outcome)
	unlock()
	select {
	case a := <-done:
		require.NoError(t, a.e)
		require.Equal(t, OutcomeAborted, a.r.Outcome)
		require.Nil(t, a.r.Entry)
		require.Equal(t, ref, a.r.Reference)
	case <-time.After(5 * time.Second):
		t.Fatal("complete transaction stuck")
	}
	after, err := b.readDomain(ctx, keys...)
	require.NoError(t, err)
	require.Equal(t, before, after, "all six writes and reserved index must remain untouched")
}
func TestRuntimePublicationImmutableCAS(t *testing.T) {
	for _, fault := range []string{"dispatch", "input", "dispatch receipt", "binding", "certificate", "binding receipt", "mount", "mount receipt", "index", "recreated index", "claim", "guard", "control", "restore"} {
		t.Run(fault, func(t *testing.T) {
			b, raw, in, c, wire := publicationFixture(t, "fuse")
			ctx := context.Background()
			bundle, err := b.loadRuntimePreparation(ctx, in.Workspace, in.IntentID, "")
			require.NoError(t, err)
			keys := publicationKeys(t, b, c)
			before, err := b.readDomain(ctx, keys...)
			require.NoError(t, err)
			choices := map[string]string{"dispatch": string(bundle.DispatchKVs[0].Key), "input": string(bundle.DispatchKVs[1].Key), "dispatch receipt": string(bundle.DispatchKVs[2].Key), "binding": bundle.BindingKey, "certificate": bundle.CertificateKey, "binding receipt": string(bundle.BindingKVs[3].Key), "mount": bundle.MountKey, "mount receipt": string(bundle.MountKVs[1].Key), "index": bundle.IndexKey, "recreated index": bundle.IndexKey, "claim": c.claimKey, "guard": c.guardKey, "control": keys[1], "restore": b.restoreKey}
			changed := choices[fault]
			b.client.Lease = &creationFaultLease{Lease: b.client.Lease, grant: func(r *clientv3.LeaseGrantResponse, e error) (*clientv3.LeaseGrantResponse, error) {
				require.NoError(t, e)
				got, e := raw.Get(ctx, changed)
				require.NoError(t, e)
				require.Len(t, got.Kvs, 1)
				if fault == "claim" || fault == "guard" || fault == "recreated index" {
					_, e = raw.Delete(ctx, changed)
					require.NoError(t, e)
				}
				value := string(got.Kvs[0].Value)
				if fault == "restore" {
					value = "changed-restore"
				}
				_, e = raw.Put(ctx, changed, value, clientv3.WithLease(clientv3.LeaseID(got.Kvs[0].Lease)))
				require.NoError(t, e)
				return r, nil
			}}
			result, err := b.PublishRuntime(ctx, c, wire)
			require.Error(t, err)
			require.Nil(t, result.Entry)
			for i, key := range keys {
				got, e := raw.Get(ctx, key)
				require.NoError(t, e)
				if before[i] == nil {
					require.Empty(t, got.Kvs)
				} else {
					require.Len(t, got.Kvs, 1)
					require.Equal(t, before[i].Value, got.Kvs[0].Value)
				}
			}
		})
	}
}
func TestRuntimePublicationPostBeginChecksAndCopies(t *testing.T) {
	for _, fault := range []string{"clock before", "clock after", "expired after", "cancel after", "lost after", "caller copy", "cleanup"} {
		t.Run(fault, func(t *testing.T) {
			var changed atomic.Bool
			clock := testAuthorityClock(func(context.Context) (ClockObservation, error) {
				if changed.Load() {
					if fault == "expired after" {
						return ClockObservation{UTC: time.Now().UTC().Add(time.Hour)}, nil
					}
					return ClockObservation{}, errTestClock
				}
				return ClockObservation{UTC: time.Now().UTC()}, nil
			})
			b, raw, in, c, wire := publicationFixture(t, "plain", clock)
			original := append([]byte(nil), wire...)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if fault == "clock before" {
				changed.Store(true)
			}
			cleanupErr := errors.New("publication cleanup fault")
			var cleanupID clientv3.LeaseID
			lease := &creationFaultLease{Lease: b.client.Lease, grant: func(r *clientv3.LeaseGrantResponse, e error) (*clientv3.LeaseGrantResponse, error) {
				switch fault {
				case "clock after", "expired after":
					changed.Store(true)
				case "cancel after":
					cancel()
				case "lost after":
					c.mu.Lock()
					c.lost = true
					c.mu.Unlock()
				case "caller copy":
					for i := range wire {
						wire[i] = '!'
					}
				}
				return r, e
			}}
			if fault == "cleanup" {
				lease.revoke = func(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
					require.NotEqual(t, clientv3.LeaseID(c.reference.LeaseID), id)
					require.NotNil(t, ctx)
					_, bounded := ctx.Deadline()
					require.True(t, bounded)
					cleanupID = id
					return nil, cleanupErr
				}
			}
			b.client.Lease = lease
			defer func() { b.client.Lease = lease.Lease }()
			result, err := b.PublishRuntime(ctx, c, wire)
			if fault == "caller copy" || fault == "cleanup" {
				require.NoError(t, err)
				require.Equal(t, OutcomeCommitted, result.Outcome)
				require.Equal(t, original, []byte(result.Entry.Proof))
				result.Entry.Proof[0] = '!'
				loaded, e := b.LoadRuntimePublication(context.Background(), in.Workspace, in.IntentID)
				require.NoError(t, e)
				require.Equal(t, original, []byte(loaded.Proof))
				if fault == "cleanup" {
					require.ErrorIs(t, result.GuardCleanupError, cleanupErr)
					_, e = raw.Revoke(context.Background(), cleanupID)
					require.NoError(t, e)
				}
			} else {
				require.Error(t, err)
				require.Nil(t, result.Entry)
				if fault == "clock before" {
					require.Zero(t, lease.grants.Load())
					require.Empty(t, result.Reference.Digest)
				} else {
					require.NotEmpty(t, result.Reference.Digest)
					outcome, e := b.ResolveStage(context.Background(), result.Reference)
					require.NoError(t, e)
					require.Equal(t, OutcomeAborted, outcome)
				}
			}
		})
	}
}
func TestRuntimePublicationConcurrentOnce(t *testing.T) {
	b, _, in, c, wire := publicationFixture(t, "fuse")
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	journal := publicationKeys(t, b, c)[4]
	b.client.KV = &faultKV{KV: b.client.KV, match: putsKey(journal), before: func() { arrived <- struct{}{}; <-release }}
	type answer struct {
		r RuntimePublishResult
		e error
	}
	done := make(chan answer, 2)
	for i := 0; i < 2; i++ {
		go func() { r, e := b.PublishRuntime(context.Background(), c, wire); done <- answer{r, e} }()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("publish did not arrive")
		}
	}
	close(release)
	committed := 0
	for i := 0; i < 2; i++ {
		a := <-done
		if a.e == nil {
			require.Equal(t, OutcomeCommitted, a.r.Outcome)
			require.Equal(t, PublishDeclared, a.r.Disposition)
			committed++
		} else {
			require.ErrorIs(t, a.e, ErrConflict)
			require.Nil(t, a.r.Entry)
		}
	}
	require.Equal(t, 1, committed)
	entry, err := b.LoadRuntimePublication(context.Background(), in.Workspace, in.IntentID)
	require.NoError(t, err)
	require.NotNil(t, entry)
}
