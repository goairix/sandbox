package etcd

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func duePageRangeOps(ops []clientv3.Op) bool {
	for _, op := range ops {
		if op.IsGet() && len(op.RangeBytes()) > 0 {
			return true
		}
	}
	return false
}

func TestDuePagePhysicalJoin(t *testing.T) {
	for _, action := range []string{"partition", "share", "session", "Close", "caller"} {
		t.Run(action, func(t *testing.T) {
			f := newDuePageFixture(t)
			f.seed(t, 1, f.now)
			f.earn()
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			returned := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			original := f.b.client.KV
			f.b.client.KV = &faultKV{KV: original, match: duePageRangeOps, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				entered <- struct{}{}
				<-release
				close(returned)
				return r, err
			}}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			type result struct {
				page DuePage
				err  error
			}
			done := make(chan result, 1)
			go func() { page, err := f.b.ReadDuePage(ctx, f.partition, f.share, nil); done <- result{page, err} }()
			workerTestReceive(t, entered)
			_, err := f.b.ReadDuePage(context.Background(), f.partition, f.share, nil)
			require.ErrorIs(t, err, ErrConflict, "one actual original Range per share")
			joined := make(chan error, 1)
			go func() {
				switch action {
				case "partition":
					joined <- f.b.ReleaseWorkerPartition(context.Background(), f.partition)
				case "share":
					joined <- f.b.ReleaseWorkerBudget(context.Background(), f.share)
				case "session":
					joined <- f.b.ReleaseWorkerSession(context.Background(), f.session)
				case "Close":
					joined <- f.b.Close()
				case "caller":
					cancel()
					joined <- nil
				}
			}()
			if action == "caller" {
				require.NoError(t, workerTestReceive(t, joined))
			} else {
				select {
				case err := <-joined:
					t.Fatalf("%s returned before actual Range returned: %v", action, err)
				case <-time.After(20 * time.Millisecond):
				}
			}
			select {
			case <-done:
				t.Fatal("cancellation cannot pretend the physical Range returned")
			default:
			}
			unblock()
			workerTestReceive(t, returned)
			resultValue := workerTestReceive(t, done)
			require.Error(t, resultValue.err)
			require.Empty(t, resultValue.page.Candidates)
			require.Nil(t, resultValue.page.Next)
			if action != "caller" {
				if action == "partition" || action == "share" {
					require.ErrorIs(t, workerTestReceive(t, joined), ErrGuardExpired, "unknown dispatched native call loses the original session after its physical join")
				} else {
					require.NoError(t, workerTestReceive(t, joined))
				}
			}
		})
	}
}

func TestDuePageNoRefundOnUnknown(t *testing.T) {
	f := newDuePageFixture(t)
	f.seed(t, 1, f.now)
	f.earn()
	f.b.client.KV = &faultKV{KV: f.b.client.KV, match: duePageRangeOps, after: func(*clientv3.TxnResponse, error) (*clientv3.TxnResponse, error) {
		return nil, context.DeadlineExceeded
	}}
	_, err := f.b.ReadDuePage(context.Background(), f.partition, f.share, nil)
	require.Error(t, err)
	f.b.archiveLife.mu.Lock()
	tokens := f.share.bucket.tokens
	f.b.archiveLife.mu.Unlock()
	require.Zero(t, tokens, "unknown native result cannot refund its original worst-case chunk")
}
