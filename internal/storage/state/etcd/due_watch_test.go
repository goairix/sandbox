package etcd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type dueWatchTestFixture struct {
	*dueExpiryFixture
	cache *dueWorkerCache
	scan  *dueWorkerScan
}

func newDueWatchTestFixture(t *testing.T) *dueWatchTestFixture {
	t.Helper()
	f := newDueExpiryFixture(t)
	cache, err := newDueWorkerCache(f.b, f.session)
	require.NoError(t, err)
	scan := dueCheckpointTestScan(t, f)
	f.earn()
	require.NoError(t, scan.step(context.Background()))
	require.NoError(t, cache.addPage(scan))
	return &dueWatchTestFixture{dueExpiryFixture: f, cache: cache, scan: scan}
}

func dueWatchTestHeader(s *workerTestServer) *pb.ResponseHeader {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.header()
}

func (f *dueWatchTestFixture) install(handler func(pb.Watch_WatchServer) error) {
	f.server.mu.Lock()
	f.server.watch = handler
	f.server.mu.Unlock()
}

func dueWatchTestCreates(stream pb.Watch_WatchServer, count int, header *pb.ResponseHeader) ([]*pb.WatchCreateRequest, error) {
	var creates []*pb.WatchCreateRequest
	for i := 0; i < count; i++ {
		request, err := stream.Recv()
		if err != nil {
			return nil, err
		}
		create := request.GetCreateRequest()
		if create == nil {
			return nil, fmt.Errorf("progress sent before all subscriptions were created")
		}
		creates = append(creates, create)
		if err := stream.Send(&pb.WatchResponse{Header: header, WatchId: create.WatchId, Created: true}); err != nil {
			return nil, err
		}
	}
	return creates, nil
}

func dueWatchTestProgress(stream pb.Watch_WatchServer, s *workerTestServer, suppress int) error {
	for count := 0; ; {
		request, err := stream.Recv()
		if err != nil {
			return nil
		}
		if request.GetProgressRequest() == nil {
			return fmt.Errorf("unexpected request after subscription setup")
		}
		count++
		if count <= suppress {
			continue
		}
		if err := stream.Send(&pb.WatchResponse{Header: dueWatchTestHeader(s), WatchId: -1}); err != nil {
			return nil
		}
	}
}

func (f *dueWatchTestFixture) progress(count int) {
	f.install(func(stream pb.Watch_WatchServer) error {
		if _, err := dueWatchTestCreates(stream, count, dueWatchTestHeader(f.server)); err != nil {
			return err
		}
		return dueWatchTestProgress(stream, f.server, 0)
	})
}

func (f *dueWatchTestFixture) run(t *testing.T) dueWatchBatchResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	result, err := f.b.runDueWatchBatch(ctx, f.cache)
	require.NoError(t, err)
	require.False(t, result.Invalidated)
	require.LessOrEqual(t, result.Frames, 512)
	require.LessOrEqual(t, result.Events, 10000)
	require.LessOrEqual(t, result.WireBytes, uint64(8*1024*1024))
	return result
}

func dueWatchTestCandidate(t *testing.T, f *dueWatchTestFixture, name string, revision int64) *mvccpb.Event {
	t.Helper()
	c := f.candidate
	c.SandboxID, c.IntentID = "watch-"+name, "watch-intent-"+name
	key, err := f.b.namespace.dueCandidateKey(c)
	require.NoError(t, err)
	wire, err := encodeDueCandidate(c)
	require.NoError(t, err)
	return &mvccpb.Event{Kv: &mvccpb.KeyValue{Key: []byte(key), Value: []byte(wire), CreateRevision: revision, ModRevision: revision, Version: 1}}
}

func TestDueWatchOriginalConnectionAndSharedSubscription(t *testing.T) {
	f := newDueWatchTestFixture(t)
	connection := f.b.client.ActiveConnection()
	watcher := f.b.client.Watcher
	f.b.client.Watcher = nil // A WatchChan implementation would fail here.
	defer func() { f.b.client.Watcher = watcher }()
	captured := make(chan []*pb.WatchCreateRequest, 1)
	f.install(func(stream pb.Watch_WatchServer) error {
		creates, err := dueWatchTestCreates(stream, 1, dueWatchTestHeader(f.server))
		if err != nil {
			return err
		}
		captured <- creates
		return dueWatchTestProgress(stream, f.server, 0)
	})
	result := f.run(t)
	creates := workerTestReceive(t, captured)
	require.Same(t, connection, f.b.client.ActiveConnection())
	require.Len(t, creates, 1)
	c := creates[0]
	require.Equal(t, int64(0xaa+1), c.WatchId)
	require.Equal(t, f.scan.checkpoint.record.Revision+1, c.StartRevision)
	require.Equal(t, f.b.namespace.Root()+"p/aa/due/", string(c.Key))
	require.Equal(t, string(c.Key)+f.scan.checkpoint.record.WindowEnd.Format(dueTimeLayout)+"0", string(c.RangeEnd))
	require.True(t, c.Fragment)
	require.True(t, c.ProgressNotify)
	require.False(t, c.PrevKv)
	require.Empty(t, c.Filters)
	require.Len(t, result.Partitions, 1)
	require.True(t, result.Partitions[0].Synchronized)
	f.server.mu.Lock()
	require.NotEmpty(t, f.server.watchPeer)
	require.Equal(t, f.server.txnPeer, f.server.watchPeer, "native Watch uses original authenticated client connection")
	f.server.mu.Unlock()
}

func TestDueWatchHistoricalHeaderAndNativeKeyOrder(t *testing.T) {
	t.Run("Created-above-historical-header-and-event", func(t *testing.T) {
		f := newDueWatchTestFixture(t)
		base := f.scan.checkpoint.record.Revision
		f.server.mu.Lock()
		f.server.revision = base + 100
		f.server.mu.Unlock()
		event := dueWatchTestCandidate(t, f, "historical", base+80)
		f.install(func(stream pb.Watch_WatchServer) error {
			created := dueWatchTestHeader(f.server)
			if _, err := dueWatchTestCreates(stream, 1, created); err != nil {
				return err
			}
			historical := *created
			historical.Revision = base + 90
			if err := stream.Send(&pb.WatchResponse{Header: &historical, WatchId: 0xab, Events: []*mvccpb.Event{event}}); err != nil {
				return err
			}
			for {
				if _, err := stream.Recv(); err != nil {
					return nil
				}
			} // no progress certificate
		})
		first := f.run(t)
		require.False(t, first.Partitions[0].Synchronized)
		require.Equal(t, base+80, first.Partitions[0].CoveredRevision)
		require.Equal(t, base+81, first.Partitions[0].ReplayFrom, "historical header is not event progress")
		resumed := make(chan int64, 1)
		f.install(func(stream pb.Watch_WatchServer) error {
			creates, err := dueWatchTestCreates(stream, 1, dueWatchTestHeader(f.server))
			if err != nil {
				return err
			}
			resumed <- creates[0].StartRevision
			return dueWatchTestProgress(stream, f.server, 0)
		})
		second := f.run(t)
		require.Equal(t, base+81, workerTestReceive(t, resumed))
		require.True(t, second.Partitions[0].Synchronized)
	})
	t.Run("reverse-distinct-keys-in-one-revision", func(t *testing.T) {
		f := newDueWatchTestFixture(t)
		revision := dueWatchTestHeader(f.server).Revision
		events := []*mvccpb.Event{dueWatchTestCandidate(t, f, "z", revision), dueWatchTestCandidate(t, f, "a", revision)}
		f.install(func(stream pb.Watch_WatchServer) error {
			if _, err := dueWatchTestCreates(stream, 1, dueWatchTestHeader(f.server)); err != nil {
				return err
			}
			if err := stream.Send(&pb.WatchResponse{Header: dueWatchTestHeader(f.server), WatchId: 0xab, Events: events}); err != nil {
				return err
			}
			return dueWatchTestProgress(stream, f.server, 0)
		})
		result := f.run(t)
		require.True(t, result.Partitions[0].Synchronized)
		require.Equal(t, 3, f.cache.snapshot().Keys)
	})
}

func TestDueWatchCreatedIsNotProgressAndRetriesAfterAllACKs(t *testing.T) {
	t.Run("Created-only", func(t *testing.T) {
		f := newDueWatchTestFixture(t)
		f.install(func(stream pb.Watch_WatchServer) error {
			if _, err := dueWatchTestCreates(stream, 1, dueWatchTestHeader(f.server)); err != nil {
				return err
			}
			for {
				if _, err := stream.Recv(); err != nil {
					return nil
				}
			}
		})
		result := f.run(t)
		require.True(t, result.Idle)
		require.False(t, result.Partitions[0].Synchronized)
		_, ok := f.cache.peek()
		require.False(t, ok)
	})
	t.Run("suppressed-progress-needs-another-request", func(t *testing.T) {
		f := newDueWatchTestFixture(t)
		var requests atomic.Int32
		f.install(func(stream pb.Watch_WatchServer) error {
			if _, err := dueWatchTestCreates(stream, 1, dueWatchTestHeader(f.server)); err != nil {
				return err
			}
			for {
				r, err := stream.Recv()
				if err != nil {
					return nil
				}
				if r.GetProgressRequest() == nil {
					return fmt.Errorf("unexpected request")
				}
				if requests.Add(1) == 1 {
					continue
				}
				if err := stream.Send(&pb.WatchResponse{Header: dueWatchTestHeader(f.server), WatchId: -1}); err != nil {
					return nil
				}
			}
		})
		result := f.run(t)
		require.True(t, result.Partitions[0].Synchronized)
		require.GreaterOrEqual(t, requests.Load(), int32(2))
		require.LessOrEqual(t, requests.Load(), int32(10))
	})
	t.Run("no-progress-before-last-Created-ACK", func(t *testing.T) {
		f := newDueWatchTestFixture(t)
		f.scan.page = nil
		p, err := f.b.ClaimWorkerPartition(context.Background(), f.session, 0xbb)
		require.NoError(t, err)
		scan, err := newDueWorkerScan(f.b, p, f.share, f.claimShare, 30*time.Second)
		require.NoError(t, err)
		f.earn()
		require.NoError(t, scan.step(context.Background()))
		require.NoError(t, f.cache.addPage(scan))
		f.install(func(stream pb.Watch_WatchServer) error {
			if _, err := dueWatchTestCreates(stream, 1, dueWatchTestHeader(f.server)); err != nil {
				return err
			}
			last, err := stream.Recv()
			if err != nil {
				return err
			}
			create := last.GetCreateRequest()
			if create == nil {
				return fmt.Errorf("progress preceded last subscription")
			}
			type received struct {
				request *pb.WatchRequest
				err     error
			}
			incoming := make(chan received, 1)
			go func() { r, err := stream.Recv(); incoming <- received{r, err} }()
			select {
			case <-incoming:
				return fmt.Errorf("progress sent before final Created ACK")
			case <-time.After(30 * time.Millisecond):
			case <-stream.Context().Done():
				return stream.Context().Err()
			}
			if err := stream.Send(&pb.WatchResponse{Header: dueWatchTestHeader(f.server), WatchId: create.WatchId, Created: true}); err != nil {
				return err
			}
			select {
			case got := <-incoming:
				if got.err != nil || got.request.GetProgressRequest() == nil {
					return fmt.Errorf("missing progress after final ACK")
				}
			case <-stream.Context().Done():
				return stream.Context().Err()
			}
			if err := stream.Send(&pb.WatchResponse{Header: dueWatchTestHeader(f.server), WatchId: -1}); err != nil {
				return err
			}
			for {
				if _, err := stream.Recv(); err != nil {
					return nil
				}
			}
		})
		result := f.run(t)
		require.Len(t, result.Partitions, 2)
		for _, row := range result.Partitions {
			require.True(t, row.Synchronized)
		}
	})
}

func TestDueWatchIdleFutureStartAndUnrelatedPostGuardTail(t *testing.T) {
	f := newDueWatchTestFixture(t)
	f.progress(1)
	first := f.run(t)
	v := first.Partitions[0].CoveredRevision
	require.True(t, first.Partitions[0].Synchronized)
	stopped := make(chan struct{})
	started := make(chan int64, 1)
	f.install(func(stream pb.Watch_WatchServer) error {
		defer close(stopped)
		creates, err := dueWatchTestCreates(stream, 1, dueWatchTestHeader(f.server))
		if err != nil {
			return err
		}
		started <- creates[0].StartRevision
		// Native progressIfSync suppresses start=V+1 when current global=V.
		for {
			if _, err := stream.Recv(); err != nil {
				return nil
			}
		}
	})
	originalKV := f.b.client.KV
	guards := 0
	f.b.client.KV = &faultKV{KV: originalKV, match: func(ops []clientv3.Op) bool {
		return len(ops) == 1 && ops[0].IsGet() && string(ops[0].KeyBytes()) == f.b.identityKey
	}, before: func() {
		guards++
		if guards == 2 {
			<-stopped
			f.server.put(f.b.namespace.Root()+"unrelated-after-watch", "tail", 0)
		}
	}}
	before, _, _ := f.server.counts()
	second := f.run(t)
	f.b.client.KV = originalKV
	after, _, _ := f.server.counts()
	require.Equal(t, before, after, "idle does not recreate session or Stage")
	require.Equal(t, v+1, workerTestReceive(t, started))
	row := second.Partitions[0]
	require.True(t, row.Synchronized, "pre-barrier was already covered; unrelated post-stop writes do not erase history")
	require.Equal(t, v, row.CoveredRevision)
	require.Greater(t, row.ObservedRevision, v, "post-guard tail is reported separately")
	require.Equal(t, v+1, row.ReplayFrom)
	require.Equal(t, f.scan.checkpoint.record.Revision+1, dueCheckpointTestRaw(t, f.duePageFixture).ReplayFrom)
	// Now that the NEXT pre-barrier is above V, Created alone is insufficient.
	f.install(func(stream pb.Watch_WatchServer) error {
		if _, err := dueWatchTestCreates(stream, 1, dueWatchTestHeader(f.server)); err != nil {
			return err
		}
		for {
			if _, err := stream.Recv(); err != nil {
				return nil
			}
		}
	})
	third := f.run(t)
	require.False(t, third.Partitions[0].Synchronized)
	f.progress(1)
	fourth := f.run(t)
	require.True(t, fourth.Partitions[0].Synchronized)
}

func TestDueWatchLateInsertBelowSettledFrontier(t *testing.T) {
	f := newDueWatchTestFixture(t)
	f.earnClaim()
	require.NoError(t, f.scan.step(context.Background()))
	f.cache.settled(f.candidate)
	frontier := dueCheckpointTestRaw(t, f.duePageFixture).Frontier
	candidate := f.candidate
	candidate.SandboxID, candidate.IntentID = "late-sandbox", "late-intent"
	candidate.ExpiresAt = candidate.ExpiresAt.Add(-time.Second)
	key, err := f.b.namespace.dueCandidateKey(candidate)
	require.NoError(t, err)
	require.Less(t, key, frontier)
	wire, err := encodeDueCandidate(candidate)
	require.NoError(t, err)
	f.server.put(key, wire, 0)
	kv := f.server.point(key)
	f.install(func(stream pb.Watch_WatchServer) error {
		if _, err := dueWatchTestCreates(stream, 1, dueWatchTestHeader(f.server)); err != nil {
			return err
		}
		if err := stream.Send(&pb.WatchResponse{Header: dueWatchTestHeader(f.server), WatchId: 0xab, Events: []*mvccpb.Event{{Kv: kv}}}); err != nil {
			return err
		}
		return dueWatchTestProgress(stream, f.server, 0)
	})
	result := f.run(t)
	require.True(t, result.Partitions[0].Synchronized)
	got, ok := f.cache.peek()
	require.True(t, ok)
	require.Equal(t, candidate, got)
	require.Equal(t, frontier, dueCheckpointTestRaw(t, f.duePageFixture).Frontier, "Watch discovery cannot regress or advance fixed-R settlement")
}

func TestDueWatchLaterRevisionDeleteNeedsNoCandidateValue(t *testing.T) {
	f := newDueWatchTestFixture(t)
	f.server.mu.Lock()
	f.server.revision += 2
	f.server.mu.Unlock()
	h := dueWatchTestHeader(f.server)
	put := dueWatchTestCandidate(t, f, "updated-and-deleted", h.Revision-1)
	deleted := &mvccpb.Event{Type: mvccpb.DELETE, Kv: &mvccpb.KeyValue{Key: put.Kv.Key, ModRevision: h.Revision}}
	f.install(func(stream pb.Watch_WatchServer) error {
		if _, err := dueWatchTestCreates(stream, 1, h); err != nil {
			return err
		}
		if err := stream.Send(&pb.WatchResponse{Header: h, WatchId: 0xab, Events: []*mvccpb.Event{put, deleted}}); err != nil {
			return err
		}
		return dueWatchTestProgress(stream, f.server, 0)
	})
	result := f.run(t)
	require.True(t, result.Partitions[0].Synchronized)
	require.Equal(t, h.Revision+1, result.Partitions[0].ReplayFrom)
	require.Equal(t, 1, f.cache.snapshot().Keys, "deleted candidate does not survive as a queued entry")
}

func TestDueWatchAggregateFrameLimitAcrossActualResponses(t *testing.T) {
	f := newDueWatchTestFixture(t)
	base := f.scan.checkpoint.record.Revision
	f.server.mu.Lock()
	f.server.revision = base + 10000 // historical catchup cannot finish in this script
	f.server.mu.Unlock()
	key, err := f.b.namespace.dueCandidateKey(f.candidate)
	require.NoError(t, err)
	wire, err := encodeDueCandidate(f.candidate)
	require.NoError(t, err)
	f.install(func(stream pb.Watch_WatchServer) error {
		if _, err := dueWatchTestCreates(stream, 1, dueWatchTestHeader(f.server)); err != nil {
			return err
		}
		for i := int64(1); i <= 600; i++ {
			h := dueWatchTestHeader(f.server)
			h.Revision = base + i
			kv := &mvccpb.KeyValue{Key: []byte(key), Value: []byte(wire), CreateRevision: base, ModRevision: base + i, Version: i + 1}
			if err := stream.Send(&pb.WatchResponse{Header: h, WatchId: 0xab, Events: []*mvccpb.Event{{Kv: kv}}}); err != nil {
				return nil
			}
		}
		for {
			if _, err := stream.Recv(); err != nil {
				return nil
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	result, err := f.b.runDueWatchBatch(ctx, f.cache)
	require.Error(t, err)
	require.True(t, result.Invalidated, "whole-stream counters cannot reset on each Recv")
	require.Zero(t, f.cache.snapshot().Keys)
}

func TestDueWatchFragmentOrderGapCompactionAndOversize(t *testing.T) {
	for _, scenario := range []string{"complete", "unfinished", "interleaved", "progress-inside-fragment", "assembly-events", "duplicate-key", "decreasing-revision", "unknown-watch-id", "unexpected-EOF", "compacted", "native-oversize", "tiny-events"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDueWatchTestFixture(t)
			h := dueWatchTestHeader(f.server)
			a := dueWatchTestCandidate(t, f, "fragment-z", h.Revision)
			b := dueWatchTestCandidate(t, f, "fragment-a", h.Revision)
			f.install(func(stream pb.Watch_WatchServer) error {
				if _, err := dueWatchTestCreates(stream, 1, h); err != nil {
					return err
				}
				first := &pb.WatchResponse{Header: h, WatchId: 0xab, Events: []*mvccpb.Event{a}}
				last := &pb.WatchResponse{Header: h, WatchId: 0xab, Events: []*mvccpb.Event{b}}
				switch scenario {
				case "unexpected-EOF":
					return nil
				case "complete", "unfinished", "interleaved", "progress-inside-fragment", "assembly-events":
					first.Fragment = true
				case "duplicate-key":
					first.Events = []*mvccpb.Event{a, a}
				case "decreasing-revision":
					b.Kv.ModRevision--
					b.Kv.CreateRevision--
					first.Events = []*mvccpb.Event{a, b}
				case "unknown-watch-id":
					first.WatchId = 7
				case "compacted":
					first.Events = nil
					first.Canceled, first.CompactRevision = true, h.Revision
				case "native-oversize":
					first = dueWatchCodecTestFrame(130, 1024, 1024)
					first.WatchId, first.Header = 0xab, h
				case "tiny-events":
					first.Events = make([]*mvccpb.Event, 119)
					for i := range first.Events {
						first.Events[i] = &mvccpb.Event{}
					}
				}
				if scenario == "assembly-events" {
					first.Events, last.Events = nil, nil
					for i := 0; i < 80; i++ {
						first.Events = append(first.Events, dueWatchTestCandidate(t, f, fmt.Sprintf("a-%03d", i), h.Revision))
						last.Events = append(last.Events, dueWatchTestCandidate(t, f, fmt.Sprintf("b-%03d", i), h.Revision))
					}
				}
				if err := stream.Send(first); err != nil {
					return nil
				}
				if scenario == "complete" || scenario == "assembly-events" {
					_ = stream.Send(last)
				}
				if scenario == "interleaved" {
					last.WatchId = 7
					_ = stream.Send(last)
				}
				if scenario == "progress-inside-fragment" {
					_ = stream.Send(&pb.WatchResponse{Header: h, WatchId: -1})
				}
				if scenario == "complete" {
					return dueWatchTestProgress(stream, f.server, 0)
				}
				for {
					if _, err := stream.Recv(); err != nil {
						return nil
					}
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			result, err := f.b.runDueWatchBatch(ctx, f.cache)
			if scenario == "complete" {
				require.NoError(t, err)
				require.False(t, result.Invalidated)
				require.Equal(t, 3, f.cache.snapshot().Keys)
			} else {
				require.Error(t, err)
				require.True(t, result.Invalidated)
				require.Zero(t, f.cache.snapshot().Keys)
				_, ok := f.cache.peek()
				require.False(t, ok)
				require.NotNil(t, f.server.point(dueCheckpointTestKey(f.duePageFixture)))
			}
		})
	}
}

// Compaction ends coverage for one actual subscription; it does not establish
// malformed data or retire its original session. Paired malformed responses
// must remain corruption rather than gaining a generic cancellation bypass.
func TestDueWatchCompactionClassificationRetainsOriginalOwner(t *testing.T) {
	for _, scenario := range []string{"compacted", "compacted-zero-header", "wrong-cluster", "unknown-watch-id", "missing-compaction", "not-canceled", "events", "fragment", "zero-header-wrong-cluster", "zero-header-missing-member", "zero-header-missing-term", "before-replay", "cancel-reason", "zero-header-progress"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDueWatchTestFixture(t)
			joined := make(chan struct{})
			f.install(func(stream pb.Watch_WatchServer) error {
				defer close(joined)
				header := dueWatchTestHeader(f.server)
				creates, err := dueWatchTestCreates(stream, 1, header)
				if err != nil {
					return err
				}
				reply := &pb.WatchResponse{Header: header, WatchId: creates[0].WatchId, Canceled: true, CompactRevision: header.Revision}
				if strings.HasPrefix(scenario, "zero-header-") || scenario == "compacted-zero-header" {
					copy := *header
					copy.Revision = 0
					reply.Header = &copy
				}
				switch scenario {
				case "wrong-cluster", "zero-header-wrong-cluster":
					copy := *reply.Header
					copy.ClusterId++
					reply.Header = &copy
				case "unknown-watch-id":
					reply.WatchId++
				case "missing-compaction":
					reply.CompactRevision = 0
				case "not-canceled":
					reply.Canceled = false
				case "events":
					reply.Events = []*mvccpb.Event{dueWatchTestCandidate(t, f, "unexpected-compaction-event", header.Revision)}
				case "fragment":
					reply.Fragment = true
				case "zero-header-missing-member":
					reply.Header.MemberId = 0
				case "zero-header-missing-term":
					reply.Header.RaftTerm = 0
				case "before-replay":
					reply.CompactRevision = creates[0].StartRevision - 1
				case "cancel-reason":
					reply.CancelReason = "unrecognized cancellation"
				case "zero-header-progress":
					reply.Canceled, reply.CompactRevision = false, 0
				}
				if err := stream.Send(reply); err != nil {
					return err
				}
				for {
					if _, err := stream.Recv(); err != nil {
						return nil
					}
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			result, err := f.b.runDueWatchBatch(ctx, f.cache)
			require.True(t, result.Invalidated)
			require.Zero(t, f.cache.snapshot().Keys)
			_, available := f.cache.peek()
			require.False(t, available)
			select {
			case <-joined:
			case <-ctx.Done():
				t.Fatal("original raw Watch handler did not join")
			}
			f.b.archiveLife.mu.Lock()
			live := f.session.live()
			owned := f.b.validWorkerBudget(f.share) && !f.share.lost && f.session.operation == nil && f.session.dueWatch == nil
			f.b.archiveLife.mu.Unlock()
			require.NoError(t, live)
			require.True(t, owned, "no replacement session/share or native owner may be adopted")
			if scenario == "compacted" || scenario == "compacted-zero-header" {
				require.ErrorIs(t, err, rpctypes.ErrCompacted)
				require.False(t, errors.Is(err, ErrCorruptRecord), "a legal same-subscription compaction is an expected relist")
			} else {
				require.ErrorIs(t, err, ErrCorruptRecord)
				require.False(t, errors.Is(err, rpctypes.ErrCompacted))
			}
		})
	}
}

func TestDueWatchAllPartitionsOriginalGuardsStayWithinNativeBounds(t *testing.T) {
	f := newDueWatchTestFixture(t)
	f.scan.page = nil // release the one globally accounted materialized page
	shares := []*WorkerBudgetShare{f.share}
	for i := uint16(1); i < 20; i++ {
		share, err := f.b.AcquireWorkerBudget(context.Background(), f.session, WorkerBudgetRange, i)
		require.NoError(t, err)
		shares = append(shares, share)
	}
	for i := 0; i < 256; i++ {
		if i == 0xaa {
			continue
		}
		if i%16 == 0 {
			require.NoError(t, f.b.RenewWorkerSession(context.Background(), f.session))
		}
		p, err := f.b.ClaimWorkerPartition(context.Background(), f.session, uint8(i))
		require.NoError(t, err)
		scan, err := newDueWorkerScan(f.b, p, shares[i%20], f.claimShare, 30*time.Second)
		require.NoError(t, err)
		f.b.archiveLife.mu.Lock()
		scan.ranges.bucket.last = time.Now().Add(-time.Second)
		f.b.archiveLife.mu.Unlock()
		require.NoError(t, scan.step(context.Background()))
		require.NoError(t, f.cache.addPage(scan))
		scan.page = nil
	}
	f.server.mu.Lock()
	baseline := len(f.server.requests)
	f.server.mu.Unlock()
	f.progress(256)
	result := f.run(t)
	require.Len(t, result.Partitions, 256)
	for _, row := range result.Partitions {
		require.True(t, row.Synchronized)
	}
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	fences := make(map[string]int)
	guards := 0
	for _, q := range f.server.requests[baseline:] {
		if len(q.Success) != 1 || q.Success[0].GetRequestRange() == nil || string(q.Success[0].GetRequestRange().Key) != f.b.identityKey {
			continue
		}
		guards++
		require.LessOrEqual(t, len(q.Compare), 56)
		require.Len(t, q.Failure, 4)
		require.LessOrEqual(t, len(q.Compare)+len(q.Success)+len(q.Failure), 61)
		require.LessOrEqual(t, q.Size(), 128*1024)
		seen := make(map[string]bool)
		for _, cmp := range q.Compare {
			if cmp.Target != pb.Compare_VALUE {
				continue
			}
			key := string(cmp.Key)
			require.False(t, seen[key], "one complete fence per key in each grouped guard")
			seen[key] = true
			fences[key]++
		}
	}
	require.LessOrEqual(t, guards, 104)
	for _, p := range f.session.partitions {
		require.GreaterOrEqual(t, fences[p.fence.key], 2)
	}
	for _, share := range shares {
		require.GreaterOrEqual(t, fences[share.fence.key], 2)
	}
}

func TestDueWatchReleaseAndCloseJoinNativeOwner(t *testing.T) {
	for _, release := range []string{"partition", "range-share", "session", "Backend.Close"} {
		t.Run(release, func(t *testing.T) {
			f := newDueWatchTestFixture(t)
			entered, stopped := make(chan struct{}), make(chan struct{})
			f.install(func(stream pb.Watch_WatchServer) error {
				defer close(stopped)
				if _, err := dueWatchTestCreates(stream, 1, dueWatchTestHeader(f.server)); err != nil {
					return err
				}
				close(entered)
				for {
					if _, err := stream.Recv(); err != nil {
						return nil
					}
				}
			})
			done := make(chan error, 1)
			go func() { _, err := f.b.runDueWatchBatch(context.Background(), f.cache); done <- err }()
			workerTestReceive(t, entered)
			f.b.archiveLife.mu.Lock()
			owner := f.session.dueWatch
			f.b.archiveLife.mu.Unlock()
			require.NotNil(t, owner)
			var err error
			switch release {
			case "partition":
				err = f.b.ReleaseWorkerPartition(context.Background(), f.partition)
			case "range-share":
				err = f.b.ReleaseWorkerBudget(context.Background(), f.share)
			case "session":
				err = f.b.ReleaseWorkerSession(context.Background(), f.session)
			case "Backend.Close":
				err = f.b.Close()
			}
			require.NoError(t, err)
			workerTestReceive(t, done)
			workerTestReceive(t, stopped)
			select {
			case <-owner.done:
			default:
				t.Fatal("release returned before actual original Watch owner finished")
			}
		})
	}
}

func TestDueWatchReleaseWaitsForActualBlockedGuardReturn(t *testing.T) {
	f := newDueWatchTestFixture(t)
	arrived, unblock := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unlock := func() { once.Do(func() { close(unblock) }) }
	t.Cleanup(unlock)
	originalKV := f.b.client.KV
	f.b.client.KV = &faultKV{KV: originalKV, match: func(ops []clientv3.Op) bool {
		return len(ops) == 1 && ops[0].IsGet() && string(ops[0].KeyBytes()) == f.b.identityKey
	}, before: func() { close(arrived); <-unblock }}
	done := make(chan error, 1)
	go func() { _, err := f.b.runDueWatchBatch(context.Background(), f.cache); done <- err }()
	workerTestReceive(t, arrived)
	released := make(chan error, 1)
	go func() { released <- f.b.ReleaseWorkerPartition(context.Background(), f.partition) }()
	select {
	case <-released:
		t.Fatal("cancel incorrectly freed the blocked native owner")
	case <-time.After(20 * time.Millisecond):
	}
	f.b.archiveLife.mu.Lock()
	owner, call := f.session.dueWatch, f.session.operation
	f.b.archiveLife.mu.Unlock()
	require.NotNil(t, owner)
	require.NotNil(t, call)
	unlock()
	workerTestReceive(t, done)
	workerTestReceive(t, released)
	f.b.client.KV = originalKV
	select {
	case <-owner.done:
	default:
		t.Fatal("blocked original owner was not physically joined")
	}
}

func TestDueWatchCompactedRangeRetainsHealthyOriginalSession(t *testing.T) {
	f := newDueWatchTestFixture(t)
	originalKV := f.b.client.KV
	f.b.client.KV = &faultKV{KV: originalKV, match: duePageRangeOps, after: func(*clientv3.TxnResponse, error) (*clientv3.TxnResponse, error) { return nil, rpctypes.ErrCompacted }}
	f.earn()
	_, err := f.b.ReadDuePage(context.Background(), f.partition, f.share, nil)
	require.ErrorIs(t, err, rpctypes.ErrCompacted)
	f.b.client.KV = originalKV
	f.b.archiveLife.mu.Lock()
	live := f.session.live()
	f.b.archiveLife.mu.Unlock()
	require.NoError(t, live, "definite discovery compaction plus actual original guard must not churn session")
	f.earn()
	page, err := f.b.ReadDuePage(context.Background(), f.partition, f.share, nil)
	require.NoError(t, err)
	require.NotEmpty(t, page.Candidates)
}

func TestDueWatchRepeatedProgressCertificate(t *testing.T) {
	f := newDueWatchTestFixture(t)
	v := f.scan.checkpoint.record.Revision + 1
	f.server.put(f.b.namespace.Root()+"unrelated-before-watch", "barrier", 0)
	b := dueWatchTestHeader(f.server)
	require.Greater(t, b.Revision, v)
	f.install(func(stream pb.Watch_WatchServer) error {
		if _, err := dueWatchTestCreates(stream, 1, b); err != nil {
			return err
		}
		prior := *b
		prior.Revision = v
		for i := 0; i < 2; i++ {
			if err := stream.Send(&pb.WatchResponse{Header: &prior, WatchId: -1}); err != nil {
				return err
			}
		}
		if err := stream.Send(&pb.WatchResponse{Header: b, WatchId: -1}); err != nil {
			return err
		}
		for {
			if _, err := stream.Recv(); err != nil {
				return nil
			}
		}
	})
	result := f.run(t)
	require.True(t, result.Partitions[0].Synchronized)
	require.Equal(t, b.Revision, result.Partitions[0].CoveredRevision)
	require.Equal(t, b.Revision+1, result.Partitions[0].ReplayFrom)
}
