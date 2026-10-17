package etcd

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The second host listener shares the existing protocol model's durable state.
// Faults live at the real gRPC boundary, rather than replacing any production
// RPC helper. This model proves envelope and send-count behavior, not etcd
// transaction semantics, TLS bytes, HTTP/2 frames or status/trailer accounting.
type coldQuotaRPCHost struct {
	mu sync.Mutex

	counts map[string]int
	fault  error
	wire   []byte
	gate   chan struct{}
	seen   chan struct{}
}

func (h *coldQuotaRPCHost) intercept(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	h.mu.Lock()
	h.counts[info.FullMethod]++
	fault, gate, seen := h.fault, h.gate, h.seen
	if seen != nil {
		h.seen = nil
	}
	h.mu.Unlock()
	if seen != nil {
		close(seen)
	}
	if gate != nil {
		<-gate
	}
	if fault != nil {
		return nil, fault
	}
	return handler(ctx, request)
}

func (h *coldQuotaRPCHost) count(method string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counts[method]
}

// Only tests may inject malformed serialized ACKs. The production codec still
// receives those bytes through an actual unary transport before Unmarshal.
type coldQuotaRPCServerCodec struct{ host *coldQuotaRPCHost }

func (c *coldQuotaRPCServerCodec) Name() string { return "proto" }

func (c *coldQuotaRPCServerCodec) Marshal(value any) ([]byte, error) {
	c.host.mu.Lock()
	wire := c.host.wire
	c.host.mu.Unlock()
	if wire != nil {
		return wire, nil
	}
	return value.(interface{ Marshal() ([]byte, error) }).Marshal()
}

func (c *coldQuotaRPCServerCodec) Unmarshal(wire []byte, value any) error {
	return value.(interface{ Unmarshal([]byte) error }).Unmarshal(wire)
}

func newColdQuotaRPCHost(t *testing.T) (*coldQuotaAdmissionFixture, *coldQuotaRPCHost) {
	t.Helper()
	f := newColdQuotaAdmissionFixture(t)
	h := &coldQuotaRPCHost{counts: make(map[string]int)}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer(grpc.UnaryInterceptor(h.intercept), grpc.ForceServerCodec(&coldQuotaRPCServerCodec{host: h}))
	pb.RegisterKVServer(server, f.server)
	pb.RegisterLeaseServer(server, f.server)
	pb.RegisterAuthServer(server, &pb.UnimplementedAuthServer{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(listener)
	}()
	endpoint := "http://" + listener.Addr().String()
	// A real native retry policy makes removal of MaxRetryRPCBufferSize(0)
	// observable: gRPC otherwise reuses its serialized payload without calling
	// Marshal again. Disable the resolver's service config only on this test
	// client so its default policy is applied instead of the etcd resolver's.
	const retryPolicy = `{"methodConfig":[{"name":[{"service":"etcdserverpb.Lease","method":"LeaseGrant"}],"retryPolicy":{"maxAttempts":3,"initialBackoff":"0.001s","maxBackoff":"0.002s","backoffMultiplier":1,"retryableStatusCodes":["UNAVAILABLE"]}}]}`
	transport, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{endpoint, endpoint},
		DialTimeout: time.Second,
		DialOptions: []grpc.DialOption{grpc.WithBlock(), grpc.WithDisableServiceConfig(), grpc.WithDefaultServiceConfig(retryPolicy)},
	})
	require.NoError(t, err)
	require.NotNil(t, transport.ActiveConnection().GetMethodConfig("/etcdserverpb.Lease/LeaseGrant").RetryPolicy)
	original := f.b.client
	f.b.client = transport
	t.Cleanup(func() {
		f.b.client = original
		require.NoError(t, transport.Close())
		server.Stop()
		_ = listener.Close()
		<-done
	})
	return f, h
}

func coldQuotaRPCPointRequest(key string) *pb.TxnRequest {
	return &pb.TxnRequest{Success: []*pb.RequestOp{{Request: &pb.RequestOp_RequestRange{RequestRange: &pb.RangeRequest{Key: []byte(key)}}}}}
}

func TestColdQuotaWorkerRPCStageUsesOneOriginalCallAndFourNativeSends(t *testing.T) {
	f, host := newColdQuotaRPCHost(t)
	call, _, err := f.begin()
	require.NoError(t, err)
	defer call.finish()
	connection := f.b.client.ActiveConnection()
	grant, err := f.b.coldQuotaWorkerGrantRPC(call)
	require.NoError(t, err)
	require.Equal(t, int64(10), grant.TTL)
	request := coldQuotaRPCPointRequest(f.b.identityKey)
	guard, err := f.b.coldQuotaWorkerGuardRPC(call, request)
	require.NoError(t, err)
	require.True(t, guard.Succeeded)
	commit, err := f.b.coldQuotaWorkerCommitRPC(call, request)
	require.NoError(t, err)
	require.True(t, commit.Succeeded)
	_, err = f.b.coldQuotaWorkerRevokeRPC(call, &pb.LeaseRevokeRequest{ID: grant.ID})
	require.NoError(t, err)
	require.Equal(t, 1, host.count("/etcdserverpb.Lease/LeaseGrant"))
	require.Equal(t, 2, host.count("/etcdserverpb.KV/Txn"))
	require.Equal(t, 1, host.count("/etcdserverpb.Lease/LeaseRevoke"))
	require.Same(t, connection, f.b.client.ActiveConnection())
	require.Same(t, call, f.session.operation)

	_, err = f.b.coldQuotaWorkerGrantRPC(call)
	require.Error(t, err)
	_, err = f.b.coldQuotaWorkerJournalRPC(call, request)
	require.Error(t, err)
	_, err = f.b.coldQuotaWorkerRevokeRPC(call, &pb.LeaseRevokeRequest{ID: grant.ID})
	require.Error(t, err)
	require.Equal(t, 1, host.count("/etcdserverpb.Lease/LeaseGrant"))
	require.Equal(t, 2, host.count("/etcdserverpb.KV/Txn"))
	require.Equal(t, 1, host.count("/etcdserverpb.Lease/LeaseRevoke"))
}

func TestColdQuotaWorkerRPCJournalPreservesActualFailureAndExcludesStageCohort(t *testing.T) {
	f, host := newColdQuotaRPCHost(t)
	call, _, err := f.begin()
	require.NoError(t, err)
	defer call.finish()
	request := &pb.TxnRequest{
		Compare: taskProtoComparisons([]clientv3.Cmp{clientv3.Compare(clientv3.CreateRevision(f.b.identityKey), "=", 0)}),
		Success: taskProtoPoints([]string{f.b.identityKey}),
		Failure: taskProtoPoints([]string{f.b.identityKey, f.b.restoreKey, f.b.workerControl.profile.key}),
	}
	response, err := f.b.coldQuotaWorkerJournalRPC(call, request)
	require.NoError(t, err)
	require.False(t, response.Succeeded)
	require.Len(t, response.Responses, 3)
	require.Equal(t, []byte(f.b.workerControl.profile.key), response.Responses[2].GetResponseRange().Kvs[0].Key)
	_, err = f.b.coldQuotaWorkerJournalRPC(call, request)
	require.Error(t, err)
	_, err = f.b.coldQuotaWorkerGrantRPC(call)
	require.Error(t, err)
	require.Equal(t, 1, host.count("/etcdserverpb.KV/Txn"))
	require.Zero(t, host.count("/etcdserverpb.Lease/LeaseGrant"))
}

func TestColdQuotaWorkerRPCRejectsOutOfOrderAndForeignCallWithoutSending(t *testing.T) {
	f, host := newColdQuotaRPCHost(t)
	call, _, err := f.begin()
	require.NoError(t, err)
	defer call.finish()
	copy := *call
	_, err = f.b.coldQuotaWorkerGrantRPC(&copy)
	require.Error(t, err)
	_, err = f.b.coldQuotaWorkerCommitRPC(call, coldQuotaRPCPointRequest(f.b.identityKey))
	require.Error(t, err)
	_, err = f.b.coldQuotaWorkerRevokeRPC(call, &pb.LeaseRevokeRequest{ID: f.session.reference.LeaseID})
	require.Error(t, err)
	require.Zero(t, host.count("/etcdserverpb.Lease/LeaseGrant"))
	require.Zero(t, host.count("/etcdserverpb.KV/Txn"))
	require.Zero(t, host.count("/etcdserverpb.Lease/LeaseRevoke"))
	_, err = f.b.coldQuotaWorkerGrantRPC(call)
	require.NoError(t, err)
}

func TestColdQuotaWorkerRPCRejectsUnboundedRequestsBeforeNativeSend(t *testing.T) {
	for _, name := range []string{"too-many-nodes", "too-many-bytes", "nested-txn", "recursive-txn", "range-prefix", "prev-kv"} {
		t.Run(name, func(t *testing.T) {
			f, host := newColdQuotaRPCHost(t)
			call, _, err := f.begin()
			require.NoError(t, err)
			defer call.finish()
			request := coldQuotaRPCPointRequest(f.b.identityKey)
			switch name {
			case "too-many-nodes":
				for len(request.Success) < 65 {
					request.Success = append(request.Success, request.Success[0])
				}
			case "too-many-bytes":
				request.Success = []*pb.RequestOp{taskProtoPut(f.b.identityKey, strings.Repeat("x", 128*1024), 0)}
			case "nested-txn":
				request.Success = []*pb.RequestOp{{Request: &pb.RequestOp_RequestTxn{RequestTxn: coldQuotaRPCPointRequest(f.b.identityKey)}}}
			case "recursive-txn":
				request.Success = []*pb.RequestOp{{Request: &pb.RequestOp_RequestTxn{RequestTxn: request}}}
			case "range-prefix":
				request.Success[0].GetRequestRange().RangeEnd = []byte("z")
			case "prev-kv":
				request.Success = []*pb.RequestOp{{Request: &pb.RequestOp_RequestPut{RequestPut: &pb.PutRequest{Key: []byte(f.b.identityKey), Value: []byte("x"), PrevKv: true}}}}
			}
			_, err = f.b.coldQuotaWorkerJournalRPC(call, request)
			require.Error(t, err)
			require.Zero(t, host.count("/etcdserverpb.KV/Txn"))
		})
	}
}

// Removing the original call's aggregate request charge would allow these
// individually legal requests to exceed one paid 256 KiB protobuf cohort.
// The refused Commit burns its phase, while the trusted Grant's cleanup stays
// reserved and available through the same original call.
func TestColdQuotaWorkerRPCAggregateRefusalRetainsOriginalRevokeBudget(t *testing.T) {
	f, host := newColdQuotaRPCHost(t)
	call, _, err := f.begin()
	require.NoError(t, err)
	defer call.finish()
	grant, err := f.b.coldQuotaWorkerGrantRPC(call)
	require.NoError(t, err)
	guardKey, err := f.b.namespace.Key("meta", "rpc_budget_guard")
	require.NoError(t, err)
	commitKey, err := f.b.namespace.Key("meta", "rpc_budget_commit")
	require.NoError(t, err)
	guard := &pb.TxnRequest{Success: []*pb.RequestOp{taskProtoPut(guardKey, strings.Repeat("g", 120000), 0)}}
	commit := &pb.TxnRequest{Success: []*pb.RequestOp{taskProtoPut(commitKey, strings.Repeat("c", 105000), 0)}}
	require.Less(t, guard.Size(), 131072)
	require.Less(t, commit.Size(), 131072)
	require.Greater(t, guard.Size()+commit.Size()+2+10+37888, 262144)
	_, err = f.b.coldQuotaWorkerGuardRPC(call, guard)
	require.NoError(t, err)
	_, err = f.b.coldQuotaWorkerCommitRPC(call, commit)
	require.Error(t, err)
	_, err = f.b.coldQuotaWorkerCommitRPC(call, coldQuotaRPCPointRequest(f.b.identityKey))
	require.Error(t, err, "aggregate refusal must burn the original Commit phase")
	require.Equal(t, 1, host.count("/etcdserverpb.KV/Txn"))
	_, err = f.b.coldQuotaWorkerRevokeRPC(call, &pb.LeaseRevokeRequest{ID: grant.ID})
	require.NoError(t, err)
	require.Equal(t, 1, host.count("/etcdserverpb.Lease/LeaseRevoke"))
}

func TestColdQuotaWorkerRPCFaultDoesNotResendOriginalPayload(t *testing.T) {
	for name, fault := range map[string]error{
		"auth-old-revision": rpctypes.ErrGRPCAuthOldRevision,
		"learner":           rpctypes.ErrGRPCNotSupportedForLearner,
		"unavailable":       status.Error(codes.Unavailable, "host unavailable"),
		"deadline":          status.Error(codes.DeadlineExceeded, "host deadline"),
		"cancel":            status.Error(codes.Canceled, "host cancel"),
	} {
		t.Run(name, func(t *testing.T) {
			f, host := newColdQuotaRPCHost(t)
			call, _, err := f.begin()
			require.NoError(t, err)
			defer call.finish()
			host.mu.Lock()
			host.fault = fault
			host.mu.Unlock()
			_, err = f.b.coldQuotaWorkerGrantRPC(call)
			require.ErrorIs(t, err, ErrOutcomeUnknown)
			require.Equal(t, 1, host.count("/etcdserverpb.Lease/LeaseGrant"))
			require.Zero(t, host.count("/etcdserverpb.Auth/Authenticate"))
			_, err = f.b.coldQuotaWorkerGrantRPC(call)
			require.Error(t, err)
			require.Equal(t, 1, host.count("/etcdserverpb.Lease/LeaseGrant"))
		})
	}
}

func TestColdQuotaWorkerRPCCleanupRetainsOriginalLeaseAfterCancellation(t *testing.T) {
	f, host := newColdQuotaRPCHost(t)
	call, _, err := f.begin()
	require.NoError(t, err)
	defer call.finish()
	grant, err := f.b.coldQuotaWorkerGrantRPC(call)
	require.NoError(t, err)
	_, err = f.b.coldQuotaWorkerRevokeRPC(call, &pb.LeaseRevokeRequest{ID: grant.ID + 100})
	require.Error(t, err)
	require.Zero(t, host.count("/etcdserverpb.Lease/LeaseRevoke"))
	f.owner.cancel()
	f.session.cancel()
	f.b.archiveLife.mu.Lock()
	f.b.archiveLife.closed = true
	f.b.archiveLife.mu.Unlock()
	t.Cleanup(func() {
		f.b.archiveLife.mu.Lock()
		f.b.archiveLife.closed = false
		f.b.archiveLife.mu.Unlock()
	})
	_, err = f.b.coldQuotaWorkerRevokeRPC(call, &pb.LeaseRevokeRequest{ID: grant.ID})
	require.NoError(t, err)
	require.Equal(t, 1, host.count("/etcdserverpb.Lease/LeaseRevoke"))
	require.Same(t, call, f.session.operation)
}

func TestColdQuotaWorkerRPCBadTTLStillPermitsTrustedLeaseCleanup(t *testing.T) {
	f, host := newColdQuotaRPCHost(t)
	call, _, err := f.begin()
	require.NoError(t, err)
	defer call.finish()
	f.server.mu.Lock()
	f.server.grantReply = func(response *pb.LeaseGrantResponse) { response.TTL = 0 }
	f.server.mu.Unlock()
	_, err = f.b.coldQuotaWorkerGrantRPC(call)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	f.server.mu.Lock()
	lease := f.server.nextLease
	f.server.mu.Unlock()
	_, err = f.b.coldQuotaWorkerRevokeRPC(call, &pb.LeaseRevokeRequest{ID: lease})
	require.NoError(t, err)
	require.Equal(t, 1, host.count("/etcdserverpb.Lease/LeaseRevoke"))
}

func TestColdQuotaWorkerRPCMalformedACKIsRejectedAtActualUnaryBoundary(t *testing.T) {
	for name, wire := range map[string][]byte{
		"unknown":      {0x28, 1},
		"duplicate-id": {0x10, 1, 0x10, 2},
		"truncated":    {0x0a, 0xff},
		"oversized":    make([]byte, 4097),
		"long-error":   append([]byte{0x22, 0x81, 0x08}, make([]byte, 1025)...),
	} {
		t.Run(name, func(t *testing.T) {
			f, host := newColdQuotaRPCHost(t)
			call, _, err := f.begin()
			require.NoError(t, err)
			defer call.finish()
			host.mu.Lock()
			host.wire = wire
			host.mu.Unlock()
			_, err = f.b.coldQuotaWorkerGrantRPC(call)
			require.ErrorIs(t, err, ErrOutcomeUnknown)
			require.Equal(t, 1, host.count("/etcdserverpb.Lease/LeaseGrant"))
		})
	}
}

func TestColdQuotaWorkerRPCCodecRejectsGraphBeforeGeneratedUnmarshal(t *testing.T) {
	for name, wire := range map[string][]byte{
		"unknown":           {0x20, 0},
		"duplicate-success": {0x10, 1, 0x10, 1},
		"recursive-txn":     {0x1a, 4, 0x22, 2, 0x1a, 0},
		"two-oneofs":        {0x1a, 4, 0x0a, 0, 0x12, 0},
		"two-point-kvs":     {0x1a, 6, 0x0a, 4, 0x12, 0, 0x12, 0},
		"too-many-ops":      []byte(strings.Repeat("\x1a\x02\x12\x00", 65)),
	} {
		t.Run(name, func(t *testing.T) {
			codec := &coldQuotaWorkerRPCCodec{phase: coldQuotaRPCJournal}
			response := &pb.TxnResponse{Succeeded: true}
			require.Error(t, codec.Unmarshal(wire, response))
			require.True(t, response.Succeeded, "rejection must precede generated Unmarshal and destination reset")
			require.Empty(t, response.Responses)
		})
	}
}

func TestColdQuotaWorkerRPCCodecSerializesOnlyOnce(t *testing.T) {
	codec := &coldQuotaWorkerRPCCodec{phase: coldQuotaRPCGrant}
	wire, err := codec.Marshal(&pb.LeaseGrantRequest{TTL: 10})
	require.NoError(t, err)
	require.Equal(t, []byte{0x08, 10}, wire)
	_, err = codec.Marshal(&pb.LeaseGrantRequest{TTL: 10})
	require.Error(t, err)
}
