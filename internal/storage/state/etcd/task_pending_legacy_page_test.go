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

type legacyPageFixture struct {
	*backfillBootstrapFixture
	pages  *duePageFixture
	mutate func(*pb.RangeRequest, *pb.RangeResponse)
}

// Capture history at the actual guarded B-point revision, before bootstrap or
// refresh writes B. The existing bounded host model supplies historical Range;
// this adapter only captures that view and implements native KeysOnly output.
func newLegacyPageFixture(t *testing.T) *legacyPageFixture {
	t.Helper()
	f := &legacyPageFixture{backfillBootstrapFixture: newBackfillBootstrapFixture(t, 60*time.Second)}
	f.pages = &duePageFixture{server: f.server, snapshots: make(map[int64][]*mvccpb.KeyValue)}
	bkey := f.b.namespace.Root() + "p/aa/cleanup-pending-backfill"
	f.server.mu.Lock()
	f.server.txnReply = func(request *pb.TxnRequest, response *pb.TxnResponse) {
		if !response.Succeeded || len(request.Success) != 1 {
			return
		}
		q := request.Success[0].GetRequestRange()
		if q == nil {
			return
		}
		if string(q.Key) == bkey && len(q.RangeEnd) == 0 && q.Revision == 0 {
			var snapshot []*mvccpb.KeyValue
			for _, kv := range f.server.values {
				snapshot = append(snapshot, workerTestKV(kv))
			}
			f.pages.snapshots[response.Header.Revision] = snapshot
		}
		if len(q.RangeEnd) == 0 {
			return
		}
		f.pages.rangeReply(request, response)
		r := response.Responses[0].GetResponseRange()
		if q.KeysOnly {
			for _, kv := range r.Kvs {
				kv.Value = nil
			}
		}
		if f.mutate != nil {
			f.mutate(q, r)
		}
	}
	f.server.mu.Unlock()
	return f
}

// Values are intentionally opaque: discovery must not authenticate an intent
// from keys-only output, including generic Stage values above the typed cap.
func (f *legacyPageFixture) seed(first, count, valueBytes int) []cleanupLegacyRow {
	var rows []cleanupLegacyRow
	for i := first; i < first+count; i++ {
		id := fmt.Sprintf("%08x-1111-4111-8111-111111111111", i+1)
		key := f.b.namespace.Root() + "p/aa/cleanup-intents/" + id
		f.server.put(key, strings.Repeat("i", valueBytes), 0)
		rows = append(rows, cleanupLegacyRow{taskID: id, birth: f.server.point(key).CreateRevision})
	}
	return rows
}

func (f *legacyPageFixture) read(t *testing.T, state *cleanupBackfillState, cursor *cleanupLegacyCursor) *cleanupLegacyPage {
	t.Helper()
	backfillBootstrapCredit(f.b, f.ranges, 262144)
	page, err := f.b.readCleanupLegacyPage(context.Background(), f.partition, f.ranges, state, cursor)
	require.NoError(t, err)
	require.NotNil(t, page)
	return page
}

func TestCleanupLegacyPage(t *testing.T) {
	for _, count := range []int{0, 1, 2, 3} {
		t.Run(fmt.Sprintf("rows-%d-and-eof-is-not-coverage", count), func(t *testing.T) {
			f := newLegacyPageFixture(t)
			rows := f.seed(0, count, 16)
			state := backfillBootstrapCreate(t, f.backfillBootstrapFixture)
			before := f.server.point(state.fence.key)
			page := f.read(t, state, nil)
			require.Same(t, page, page.self)
			require.Same(t, f.b, page.origin)
			require.Same(t, state, page.state)
			require.Same(t, f.ranges, page.ranges)
			require.Equal(t, state.fence, page.fence)
			require.Equal(t, state.record.ReadViewRevision, page.revision)
			require.Len(t, page.rows, min(count, 2))
			if count > 0 {
				require.Equal(t, rows[:min(count, 2)], page.rows)
			}
			require.Equal(t, count <= 2, page.eof)
			if count > 2 {
				require.NotNil(t, page.next)
				last := f.read(t, state, page.next)
				require.Equal(t, rows[2:], last.rows)
				require.True(t, last.eof)
				require.Nil(t, last.next)
			} else {
				require.Nil(t, page.next)
			}
			require.Equal(t, before, f.server.point(state.fence.key), "empty and nonempty EOF cannot advance durable coverage")
			require.Equal(t, "list", state.record.Phase)
			require.Empty(t, state.record.Frontier)
		})
	}
	t.Run("fixed-view-native-shape-and-bounded-lookahead", func(t *testing.T) {
		f := newLegacyPageFixture(t)
		rows := f.seed(0, 3, 65536)
		state := backfillBootstrapCreate(t, f.backfillBootstrapFixture)
		prefix := f.b.namespace.Root() + "p/aa/cleanup-intents/"
		end := clientv3.GetPrefixRangeEnd(prefix)
		f.b.archiveLife.mu.Lock()
		cmps, err := f.b.workerComparisons(f.session, f.partition, f.ranges)
		f.b.archiveLife.mu.Unlock()
		require.NoError(t, err)
		require.Equal(t, 2, cleanupLegacyPageEntries)
		require.Equal(t, 204160, 4096+3*(1024+65536+128), "backend lookahead includes a third full generic Stage value")
		require.Equal(t, 6400, 4096+2*(1024+128), "keys-only reply padding is a separate bound")
		require.NoError(t, f.b.preflightCleanupLegacyPage(cmps, prefix, end, math.MaxInt64, f.session))
		oversize := append([]clientv3.Cmp(nil), cmps...)
		oversize[0] = clientv3.Compare(clientv3.Value(f.b.identityKey), "=", strings.Repeat("x", 128*1024))
		require.Error(t, f.b.preflightCleanupLegacyPage(oversize, prefix, end, math.MaxInt64, f.session))
		var retained []*mvccpb.KeyValue
		f.b.client.KV = &faultKV{KV: f.b.client.KV, match: duePageRangeOps, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
			if err == nil {
				retained = r.Responses[0].GetResponseRange().Kvs
				require.LessOrEqual(t, (*pb.TxnResponse)(r).Size(), 6400)
			}
			return r, err
		}}
		before := len(backfillBootstrapRequests(f.backfillBootstrapFixture))
		first := f.read(t, state, nil)
		require.Equal(t, rows[:2], first.rows)
		require.Equal(t, prefix, first.start)
		require.Equal(t, end, first.end)
		require.NotNil(t, first.next)
		for _, kv := range retained {
			for i := range kv.Key {
				kv.Key[i] = 'x'
			}
		}
		require.Equal(t, rows[:2], first.rows, "row IDs and continuation own their response bytes")
		_, err = f.b.client.Txn(context.Background()).Then(clientv3.OpDelete(prefix + rows[2].taskID)).Commit()
		require.NoError(t, err)
		f.seed(3, 1, 16)
		last := f.read(t, state, first.next)
		require.Equal(t, rows[2:], last.rows, "historical deletion and new insertion cannot change the fixed view")
		require.True(t, last.eof)
		require.Equal(t, first.revision, last.revision)
		var pages []*pb.TxnRequest
		for _, request := range backfillBootstrapRequests(f.backfillBootstrapFixture)[before:] {
			if len(request.Success) == 1 && request.Success[0].GetRequestRange() != nil {
				pages = append(pages, request)
			}
		}
		require.Len(t, pages, 2)
		for i, request := range pages {
			require.Equal(t, taskProtoComparisons(cmps), request.Compare)
			require.Equal(t, 25, len(request.Compare)+len(request.Success)+len(request.Failure))
			require.Len(t, request.Failure, 4)
			require.LessOrEqual(t, request.Size(), 128*1024)
			for _, cmp := range request.Compare {
				require.NotEqual(t, state.fence.key, string(cmp.Key), "B is a later CAS predecessor, not Range authority")
			}
			q := request.Success[0].GetRequestRange()
			require.Equal(t, int64(2), q.Limit)
			require.Equal(t, state.record.ReadViewRevision, q.Revision)
			require.Equal(t, end, string(q.RangeEnd))
			require.True(t, q.KeysOnly)
			require.False(t, q.CountOnly)
			require.False(t, q.Serializable)
			require.Equal(t, pb.RangeRequest_KEY, q.SortTarget)
			require.Equal(t, pb.RangeRequest_NONE, q.SortOrder)
			require.Zero(t, q.MinCreateRevision)
			require.Zero(t, q.MaxCreateRevision)
			require.Zero(t, q.MinModRevision)
			require.Zero(t, q.MaxModRevision)
			if i == 0 {
				require.Equal(t, prefix, string(q.Key))
			} else {
				require.Equal(t, prefix+rows[1].taskID+"\x00", string(q.Key))
			}
		}
	})
	t.Run("durable-frontier-seek", func(t *testing.T) {
		f := newLegacyPageFixture(t)
		rows := f.seed(0, 3, 16)
		state := backfillBootstrapCreate(t, f.backfillBootstrapFixture)
		// Seed a future protocol's durable checkpoint; this reader never writes it.
		record := state.record
		record.Frontier = rows[0].taskID
		wire, err := encodeCleanupBackfill(record)
		require.NoError(t, err)
		f.server.put(state.fence.key, wire, 0)
		state = backfillBootstrapRead(t, f.backfillBootstrapFixture).state
		page := f.read(t, state, nil)
		require.Equal(t, rows[1:], page.rows)
		require.Equal(t, f.b.namespace.Root()+"p/aa/cleanup-intents/"+record.Frontier+"\x00", page.start)
		require.True(t, page.eof)
		require.Equal(t, record, state.record)
	})
	t.Run("refreshed-view-returns-both-sides-of-cutoff", func(t *testing.T) {
		f := newLegacyPageFixture(t)
		old := f.seed(0, 1, 16)
		state := backfillBootstrapCreate(t, f.backfillBootstrapFixture)
		fresh := f.seed(1, 1, 16)
		observed := backfillBootstrapRead(t, f.backfillBootstrapFixture)
		backfillBootstrapCredit(f.b, f.claims, 1)
		next, err := f.b.refreshCleanupBackfill(context.Background(), f.partition, f.claims, observed)
		require.NoError(t, err)
		page := f.read(t, next, nil)
		require.Equal(t, append(old, fresh...), page.rows)
		require.LessOrEqual(t, page.rows[0].birth, state.record.LegacyCutoffRevision)
		require.Greater(t, page.rows[1].birth, state.record.LegacyCutoffRevision)
		require.Equal(t, state.record.LegacyCutoffRevision, next.record.LegacyCutoffRevision)
	})
	for name, mutate := range map[string]func(*pb.RangeRequest, *pb.RangeResponse){
		"descendant": func(_ *pb.RangeRequest, r *pb.RangeResponse) {
			r.Kvs[0].Key = append(r.Kvs[0].Key, []byte("/child")...)
		},
		"uppercase-uuid": func(_ *pb.RangeRequest, r *pb.RangeResponse) {
			r.Kvs[0].Key[len(r.Kvs[0].Key)-1] = 'A'
		},
		"lease": func(_ *pb.RangeRequest, r *pb.RangeResponse) {
			r.Kvs[0].Lease = 1
		},
		"mutable-intent": func(_ *pb.RangeRequest, r *pb.RangeResponse) {
			r.Kvs[0].Version = 2
		},
		"zero-birth": func(_ *pb.RangeRequest, r *pb.RangeResponse) {
			r.Kvs[0].CreateRevision = 0
		},
		"changed-mod": func(_ *pb.RangeRequest, r *pb.RangeResponse) {
			r.Kvs[0].ModRevision++
		},
		"future-birth": func(q *pb.RangeRequest, r *pb.RangeResponse) {
			r.Kvs[0].CreateRevision, r.Kvs[0].ModRevision = q.Revision+1, q.Revision+1
		},
		"value-leak": func(_ *pb.RangeRequest, r *pb.RangeResponse) {
			r.Kvs[0].Value = []byte("i")
		},
		"reverse-order": func(_ *pb.RangeRequest, r *pb.RangeResponse) {
			r.Kvs[0], r.Kvs[1] = r.Kvs[1], r.Kvs[0]
		},
		"duplicate": func(_ *pb.RangeRequest, r *pb.RangeResponse) {
			r.Kvs[1] = workerTestKV(r.Kvs[0])
		},
		"out-of-range": func(q *pb.RangeRequest, r *pb.RangeResponse) {
			r.Kvs[0].Key = bytes.Clone(q.RangeEnd)
		},
		"count-too-small": func(_ *pb.RangeRequest, r *pb.RangeResponse) {
			r.Count = 1
		},
		"count-without-more": func(_ *pb.RangeRequest, r *pb.RangeResponse) {
			r.Count = 3
		},
		"more-without-count": func(_ *pb.RangeRequest, r *pb.RangeResponse) {
			r.More = true
		},
		"short-more": func(_ *pb.RangeRequest, r *pb.RangeResponse) {
			r.Kvs, r.Count, r.More = r.Kvs[:1], 3, true
		},
		"wrong-cluster": func(_ *pb.RangeRequest, r *pb.RangeResponse) {
			r.Header.ClusterId++
		},
		"future-header": func(_ *pb.RangeRequest, r *pb.RangeResponse) {
			r.Header.Revision++
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newLegacyPageFixture(t)
			f.seed(0, 2, 16)
			state := backfillBootstrapCreate(t, f.backfillBootstrapFixture)
			f.mutate = mutate
			backfillBootstrapCredit(f.b, f.ranges, 262144)
			page, err := f.b.readCleanupLegacyPage(context.Background(), f.partition, f.ranges, state, nil)
			require.Error(t, err)
			require.Nil(t, page, "malformed rows cannot return partial discovery or EOF evidence")
			f.b.archiveLife.mu.Lock()
			tokens := f.ranges.bucket.tokens
			f.b.archiveLife.mu.Unlock()
			require.Zero(t, tokens, "a dispatched invalid page never refunds Range credit")
		})
	}
	for name, mutate := range map[string]func(*clientv3.TxnResponse){
		"nil-kv": func(r *clientv3.TxnResponse) {
			r.Responses[0].GetResponseRange().Kvs[0] = nil
		},
		"nil-header": func(r *clientv3.TxnResponse) {
			r.Header = nil
		},
		"nil-range-header": func(r *clientv3.TxnResponse) {
			r.Responses[0].GetResponseRange().Header = nil
		},
		"extra-response": func(r *clientv3.TxnResponse) {
			r.Responses = append(r.Responses, r.Responses[0])
		},
		"too-many-rows": func(r *clientv3.TxnResponse) {
			page := r.Responses[0].GetResponseRange()
			page.Kvs = append(page.Kvs, workerTestKV(page.Kvs[1]))
			page.Count = 3
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newLegacyPageFixture(t)
			f.seed(0, 2, 16)
			state := backfillBootstrapCreate(t, f.backfillBootstrapFixture)
			f.b.client.KV = &faultKV{KV: f.b.client.KV, match: duePageRangeOps, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				if err == nil {
					mutate(r)
				}
				return r, err
			}}
			backfillBootstrapCredit(f.b, f.ranges, 262144)
			page, err := f.b.readCleanupLegacyPage(context.Background(), f.partition, f.ranges, state, nil)
			require.Error(t, err)
			require.Nil(t, page)
		})
	}
}

func TestCleanupLegacyPageOwnership(t *testing.T) {
	t.Run("exact-state-pointer-credit-and-one-use", func(t *testing.T) {
		f := newLegacyPageFixture(t)
		f.seed(0, 3, 16)
		state := backfillBootstrapCreate(t, f.backfillBootstrapFixture)
		first := f.read(t, state, nil)
		require.NotNil(t, first.next)
		copyState, copyCursor := *state, *first.next
		reread := backfillBootstrapRead(t, f.backfillBootstrapFixture).state
		require.NotSame(t, state, reread)
		require.Equal(t, state.fence, reread.fence)
		otherShare, err := f.b.AcquireWorkerBudget(context.Background(), f.session, WorkerBudgetRange, 1)
		require.NoError(t, err)
		otherPartition, err := f.b.ClaimWorkerPartition(context.Background(), f.session, 0xab)
		require.NoError(t, err)
		foreign := newLegacyPageFixture(t)
		backfillBootstrapCredit(f.b, f.ranges, 262144)
		before := len(backfillBootstrapRequests(f.backfillBootstrapFixture))
		foreignBefore := len(backfillBootstrapRequests(foreign.backfillBootstrapFixture))
		for _, attempt := range []func() (*cleanupLegacyPage, error){
			func() (*cleanupLegacyPage, error) {
				return f.b.readCleanupLegacyPage(nil, f.partition, f.ranges, state, first.next)
			},
			func() (*cleanupLegacyPage, error) {
				return (*Backend)(nil).readCleanupLegacyPage(context.Background(), f.partition, f.ranges, state, first.next)
			},
			func() (*cleanupLegacyPage, error) {
				return f.b.readCleanupLegacyPage(context.Background(), nil, f.ranges, state, first.next)
			},
			func() (*cleanupLegacyPage, error) {
				return f.b.readCleanupLegacyPage(context.Background(), f.partition, f.ranges, nil, first.next)
			},
			func() (*cleanupLegacyPage, error) {
				return f.b.readCleanupLegacyPage(context.Background(), f.partition, f.ranges, &copyState, first.next)
			},
			func() (*cleanupLegacyPage, error) {
				return f.b.readCleanupLegacyPage(context.Background(), f.partition, f.ranges, state, &copyCursor)
			},
			func() (*cleanupLegacyPage, error) {
				return f.b.readCleanupLegacyPage(context.Background(), f.partition, f.ranges, reread, first.next)
			},
			func() (*cleanupLegacyPage, error) {
				return f.b.readCleanupLegacyPage(context.Background(), f.partition, otherShare, state, first.next)
			},
			func() (*cleanupLegacyPage, error) {
				return f.b.readCleanupLegacyPage(context.Background(), otherPartition, f.ranges, state, first.next)
			},
			func() (*cleanupLegacyPage, error) {
				return foreign.b.readCleanupLegacyPage(context.Background(), foreign.partition, foreign.ranges, state, first.next)
			},
		} {
			page, err := attempt()
			require.Error(t, err)
			require.Nil(t, page)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = f.b.readCleanupLegacyPage(ctx, f.partition, f.ranges, state, first.next)
		require.ErrorIs(t, err, context.Canceled)
		require.Len(t, backfillBootstrapRequests(f.backfillBootstrapFixture), before)
		require.Len(t, backfillBootstrapRequests(foreign.backfillBootstrapFixture), foreignBefore)
		f.b.archiveLife.mu.Lock()
		tokens := f.ranges.bucket.tokens
		f.b.archiveLife.mu.Unlock()
		require.Equal(t, uint64(262144), tokens)
		backfillBootstrapCredit(f.b, f.ranges, 0)
		_, err = f.b.readCleanupLegacyPage(context.Background(), f.partition, f.ranges, state, first.next)
		require.ErrorIs(t, err, ErrConflict)
		last := f.read(t, state, first.next)
		require.Len(t, last.rows, 1, "local refusal preserved the original cursor")
		f.b.archiveLife.mu.Lock()
		tokens = f.ranges.bucket.tokens
		f.b.archiveLife.mu.Unlock()
		require.Zero(t, tokens)
		before = len(backfillBootstrapRequests(f.backfillBootstrapFixture))
		backfillBootstrapCredit(f.b, f.ranges, 262144)
		_, err = f.b.readCleanupLegacyPage(context.Background(), f.partition, f.ranges, state, first.next)
		require.Error(t, err)
		require.Len(t, backfillBootstrapRequests(f.backfillBootstrapFixture), before)
	})
	t.Run("refresh-cannot-adopt-old-cursor", func(t *testing.T) {
		f := newLegacyPageFixture(t)
		rows := f.seed(0, 3, 16)
		state := backfillBootstrapCreate(t, f.backfillBootstrapFixture)
		first := f.read(t, state, nil)
		observed := backfillBootstrapRead(t, f.backfillBootstrapFixture)
		backfillBootstrapCredit(f.b, f.claims, 1)
		refreshed, err := f.b.refreshCleanupBackfill(context.Background(), f.partition, f.claims, observed)
		require.NoError(t, err)
		before := len(backfillBootstrapRequests(f.backfillBootstrapFixture))
		backfillBootstrapCredit(f.b, f.ranges, 262144)
		page, err := f.b.readCleanupLegacyPage(context.Background(), f.partition, f.ranges, refreshed, first.next)
		require.Error(t, err)
		require.Nil(t, page)
		require.Len(t, backfillBootstrapRequests(f.backfillBootstrapFixture), before)
		page = f.read(t, state, first.next)
		require.Equal(t, rows[2:], page.rows, "old B permits discovery only; a later exact-B CAS must reject stale coverage")
		require.Equal(t, refreshed.fence.mod, f.server.point(state.fence.key).ModRevision)
	})
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprintf("compaction-guard-lost-%t", lost), func(t *testing.T) {
			f := newLegacyPageFixture(t)
			f.seed(0, 3, 16)
			state := backfillBootstrapCreate(t, f.backfillBootstrapFixture)
			first := f.read(t, state, nil)
			f.b.client.KV = &faultKV{KV: f.b.client.KV, match: duePageRangeOps, after: func(*clientv3.TxnResponse, error) (*clientv3.TxnResponse, error) {
				if lost {
					f.server.put(f.partition.fence.key, f.partition.fence.value, f.partition.fence.lease)
				}
				return nil, rpctypes.ErrCompacted
			}}
			before := len(backfillBootstrapRequests(f.backfillBootstrapFixture))
			backfillBootstrapCredit(f.b, f.ranges, 262144)
			page, err := f.b.readCleanupLegacyPage(context.Background(), f.partition, f.ranges, state, first.next)
			require.ErrorIs(t, err, rpctypes.ErrCompacted)
			require.Nil(t, page)
			requests := backfillBootstrapRequests(f.backfillBootstrapFixture)[before:]
			require.Len(t, requests, 2, "known compaction audits guards under the same paid call")
			require.Len(t, requests[1].Compare, 20)
			require.Empty(t, requests[1].Success[0].GetRequestRange().RangeEnd)
			f.b.archiveLife.mu.Lock()
			liveErr, tokens := f.session.live(), f.ranges.bucket.tokens
			f.b.archiveLife.mu.Unlock()
			require.Equal(t, lost, liveErr != nil)
			require.Zero(t, tokens)
			backfillBootstrapCredit(f.b, f.ranges, 262144)
			before = len(backfillBootstrapRequests(f.backfillBootstrapFixture))
			_, err = f.b.readCleanupLegacyPage(context.Background(), f.partition, f.ranges, state, first.next)
			require.Error(t, err)
			require.Len(t, backfillBootstrapRequests(f.backfillBootstrapFixture), before)
		})
	}
	t.Run("close-joins-actual-page-and-relist-return", func(t *testing.T) {
		f := newLegacyPageFixture(t)
		f.seed(0, 1, 16)
		state := backfillBootstrapCreate(t, f.backfillBootstrapFixture)
		relist, err := f.b.AcquireWorkerBudget(context.Background(), f.session, WorkerBudgetRelist, 0)
		require.NoError(t, err)
		entered, release := make(chan struct{}, 1), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		t.Cleanup(unblock)
		f.b.client.KV = &faultKV{KV: f.b.client.KV, match: duePageRangeOps, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
			entered <- struct{}{}
			<-release // Hold actual return despite cancellation of the original owner.
			return r, err
		}}
		owned, done := make(chan context.Context, 1), make(chan error, 1)
		backfillBootstrapCredit(f.b, f.ranges, 262144)
		go func() {
			done <- f.b.RunWorkerRelist(context.Background(), f.partition, relist, func(ctx context.Context) error {
				owned <- ctx
				_, err := f.b.readCleanupLegacyPage(ctx, f.partition, f.ranges, state, nil)
				return err
			})
		}()
		ctx := workerTestReceive(t, owned)
		workerTestReceive(t, entered)
		closed := make(chan error, 1)
		go func() { closed <- f.b.Close() }()
		workerTestReceive(t, ctx.Done())
		f.b.archiveLife.mu.Lock()
		pageHeld, relistHeld := f.ranges.dueOwner != nil, relist.owner != nil
		f.b.archiveLife.mu.Unlock()
		require.True(t, pageHeld)
		require.True(t, relistHeld)
		select {
		case err := <-closed:
			t.Fatalf("Close returned before native page return: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
		unblock()
		require.Error(t, workerTestReceive(t, done))
		require.NoError(t, workerTestReceive(t, closed))
	})
}
