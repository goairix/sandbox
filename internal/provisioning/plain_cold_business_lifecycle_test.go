package provisioning

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	kube "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/goairix/sandbox/internal/sandboxsnapshot"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/goairix/sandbox/pkg/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"google.golang.org/grpc"
	"k8s.io/client-go/rest"
)

// This endpoint exercises only cancellation of the original concrete Backend
// lookup. It never provides creation, signed publication or runtime success.
type businessLookupCancellationServer struct {
	pb.UnimplementedKVServer
	pb.UnimplementedMaintenanceServer
	identity []byte
	root     string
	entered  chan struct{}
	joined   chan struct{}
}

func (s *businessLookupCancellationServer) Status(context.Context, *pb.StatusRequest) (*pb.StatusResponse, error) {
	return &pb.StatusResponse{Header: &pb.ResponseHeader{ClusterId: 1}}, nil
}

func (s *businessLookupCancellationServer) Txn(ctx context.Context, request *pb.TxnRequest) (*pb.TxnResponse, error) {
	if len(request.Compare) == 0 {
		response := &pb.TxnResponse{Header: &pb.ResponseHeader{ClusterId: 1}, Succeeded: true}
		for _, pair := range []struct {
			key   string
			value []byte
		}{{s.root + "meta/identity", s.identity}, {s.root + "meta/restore_epoch", []byte("restore")}} {
			response.Responses = append(response.Responses, &pb.ResponseOp{Response: &pb.ResponseOp_ResponseRange{ResponseRange: &pb.RangeResponse{Header: response.Header, Count: 1, Kvs: []*mvccpb.KeyValue{{Key: []byte(pair.key), Value: pair.value}}}}})
		}
		return response, nil
	}
	close(s.entered)
	<-ctx.Done()
	close(s.joined)
	return nil, ctx.Err()
}

func TestPlainColdBusinessLookupSingleAdmissionAndClose(t *testing.T) {
	ns, err := state.NewNamespace("/sandbox", "business", "cell")
	require.NoError(t, err)
	identity := state.Identity{SchemaVersion: 1, Prefix: "/sandbox", AuthorityID: "business", Cell: "cell", ClusterID: 1, StorageID: "storage", RuntimeID: "launcher", RestoreEpoch: "restore"}
	wire, err := json.Marshal(identity)
	require.NoError(t, err)
	endpoint := &businessLookupCancellationServer{identity: wire, root: ns.Root(), entered: make(chan struct{}), joined: make(chan struct{})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	rpc := grpc.NewServer()
	pb.RegisterKVServer(rpc, endpoint)
	pb.RegisterMaintenanceServer(rpc, endpoint)
	served := make(chan error, 1)
	go func() { served <- rpc.Serve(listener) }()
	t.Cleanup(func() { rpc.Stop(); require.NoError(t, <-served) })
	backend, err := state.New(context.Background(), state.Options{Endpoints: []string{"http://" + listener.Addr().String()}, Namespace: ns, Identity: identity, AllowInsecureLoopback: true, RequestTimeout: time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	api := kube.EnrolledBridgeOptions{Config: &rest.Config{Host: "https://127.0.0.1:1"}, Namespace: "runtimes", MaxActive: 1, RequestTimeout: time.Second}
	driver, err := kube.NewEnrolledCreateClient(kube.EnrolledCreateOptions{API: api, Image: "registry.example/sandbox:v1@sha256:" + strings.Repeat("a", 64), NodeName: "node", NodeUID: uuid.NewString(), NodeBootID: uuid.NewString()})
	require.NoError(t, err)
	bridge, err := kube.NewEnrolledBridgeClient(api)
	require.NoError(t, err)
	s := &PlainColdService{backend: backend, driver: driver, bridge: bridge, business: &PlainColdBusinessOptions{Principal: "operator", Defaults: sandboxsnapshot.Defaults{TimeoutSeconds: 60, Resources: types.ResourceLimits{CPU: "2", Memory: "512Mi", Disk: "1Mi", TmpDisk: "50Mi"}}}, limit: 1, active: make(map[*plainColdCall]struct{}), closeDone: make(chan struct{})}
	s.self = s
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	input := PlainColdBusinessInput{IdempotencyKey: "key", Request: types.CreateSandboxRequest{Mode: "ephemeral"}}
	type answer struct {
		result PlainColdBusinessResult
		err    error
	}
	done := make(chan answer, 1)
	go func() {
		result, callErr := s.CreateBusiness(context.Background(), input)
		done <- answer{result, callErr}
	}()
	select {
	case <-endpoint.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("business lookup did not start")
	}
	s.mu.Lock()
	active := len(s.active)
	s.mu.Unlock()
	require.Equal(t, 1, active, "the actual business lookup occupies the original service admission")
	_, err = s.CreateBusiness(context.Background(), input)
	require.ErrorIs(t, err, state.ErrConflict, "the lookup cannot bypass MaxActive")
	require.NoError(t, s.Close())
	result := <-done
	require.Error(t, result.err, "Close cannot turn a cancelled lookup into business success")
	require.Equal(t, "key", result.result.IdempotencyKey)
	require.Empty(t, s.active, "Close joined the actual admitted business call")
	select {
	case <-endpoint.joined:
	case <-time.After(time.Second):
		t.Fatal("cancelled original lookup handler was not joined")
	}
}
