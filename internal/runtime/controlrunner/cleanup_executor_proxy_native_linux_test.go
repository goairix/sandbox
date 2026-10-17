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
	"strings"
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
// the original TLS etcd server. Recovery faults withhold one committed native
// Txn response until its deadline; close coverage holds one actual read until
// cancellation, explicit fixture release, or its fixed three-second ceiling.
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

	// The close scenario holds one real read response, never a fabricated
	// response. This remote handler belongs to the fixture, not Backend.Close.
	closeEntered  chan struct{}
	closeRelease  chan struct{}
	closeReturned chan struct{}
	closeUnblock  sync.Once
}

type cleanupExecutorProxyStats struct {
	GrantIDs                                  []int64
	ClaimLeases                               []cleanupExecutorClaimLease
	QuiescenceGuards                          []cleanupExecutorStageGuard
	ObservationOverflow                       bool
	ClaimPuts, ArchivePuts                    int
	ClosePuts, QuiescencePuts                 int
	WatchStreams, KeepAliveStreams            int
	FaultMatched, FaultCommitted, FaultJoined int
	FaultRevision                             int64
	AdmissionReads, CloseReadJoined           int
}

// These copied observations are never input to a consumer or effect API.
// Four entries per kind bound even an unexpected repeated-write test failure.
type cleanupExecutorClaimLease struct {
	ClaimID string
	LeaseID int64
}

type cleanupExecutorStageGuard struct {
	Key     string
	LeaseID int64
	Stage   state.StageReference
}

func newCleanupExecutorEtcdProxy(t *testing.T, tlsConfig *tls.Config, task state.TaskReference, fault string) *cleanupExecutorEtcdProxy {
	t.Helper()
	require.Contains(t, []string{"", "bound-recovery", "release-settlement-recovery", "close-join", "closed-progress-recovery", "quiescent-progress-recovery"}, fault)
	connection, err := grpc.NewClient("127.0.0.1:2379", grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig.Clone())))
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serverTLS := tlsConfig.Clone()
	serverTLS.ClientCAs, serverTLS.ClientAuth = tlsConfig.RootCAs, tls.RequireAndVerifyClientCert
	p := &cleanupExecutorEtcdProxy{connection: connection, listener: listener, served: make(chan error, 1), task: task, prefix: fmt.Sprintf("%sp/%02x/tasks/%s/", task.Namespace, task.Partition, task.TaskID), fault: fault}
	if fault == "close-join" {
		p.closeEntered, p.closeRelease, p.closeReturned = make(chan struct{}), make(chan struct{}), make(chan struct{})
	}
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
	s.ClaimLeases = append([]cleanupExecutorClaimLease(nil), s.ClaimLeases...)
	s.QuiescenceGuards = append([]cleanupExecutorStageGuard(nil), s.QuiescenceGuards...)
	return s
}

func (p *cleanupExecutorEtcdProxy) close(t *testing.T) {
	t.Helper()
	p.closeOnce.Do(func() {
		p.unblockCloseRead()
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
	closeRead := false
	for _, operation := range request.Success {
		point := operation.GetRequestRange()
		if point == nil {
			continue
		}
		key := string(point.Key)
		if strings.Contains(key, "/tasks/") && strings.HasSuffix(key, "/execution") {
			p.stats.AdmissionReads++
			if p.fault == "close-join" && key == p.prefix+"execution" && p.stats.AdmissionReads == 1 && response.Succeeded {
				closeRead = true
			}
		}
	}
	drop := response.Succeeded && p.stats.FaultMatched == 0 && p.matchesFault(request)
	if drop {
		p.stats.FaultMatched++
		p.stats.FaultCommitted++
		p.stats.FaultRevision = response.Header.Revision
	}
	p.mu.Unlock()
	if closeRead {
		close(p.closeEntered)
		// The original client q is 3s. A separate ceiling also bounds the
		// fixture if its peer disappears without cancellation propagation.
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		select {
		case <-p.closeRelease:
		case <-ctx.Done():
		case <-timer.C:
		}
		p.mu.Lock()
		p.stats.CloseReadJoined++
		p.mu.Unlock()
		close(p.closeReturned)
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return response, nil
	}
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

func (p *cleanupExecutorEtcdProxy) unblockCloseRead() {
	if p.closeRelease != nil {
		p.closeUnblock.Do(func() { close(p.closeRelease) })
	}
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
			case p.prefix + "close-data":
				p.stats.ClosePuts++
			case p.prefix + "quiesce-users":
				p.stats.QuiescencePuts++
			}
			if response.Succeeded && i < len(response.Responses) && response.Responses[i].GetResponsePut() != nil {
				p.observeQuiescenceStageLease(put)
			}
		}
		if nested := op.GetRequestTxn(); nested != nil && i < len(response.Responses) && response.Responses[i].GetResponseTxn() != nil {
			p.observeWrites(nested, response.Responses[i].GetResponseTxn())
		}
	}
}

// Attribute only real successful PUTs to this exact Task's observed Claim.
// A same-partition Stage alone is insufficient: its original RequestID must
// match a Claim PUT seen by this proxy before the Stage guard was created.
func (p *cleanupExecutorEtcdProxy) observeQuiescenceStageLease(put *pb.PutRequest) {
	if len(put.Key) > 4096 || len(put.Value) > 4096 || put.Lease <= 0 {
		return
	}
	if string(put.Key) == p.prefix+"claim" {
		var claim struct {
			Task    state.TaskReference `json:"task"`
			ClaimID string              `json:"claim_id"`
			LeaseID int64               `json:"lease_id"`
		}
		if json.Unmarshal(put.Value, &claim) != nil || claim.Task != p.task || claim.ClaimID == "" || claim.LeaseID != put.Lease {
			return
		}
		if len(p.stats.ClaimLeases) == 4 {
			p.stats.ObservationOverflow = true
			return
		}
		p.stats.ClaimLeases = append(p.stats.ClaimLeases, cleanupExecutorClaimLease{claim.ClaimID, put.Lease})
		return
	}

	var guard struct {
		state.StageReference
		LeaseID int64 `json:"lease_id"`
	}
	if json.Unmarshal(put.Value, &guard) != nil || guard.LeaseID != put.Lease || guard.Namespace != p.task.Namespace || guard.Partition != p.task.Partition || guard.RestoreEpoch != p.task.RestoreEpoch || guard.StageID != "task_quiesce_users" {
		return
	}
	key := fmt.Sprintf("%sp/%02x/attempts/%s/guard", p.task.Namespace, p.task.Partition, guard.AttemptID)
	if string(put.Key) != key {
		return
	}
	for _, claim := range p.stats.ClaimLeases {
		if claim.ClaimID != guard.RequestID {
			continue
		}
		if len(p.stats.QuiescenceGuards) == 4 {
			p.stats.ObservationOverflow = true
			return
		}
		p.stats.QuiescenceGuards = append(p.stats.QuiescenceGuards, cleanupExecutorStageGuard{Key: key, LeaseID: put.Lease, Stage: guard.StageReference})
		return
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
		if cleanupExecutorEarlyPhase(p.fault) != "" && string(put.Key) == p.prefix+"execution" {
			var row cleanupExecutorRecoveryRow
			if len(put.Value) <= 4096 && json.Unmarshal(put.Value, &row) == nil && row.Task == p.task && row.Phase == "occupied" && row.Confirmed == cleanupExecutorEarlyPhase(p.fault) && row.Run != nil && !row.Run.Joined && row.Archive == nil {
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
