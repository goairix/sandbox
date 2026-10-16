//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// This proxy has no storage or effect implementation. Every request reaches
// the original TLS etcd server. Its only fault withholds one successful native
// Txn response until the downstream RPC deadline actually expires.
type cleanupExecutorEtcdProxy struct {
	pb.UnimplementedMaintenanceServer
	connection *grpc.ClientConn
	server     *grpc.Server
	listener   net.Listener
	served     chan error
	closeOnce  sync.Once
	pumps      sync.WaitGroup

	mu            sync.Mutex
	task          state.TaskReference
	prefix, fault string
	stats         cleanupExecutorProxyStats
}

type cleanupExecutorProxyStats struct {
	GrantIDs                                  []int64
	ClaimPuts, ArchivePuts                    int
	WatchStreams, KeepAliveStreams            int
	FaultMatched, FaultCommitted, FaultJoined int
	FaultRevision                             int64
}

func newCleanupExecutorEtcdProxy(t *testing.T, tlsConfig *tls.Config, task state.TaskReference, fault string) *cleanupExecutorEtcdProxy {
	t.Helper()
	require.Contains(t, []string{"", "bound-recovery", "release-settlement-recovery"}, fault)
	connection, err := grpc.NewClient("127.0.0.1:2379", grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig.Clone())))
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serverTLS := tlsConfig.Clone()
	serverTLS.ClientCAs, serverTLS.ClientAuth = tlsConfig.RootCAs, tls.RequireAndVerifyClientCert
	p := &cleanupExecutorEtcdProxy{connection: connection, listener: listener, served: make(chan error, 1), task: task, prefix: fmt.Sprintf("%sp/%02x/tasks/%s/", task.Namespace, task.Partition, task.TaskID), fault: fault}
	p.server = grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)), grpc.WaitForHandlers(true), grpc.MaxRecvMsgSize(1<<20), grpc.MaxSendMsgSize(1<<20))
	pb.RegisterKVServer(p.server, p)
	pb.RegisterLeaseServer(p.server, p)
	pb.RegisterWatchServer(p.server, p)
	pb.RegisterMaintenanceServer(p.server, p)
	go func() { p.served <- p.server.Serve(listener) }()
	t.Cleanup(func() { p.close(t) })
	return p
}

func (p *cleanupExecutorEtcdProxy) endpoint() string { return "https://" + p.listener.Addr().String() }

func (p *cleanupExecutorEtcdProxy) snapshot() cleanupExecutorProxyStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.stats
	s.GrantIDs = append([]int64(nil), s.GrantIDs...)
	return s
}

func (p *cleanupExecutorEtcdProxy) close(t *testing.T) {
	t.Helper()
	p.closeOnce.Do(func() {
		// Stop cancels downstream streams and joins native handlers. A handler
		// may return before its blocked downstream Recv wakes; pumps stay owned
		// here until that real Recv returns. Never wait for Recv before returning
		// its handler, which would prevent gRPC from canceling that stream.
		p.server.Stop()
		err := <-p.served
		require.True(t, err == nil || errors.Is(err, grpc.ErrServerStopped), "%v", err)
		require.NoError(t, p.connection.Close())
		p.pumps.Wait()
		t.Logf("cleanup TLS proxy actual Close joined: fault=%q finalstats=%+v", p.fault, p.snapshot())
	})
}

func (p *cleanupExecutorEtcdProxy) Range(ctx context.Context, r *pb.RangeRequest) (*pb.RangeResponse, error) {
	return pb.NewKVClient(p.connection).Range(ctx, r)
}

func (p *cleanupExecutorEtcdProxy) Put(ctx context.Context, r *pb.PutRequest) (*pb.PutResponse, error) {
	return pb.NewKVClient(p.connection).Put(ctx, r)
}

func (p *cleanupExecutorEtcdProxy) DeleteRange(ctx context.Context, r *pb.DeleteRangeRequest) (*pb.DeleteRangeResponse, error) {
	return pb.NewKVClient(p.connection).DeleteRange(ctx, r)
}

func (p *cleanupExecutorEtcdProxy) Compact(ctx context.Context, r *pb.CompactionRequest) (*pb.CompactionResponse, error) {
	return pb.NewKVClient(p.connection).Compact(ctx, r)
}

func (p *cleanupExecutorEtcdProxy) Txn(ctx context.Context, request *pb.TxnRequest) (*pb.TxnResponse, error) {
	response, err := pb.NewKVClient(p.connection).Txn(ctx, request)
	if err != nil || response == nil {
		return response, err
	}
	p.mu.Lock()
	p.observeWrites(request, response)
	drop := response.Succeeded && p.stats.FaultMatched == 0 && p.matchesFault(request)
	if drop {
		p.stats.FaultMatched++
		p.stats.FaultCommitted++
		p.stats.FaultRevision = response.Header.Revision
	}
	p.mu.Unlock()
	if !drop {
		return response, nil
	}
	// The actual upstream commit has returned. Do not cancel the caller or
	// manufacture an ACK: let its original finite RPC deadline lose this ACK.
	<-ctx.Done()
	p.mu.Lock()
	p.stats.FaultJoined++
	p.mu.Unlock()
	return nil, status.FromContextError(ctx.Err()).Err()
}

// Count only writes in branches selected by the actual native response,
// including nested Stage Txns. An unchosen request branch is not an effect.
func (p *cleanupExecutorEtcdProxy) observeWrites(request *pb.TxnRequest, response *pb.TxnResponse) {
	ops := request.Failure
	if response.Succeeded {
		ops = request.Success
	}
	for i, op := range ops {
		if put := op.GetRequestPut(); put != nil {
			switch string(put.Key) {
			case p.prefix + "claim":
				p.stats.ClaimPuts++
			case p.prefix + "workspace-archive-command":
				p.stats.ArchivePuts++
			}
		}
		if nested := op.GetRequestTxn(); nested != nil && i < len(response.Responses) && response.Responses[i].GetResponseTxn() != nil {
			p.observeWrites(nested, response.Responses[i].GetResponseTxn())
		}
	}
}

func (p *cleanupExecutorEtcdProxy) matchesFault(request *pb.TxnRequest) bool {
	for _, operation := range request.Success {
		put := operation.GetRequestPut()
		if put == nil {
			continue
		}
		if p.fault == "bound-recovery" && string(put.Key) == p.prefix+"execution" {
			var row cleanupExecutorRecoveryRow
			if len(put.Value) <= 4096 && json.Unmarshal(put.Value, &row) == nil && row.Task == p.task && row.Phase == "occupied" && row.Confirmed == "archive-bound" && row.Run != nil && !row.Run.Joined && row.Archive != nil {
				return true
			}
		}
		if p.fault == "release-settlement-recovery" && string(put.Key) == p.prefix+"owner-release" && len(request.Success) == 4 {
			for _, deletion := range request.Success[:3] {
				if deletion.GetRequestDeleteRange() == nil {
					return false
				}
			}
			return true
		}
	}
	return false
}

func (p *cleanupExecutorEtcdProxy) LeaseGrant(ctx context.Context, r *pb.LeaseGrantRequest) (*pb.LeaseGrantResponse, error) {
	response, err := pb.NewLeaseClient(p.connection).LeaseGrant(ctx, r)
	if err == nil && response != nil && response.Error == "" && response.ID != 0 {
		p.mu.Lock()
		p.stats.GrantIDs = append(p.stats.GrantIDs, response.ID)
		p.mu.Unlock()
	}
	return response, err
}

func (p *cleanupExecutorEtcdProxy) LeaseRevoke(ctx context.Context, r *pb.LeaseRevokeRequest) (*pb.LeaseRevokeResponse, error) {
	return pb.NewLeaseClient(p.connection).LeaseRevoke(ctx, r)
}

func (p *cleanupExecutorEtcdProxy) LeaseTimeToLive(ctx context.Context, r *pb.LeaseTimeToLiveRequest) (*pb.LeaseTimeToLiveResponse, error) {
	return pb.NewLeaseClient(p.connection).LeaseTimeToLive(ctx, r)
}

func (p *cleanupExecutorEtcdProxy) LeaseLeases(ctx context.Context, r *pb.LeaseLeasesRequest) (*pb.LeaseLeasesResponse, error) {
	return pb.NewLeaseClient(p.connection).LeaseLeases(ctx, r)
}

func (p *cleanupExecutorEtcdProxy) Status(ctx context.Context, r *pb.StatusRequest) (*pb.StatusResponse, error) {
	return pb.NewMaintenanceClient(p.connection).Status(ctx, r)
}

func (p *cleanupExecutorEtcdProxy) Watch(downstream pb.Watch_WatchServer) error {
	ctx, cancel := context.WithTimeout(downstream.Context(), 100*time.Second)
	defer cancel()
	upstream, err := pb.NewWatchClient(p.connection).Watch(ctx)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.stats.WatchStreams++
	p.mu.Unlock()
	p.pumps.Add(1)
	go func() {
		defer p.pumps.Done()
		defer upstream.CloseSend()
		for {
			request, err := downstream.Recv()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					cancel()
				}
				return
			}
			if err = upstream.Send(request); err != nil {
				cancel()
				return
			}
		}
	}()
	for {
		response, err := upstream.Recv()
		if err != nil {
			return cleanupExecutorStreamError(err)
		}
		if err = downstream.Send(response); err != nil {
			return err
		}
	}
}

func (p *cleanupExecutorEtcdProxy) LeaseKeepAlive(downstream pb.Lease_LeaseKeepAliveServer) error {
	ctx, cancel := context.WithTimeout(downstream.Context(), 100*time.Second)
	defer cancel()
	upstream, err := pb.NewLeaseClient(p.connection).LeaseKeepAlive(ctx)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.stats.KeepAliveStreams++
	p.mu.Unlock()
	p.pumps.Add(1)
	go func() {
		defer p.pumps.Done()
		defer upstream.CloseSend()
		for {
			request, err := downstream.Recv()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					cancel()
				}
				return
			}
			if err = upstream.Send(request); err != nil {
				cancel()
				return
			}
		}
	}()
	for {
		response, err := upstream.Recv()
		if err != nil {
			return cleanupExecutorStreamError(err)
		}
		if err = downstream.Send(response); err != nil {
			return err
		}
	}
}

func cleanupExecutorStreamError(err error) error {
	if errors.Is(err, io.EOF) {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, err.Error())
	}
	return err
}
