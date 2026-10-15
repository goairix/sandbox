//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// This test-only TLS endpoint forwards actual protobuf requests to the native
// etcd server. It can discard one actual committed response, never synthesize
// a success or modify the request. Its upstream uses the same namespace role.
type localReleaseNativeProxy struct {
	pb.UnimplementedKVServer
	pb.UnimplementedLeaseServer
	pb.UnimplementedMaintenanceServer
	kv            pb.KVClient
	lease         pb.LeaseClient
	maintenance   pb.MaintenanceClient
	mu            sync.Mutex
	fault         string
	dropped       int
	transactions  []localReleaseNativeTransaction
	providerKey   string
	lastRequest   *pb.TxnRequest
	lastSucceeded bool
}

type localReleaseNativeTransaction struct {
	Label                                                             string
	Nodes, Comparisons, Success, Failure, RequestBytes, ResponseBytes int
	Succeeded, Dropped                                                bool
}

func newLocalReleaseNativeProxy(t *testing.T, config *tls.Config, fault string) (string, *localReleaseNativeProxy) {
	t.Helper()
	upstream, err := clientv3.New(clientv3.Config{Endpoints: []string{"https://127.0.0.1:2379"}, TLS: config.Clone(), DialTimeout: 2 * time.Second})
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serverTLS := config.Clone()
	serverTLS.ClientCAs, serverTLS.ClientAuth = config.RootCAs, tls.RequireAndVerifyClientCert
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)), grpc.WaitForHandlers(true))
	p := &localReleaseNativeProxy{kv: pb.NewKVClient(upstream.ActiveConnection()), lease: pb.NewLeaseClient(upstream.ActiveConnection()), maintenance: pb.NewMaintenanceClient(upstream.ActiveConnection()), fault: fault}
	pb.RegisterKVServer(server, p)
	pb.RegisterLeaseServer(server, p)
	pb.RegisterMaintenanceServer(server, p)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		require.NoError(t, <-done)
		require.NoError(t, upstream.Close())
		t.Log("original TLS proxy handlers and Serve joined; upstream client closed")
	})
	return "https://" + listener.Addr().String(), p
}

func (p *localReleaseNativeProxy) Range(ctx context.Context, r *pb.RangeRequest) (*pb.RangeResponse, error) {
	return p.kv.Range(ctx, r)
}
func (p *localReleaseNativeProxy) Put(ctx context.Context, r *pb.PutRequest) (*pb.PutResponse, error) {
	return p.kv.Put(ctx, r)
}
func (p *localReleaseNativeProxy) DeleteRange(ctx context.Context, r *pb.DeleteRangeRequest) (*pb.DeleteRangeResponse, error) {
	return p.kv.DeleteRange(ctx, r)
}
func (p *localReleaseNativeProxy) LeaseGrant(ctx context.Context, r *pb.LeaseGrantRequest) (*pb.LeaseGrantResponse, error) {
	return p.lease.LeaseGrant(ctx, r)
}
func (p *localReleaseNativeProxy) LeaseRevoke(ctx context.Context, r *pb.LeaseRevokeRequest) (*pb.LeaseRevokeResponse, error) {
	return p.lease.LeaseRevoke(ctx, r)
}
func (p *localReleaseNativeProxy) Status(ctx context.Context, r *pb.StatusRequest) (*pb.StatusResponse, error) {
	return p.maintenance.Status(ctx, r)
}

func (p *localReleaseNativeProxy) Txn(ctx context.Context, r *pb.TxnRequest) (*pb.TxnResponse, error) {
	label := "other"
	for _, op := range r.Success {
		if put := op.GetRequestPut(); put != nil {
			switch {
			case strings.HasSuffix(string(put.Key), "/runtime-termination"):
				var v struct {
					Phase string `json:"phase"`
				}
				if json.Unmarshal(put.Value, &v) == nil {
					label = v.Phase
				}
			case strings.HasSuffix(string(put.Key), "/owner-release"):
				label = "release"
			case strings.HasSuffix(string(put.Key), "/runtime-provider-cleanup"):
				label = "provider"
			}
		}
	}
	response, err := p.kv.Txn(ctx, r)
	x := localReleaseNativeTransaction{Label: label, Comparisons: len(r.Compare), Success: len(r.Success), Failure: len(r.Failure), RequestBytes: r.Size()}
	x.Nodes = x.Comparisons + x.Success + x.Failure
	if response != nil {
		x.ResponseBytes, x.Succeeded = response.Size(), response.Succeeded
	}
	p.mu.Lock()
	for _, op := range r.Success {
		if put := op.GetRequestPut(); put != nil && strings.HasSuffix(string(put.Key), "/runtime-provider-cleanup") {
			p.providerKey = string(put.Key)
		}
	}
	p.lastRequest, p.lastSucceeded = r, err == nil && x.Succeeded
	if err == nil && x.Succeeded && label == strings.TrimPrefix(p.fault, "ack-") && p.dropped == 0 {
		p.dropped++
		x.Dropped = true
	}
	p.transactions = append(p.transactions, x)
	p.mu.Unlock()
	if x.Dropped {
		return nil, status.Error(codes.Unavailable, "fixture discarded an actual upstream committed response")
	}
	return response, err
}

func (p *localReleaseNativeProxy) verify(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if strings.HasPrefix(p.fault, "ack-") {
		require.Equal(t, 1, p.dropped)
	} else {
		require.Zero(t, p.dropped)
	}
	counts := map[string]int{}
	for _, x := range p.transactions {
		if x.Label == "other" {
			continue
		}
		counts[x.Label]++
		require.LessOrEqual(t, x.Nodes, 64)
		require.LessOrEqual(t, x.RequestBytes, 262144)
		require.LessOrEqual(t, x.ResponseBytes, 262144)
		switch x.Label {
		case "reserved":
			require.Equal(t, 60, x.Nodes)
		case "terminated", "cleanup-complete":
			require.Equal(t, 61, x.Nodes)
		case "release":
			require.Equal(t, 64, x.Nodes)
			require.Equal(t, 60, x.Comparisons)
			require.Equal(t, 4, x.Success)
		}
		t.Logf("actual native received+forwarded transaction label=%s nodes=%d comparisons=%d success=%d failure=%d request=%d response=%d succeeded=%t drop=%t", x.Label, x.Nodes, x.Comparisons, x.Success, x.Failure, x.RequestBytes, x.ResponseBytes, x.Succeeded, x.Dropped)
	}
	if strings.HasPrefix(p.fault, "ack-") {
		require.Equal(t, 1, counts[strings.TrimPrefix(p.fault, "ack-")], "actual native mutable request must never replay after ACK loss")
	}
}

// Inspect the independently stored native intent at the HTTP boundary. The
// last forwarded RPC must have been the fresh compare-only authority guard.
func (p *localReleaseNativeProxy) beforeEffect(ctx context.Context, raw *clientv3.Client, slot int, uid, rv string) error {
	p.mu.Lock()
	key, request, succeeded := p.providerKey, p.lastRequest, p.lastSucceeded
	p.mu.Unlock()
	if key == "" || request == nil || !succeeded || len(request.Compare) < 59 {
		return fmt.Errorf("missing fresh native effect guard")
	}
	for _, op := range request.Success {
		if op.GetRequestPut() != nil || op.GetRequestDeleteRange() != nil {
			return fmt.Errorf("last native RPC was a mutation, not fresh guard")
		}
	}
	got, err := raw.Get(ctx, key)
	if err != nil || len(got.Kvs) != 1 {
		return fmt.Errorf("actual effect intent read: %w", err)
	}
	var record struct {
		Slots []struct {
			State, UID      string
			ResourceVersion string `json:"resource_version"`
			EffectID        string `json:"effect_id"`
			Claim           json.RawMessage
		}
	}
	if json.Unmarshal(got.Kvs[0].Value, &record) != nil || len(record.Slots) != 7 {
		return fmt.Errorf("invalid actual intent slots")
	}
	s := record.Slots[slot]
	if s.State != "intent" || s.UID != uid || s.ResourceVersion != rv || s.EffectID == "" || len(s.Claim) < 3 {
		return fmt.Errorf("missing exact durable slot intent")
	}
	for _, cmp := range request.Compare {
		if string(cmp.Key) == key && cmp.Target == pb.Compare_MOD && cmp.Result == pb.Compare_EQUAL && cmp.GetModRevision() == got.Kvs[0].ModRevision {
			return nil
		}
	}
	return fmt.Errorf("fresh guard did not pin actual durable intent MOD")
}
