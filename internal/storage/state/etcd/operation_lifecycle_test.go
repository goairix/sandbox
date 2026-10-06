package etcd

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Removing original-Lease KeepAlive or using arrival+TTL fails this test.
func TestRenewOperationOriginalLease(t *testing.T) {
	b, raw, in, keys := operationControlFixture(t, "plain")
	ctx := context.Background()
	lease := &creationFaultLease{Lease: b.client.Lease}
	b.client.Lease = lease
	defer func() { b.client.Lease = lease.Lease }()
	c := admittedOperation(t, b, raw, in.SandboxID, OperationData)
	original := c.Reference()
	record := c.record
	before, e := b.readDomain(ctx, keys...)
	require.NoError(t, e)
	// A later mutation blocks new entry, but admitted data must still drain.
	m := admittedOperation(t, b, raw, in.SandboxID, OperationMutation)
	grants := lease.grants.Load()
	lease.keep = func(r *clientv3.LeaseKeepAliveResponse, e error) (*clientv3.LeaseKeepAliveResponse, error) {
		require.NoError(t, e)
		require.Equal(t, clientv3.LeaseID(original.LeaseID), r.ID)
		r.TTL = 2
		time.Sleep(150 * time.Millisecond)
		return r, e
	}
	for i := 0; i < 3; i++ {
		sent := time.Now()
		require.NoError(t, b.RenewOperation(ctx, c))
		received := time.Now()
		require.False(t, c.deadline.Before(sent.Add(2*time.Second)))
		require.True(t, c.deadline.Before(received.Add(1900*time.Millisecond)))
		require.Equal(t, original, c.Reference())
		require.Equal(t, record, c.record)
	}
	require.Equal(t, grants, lease.grants.Load())
	require.Equal(t, int64(3), lease.keeps.Load())
	lease.keep = nil
	require.NoError(t, b.RenewOperation(ctx, m))
	// Business time passes while original permanent control remains unchanged.
	b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
		return ClockObservation{UTC: record.ExpiresAt.Add(time.Hour)}, nil
	})
	require.NoError(t, b.RenewOperation(ctx, c))
	require.Equal(t, record.ExpiresAt, c.Control().ExpiresAt)
	after, e := b.readDomain(ctx, keys...)
	require.NoError(t, e)
	require.Equal(t, before, after)
}

func TestRenewOperationIrrevocableLoss(t *testing.T) {
	scenarios := []string{"parent canceled", "caller canceled", "expired deadline", "native revoke", "unknown keep", "late keep", "expired new deadline", "parent canceled during keep", "late fence", "keep nil", "keep header", "keep cluster", "keep id", "keep ttl zero", "keep ttl oversized", "fence unknown", "fence nil", "fence extra", "restore", "identity", "control expiry", "gate", "boot", "admission recreate together"}
	for _, key := range []string{"guard", "token", "receipt", "mutation", "placement", "control", "owner", "fence", "index"} {
		scenarios = append(scenarios, key+" rewrite", key+" recreate")
	}
	for _, scenario := range scenarios {
		t.Run(scenario, func(t *testing.T) {
			b, raw, in, keys := operationControlFixture(t, "plain")
			ctx := context.Background()
			parent, cancel := context.WithCancel(ctx)
			defer cancel()
			r, e := b.BeginOperation(parent, BeginOperationInput{SandboxID: in.SandboxID, RequestID: "loss", Kind: OperationMutation})
			require.NoError(t, e)
			c := r.Capability
			defer raw.Revoke(ctx, clientv3.LeaseID(c.Reference().LeaseID))
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			defer func() { b.client.Lease = lease.Lease }()
			renewCtx := ctx
			switch scenario {
			case "admission recreate together":
				receipt, e := encodeOperationReceipt(operationReceipt{Version: 1, Reference: c.Reference(), Outcome: OperationCommitted})
				require.NoError(t, e)
				_, e = raw.Txn(ctx).Then(clientv3.OpDelete(c.tokenKey), clientv3.OpDelete(c.receiptKey), clientv3.OpDelete(c.mutationKey)).Commit()
				require.NoError(t, e)
				id := clientv3.LeaseID(c.Reference().LeaseID)
				_, e = raw.Txn(ctx).Then(clientv3.OpPut(c.tokenKey, c.value, clientv3.WithLease(id)), clientv3.OpPut(c.receiptKey, receipt, clientv3.WithLease(id)), clientv3.OpPut(c.mutationKey, c.value, clientv3.WithLease(id))).Commit()
				require.NoError(t, e)
			case "parent canceled":
				cancel()
			case "caller canceled":
				var end context.CancelFunc
				renewCtx, end = context.WithCancel(ctx)
				end()
			case "expired deadline":
				c.deadline = time.Now().Add(-time.Millisecond)
			case "native revoke":
				_, e = raw.Revoke(ctx, clientv3.LeaseID(c.Reference().LeaseID))
				require.NoError(t, e)
			case "restore":
				_, e = raw.Put(ctx, b.restoreKey, "new-restore")
				require.NoError(t, e)
			case "identity":
				_, e = raw.Put(ctx, b.identityKey, "changed")
				require.NoError(t, e)
			case "control expiry":
				operationChangePoint(t, raw, keys[1], "expires_at", c.record.ExpiresAt.Add(time.Hour))
			case "gate":
				operationChangePoint(t, raw, keys[1], "data_gate_epoch", c.record.DataGateEpoch+1)
			case "boot":
				operationChangePoint(t, raw, keys[1], "runtime.boot_id", "changed-boot")
			}
			for i, key := range []string{c.guardKey, c.tokenKey, c.receiptKey, c.mutationKey, keys[0], keys[1], keys[2], keys[3], keys[4]} {
				name := []string{"guard", "token", "receipt", "mutation", "placement", "control", "owner", "fence", "index"}[i]
				if scenario == name+" rewrite" || scenario == name+" recreate" {
					got, e := raw.Get(ctx, key)
					require.NoError(t, e)
					if scenario == name+" recreate" {
						_, e = raw.Delete(ctx, key)
						require.NoError(t, e)
					}
					_, e = raw.Put(ctx, key, string(got.Kvs[0].Value), clientv3.WithLease(clientv3.LeaseID(got.Kvs[0].Lease)))
					require.NoError(t, e)
				}
			}
			if scenario == "late keep" || scenario == "late fence" {
				c.deadline = time.Now().Add(100 * time.Millisecond)
			}
			lease.keep = func(r *clientv3.LeaseKeepAliveResponse, e error) (*clientv3.LeaseKeepAliveResponse, error) {
				require.NoError(t, e)
				switch scenario {
				case "unknown keep":
					return nil, context.DeadlineExceeded
				case "late keep":
					time.Sleep(150 * time.Millisecond)
				case "expired new deadline":
					r.TTL = 1
					time.Sleep(1100 * time.Millisecond)
				case "parent canceled during keep":
					cancel()
				case "keep nil":
					return nil, nil
				case "keep header":
					r.ResponseHeader = nil
				case "keep cluster":
					r.ClusterId++
				case "keep id":
					r.ID++
				case "keep ttl zero":
					r.TTL = 0
				case "keep ttl oversized":
					r.TTL = 31
				}
				return r, e
			}
			b.client.KV = &faultKV{KV: b.client.KV, match: func([]clientv3.Op) bool { return true }, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
				if e != nil {
					return r, e
				}
				switch scenario {
				case "late fence":
					time.Sleep(150 * time.Millisecond)
				case "fence unknown":
					return nil, context.DeadlineExceeded
				case "fence nil":
					return nil, nil
				case "fence extra":
					r.Responses = append(r.Responses, nil)
				}
				return r, e
			}}
			require.Error(t, b.RenewOperation(renewCtx, c))
			require.True(t, c.lost)
			keeps := lease.keeps.Load()
			if !strings.Contains(scenario, "keep") && scenario != "expired new deadline" {
				require.Zero(t, keeps, "failed original fences must prevent KeepAlive")
			}
			require.Error(t, b.RenewOperation(ctx, c))
			require.Equal(t, keeps, lease.keeps.Load())
			require.Zero(t, lease.grants.Load())
		})
	}
}

func TestCancelOperationCapability(t *testing.T) {
	for _, scenario := range []string{"repeat", "missing Lease", "unknown revoke", "canceled context", "concurrent renew"} {
		t.Run(scenario, func(t *testing.T) {
			b, raw, in, _ := operationControlFixture(t, "plain")
			ctx := context.Background()
			c := admittedOperation(t, b, raw, in.SandboxID, OperationData)
			other := admittedOperation(t, b, raw, in.SandboxID, OperationData)
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			defer func() { b.client.Lease = lease.Lease }()
			cancelCtx := ctx
			if scenario == "missing Lease" {
				_, e := raw.Revoke(ctx, clientv3.LeaseID(c.Reference().LeaseID))
				require.NoError(t, e)
			}
			if scenario == "canceled context" {
				var end context.CancelFunc
				cancelCtx, end = context.WithCancel(ctx)
				end()
			}
			if scenario == "unknown revoke" {
				lease.revoke = func(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
					require.True(t, c.lost, "mark lost before sending RPC")
					require.Equal(t, clientv3.LeaseID(c.Reference().LeaseID), id)
					_, bounded := ctx.Deadline()
					require.True(t, bounded)
					_, e := lease.Lease.Revoke(ctx, id)
					require.NoError(t, e)
					return nil, context.DeadlineExceeded
				}
			}
			if scenario == "concurrent renew" {
				var group sync.WaitGroup
				for i := 0; i < 8; i++ {
					group.Add(1)
					go func() { defer group.Done(); _ = b.RenewOperation(ctx, c) }()
				}
				require.NoError(t, b.CancelOperationCapability(ctx, c))
				group.Wait()
			} else {
				e := b.CancelOperationCapability(cancelCtx, c)
				if scenario == "unknown revoke" || scenario == "canceled context" {
					require.Error(t, e)
				} else {
					require.NoError(t, e)
				}
			}
			require.True(t, c.lost)
			require.Error(t, b.RenewOperation(ctx, c))
			lease.revoke = nil
			require.NoError(t, b.CancelOperationCapability(ctx, c))
			require.NoError(t, b.CancelOperationCapability(ctx, c))
			outcome, e := b.ResolveOperation(ctx, c.Reference())
			require.NoError(t, e)
			require.Equal(t, OperationExpired, outcome, "metadata expiry is not target terminal evidence")
			require.NoError(t, b.RenewOperation(ctx, other))
			got, e := raw.Get(ctx, other.tokenKey)
			require.NoError(t, e)
			require.Len(t, got.Kvs, 1)
			require.Zero(t, lease.grants.Load())
		})
	}
}

func TestOperationLifecycleOrigin(t *testing.T) {
	b, raw, in, _ := operationControlFixture(t, "plain")
	c := admittedOperation(t, b, raw, in.SandboxID, OperationData)
	foreign, _ := integrationBackend(t)
	for _, f := range []func(context.Context, *OperationCapability) error{foreign.RenewOperation, foreign.CancelOperationCapability} {
		require.Error(t, f(context.Background(), c))
		require.Error(t, f(context.Background(), nil))
		require.Error(t, f(context.Background(), &OperationCapability{}))
	}
	require.NoError(t, b.RenewOperation(context.Background(), c))
}

// This is a structural fixed-path check with synthetic idle records, not a
// production capacity, QPS, or latency claim.
func TestOperationActiveCostIndependentOfIdleRecords(t *testing.T) {
	b, raw, in, _ := operationControlFixture(t, "plain")
	ctx := context.Background()
	lease := &creationFaultLease{Lease: b.client.Lease}
	b.client.Lease = lease
	defer func() { b.client.Lease = lease.Lease }()
	type cost struct {
		transactions, points int
		grants, keeps        int64
	}
	measure := func() cost {
		result := cost{}
		kv := b.client.KV
		b.client.KV = &faultKV{KV: kv, match: func(ops []clientv3.Op) bool {
			result.transactions++
			for _, op := range ops {
				if op.IsGet() {
					require.Empty(t, op.RangeBytes())
					result.points++
				}
			}
			return false
		}}
		g, k := lease.grants.Load(), lease.keeps.Load()
		for i := 0; i < 2; i++ {
			c := admittedOperation(t, b, raw, in.SandboxID, OperationData)
			require.NoError(t, b.RenewOperation(ctx, c))
			o, e := b.ResolveOperation(ctx, c.Reference())
			require.NoError(t, e)
			require.Equal(t, OperationCommitted, o)
			require.NoError(t, b.CancelOperationCapability(ctx, c))
		}
		b.client.KV = kv
		result.grants = lease.grants.Load() - g
		result.keeps = lease.keeps.Load() - k
		return result
	}
	before := measure()
	for batch := 0; batch < 20; batch++ {
		ops := make([]clientv3.Op, 50)
		for i := range ops {
			ops[i] = clientv3.OpPut(fmt.Sprintf("%ssynthetic-idle/%d", b.namespace.Root(), batch*50+i), `{"idle":true}`)
		}
		_, e := raw.Txn(ctx).Then(ops...).Commit()
		require.NoError(t, e)
	}
	after := measure()
	require.Equal(t, before, after)
	require.Equal(t, int64(2), after.grants)
	require.Equal(t, int64(2), after.keeps)
	t.Logf("fixed two active operations before/after 1000 synthetic idle points: %+v", after)
}

func TestCancelOperationSerializesWithInFlightRenew(t *testing.T) {
	b, raw, in, _ := operationControlFixture(t, "plain")
	ctx := context.Background()
	c := admittedOperation(t, b, raw, in.SandboxID, OperationData)
	entered, release, revoked := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unlock := func() { once.Do(func() { close(release) }) }
	defer unlock()
	lease := &creationFaultLease{Lease: b.client.Lease}
	lease.keep = func(r *clientv3.LeaseKeepAliveResponse, e error) (*clientv3.LeaseKeepAliveResponse, error) {
		close(entered)
		<-release
		return r, e
	}
	lease.revoke = func(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
		require.True(t, c.lost)
		require.Equal(t, clientv3.LeaseID(c.Reference().LeaseID), id)
		close(revoked)
		return lease.Lease.Revoke(ctx, id)
	}
	b.client.Lease = lease
	defer func() { b.client.Lease = lease.Lease }()
	renewal, cancellation := make(chan error, 1), make(chan error, 1)
	go func() { renewal <- b.RenewOperation(ctx, c) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("renew not intercepted")
	}
	go func() { cancellation <- b.CancelOperationCapability(ctx, c) }()
	select {
	case <-revoked:
		t.Fatal("cancel RPC raced the locked renewal")
	case <-time.After(30 * time.Millisecond):
	}
	unlock()
	require.NoError(t, <-renewal)
	require.NoError(t, <-cancellation)
	require.True(t, c.lost)
	require.Error(t, b.RenewOperation(ctx, c))
	require.Equal(t, int64(1), lease.keeps.Load())
}

func TestOperationLifecycleReplyAndInputValidation(t *testing.T) {
	for _, scenario := range []string{"renew nil context", "cancel nil context", "revoke nil", "revoke header", "revoke cluster", "revoke revision"} {
		t.Run(scenario, func(t *testing.T) {
			b, raw, in, _ := operationControlFixture(t, "plain")
			ctx := context.Background()
			c := admittedOperation(t, b, raw, in.SandboxID, OperationData)
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			defer func() { b.client.Lease = lease.Lease }()
			if strings.HasPrefix(scenario, "revoke") {
				lease.revoke = func(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
					r, e := lease.Lease.Revoke(ctx, id)
					require.NoError(t, e)
					switch scenario {
					case "revoke nil":
						return nil, nil
					case "revoke header":
						r.Header = nil
					case "revoke cluster":
						r.Header.ClusterId++
					case "revoke revision":
						r.Header.Revision = 0
					}
					return r, e
				}
			}
			if scenario == "renew nil context" {
				require.Error(t, b.RenewOperation(nil, c))
			} else if scenario == "cancel nil context" {
				require.Error(t, b.CancelOperationCapability(nil, c))
			} else {
				require.ErrorIs(t, b.CancelOperationCapability(ctx, c), ErrOutcomeUnknown)
			}
			require.True(t, c.lost)
			require.Error(t, b.RenewOperation(ctx, c))
		})
	}
}
