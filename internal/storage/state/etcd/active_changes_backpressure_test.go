package etcd

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"google.golang.org/grpc"
)

// A real gRPC transport lets the test distinguish bounded backpressure from an
// application queue placed in front of clientv3's unbounded Watch buffer.
type activeWatchFloodServer struct {
	pb.UnimplementedKVServer
	pb.UnimplementedWatchServer
	eventsPerResponse int
	responses         int
	payload           int
	sentAll           chan struct{}
}

func (s *activeWatchFloodServer) Range(context.Context, *pb.RangeRequest) (*pb.RangeResponse, error) {
	return &pb.RangeResponse{Header: &pb.ResponseHeader{Revision: 1}}, nil
}
func (s *activeWatchFloodServer) Watch(stream pb.Watch_WatchServer) error {
	request, err := stream.Recv()
	if err != nil {
		return err
	}
	create := request.GetCreateRequest()
	if err := stream.Send(&pb.WatchResponse{Header: &pb.ResponseHeader{Revision: 1}, Created: true}); err != nil {
		return err
	}
	for n := 0; n < s.responses; n++ {
		response := &pb.WatchResponse{Header: &pb.ResponseHeader{Revision: int64(n + 2)}}
		for i := 0; i < s.eventsPerResponse; i++ {
			response.Events = append(response.Events, &mvccpb.Event{Type: mvccpb.PUT, Kv: &mvccpb.KeyValue{Key: []byte(fmt.Sprintf("%sitem%d", create.Key, i)), Value: make([]byte, s.payload), ModRevision: int64(n + 2)}})
		}
		if err := stream.Send(response); err != nil {
			return err
		}
	}
	select {
	case <-s.sentAll:
	default:
		close(s.sentAll)
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}
func activeWatchTransportFixture(t *testing.T, flood *activeWatchFloodServer) *ActiveSandboxRepository {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	pb.RegisterKVServer(server, flood)
	pb.RegisterWatchServer(server, flood)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	store, err := New(context.Background(), Options{Endpoints: []string{listener.Addr().String()}, Prefix: "/watch-budget/", Timeout: time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	repo, err := NewActiveSandboxRepository(store, "watch-budget")
	require.NoError(t, err)
	return repo
}
func TestActiveChangesSlowConsumerBackpressuresIngress(t *testing.T) {
	flood := &activeWatchFloodServer{eventsPerResponse: 1, responses: 1024, payload: 64 << 10, sentAll: make(chan struct{})}
	repo := activeWatchTransportFixture(t, flood)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := repo.WatchActiveChanges(ctx, 0)
	require.NoError(t, err)
	// Intentionally never consume the first notification. A 64MiB flood must
	// block at transport flow control rather than accumulate in client memory.
	select {
	case <-flood.sentAll:
		t.Fatal("slow consumer allowed the entire flood into an unbounded ingress buffer")
	case <-time.After(2 * time.Second):
	}
	cancel()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	delivered := 0
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				require.LessOrEqual(t, delivered, 1, "only the already decoded batch may race cancellation")
				return
			}
			delivered++
		case <-deadline.C:
			t.Fatal("blocked ingress did not stop on cancellation")
		}
	}
}
func TestActiveChangesResetsOnEventBudget(t *testing.T) {
	flood := &activeWatchFloodServer{eventsPerResponse: 4097, responses: 1, sentAll: make(chan struct{})}
	repo := activeWatchTransportFixture(t, flood)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := repo.WatchActiveChanges(ctx, 0)
	require.NoError(t, err)
	select {
	case batch := <-ch:
		require.True(t, batch.Reset, "oversized revision must request authoritative rebuilding")
		require.Empty(t, batch.Changes)
	case <-time.After(3 * time.Second):
		t.Fatal("missing overflow reset")
	}
}

func TestActiveChangesResetsOnMessageBudget(t *testing.T) {
	flood := &activeWatchFloodServer{eventsPerResponse: 1, responses: 1, payload: 5 << 20, sentAll: make(chan struct{})}
	repo := activeWatchTransportFixture(t, flood)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := repo.WatchActiveChanges(ctx, 0)
	require.NoError(t, err)
	select {
	case batch := <-ch:
		require.True(t, batch.Reset, "oversized transport message must trigger bounded recovery")
		require.Empty(t, batch.Changes)
	case <-time.After(3 * time.Second):
		t.Fatal("missing message-budget reset")
	}
}
