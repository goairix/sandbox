package etcd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
)

// This bounded host protocol model exercises the actual client RPC boundary.
// It is not evidence of native etcd behavior; the sole native test owns that.
type workerTestServer struct {
	pb.UnimplementedKVServer
	pb.UnimplementedLeaseServer
	pb.UnimplementedMaintenanceServer
	mu                     sync.Mutex
	values                 map[string]*mvccpb.KeyValue
	leases                 map[int64]int64
	revision, nextLease    int64
	grants, keeps, revokes int
	requests               []*pb.TxnRequest
	grantReply             func(*pb.LeaseGrantResponse)
	keepReply              func(*pb.LeaseKeepAliveResponse)
	txnReply               func(*pb.TxnRequest, *pb.TxnResponse)
}

func (s *workerTestServer) header() *pb.ResponseHeader {
	return &pb.ResponseHeader{ClusterId: 1, MemberId: 2, Revision: s.revision, RaftTerm: 1}
}

func workerTestKV(kv *mvccpb.KeyValue) *mvccpb.KeyValue {
	if kv == nil {
		return nil
	}
	owned := *kv
	owned.Key, owned.Value = bytes.Clone(kv.Key), bytes.Clone(kv.Value)
	return &owned
}

func (s *workerTestServer) put(key, value string, lease int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revision++
	version, create := int64(1), s.revision
	if old := s.values[key]; old != nil {
		version, create = old.Version+1, old.CreateRevision
	}
	s.values[key] = &mvccpb.KeyValue{Key: []byte(key), Value: []byte(value), Lease: lease, Version: version, CreateRevision: create, ModRevision: s.revision}
}

func (s *workerTestServer) point(key string) *mvccpb.KeyValue {
	s.mu.Lock()
	defer s.mu.Unlock()
	return workerTestKV(s.values[key])
}

func (s *workerTestServer) counts() (int, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.grants, s.keeps, s.revokes
}

func (s *workerTestServer) Status(context.Context, *pb.StatusRequest) (*pb.StatusResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &pb.StatusResponse{Header: s.header(), Version: "host-protocol-model"}, nil
}

func (s *workerTestServer) Range(_ context.Context, r *pb.RangeRequest) (*pb.RangeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rangeLocked(r), nil
}

func (s *workerTestServer) rangeLocked(r *pb.RangeRequest) *pb.RangeResponse {
	result := &pb.RangeResponse{Header: s.header()}
	if kv := s.values[string(r.Key)]; kv != nil {
		result.Kvs, result.Count = []*mvccpb.KeyValue{workerTestKV(kv)}, 1
	}
	return result
}

func workerTestCompare(c *pb.Compare, kv *mvccpb.KeyValue) bool {
	if kv == nil {
		kv = &mvccpb.KeyValue{}
	}
	order := 0
	if c.Target == pb.Compare_VALUE {
		order = bytes.Compare(kv.Value, c.GetValue())
	} else {
		var actual, expected int64
		switch c.Target {
		case pb.Compare_VERSION:
			actual, expected = kv.Version, c.GetVersion()
		case pb.Compare_CREATE:
			actual, expected = kv.CreateRevision, c.GetCreateRevision()
		case pb.Compare_MOD:
			actual, expected = kv.ModRevision, c.GetModRevision()
		case pb.Compare_LEASE:
			actual, expected = kv.Lease, c.GetLease()
		default:
			return false
		}
		if actual < expected {
			order = -1
		} else if actual > expected {
			order = 1
		}
	}
	switch c.Result {
	case pb.Compare_EQUAL:
		return order == 0
	case pb.Compare_NOT_EQUAL:
		return order != 0
	case pb.Compare_LESS:
		return order < 0
	case pb.Compare_GREATER:
		return order > 0
	}
	return false
}

func (s *workerTestServer) Txn(_ context.Context, r *pb.TxnRequest) (*pb.TxnResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r)
	if len(r.Compare)+len(r.Success)+len(r.Failure) > 64 || r.Size() > 256*1024 {
		return nil, fmt.Errorf("worker Txn exceeded the frozen wire budget")
	}
	passed := true
	for _, c := range r.Compare {
		if len(c.RangeEnd) != 0 || !workerTestCompare(c, s.values[string(c.Key)]) {
			passed = false
		}
	}
	ops := r.Failure
	if passed {
		ops = r.Success
	}
	for _, op := range ops {
		if op.GetRequestPut() != nil || op.GetRequestDeleteRange() != nil {
			s.revision++
			break
		}
	}
	out := &pb.TxnResponse{Header: s.header(), Succeeded: passed}
	for _, op := range ops {
		var response *pb.ResponseOp
		switch {
		case op.GetRequestRange() != nil:
			response = &pb.ResponseOp{Response: &pb.ResponseOp_ResponseRange{ResponseRange: s.rangeLocked(op.GetRequestRange())}}
		case op.GetRequestPut() != nil:
			p := op.GetRequestPut()
			if p.Lease != 0 && s.leases[p.Lease] == 0 {
				return nil, fmt.Errorf("put uses an unknown original Lease")
			}
			key := string(p.Key)
			version, create := int64(1), s.revision
			if old := s.values[key]; old != nil {
				version, create = old.Version+1, old.CreateRevision
			}
			s.values[key] = &mvccpb.KeyValue{Key: bytes.Clone(p.Key), Value: bytes.Clone(p.Value), Lease: p.Lease, Version: version, CreateRevision: create, ModRevision: s.revision}
			response = &pb.ResponseOp{Response: &pb.ResponseOp_ResponsePut{ResponsePut: &pb.PutResponse{Header: s.header()}}}
		case op.GetRequestDeleteRange() != nil:
			d := op.GetRequestDeleteRange()
			if len(d.RangeEnd) != 0 {
				return nil, fmt.Errorf("unexpected worker range delete")
			}
			deleted := int64(0)
			if s.values[string(d.Key)] != nil {
				delete(s.values, string(d.Key))
				deleted = 1
			}
			response = &pb.ResponseOp{Response: &pb.ResponseOp_ResponseDeleteRange{ResponseDeleteRange: &pb.DeleteRangeResponse{Header: s.header(), Deleted: deleted}}}
		default:
			return nil, fmt.Errorf("unsupported worker model request")
		}
		out.Responses = append(out.Responses, response)
	}
	if s.txnReply != nil {
		s.txnReply(r, out)
	}
	return out, nil
}

func (s *workerTestServer) LeaseGrant(_ context.Context, r *pb.LeaseGrantRequest) (*pb.LeaseGrantResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants++
	s.nextLease++
	s.leases[s.nextLease] = r.TTL
	out := &pb.LeaseGrantResponse{Header: s.header(), ID: s.nextLease, TTL: r.TTL}
	if s.grantReply != nil {
		s.grantReply(out)
	}
	return out, nil
}

func (s *workerTestServer) LeaseKeepAlive(stream pb.Lease_LeaseKeepAliveServer) error {
	r, err := stream.Recv()
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.keeps++
	out := &pb.LeaseKeepAliveResponse{Header: s.header(), ID: r.ID, TTL: s.leases[r.ID]}
	if s.keepReply != nil {
		s.keepReply(out)
	}
	s.mu.Unlock()
	return stream.Send(out)
}

func (s *workerTestServer) LeaseRevoke(_ context.Context, r *pb.LeaseRevokeRequest) (*pb.LeaseRevokeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revokes++
	delete(s.leases, r.ID)
	s.revision++
	for key, kv := range s.values {
		if kv.Lease == r.ID {
			delete(s.values, key)
		}
	}
	return &pb.LeaseRevokeResponse{Header: s.header()}, nil
}

func workerTestProfile(o Options) string {
	identity, _ := json.Marshal(o.Identity)
	digest := sha256.Sum256(identity)
	return fmt.Sprintf(`{"version":1,"profile_id":"partition-worker-v1","namespace":%q,"restore_epoch":%q,"identity_digest":"%s","partition_count":256,"relist_slots":4,"range_slots":20,"range_bytes_per_second":1048576,"range_burst_bytes":262144,"claim_slots":100,"claim_per_second":1,"claim_burst":1}`, o.Namespace.Root(), o.Identity.RestoreEpoch, hex.EncodeToString(digest[:]))
}

func workerTestEndpoint(t *testing.T) (*workerTestServer, Options) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := &workerTestServer{values: make(map[string]*mvccpb.KeyValue), leases: make(map[int64]int64), revision: 100, nextLease: 500}
	server := grpc.NewServer()
	pb.RegisterKVServer(server, s)
	pb.RegisterLeaseServer(server, s)
	pb.RegisterMaintenanceServer(server, s)
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close(); <-done })
	o := validOptions(t)
	o.Endpoints = []string{"http://" + listener.Addr().String()}
	o.RequestTimeout = time.Second
	o.WorkerControl = &WorkerControlOptions{}
	identity, err := json.Marshal(o.Identity)
	require.NoError(t, err)
	key, err := o.Namespace.Key("meta", "identity")
	require.NoError(t, err)
	s.put(key, string(identity), 0)
	key, err = o.Namespace.Key("meta", "restore_epoch")
	require.NoError(t, err)
	s.put(key, o.Identity.RestoreEpoch, 0)
	key, err = o.Namespace.Key("meta", "worker_control_budget")
	require.NoError(t, err)
	s.put(key, workerTestProfile(o), 0)
	return s, o
}

func workerTestBackend(t *testing.T) (*Backend, *workerTestServer) {
	t.Helper()
	s, o := workerTestEndpoint(t)
	b, err := New(context.Background(), o)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	return b, s
}

type workerTestLease struct {
	clientv3.Lease
	grant  func(context.Context, int64) (*clientv3.LeaseGrantResponse, error)
	keep   func(context.Context, clientv3.LeaseID) (*clientv3.LeaseKeepAliveResponse, error)
	revoke func(context.Context, clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error)
}

func (l *workerTestLease) Grant(ctx context.Context, ttl int64) (*clientv3.LeaseGrantResponse, error) {
	if l.grant != nil {
		return l.grant(ctx, ttl)
	}
	return l.Lease.Grant(ctx, ttl)
}

func (l *workerTestLease) KeepAliveOnce(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseKeepAliveResponse, error) {
	if l.keep != nil {
		return l.keep(ctx, id)
	}
	return l.Lease.KeepAliveOnce(ctx, id)
}

func (l *workerTestLease) Revoke(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
	if l.revoke != nil {
		return l.revoke(ctx, id)
	}
	return l.Lease.Revoke(ctx, id)
}
