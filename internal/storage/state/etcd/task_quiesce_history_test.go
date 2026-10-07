package etcd

import (
	"context"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"testing"
	"time"
)

func quiescenceNativeSigning(t *testing.T) {
	for _, fault := range []string{"context", "window", "claim-loss", "cancel", "clock", "runtime"} {
		t.Run(fault, func(t *testing.T) {
			f := newQuiescenceNativeFixture(t)
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			f.issuer.sign = func(ctx context.Context, digest string, claims p.TaskUserQuiescenceTicketClaims) ([]byte, error) {
				switch fault {
				case "context":
					claims.Context.Current.WorkerID = "wrong-worker"
				case "window":
					claims.NotAfter = claims.NotAfter.Add(-time.Second)
				case "claim-loss":
					_, e := f.raw.Revoke(f.ctx, clientv3.LeaseID(f.c.reference.LeaseID))
					require.NoError(t, e)
				case "cancel":
					cancel()
				case "clock":
					f.b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
						return ClockObservation{UTC: f.now.Add(time.Minute)}, nil
					})
				case "runtime":
					f.target.clock.now.Store(f.now.Add(time.Hour).UnixNano())
				}
				return p.SignTaskUserQuiescenceTicket(f.issuer.old.key, f.issuer.old.identity, claims)
			}
			if fault == "runtime" {
				// The target rejects its freshly authenticated old CloseData query before
				// signing; no new metadata effect may follow a stale target certificate.
				f.target.clock.now.Store(f.now.Add(time.Hour).UnixNano())
			}
			r, e := f.b.PrepareTaskQuiescence(ctx, f.c, f.target.destination)
			require.Error(t, e)
			require.Nil(t, r.Prepared)
			key, e := f.b.namespace.taskQuiescenceKey(f.c.reference.Task)
			require.NoError(t, e)
			g, e := f.raw.Get(f.ctx, key)
			require.NoError(t, e)
			require.Empty(t, g.Kvs)
		})
	}
}
func quiescenceNativeHistory(t *testing.T) {
	for _, fault := range []string{"cold", "no-adoption", "missing-receipt", "receipt-rewrite", "receipt-recreate", "old-receipt-rewrite", "partial-discovery", "claim-during-read"} {
		t.Run(fault, func(t *testing.T) {
			f := newQuiescenceNativeFixture(t)
			r, e := f.b.PrepareTaskQuiescence(f.ctx, f.c, f.target.destination)
			require.NoError(t, e)
			require.NotNil(t, r.Prepared)
			key, e := f.b.namespace.taskQuiescenceKey(f.c.reference.Task)
			require.NoError(t, e)
			_, receipt, e := f.b.stageKeys(r.Reference.Stage)
			require.NoError(t, e)
			original := f.b.client.KV
			defer func() { f.b.client.KV = original }()
			switch fault {
			case "cold":
				id, e := decodeIdentity(f.b.identityValue, f.b.namespace)
				require.NoError(t, e)
				id.RestoreEpoch = f.b.restoreEpoch
				cold, e := New(f.ctx, Options{Endpoints: f.raw.Endpoints(), Namespace: f.b.namespace, Identity: id, AllowInsecureLoopback: true, RequestTimeout: time.Second})
				require.NoError(t, e)
				defer cold.Close()
				entry, e := cold.LoadTaskQuiescence(f.ctx, r.Reference.Task)
				require.NoError(t, e)
				require.Equal(t, r.Reference, entry.Reference)
				return
			case "no-adoption":
				require.NoError(t, f.b.ReleaseTaskClaim(f.ctx, f.c))
				newClaim, e := f.b.ClaimTask(f.ctx, r.Reference.Task, "cold-reconciler", 30*time.Second)
				require.NoError(t, e)
				defer taskReleaseClaim(t, f.b, newClaim)
				rejected, e := f.b.PrepareTaskQuiescence(f.ctx, newClaim, f.target.destination)
				require.ErrorIs(t, e, ErrConflict)
				require.Nil(t, rejected.Prepared)
				require.Equal(t, 1, f.issuer.calls)
				entry, e := f.b.LoadTaskQuiescence(f.ctx, r.Reference.Task)
				require.NoError(t, e)
				require.Equal(t, r.Reference, entry.Reference)
				return
			case "missing-receipt":
				_, e = f.raw.Delete(f.ctx, receipt)
			case "receipt-rewrite", "receipt-recreate", "old-receipt-rewrite":
				if fault == "old-receipt-rewrite" {
					_, receipt, e = f.b.stageKeys(f.old.Stage)
					require.NoError(t, e)
				}
				g, x := f.raw.Get(f.ctx, receipt)
				require.NoError(t, x)
				require.Len(t, g.Kvs, 1)
				if fault == "receipt-recreate" {
					_, e = f.raw.Delete(f.ctx, receipt)
					require.NoError(t, e)
				}
				_, e = f.raw.Put(f.ctx, receipt, string(g.Kvs[0].Value))
			case "partial-discovery", "claim-during-read":
				fired := false
				f.b.client.KV = &faultKV{KV: original, match: func(o []clientv3.Op) bool {
					return !fired && len(o) == 3 && o[2].IsGet() && string(o[2].KeyBytes()) == key
				}, after: func(resp *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
					require.NoError(t, e)
					fired = true
					if fault == "partial-discovery" {
						resp.Responses = resp.Responses[:2]
					} else {
						_, e = f.raw.Revoke(f.ctx, clientv3.LeaseID(f.c.reference.LeaseID))
						require.NoError(t, e)
					}
					return resp, nil
				}}
				if fault == "claim-during-read" {
					again, e := f.b.PrepareTaskQuiescence(f.ctx, f.c, f.target.destination)
					require.True(t, fired)
					require.Error(t, e)
					require.Nil(t, again.Prepared)
					return
				}
			}
			require.NoError(t, e)
			entry, e := f.b.LoadTaskQuiescence(f.ctx, r.Reference.Task)
			require.Error(t, e)
			require.Nil(t, entry)
		})
	}
}
