package etcd

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// These rows are discovery hints only. The existing bounded gRPC fixture
// supplies original worker authority and snapshots; no Task is executable.
func pendingPageSeed(t *testing.T, f *duePageFixture, first, count int) []cleanupPendingRecord {
	t.Helper()
	rows := make([]cleanupPendingRecord, 0, count)
	for i := first; i < first+count; i++ {
		row := cleanupPendingRecord{Version: 1, Reference: TaskReference{
			Namespace: f.b.namespace.Root(), RestoreEpoch: f.b.restoreEpoch,
			TaskID:    fmt.Sprintf("%08x-1111-4111-8111-111111111111", i+1),
			SandboxID: fmt.Sprintf("sandbox-%03d", i), Partition: 0xaa,
		}, TaskDigest: strings.Repeat("a", 64)}
		key, err := f.b.namespace.cleanupPendingKey(row.Reference)
		require.NoError(t, err)
		value, err := encodeCleanupPending(row)
		require.NoError(t, err)
		f.server.put(key, value, 0)
		rows = append(rows, row)
	}
	return rows
}

// Freeze replenishment while examining admission/refund behavior. The
// original bucket, share and native guards are still used by every read.
func pendingPageCredit(f *duePageFixture, tokens uint64) {
	f.b.archiveLife.mu.Lock()
	f.share.bucket.last = time.Now().Add(time.Hour)
	f.share.bucket.tokens = tokens
	f.share.bucket.remainder = 0
	f.b.archiveLife.mu.Unlock()
}

func pendingPageRequests(f *duePageFixture) []*pb.TxnRequest {
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	return append([]*pb.TxnRequest(nil), f.server.requests...)
}

func TestCleanupPendingPage(t *testing.T) {
	for _, count := range []int{0, 1, 80, 81} {
		t.Run(fmt.Sprintf("rows-%d", count), func(t *testing.T) {
			f := newDuePageFixture(t)
			rows := pendingPageSeed(t, f, 0, count)
			f.earn()
			page, err := f.b.readCleanupPendingPage(context.Background(), f.partition, f.share, nil)
			require.NoError(t, err)
			require.Greater(t, page.Revision, int64(0))
			require.Zero(t, page.Rejected)
			require.Equal(t, count > 80, page.Next != nil)
			if count == 0 {
				require.Empty(t, page.Candidates)
			} else {
				require.Equal(t, rows[:min(count, 80)], page.Candidates)
			}
		})
	}
	t.Run("fixed-revision-insert-delete", func(t *testing.T) {
		f := newDuePageFixture(t)
		rows := pendingPageSeed(t, f, 0, 81)
		f.earn()
		first, err := f.b.readCleanupPendingPage(context.Background(), f.partition, f.share, nil)
		require.NoError(t, err)
		require.NotNil(t, first.Next)
		key, err := f.b.namespace.cleanupPendingKey(rows[80].Reference)
		require.NoError(t, err)
		_, err = f.b.client.Txn(context.Background()).Then(clientv3.OpDelete(key)).Commit()
		require.NoError(t, err)
		pendingPageSeed(t, f, 81, 1)
		f.earn()
		last, err := f.b.readCleanupPendingPage(context.Background(), f.partition, f.share, first.Next)
		require.NoError(t, err)
		require.Equal(t, first.Revision, last.Revision)
		require.Equal(t, rows[80:], last.Candidates, "deletion and insertion after R cannot alter this view")
		require.Nil(t, last.Next)
	})
	for _, rejected := range []int{1, 80} {
		t.Run(fmt.Sprintf("rejected-prefix-%d", rejected), func(t *testing.T) {
			f := newDuePageFixture(t)
			rows := pendingPageSeed(t, f, 0, 81)
			// Mutation stays inside a well-formed MVCC envelope. The page
			// must distinguish a bad hint from a broken Range response.
			f.mutate = func(_ *pb.RangeRequest, r *pb.RangeResponse) {
				if len(r.Kvs) == 80 {
					for _, kv := range r.Kvs[:rejected] {
						kv.Value = []byte(`{}`)
					}
				}
			}
			f.earn()
			first, err := f.b.readCleanupPendingPage(context.Background(), f.partition, f.share, nil)
			require.NoError(t, err)
			require.Equal(t, uint32(rejected), first.Rejected)
			require.Len(t, first.Candidates, 80-rejected)
			if rejected < 80 {
				require.Equal(t, rows[rejected:80], first.Candidates)
			}
			require.NotNil(t, first.Next, "all rejected rows with More must still advance")
			f.earn()
			last, err := f.b.readCleanupPendingPage(context.Background(), f.partition, f.share, first.Next)
			require.NoError(t, err)
			require.Equal(t, rows[80:], last.Candidates)
			require.Nil(t, last.Next)
		})
	}
	t.Run("bounded-invalid-records-and-owned-results", func(t *testing.T) {
		f := newDuePageFixture(t)
		rows := pendingPageSeed(t, f, 0, 5)
		f.mutate = func(_ *pb.RangeRequest, r *pb.RangeResponse) {
			r.Kvs[0].Value = append(r.Kvs[0].Value, ' ')
			r.Kvs[1].Lease = 1
			r.Kvs[2].Version = 2
			r.Kvs[3].Value = bytes.ReplaceAll(r.Kvs[3].Value, []byte(f.b.restoreEpoch), []byte("foreign-epoch"))
		}
		var retained *mvccpb.KeyValue
		f.b.client.KV = &faultKV{KV: f.b.client.KV, match: duePageRangeOps, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
			if err == nil {
				retained = r.Responses[0].GetResponseRange().Kvs[4]
			}
			return r, err
		}}
		f.earn()
		page, err := f.b.readCleanupPendingPage(context.Background(), f.partition, f.share, nil)
		require.NoError(t, err)
		require.Equal(t, uint32(4), page.Rejected)
		require.Equal(t, rows[4:], page.Candidates)
		for i := range retained.Value {
			retained.Value[i] = 'x'
		}
		require.Equal(t, rows[4:], page.Candidates, "returned records must own their decoded bytes")
	})
}

func TestCleanupPendingPageOwnership(t *testing.T) {
	t.Run("credit-originals-and-one-use", func(t *testing.T) {
		f := newDuePageFixture(t)
		pendingPageSeed(t, f, 0, 81)
		pendingPageCredit(f, 0)
		before := len(pendingPageRequests(f))
		_, err := f.b.readCleanupPendingPage(context.Background(), f.partition, f.share, nil)
		require.ErrorIs(t, err, ErrConflict)
		require.Len(t, pendingPageRequests(f), before)
		f.earn()
		page, err := f.b.readCleanupPendingPage(context.Background(), f.partition, f.share, nil)
		require.NoError(t, err)
		require.NotNil(t, page.Next)
		otherShare, err := f.b.AcquireWorkerBudget(context.Background(), f.session, WorkerBudgetRange, 1)
		require.NoError(t, err)
		otherPartition, err := f.b.ClaimWorkerPartition(context.Background(), f.session, 0xab)
		require.NoError(t, err)
		foreign := newDuePageFixture(t)
		copy := *page.Next
		pendingPageCredit(f, 262144)
		before = len(pendingPageRequests(f))
		for _, attempt := range []func() error{
			func() error {
				_, err := f.b.readCleanupPendingPage(context.Background(), f.partition, f.share, &copy)
				return err
			},
			func() error {
				_, err := f.b.readCleanupPendingPage(context.Background(), f.partition, otherShare, page.Next)
				return err
			},
			func() error {
				_, err := f.b.readCleanupPendingPage(context.Background(), otherPartition, f.share, page.Next)
				return err
			},
			func() error {
				_, err := foreign.b.readCleanupPendingPage(context.Background(), foreign.partition, foreign.share, page.Next)
				return err
			},
		} {
			require.Error(t, attempt())
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = f.b.readCleanupPendingPage(ctx, f.partition, f.share, page.Next)
		require.ErrorIs(t, err, context.Canceled)
		require.Len(t, pendingPageRequests(f), before)
		f.b.archiveLife.mu.Lock()
		tokens := f.share.bucket.tokens
		f.b.archiveLife.mu.Unlock()
		require.Equal(t, uint64(262144), tokens, "local refusals must not spend credit")
		pendingPageCredit(f, 0)
		_, err = f.b.readCleanupPendingPage(context.Background(), f.partition, f.share, page.Next)
		require.ErrorIs(t, err, ErrConflict)
		pendingPageCredit(f, 262144)
		last, err := f.b.readCleanupPendingPage(context.Background(), f.partition, f.share, page.Next)
		require.NoError(t, err, "no-credit/canceled attempts preserve the original cursor")
		require.Len(t, last.Candidates, 1)
		f.b.archiveLife.mu.Lock()
		tokens = f.share.bucket.tokens
		f.b.archiveLife.mu.Unlock()
		require.Zero(t, tokens, "a short successful page still spends all 256 KiB")
		pendingPageCredit(f, 262144)
		before = len(pendingPageRequests(f))
		_, err = f.b.readCleanupPendingPage(context.Background(), f.partition, f.share, page.Next)
		require.Error(t, err)
		require.Len(t, pendingPageRequests(f), before, "a dispatched cursor is never replayable")
	})
	t.Run("relist-pre-and-post-guards", func(t *testing.T) {
		f := newDuePageFixture(t)
		pendingPageSeed(t, f, 0, 1)
		relist, err := f.b.AcquireWorkerBudget(context.Background(), f.session, WorkerBudgetRelist, 0)
		require.NoError(t, err)
		before := len(pendingPageRequests(f))
		f.earn()
		err = f.b.RunWorkerRelist(context.Background(), f.partition, relist, func(ctx context.Context) error {
			_, readErr := f.b.readCleanupPendingPage(ctx, f.partition, f.share, nil)
			return readErr
		})
		require.NoError(t, err)
		requests := pendingPageRequests(f)[before:]
		require.Len(t, requests, 3, "original Relist guards surround the actual pending Range")
		for i, request := range requests {
			require.Len(t, request.Compare, 20)
			require.Len(t, request.Success, 1)
			require.Len(t, request.Failure, 4)
			r := request.Success[0].GetRequestRange()
			require.NotNil(t, r)
			require.Equal(t, i == 1, len(r.RangeEnd) > 0)
		}
	})
	t.Run("close-joins-actual-page-and-relist-return", func(t *testing.T) {
		f := newDuePageFixture(t)
		pendingPageSeed(t, f, 0, 1)
		relist, err := f.b.AcquireWorkerBudget(context.Background(), f.session, WorkerBudgetRelist, 0)
		require.NoError(t, err)
		entered := make(chan struct{}, 1)
		release := make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		t.Cleanup(unblock)
		f.b.client.KV = &faultKV{KV: f.b.client.KV, match: duePageRangeOps, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
			entered <- struct{}{}
			<-release // Hold actual client return, even after its context is canceled.
			return r, err
		}}
		owned := make(chan context.Context, 1)
		done := make(chan error, 1)
		f.earn()
		go func() {
			done <- f.b.RunWorkerRelist(context.Background(), f.partition, relist, func(ctx context.Context) error {
				owned <- ctx
				_, err := f.b.readCleanupPendingPage(ctx, f.partition, f.share, nil)
				return err
			})
		}()
		ctx := workerTestReceive(t, owned)
		workerTestReceive(t, entered)
		closed := make(chan error, 1)
		go func() { closed <- f.b.Close() }()
		workerTestReceive(t, ctx.Done())
		f.b.archiveLife.mu.Lock()
		pageHeld, relistHeld := f.share.dueOwner != nil, relist.owner != nil
		f.b.archiveLife.mu.Unlock()
		require.True(t, pageHeld)
		require.True(t, relistHeld)
		select {
		case err := <-closed:
			t.Fatalf("Close returned before the native page returned: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
		unblock()
		require.Error(t, workerTestReceive(t, done))
		require.NoError(t, workerTestReceive(t, closed))
	})
}

func TestCleanupPendingPageWire(t *testing.T) {
	t.Run("native-shape-and-padded-bound", func(t *testing.T) {
		f := newDuePageFixture(t)
		rows := pendingPageSeed(t, f, 0, 81)
		f.b.archiveLife.mu.Lock()
		cmps, err := f.b.workerComparisons(f.session, f.partition, f.share)
		f.b.archiveLife.mu.Unlock()
		require.NoError(t, err)
		prefix := f.b.namespace.Root() + "p/aa/cleanup-pending/"
		end := clientv3.GetPrefixRangeEnd(prefix)
		require.Equal(t, 80, cleanupPendingPageEntries)
		require.Equal(t, 260096, 4096+80*(1024+2048+128))
		require.NoError(t, f.b.preflightCleanupPendingPage(cmps, prefix, end, math.MaxInt64, f.session))
		oversize := append([]clientv3.Cmp(nil), cmps...)
		oversize[0] = clientv3.Compare(clientv3.Value(f.b.identityKey), "=", strings.Repeat("x", 128*1024))
		require.Error(t, f.b.preflightCleanupPendingPage(oversize, prefix, end, math.MaxInt64, f.session))
		before := len(pendingPageRequests(f))
		f.earn()
		first, err := f.b.readCleanupPendingPage(context.Background(), f.partition, f.share, nil)
		require.NoError(t, err)
		f.earn()
		_, err = f.b.readCleanupPendingPage(context.Background(), f.partition, f.share, first.Next)
		require.NoError(t, err)
		requests := pendingPageRequests(f)[before:]
		require.Len(t, requests, 2)
		lastKey, err := f.b.namespace.cleanupPendingKey(rows[79].Reference)
		require.NoError(t, err)
		for i, request := range requests {
			require.Equal(t, 25, len(request.Compare)+len(request.Success)+len(request.Failure))
			require.Len(t, request.Compare, 20)
			require.Equal(t, taskProtoComparisons(cmps), request.Compare, "every original worker fence remains on the actual Range")
			require.Len(t, request.Success, 1)
			require.Len(t, request.Failure, 4)
			require.LessOrEqual(t, request.Size(), 128*1024)
			r := request.Success[0].GetRequestRange()
			require.Equal(t, int64(80), r.Limit)
			require.Equal(t, end, string(r.RangeEnd))
			require.Equal(t, pb.RangeRequest_NONE, r.SortOrder)
			require.Equal(t, pb.RangeRequest_KEY, r.SortTarget)
			require.False(t, r.Serializable)
			require.False(t, r.KeysOnly)
			require.False(t, r.CountOnly)
			require.Zero(t, r.MinModRevision)
			require.Zero(t, r.MaxModRevision)
			require.Zero(t, r.MinCreateRevision)
			require.Zero(t, r.MaxCreateRevision)
			if i == 0 {
				require.Equal(t, prefix, string(r.Key))
				require.Zero(t, r.Revision)
			} else {
				require.Equal(t, lastKey+"\x00", string(r.Key))
				require.Equal(t, first.Revision, r.Revision)
			}
		}
	})
	for name, mutate := range map[string]func(*pb.RangeRequest, *pb.RangeResponse){
		"future-mod":       func(_ *pb.RangeRequest, r *pb.RangeResponse) { r.Kvs[0].ModRevision = r.Header.Revision + 1 },
		"zero-create":      func(_ *pb.RangeRequest, r *pb.RangeResponse) { r.Kvs[0].CreateRevision = 0 },
		"create-after-mod": func(_ *pb.RangeRequest, r *pb.RangeResponse) { r.Kvs[0].CreateRevision = r.Kvs[0].ModRevision + 1 },
		"oversized-key": func(_ *pb.RangeRequest, r *pb.RangeResponse) {
			r.Kvs[0].Key = append(r.Kvs[0].Key, bytes.Repeat([]byte("x"), 1025)...)
		},
		"oversized-value":    func(_ *pb.RangeRequest, r *pb.RangeResponse) { r.Kvs[0].Value = bytes.Repeat([]byte("x"), 2049) },
		"out-of-range":       func(q *pb.RangeRequest, r *pb.RangeResponse) { r.Kvs[0].Key = bytes.Clone(q.RangeEnd) },
		"reverse-order":      func(_ *pb.RangeRequest, r *pb.RangeResponse) { r.Kvs[0], r.Kvs[1] = r.Kvs[1], r.Kvs[0] },
		"duplicate":          func(_ *pb.RangeRequest, r *pb.RangeResponse) { r.Kvs[1] = workerTestKV(r.Kvs[0]) },
		"count-too-small":    func(_ *pb.RangeRequest, r *pb.RangeResponse) { r.Count = 1 },
		"count-without-more": func(_ *pb.RangeRequest, r *pb.RangeResponse) { r.Count++ },
		"negative-count":     func(_ *pb.RangeRequest, r *pb.RangeResponse) { r.Count = -1 },
		"more-short": func(_ *pb.RangeRequest, r *pb.RangeResponse) {
			r.More = true
			r.Count = 81
		},
		"wrong-cluster": func(_ *pb.RangeRequest, r *pb.RangeResponse) { r.Header.ClusterId++ },
		"wrong-member":  func(_ *pb.RangeRequest, r *pb.RangeResponse) { r.Header.MemberId++ },
		"wrong-term":    func(_ *pb.RangeRequest, r *pb.RangeResponse) { r.Header.RaftTerm++ },
		"old-header":    func(_ *pb.RangeRequest, r *pb.RangeResponse) { r.Header.Revision-- },
		"future-header": func(_ *pb.RangeRequest, r *pb.RangeResponse) { r.Header.Revision++ },
	} {
		t.Run(name, func(t *testing.T) {
			f := newDuePageFixture(t)
			pendingPageSeed(t, f, 0, 2)
			f.mutate = mutate
			pendingPageCredit(f, 262144)
			page, err := f.b.readCleanupPendingPage(context.Background(), f.partition, f.share, nil)
			require.Error(t, err)
			require.Empty(t, page.Candidates)
			require.Nil(t, page.Next)
			f.b.archiveLife.mu.Lock()
			tokens := f.share.bucket.tokens
			f.b.archiveLife.mu.Unlock()
			require.Zero(t, tokens, "a dispatched corrupt response does not refund earned credit")
		})
	}
	for name, mutate := range map[string]func(*clientv3.TxnResponse){
		"nil-kv":                func(r *clientv3.TxnResponse) { r.Responses[0].GetResponseRange().Kvs[0] = nil },
		"nil-nested-header":     func(r *clientv3.TxnResponse) { r.Responses[0].GetResponseRange().Header = nil },
		"nil-outer-header":      func(r *clientv3.TxnResponse) { r.Header = nil },
		"foreign-outer-cluster": func(r *clientv3.TxnResponse) { r.Header.ClusterId++ },
		"missing-response":      func(r *clientv3.TxnResponse) { r.Responses = nil },
		"extra-response":        func(r *clientv3.TxnResponse) { r.Responses = append(r.Responses, r.Responses[0]) },
		"too-many-rows": func(r *clientv3.TxnResponse) {
			page := r.Responses[0].GetResponseRange()
			page.Kvs = append(page.Kvs, workerTestKV(page.Kvs[79]))
			page.Count = 81
			page.More = false
		},
		"more-without-count": func(r *clientv3.TxnResponse) { r.Responses[0].GetResponseRange().Count = 80 },
	} {
		t.Run(name, func(t *testing.T) {
			f := newDuePageFixture(t)
			pendingPageSeed(t, f, 0, 81)
			f.b.client.KV = &faultKV{KV: f.b.client.KV, match: duePageRangeOps, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				if err == nil {
					mutate(r)
				}
				return r, err
			}}
			f.earn()
			page, err := f.b.readCleanupPendingPage(context.Background(), f.partition, f.share, nil)
			require.Error(t, err)
			require.Empty(t, page.Candidates)
			require.Nil(t, page.Next)
		})
	}
	t.Run("partial-native-header", func(t *testing.T) {
		f := newDuePageFixture(t)
		rows := pendingPageSeed(t, f, 0, 1)
		f.mutate = func(_ *pb.RangeRequest, r *pb.RangeResponse) {
			r.Header = &pb.ResponseHeader{Revision: r.Header.Revision}
		}
		f.earn()
		page, err := f.b.readCleanupPendingPage(context.Background(), f.partition, f.share, nil)
		require.NoError(t, err)
		require.Equal(t, rows, page.Candidates)
	})
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprintf("compaction-guard-lost-%t", lost), func(t *testing.T) {
			f := newDuePageFixture(t)
			pendingPageSeed(t, f, 0, 81)
			f.earn()
			first, err := f.b.readCleanupPendingPage(context.Background(), f.partition, f.share, nil)
			require.NoError(t, err)
			f.b.client.KV = &faultKV{KV: f.b.client.KV, match: duePageRangeOps, after: func(*clientv3.TxnResponse, error) (*clientv3.TxnResponse, error) {
				if lost {
					f.server.put(f.partition.fence.key, f.partition.fence.value, f.partition.fence.lease)
				}
				return nil, rpctypes.ErrCompacted
			}}
			before := len(pendingPageRequests(f))
			pendingPageCredit(f, 262144)
			page, err := f.b.readCleanupPendingPage(context.Background(), f.partition, f.share, first.Next)
			require.ErrorIs(t, err, rpctypes.ErrCompacted)
			require.Empty(t, page.Candidates)
			require.Nil(t, page.Next)
			requests := pendingPageRequests(f)[before:]
			require.Len(t, requests, 2, "known compaction must audit original guards under the same paid call")
			require.Len(t, requests[1].Compare, 20)
			require.Empty(t, requests[1].Success[0].GetRequestRange().RangeEnd)
			f.b.archiveLife.mu.Lock()
			liveErr, tokens := f.session.live(), f.share.bucket.tokens
			f.b.archiveLife.mu.Unlock()
			require.Equal(t, lost, liveErr != nil)
			require.Zero(t, tokens)
			pendingPageCredit(f, 262144)
			before = len(pendingPageRequests(f))
			_, err = f.b.readCleanupPendingPage(context.Background(), f.partition, f.share, first.Next)
			require.Error(t, err)
			require.Len(t, pendingPageRequests(f), before)
		})
	}
}
