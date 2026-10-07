package etcd

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestTaskDestroyUnknown(t *testing.T) {
	t.Run("lost-committed-reply", func(t *testing.T) {
		b, _, in, keys, ctx := taskDestroyFixture(t)
		prior, e := b.loadOperationControl(ctx, in.SandboxID)
		require.NoError(t, e)
		s, r := taskDestroyPrepare(t, b, ctx, in)
		b.client.KV = &faultKV{KV: b.client.KV, match: putsKey(keys[1]), after: func(response *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
			require.NoError(t, e)
			require.True(t, response.Succeeded)
			return nil, context.DeadlineExceeded
		}}
		out, e := b.CommitStage(ctx, s)
		require.ErrorIs(t, e, ErrOutcomeUnknown)
		require.Equal(t, OutcomeUnknown, out)
		require.NoError(t, b.ReleaseStage(ctx, s))
		out, e = b.ResolveStage(ctx, s.Reference())
		require.NoError(t, e)
		require.Equal(t, OutcomeCommitted, out)
		taskDestroyAssertBirth(t, b, ctx, s, r, prior)
	})
	t.Run("delayed-commit-after-abort", func(t *testing.T) {
		b, raw, in, keys, ctx := taskDestroyFixture(t)
		s, r := taskDestroyPrepare(t, b, ctx, in)
		arrived, release, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var once sync.Once
		unlock := func() { once.Do(func() { close(release) }) }
		b.client.KV = &faultKV{KV: b.client.KV, match: putsKey(keys[1]), before: func() {
			close(arrived)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}}
		type result struct {
			out Outcome
			err error
		}
		done := make(chan result, 1)
		go func() { defer close(joined); o, e := b.CommitStage(ctx, s); done <- result{o, e} }()
		t.Cleanup(func() {
			unlock()
			select {
			case <-joined:
			case <-time.After(5 * time.Second):
				t.Error("owned delayed commit did not join")
			}
		})
		select {
		case <-arrived:
		case <-time.After(2 * time.Second):
			t.Fatal("commit did not reach bounded pre-dispatch pause")
		}
		// Exact absence is diagnostic only; it does not authorize an abort assertion.
		absent, e := b.LoadTask(ctx, r)
		require.NoError(t, e)
		require.Nil(t, absent)
		taskDestroyAssertAbsent(t, ctx, raw, []string{s.receiptKey})
		out, e := b.ResolveStage(ctx, s.Reference())
		require.NoError(t, e)
		require.Equal(t, OutcomeAborted, out)
		unlock()
		select {
		case result := <-done:
			require.NoError(t, result.err)
			require.Equal(t, OutcomeAborted, result.out)
		case <-time.After(5 * time.Second):
			t.Fatal("delayed commit did not finish")
		}
		taskDestroyAssertAbsent(t, ctx, raw, taskDestroyKeys(t, b, r))
		control, e := b.LoadControl(ctx, r.Partition, r.SandboxID)
		require.NoError(t, e)
		require.Equal(t, PhaseActive, control.Phase)
	})
}

func TestTaskDestroyFences(t *testing.T) {
	for i, name := range []string{"placement", "control", "owner", "fence", "index"} {
		for _, change := range []string{"value", "revision", "recreation", "lease"} {
			t.Run(name+"/"+change, func(t *testing.T) {
				b, raw, in, keys, ctx := taskDestroyFixture(t)
				s, r := taskDestroyPrepare(t, b, ctx, in)
				before, e := raw.Get(ctx, keys[i])
				require.NoError(t, e)
				require.Len(t, before.Kvs, 1)
				value := string(before.Kvs[0].Value)
				var options []clientv3.OpOption
				switch change {
				case "value":
					value += " "
				case "recreation":
					_, e = raw.Delete(ctx, keys[i])
					require.NoError(t, e)
				case "lease":
					lease, e := raw.Grant(ctx, 30)
					require.NoError(t, e)
					options = []clientv3.OpOption{clientv3.WithLease(lease.ID)}
					t.Cleanup(func() {
						c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						_, e := raw.Revoke(c, lease.ID)
						require.NoError(t, e)
					})
				}
				_, e = raw.Put(ctx, keys[i], value, options...)
				require.NoError(t, e)
				current, e := raw.Get(ctx, keys[1])
				require.NoError(t, e)
				out, e := b.CommitStage(ctx, s)
				require.ErrorIs(t, e, ErrConflict)
				require.Equal(t, OutcomeUnknown, out)
				taskDestroyAssertAbsent(t, ctx, raw, append(taskDestroyKeys(t, b, r), s.receiptKey))
				after, e := raw.Get(ctx, keys[1])
				require.NoError(t, e)
				require.Equal(t, current.Kvs, after.Kvs, "failed CAS must not mutate control")
			})
		}
	}
	for _, scenario := range []string{"task-present", "intent-present", "link-present", "checkpoint-present", "restore", "guard-recreation", "guard-revocation", "receipt-aborted"} {
		t.Run(scenario, func(t *testing.T) {
			b, raw, in, keys, ctx := taskDestroyFixture(t)
			s, r := taskDestroyPrepare(t, b, ctx, in)
			taskKeys := taskDestroyKeys(t, b, r)
			before, e := raw.Get(ctx, keys[1])
			require.NoError(t, e)
			occupied := ""
			for i, name := range []string{"task", "intent", "link", "checkpoint"} {
				if scenario == name+"-present" {
					occupied = taskKeys[i]
					_, e = raw.Put(ctx, occupied, "{}")
					require.NoError(t, e)
				}
			}
			want := ErrConflict
			switch scenario {
			case "restore":
				_, e = raw.Put(ctx, b.restoreKey, "new-restore")
				require.NoError(t, e)
				want = ErrIdentityMismatch
			case "guard-recreation":
				_, e = raw.Delete(ctx, s.guardKey)
				require.NoError(t, e)
				_, e = raw.Put(ctx, s.guardKey, s.guardValue, clientv3.WithLease(s.leaseID))
				require.NoError(t, e)
				want = ErrGuardExpired
			case "guard-revocation":
				require.NoError(t, b.ReleaseStage(ctx, s))
				want = ErrGuardExpired
			case "receipt-aborted":
				value, e := encodeReceipt(s.Reference(), OutcomeAborted)
				require.NoError(t, e)
				_, e = raw.Put(ctx, s.receiptKey, value)
				require.NoError(t, e)
			}
			out, e := b.CommitStage(ctx, s)
			if scenario == "receipt-aborted" {
				require.NoError(t, e)
				require.Equal(t, OutcomeAborted, out)
			} else {
				require.ErrorIs(t, e, want)
				require.Equal(t, OutcomeUnknown, out)
			}
			for _, key := range taskKeys {
				if key != occupied {
					taskDestroyAssertAbsent(t, ctx, raw, []string{key})
				}
			}
			after, e := raw.Get(ctx, keys[1])
			require.NoError(t, e)
			require.Equal(t, before.Kvs, after.Kvs)
			if strings.HasSuffix(scenario, "-present") {
				got, e := raw.Get(ctx, occupied)
				require.NoError(t, e)
				require.Equal(t, "{}", string(got.Kvs[0].Value), "existing record must not be overwritten")
			}
		})
	}
}
