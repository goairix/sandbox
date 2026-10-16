package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type duePageFixture struct {
	b         *Backend
	server    *workerTestServer
	session   *WorkerSession
	partition *WorkerPartitionClaim
	share     *WorkerBudgetShare
	now       time.Time
	snapshots map[int64][]*mvccpb.KeyValue
	mutate    func(*pb.RangeRequest, *pb.RangeResponse)
}

func newDuePageFixture(t *testing.T) *duePageFixture {
	t.Helper()
	server, o := workerTestEndpoint(t)
	now := time.Now().UTC()
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	o.PublicationTrust = &RuntimePublicationTrust{AuthorityID: o.Identity.RuntimeID, Target: "target", Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}
	o.Clock = testAuthorityClock(func(context.Context) (ClockObservation, error) { return ClockObservation{UTC: now}, nil })
	b, err := New(context.Background(), o)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	s, err := b.OpenWorkerSession(context.Background(), "due-worker")
	require.NoError(t, err)
	p, err := b.ClaimWorkerPartition(context.Background(), s, 0xaa)
	require.NoError(t, err)
	share, err := b.AcquireWorkerBudget(context.Background(), s, WorkerBudgetRange, 0)
	require.NoError(t, err)
	f := &duePageFixture{b: b, server: server, session: s, partition: p, share: share, now: now, snapshots: make(map[int64][]*mvccpb.KeyValue)}
	server.mu.Lock()
	server.txnReply = f.rangeReply
	server.mu.Unlock()
	return f
}

// The finite host model stores a snapshot when revision0 is actually read.
// It deliberately reports a newer native header for historical reads and a
// total-match Count before applying Limit, matching native etcd semantics.
func (f *duePageFixture) rangeReply(request *pb.TxnRequest, response *pb.TxnResponse) {
	if !response.Succeeded || len(request.Success) != 1 || request.Success[0].GetRequestRange() == nil {
		return
	}
	r := request.Success[0].GetRequestRange()
	if len(r.RangeEnd) == 0 {
		return
	}
	revision := r.Revision
	if revision == 0 {
		revision = response.Header.Revision
		var snapshot []*mvccpb.KeyValue
		for _, kv := range f.server.values {
			snapshot = append(snapshot, workerTestKV(kv))
		}
		f.snapshots[revision] = snapshot
	}
	var rows []*mvccpb.KeyValue
	for _, kv := range f.snapshots[revision] {
		if bytes.Compare(kv.Key, r.Key) >= 0 && bytes.Compare(kv.Key, r.RangeEnd) < 0 {
			rows = append(rows, workerTestKV(kv))
		}
	}
	sort.Slice(rows, func(i, j int) bool { return bytes.Compare(rows[i].Key, rows[j].Key) < 0 })
	out := &pb.RangeResponse{Header: f.server.header(), Count: int64(len(rows)), Kvs: rows}
	if r.Limit > 0 && int64(len(rows)) > r.Limit {
		out.More = true
		out.Kvs = out.Kvs[:r.Limit]
	}
	if f.mutate != nil {
		f.mutate(r, out)
	}
	response.Responses = []*pb.ResponseOp{{Response: &pb.ResponseOp_ResponseRange{ResponseRange: out}}}
}

func (f *duePageFixture) earn() {
	f.b.archiveLife.mu.Lock()
	f.share.bucket.last = time.Now().Add(-time.Second)
	f.b.archiveLife.mu.Unlock()
}

func (f *duePageFixture) seed(t *testing.T, count int, expires time.Time) []DueCandidateRecord {
	t.Helper()
	var records []DueCandidateRecord
	for i := 0; i < count; i++ {
		r := DueCandidateRecord{Version: 1, Partition: 0xaa, SandboxID: fmt.Sprintf("sandbox-%03d", i), WorkspaceHash: strings.Repeat("a", 64), RestoreEpoch: f.b.restoreEpoch, Generation: 1, IntentID: fmt.Sprintf("intent-%03d", i), ExpiresAt: expires.UTC()}
		key, err := f.b.namespace.dueCandidateKey(r)
		require.NoError(t, err)
		value, err := encodeDueCandidate(r)
		require.NoError(t, err)
		f.server.put(key, value, 0)
		records = append(records, r)
	}
	return records
}

func TestDueRecordsCanonicalBounds(t *testing.T) {
	n, err := NewNamespace("/"+strings.Repeat("p", 252), strings.Repeat("s", 128), strings.Repeat("c", 128))
	require.NoError(t, err)
	r := DueCandidateRecord{Version: 1, Partition: 0xaa, SandboxID: strings.Repeat("s", 128), WorkspaceHash: strings.Repeat("a", 64), RestoreEpoch: strings.Repeat("e", 128), Generation: math.MaxInt64, IntentID: strings.Repeat("i", 128), ExpiresAt: time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)}
	key, err := n.dueCandidateKey(r)
	require.NoError(t, err)
	require.Len(t, key, 825)
	wire, err := encodeDueCandidate(r)
	require.NoError(t, err)
	require.LessOrEqual(t, len(wire), 1024)
	kv := &mvccpb.KeyValue{Key: []byte(key), Value: []byte(wire), Version: 1, CreateRevision: 5, ModRevision: 5}
	got, err := decodeDueCandidate(n, kv, 5)
	require.NoError(t, err)
	require.Equal(t, r, got)
	for name, mutate := range map[string]func(*mvccpb.KeyValue){
		"extra": func(k *mvccpb.KeyValue) {
			k.Value = bytes.Replace(k.Value, []byte(`"version":1`), []byte(`"version":1,"extra":0`), 1)
		},
		"duplicate": func(k *mvccpb.KeyValue) {
			k.Value = bytes.Replace(k.Value, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1)
		},
		"wire":     func(k *mvccpb.KeyValue) { k.Value = append(k.Value, ' ') },
		"oversize": func(k *mvccpb.KeyValue) { k.Value = bytes.Repeat([]byte("x"), 1025) },
		"key":      func(k *mvccpb.KeyValue) { k.Key = append(k.Key, 'x') },
		"partition": func(k *mvccpb.KeyValue) {
			k.Value = bytes.Replace(k.Value, []byte(`"partition":170`), []byte(`"partition":171`), 1)
		},
		"lease":           func(k *mvccpb.KeyValue) { k.Lease = 1 },
		"future-revision": func(k *mvccpb.KeyValue) { k.ModRevision = 6 },
		"zero-version":    func(k *mvccpb.KeyValue) { k.Version = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := workerTestKV(kv)
			mutate(bad)
			_, err := decodeDueCandidate(n, bad, 5)
			require.Error(t, err)
		})
	}
	var previous string
	zero := r
	zero.ExpiresAt = time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err = n.dueCandidateKey(zero)
	require.Error(t, err, "the existing domain rejects time.Time zero")
	for _, instant := range []time.Time{time.Date(1, 1, 1, 0, 0, 0, 1, time.UTC), time.Date(1, 1, 1, 0, 0, 0, 2, time.UTC), time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), r.ExpiresAt} {
		copy := r
		copy.ExpiresAt = instant
		key, err := n.dueCandidateKey(copy)
		require.NoError(t, err)
		require.Greater(t, key, previous)
		previous = key
	}
	for _, generation := range []int64{1, 9, 10, math.MaxInt64} {
		copy := r
		copy.Generation = generation
		key, err := n.dueCandidateKey(copy)
		require.NoError(t, err)
		if generation != 1 {
			require.Greater(t, key, previous)
		}
		previous = key
	}
}

func TestDuePageFixedRevisionAndBudget(t *testing.T) {
	f := newDuePageFixture(t)
	rows := f.seed(t, 120, f.now.Add(-time.Second))
	f.seed(t, 1, f.now.Add(61*time.Second))
	_, err := f.b.ReadDuePage(context.Background(), f.partition, f.share, nil)
	require.ErrorIs(t, err, ErrConflict, "new original bucket starts empty")
	f.earn()
	first, err := f.b.ReadDuePage(context.Background(), f.partition, f.share, nil)
	require.NoError(t, err)
	require.Len(t, first.Candidates, 118)
	require.NotNil(t, first.Next)
	require.Greater(t, first.Revision, int64(0))
	require.Equal(t, rows[:118], first.Candidates)
	require.Equal(t, f.now.Add(60*time.Second), first.WindowEnd)
	f.b.archiveLife.mu.Lock()
	f.share.bucket = newWorkerBucket(time.Now().Add(time.Hour), 1048576, 262144)
	f.b.archiveLife.mu.Unlock()
	_, err = f.b.ReadDuePage(context.Background(), f.partition, f.share, first.Next)
	require.ErrorIs(t, err, ErrConflict)
	// A local no-credit refusal must preserve this very cursor, not restart R.
	f.earn()
	key, _ := f.b.namespace.dueCandidateKey(rows[119])
	value, _ := encodeDueCandidate(rows[119])
	f.server.put(key, value, 0)
	second, err := f.b.ReadDuePage(context.Background(), f.partition, f.share, first.Next)
	require.NoError(t, err)
	require.Equal(t, first.Revision, second.Revision)
	require.Equal(t, first.WindowEnd, second.WindowEnd)
	require.Equal(t, rows[118:], second.Candidates)
	require.Nil(t, second.Next)
	f.earn()
	_, err = f.b.ReadDuePage(context.Background(), f.partition, f.share, first.Next)
	require.Error(t, err, "actual dispatched cursor is one-use")
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	var seen int
	for _, request := range f.server.requests {
		if len(request.Success) != 1 || request.Success[0].GetRequestRange() == nil || len(request.Success[0].GetRequestRange().RangeEnd) == 0 {
			continue
		}
		r := request.Success[0].GetRequestRange()
		require.Equal(t, int64(118), r.Limit)
		require.False(t, r.Serializable)
		require.Equal(t, pb.RangeRequest_NONE, r.SortOrder, "native key order keeps the server's limit bounded")
		require.Equal(t, pb.RangeRequest_KEY, r.SortTarget)
		require.Zero(t, r.MinModRevision)
		require.Zero(t, r.MaxModRevision)
		require.Zero(t, r.MinCreateRevision)
		require.Zero(t, r.MaxCreateRevision)
		require.Len(t, request.Compare, 20)
		require.Equal(t, 25, len(request.Compare)+len(request.Success)+len(request.Failure))
		require.LessOrEqual(t, request.Size(), 262144)
		if seen == 0 {
			require.Zero(t, r.Revision)
		} else {
			require.Equal(t, first.Revision, r.Revision)
		}
		seen++
	}
	require.Equal(t, 2, seen, "local no-credit/reused cursor attempts issue no Range")
}

func TestDuePageRejectsMalformed(t *testing.T) {
	for name, mutate := range map[string]func(*pb.RangeRequest, *pb.RangeResponse){
		"leased":          func(_ *pb.RangeRequest, r *pb.RangeResponse) { r.Kvs[0].Lease = 1 },
		"future-revision": func(_ *pb.RangeRequest, r *pb.RangeResponse) { r.Kvs[0].ModRevision = r.Header.Revision + 1 },
		"oversized-value": func(_ *pb.RangeRequest, r *pb.RangeResponse) { r.Kvs[0].Value = bytes.Repeat([]byte("x"), 262145) },
		"out-of-range":    func(q *pb.RangeRequest, r *pb.RangeResponse) { r.Kvs[0].Key = bytes.Clone(q.RangeEnd) },
		"duplicate":       func(_ *pb.RangeRequest, r *pb.RangeResponse) { r.Kvs = append(r.Kvs, r.Kvs[0]); r.Count = 2 },
		"count":           func(_ *pb.RangeRequest, r *pb.RangeResponse) { r.Count = 0 },
		"more-empty":      func(_ *pb.RangeRequest, r *pb.RangeResponse) { r.Kvs = nil; r.More = true; r.Count = 1 },
		"more-no-count":   func(_ *pb.RangeRequest, r *pb.RangeResponse) { r.More = true },
		"wrong-cluster":   func(_ *pb.RangeRequest, r *pb.RangeResponse) { r.Header.ClusterId++ },
		"wrong-epoch": func(_ *pb.RangeRequest, r *pb.RangeResponse) {
			var c DueCandidateRecord
			_ = jsonUnmarshalDueForTest(r.Kvs[0].Value, &c)
			c.RestoreEpoch = "other"
			wire, _ := encodeDueCandidate(c)
			r.Kvs[0].Value = []byte(wire)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newDuePageFixture(t)
			f.seed(t, 1, f.now)
			f.mutate = mutate
			f.earn()
			page, err := f.b.ReadDuePage(context.Background(), f.partition, f.share, nil)
			require.Error(t, err)
			require.Nil(t, page.Next)
			require.Empty(t, page.Candidates)
		})
	}
	t.Run("nil-kv", func(t *testing.T) {
		f := newDuePageFixture(t)
		f.seed(t, 1, f.now)
		f.earn()
		f.b.client.KV = &faultKV{KV: f.b.client.KV, match: duePageRangeOps, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
			if err == nil {
				r.Responses[0].GetResponseRange().Kvs[0] = nil
			}
			return r, err
		}}
		page, err := f.b.ReadDuePage(context.Background(), f.partition, f.share, nil)
		require.Error(t, err)
		require.Nil(t, page.Next)
		require.Empty(t, page.Candidates)
	})
}

func TestDuePageNativePartialHeader(t *testing.T) {
	f := newDuePageFixture(t)
	rows := f.seed(t, 1, f.now)
	f.mutate = func(_ *pb.RangeRequest, r *pb.RangeResponse) {
		r.Header = &pb.ResponseHeader{Revision: r.Header.Revision}
	}
	f.earn()
	page, err := f.b.ReadDuePage(context.Background(), f.partition, f.share, nil)
	require.NoError(t, err, "native nested Range fills revision; only outer transaction carries cluster identity")
	require.Equal(t, rows, page.Candidates)
}

func jsonUnmarshalDueForTest(wire []byte, r *DueCandidateRecord) error {
	return json.Unmarshal(wire, r)
}

func TestDuePageCursorOriginAndCompaction(t *testing.T) {
	f := newDuePageFixture(t)
	f.seed(t, 119, f.now)
	f.earn()
	page, err := f.b.ReadDuePage(context.Background(), f.partition, f.share, nil)
	require.NoError(t, err)
	copy := *page.Next
	f.earn()
	_, err = f.b.ReadDuePage(context.Background(), f.partition, f.share, &copy)
	require.Error(t, err)
	other, err := f.b.AcquireWorkerBudget(context.Background(), f.session, WorkerBudgetRange, 1)
	require.NoError(t, err)
	_, err = f.b.ReadDuePage(context.Background(), f.partition, other, page.Next)
	require.Error(t, err)
	f.b.client.KV = &faultKV{KV: f.b.client.KV, match: func(ops []clientv3.Op) bool {
		for _, op := range ops {
			if op.IsGet() && len(op.RangeBytes()) > 0 {
				return true
			}
		}
		return false
	}, after: func(*clientv3.TxnResponse, error) (*clientv3.TxnResponse, error) { return nil, rpctypes.ErrCompacted }}
	_, err = f.b.ReadDuePage(context.Background(), f.partition, f.share, page.Next)
	require.ErrorIs(t, err, rpctypes.ErrCompacted)
	f.earn()
	_, err = f.b.ReadDuePage(context.Background(), f.partition, f.share, page.Next)
	require.Error(t, err, "compaction cannot silently switch the view")
}
