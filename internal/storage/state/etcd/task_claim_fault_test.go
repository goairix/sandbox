package etcd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// All fault responses wrap a real native RPC. They alter only its delivered
// reply; no fabricated Lease or capability substitutes for the fixture.
func TestTaskClaimFences(t *testing.T) {
	names := []string{"task", "intent", "link", "placement", "control", "owner", "fence", "index", "claim", "guard"}
	for index, name := range names {
		for _, kind := range []string{"value", "rewrite", "lease", "recreate"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				b, raw, ref, ctx := taskClaimFixture(t)
				c, e := b.ClaimTask(ctx, ref, "worker", 30*time.Second)
				require.NoError(t, e)
				defer taskReleaseClaim(t, b, c)
				key := c.claimKey
				if index < 8 {
					key = c.fences[index].key
				}
				if index == 9 {
					key = c.guardKey
				}
				before, e := raw.Get(ctx, key)
				require.NoError(t, e)
				require.Len(t, before.Kvs, 1)
				old := before.Kvs[0]
				lease := &creationFaultLease{Lease: b.client.Lease}
				b.client.Lease = lease
				value := string(old.Value)
				id := clientv3.LeaseID(old.Lease)
				switch kind {
				case "value":
					value += " "
				case "lease":
					grant, e := raw.Grant(ctx, 30)
					require.NoError(t, e)
					id = grant.ID
					defer raw.Revoke(ctx, id)
				case "recreate":
					_, e = raw.Delete(ctx, key)
					require.NoError(t, e)
				}
				_, e = raw.Put(ctx, key, value, clientv3.WithLease(id))
				require.NoError(t, e)
				require.Error(t, b.RenewTaskClaim(ctx, c))
				require.Zero(t, lease.keeps.Load())
				require.True(t, c.lost)
				_, e = raw.Put(ctx, key, string(old.Value), clientv3.WithLease(clientv3.LeaseID(old.Lease)))
				require.NoError(t, e)
				require.Error(t, b.RenewTaskClaim(ctx, c))
				require.Zero(t, lease.keeps.Load())
				require.Zero(t, lease.grants.Load())
			})
		}
	}
	t.Run("restore", func(t *testing.T) {
		b, raw, ref, ctx := taskClaimFixture(t)
		c, e := b.ClaimTask(ctx, ref, "worker", 30*time.Second)
		require.NoError(t, e)
		defer taskReleaseClaim(t, b, c)
		before, e := raw.Get(ctx, b.restoreKey)
		require.NoError(t, e)
		require.Len(t, before.Kvs, 1)
		_, e = raw.Put(ctx, b.restoreKey, "changed")
		require.NoError(t, e)
		require.Error(t, b.RenewTaskClaim(ctx, c))
		_, e = raw.Put(ctx, b.restoreKey, string(before.Kvs[0].Value))
		require.NoError(t, e)
		require.Error(t, b.RenewTaskClaim(ctx, c))
	})
	t.Run("post-renew-fence", func(t *testing.T) {
		b, raw, ref, ctx := taskClaimFixture(t)
		c, e := b.ClaimTask(ctx, ref, "worker", 30*time.Second)
		require.NoError(t, e)
		defer taskReleaseClaim(t, b, c)
		lease := &creationFaultLease{Lease: b.client.Lease, keep: func(r *clientv3.LeaseKeepAliveResponse, e error) (*clientv3.LeaseKeepAliveResponse, error) {
			require.NoError(t, e)
			_, e = raw.Put(ctx, c.fences[5].key, c.fences[5].value)
			require.NoError(t, e)
			return r, nil
		}}
		b.client.Lease = lease
		require.Error(t, b.RenewTaskClaim(ctx, c))
		require.EqualValues(t, 1, lease.keeps.Load())
		require.Error(t, b.RenewTaskClaim(ctx, c))
		require.EqualValues(t, 1, lease.keeps.Load())
	})
	for index, name := range names[:8] {
		t.Run("admission/"+name, func(t *testing.T) {
			b, raw, ref, ctx := taskClaimFixture(t)
			bundle, e := b.loadTaskClaimBundle(ctx, ref)
			require.NoError(t, e)
			_, e = raw.Put(ctx, bundle.fences[index].key, "{}")
			require.NoError(t, e)
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			c, e := b.ClaimTask(ctx, ref, "worker", 30*time.Second)
			require.Error(t, e)
			require.Nil(t, c)
			require.Zero(t, lease.grants.Load())
		})
	}
}

func TestTaskClaimUnknown(t *testing.T) {
	for _, kind := range []string{"error", "nil", "header", "cluster", "revision", "id", "ttl-zero", "ttl-excess", "error-field", "cancel", "delayed"} {
		t.Run("grant/"+kind, func(t *testing.T) {
			b, raw, ref, parent := taskClaimFixture(t)
			ctx, cancel := context.WithCancel(parent)
			defer cancel()
			if kind == "delayed" {
				b.requestTimeout = 10 * time.Second
			}
			var original clientv3.LeaseID
			lease := &creationFaultLease{Lease: b.client.Lease, grant: func(r *clientv3.LeaseGrantResponse, e error) (*clientv3.LeaseGrantResponse, error) {
				require.NoError(t, e)
				original = r.ID
				switch kind {
				case "error":
					return r, errors.New("lost native Grant reply")
				case "nil":
					return nil, nil
				case "header":
					r.ResponseHeader = nil
				case "cluster":
					r.ClusterId++
				case "revision":
					r.Revision = 0
				case "id":
					r.ID = 0
				case "ttl-zero":
					r.TTL = 0
				case "ttl-excess":
					r.TTL = 86401
				case "error-field":
					r.Error = "malformed"
				case "cancel":
					cancel()
				case "delayed":
					require.Less(t, r.TTL, int64(8), "owned fixture native minimum must fit bounded test window")
					time.Sleep(time.Duration(r.TTL)*time.Second + 10*time.Millisecond)
					require.NoError(t, ctx.Err())
				}
				return r, nil
			}}
			b.client.Lease = lease
			ttl := 30 * time.Second
			if kind == "delayed" {
				ttl = time.Second
			}
			c, e := b.ClaimTask(ctx, ref, "worker", ttl)
			require.Error(t, e)
			if kind == "delayed" {
				require.ErrorIs(t, e, ErrGuardExpired)
			}
			require.Nil(t, c)
			require.EqualValues(t, 1, lease.grants.Load())
			key, e := b.namespace.taskClaimKey(ref)
			require.NoError(t, e)
			v, e := raw.Get(parent, key)
			require.NoError(t, e)
			require.Empty(t, v.Kvs)
			// A hidden or malformed server identity cannot authorize automatic cleanup;
			// this trusted fixture separately releases its independently retained Lease.
			if original != 0 {
				_, e = raw.Revoke(parent, original)
				require.True(t, e == nil || errors.Is(e, rpctypes.ErrLeaseNotFound), "original fixture cleanup: %v", e)
			}
		})
	}
	for _, kind := range []string{"error", "nil", "header", "cluster", "revision", "id", "ttl-zero", "ttl-excess", "cancel", "delayed"} {
		t.Run("keep/"+kind, func(t *testing.T) {
			b, _, ref, parent := taskClaimFixture(t)
			ctx, cancel := context.WithCancel(parent)
			defer cancel()
			if kind == "delayed" {
				b.requestTimeout = 10 * time.Second
			}
			ttl := 30 * time.Second
			if kind == "delayed" {
				ttl = time.Second
			}
			c, e := b.ClaimTask(ctx, ref, "worker", ttl)
			require.NoError(t, e)
			defer taskReleaseClaim(t, b, c)
			deadline := c.deadline
			lease := &creationFaultLease{Lease: b.client.Lease, keep: func(r *clientv3.LeaseKeepAliveResponse, e error) (*clientv3.LeaseKeepAliveResponse, error) {
				require.NoError(t, e)
				switch kind {
				case "error":
					return r, errors.New("lost native KeepAlive reply")
				case "nil":
					return nil, nil
				case "header":
					r.ResponseHeader = nil
				case "cluster":
					r.ClusterId++
				case "revision":
					r.Revision = 0
				case "id":
					r.ID++
				case "ttl-zero":
					r.TTL = 0
				case "ttl-excess":
					r.TTL = 86401
				case "cancel":
					cancel()
				case "delayed":
					require.Less(t, time.Until(deadline), 8*time.Second)
					time.Sleep(time.Until(deadline) + 10*time.Millisecond)
					require.NoError(t, ctx.Err())
				}
				return r, nil
			}}
			b.client.Lease = lease
			e = b.RenewTaskClaim(ctx, c)
			require.Error(t, e)
			if kind == "delayed" {
				require.ErrorIs(t, e, ErrGuardExpired)
			}
			require.True(t, c.lost)
			require.Equal(t, deadline, c.deadline)
			require.Error(t, b.RenewTaskClaim(parent, c))
			require.EqualValues(t, 1, lease.keeps.Load())
			require.Zero(t, lease.grants.Load())
		})
	}
	for _, kind := range []string{"lost-reply", "bad-put", "bad-header", "cancel"} {
		t.Run("claim-txn/"+kind, func(t *testing.T) {
			b, raw, ref, parent := taskClaimFixture(t)
			ctx, cancel := context.WithCancel(parent)
			defer cancel()
			if kind == "delayed" {
				b.requestTimeout = 10 * time.Second
			}
			key, e := b.namespace.taskClaimKey(ref)
			require.NoError(t, e)
			kv := b.client.KV
			b.client.KV = &faultKV{KV: kv, match: putsKey(key), after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
				require.NoError(t, e)
				require.True(t, r.Succeeded)
				switch kind {
				case "lost-reply":
					return nil, errors.New("lost committed native reply")
				case "bad-put":
					r.Responses = nil
				case "bad-header":
					r.Header = nil
				case "cancel":
					cancel()
				}
				return r, nil
			}}
			c, e := b.ClaimTask(ctx, ref, "worker", 30*time.Second)
			require.Error(t, e)
			require.Nil(t, c)
			b.client.KV = kv
			v, e := raw.Get(parent, key)
			require.NoError(t, e)
			require.Empty(t, v.Kvs, "known original Lease is revoked after failed capability delivery")
		})
	}
	t.Run("release-unknown-is-cleanup-only", func(t *testing.T) {
		b, raw, ref, ctx := taskClaimFixture(t)
		c, e := b.ClaimTask(ctx, ref, "worker", 30*time.Second)
		require.NoError(t, e)
		original := b.client.Lease
		b.client.Lease = &creationFaultLease{Lease: original, revoke: func(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
			_, e := original.Revoke(ctx, id)
			require.NoError(t, e)
			return nil, errors.New("lost revoke reply")
		}}
		require.ErrorIs(t, b.ReleaseTaskClaim(ctx, c), ErrOutcomeUnknown)
		require.Error(t, b.RenewTaskClaim(ctx, c))
		b.client.Lease = original
		for _, f := range c.fences {
			v, e := raw.Get(ctx, f.key)
			require.NoError(t, e)
			require.Len(t, v.Kvs, 1)
			require.True(t, f.matches(v.Kvs[0]))
		}
	})
}
