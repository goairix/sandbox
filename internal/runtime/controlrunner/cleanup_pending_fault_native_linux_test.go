//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/provisioning/deployment"
	"github.com/goairix/sandbox/internal/runtime/launcher"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// Only pending retirement is specialized. All other KV methods, real Lease/
// Watch forwarding, stream pumps and actual server/connection join are inherited
// from the accepted native TLS proxy. This fixture implements no storage/proof.
type cleanupPendingFaultProxy struct {
	*cleanupExecutorEtcdProxy
	mode                  string
	original              *mvccpb.KeyValue
	firstDone, secondDone chan struct{}
	pendingStats          cleanupPendingFaultStats // Protected by the embedded mu.
}

type cleanupPendingFaultStats struct {
	CASCount                    int
	Overflow                    bool
	Transactions                [2]cleanupPendingFaultTxn
	Replacement                 *mvccpb.KeyValue
	Survived                    bool
	DeadlinePresent             bool
	Deadline, WithheldUntil     time.Time
	AckCommitted, HandlerJoined int
	FaultError                  string
}

type cleanupPendingFaultTxn struct {
	Compares, Nodes, RequestBytes int
	PendingCreate, PendingMod     int64
	PendingLease                  bool
	PendingValue                  bool
	Succeeded                     bool
	Deleted, Revision             int64
	Started, Completed            time.Time
}

func newCleanupPendingFaultProxy(t *testing.T, tlsConfig *tls.Config, task state.TaskReference, pending *mvccpb.KeyValue, mode string) *cleanupPendingFaultProxy {
	t.Helper()
	require.Contains(t, []string{"automatic-pending-aba", "automatic-pending-ack"}, mode)
	connection, err := grpc.NewClient("127.0.0.1:2379", grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig.Clone())))
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serverTLS := tlsConfig.Clone()
	serverTLS.ClientCAs, serverTLS.ClientAuth = tlsConfig.RootCAs, tls.RequireAndVerifyClientCert
	base := &cleanupExecutorEtcdProxy{connection: connection, listener: listener, served: make(chan error, 1), task: task, prefix: fmt.Sprintf("%sp/%02x/tasks/%s/", task.Namespace, task.Partition, task.TaskID)}
	p := &cleanupPendingFaultProxy{cleanupExecutorEtcdProxy: base, mode: mode, original: pending, firstDone: make(chan struct{}), secondDone: make(chan struct{})}
	base.server = grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)), grpc.WaitForHandlers(true), grpc.MaxRecvMsgSize(1<<20), grpc.MaxSendMsgSize(1<<20))
	pb.RegisterKVServer(base.server, p)
	pb.RegisterLeaseServer(base.server, base)
	pb.RegisterWatchServer(base.server, base)
	pb.RegisterMaintenanceServer(base.server, base)
	go func() { base.served <- base.server.Serve(listener) }()
	t.Cleanup(func() { base.close(t) })
	return p
}

func (p *cleanupPendingFaultProxy) pendingSnapshot() cleanupPendingFaultStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pendingStats
}

func (p *cleanupPendingFaultProxy) Txn(ctx context.Context, request *pb.TxnRequest) (*pb.TxnResponse, error) {
	if len(request.Success) != 1 {
		return p.cleanupExecutorEtcdProxy.Txn(ctx, request)
	}
	deletion := request.Success[0].GetRequestDeleteRange()
	if deletion == nil || string(deletion.Key) != string(p.original.Key) || len(deletion.RangeEnd) != 0 {
		return p.cleanupExecutorEtcdProxy.Txn(ctx, request)
	}
	p.mu.Lock()
	index := p.pendingStats.CASCount
	p.pendingStats.CASCount++
	if index >= len(p.pendingStats.Transactions) {
		p.pendingStats.Overflow = true
	}
	p.mu.Unlock()
	observed := p.observePendingRequest(request)
	defer func() {
		observed.Completed = time.Now()
		p.mu.Lock()
		if index < len(p.pendingStats.Transactions) {
			p.pendingStats.Transactions[index] = observed
		}
		p.mu.Unlock()
		if index == 0 {
			close(p.firstDone)
		} else if index == 1 {
			close(p.secondDone)
		}
	}()

	if index == 0 && p.mode == "automatic-pending-aba" {
		if err := p.replacePending(ctx); err != nil {
			return nil, err
		}
	}
	response, err := p.cleanupExecutorEtcdProxy.Txn(ctx, request)
	if err != nil || response == nil {
		return response, err
	}
	observed.Succeeded = response.Succeeded
	if response.Header != nil {
		observed.Revision = response.Header.Revision
	}
	if response.Succeeded && len(response.Responses) == 1 && response.Responses[0].GetResponseDeleteRange() != nil {
		observed.Deleted = response.Responses[0].GetResponseDeleteRange().Deleted
	}
	if index == 0 && p.mode == "automatic-pending-aba" {
		survived, readErr := pb.NewKVClient(p.connection).Range(ctx, &pb.RangeRequest{Key: p.original.Key})
		if readErr != nil {
			return nil, readErr
		}
		p.mu.Lock()
		p.pendingStats.Survived = len(survived.Kvs) == 1 && p.pendingStats.Replacement != nil && survived.Kvs[0].CreateRevision == p.pendingStats.Replacement.CreateRevision && string(survived.Kvs[0].Value) == string(p.original.Value)
		p.mu.Unlock()
	}
	if index != 0 || p.mode != "automatic-pending-ack" || !response.Succeeded || observed.Deleted != 1 {
		return response, nil
	}

	// This is the real committed reply. The original caller's deadline loses
	// its ACK; no response, success flag, proof or replacement session is made.
	deadline, present := ctx.Deadline()
	p.mu.Lock()
	p.pendingStats.AckCommitted++
	p.pendingStats.DeadlinePresent, p.pendingStats.Deadline = present, deadline
	p.mu.Unlock()
	<-ctx.Done()
	if present {
		// gRPC can deliver the client's deadline cancellation just before the
		// propagated server deadline because timeout headers are rounded. Keep
		// withholding until that same original deadline; introduce no new limit.
		timer := time.NewTimer(max(time.Until(deadline), 0))
		<-timer.C
	}
	p.mu.Lock()
	p.pendingStats.WithheldUntil = time.Now()
	p.pendingStats.HandlerJoined++
	p.mu.Unlock()
	return nil, status.FromContextError(ctx.Err()).Err()
}

func (p *cleanupPendingFaultProxy) observePendingRequest(request *pb.TxnRequest) cleanupPendingFaultTxn {
	observed := cleanupPendingFaultTxn{
		Compares: len(request.Compare), Nodes: len(request.Compare) + len(request.Success) + len(request.Failure),
		RequestBytes: request.Size(), Started: time.Now(),
	}
	for _, cmp := range request.Compare {
		if cmp == nil || string(cmp.Key) != string(p.original.Key) || len(cmp.RangeEnd) != 0 || cmp.Result != pb.Compare_EQUAL {
			continue
		}
		switch cmp.Target {
		case pb.Compare_CREATE:
			observed.PendingCreate = cmp.GetCreateRevision()
		case pb.Compare_MOD:
			observed.PendingMod = cmp.GetModRevision()
		case pb.Compare_LEASE:
			observed.PendingLease = cmp.GetLease() == 0
		case pb.Compare_VALUE:
			observed.PendingValue = string(cmp.GetValue()) == string(p.original.Value)
		}
	}
	return observed
}

func (p *cleanupPendingFaultProxy) replacePending(ctx context.Context) error {
	upstream := pb.NewKVClient(p.connection)
	deleted, err := upstream.DeleteRange(ctx, &pb.DeleteRangeRequest{Key: p.original.Key})
	if err != nil {
		return err
	}
	if deleted.Deleted != 1 {
		p.mu.Lock()
		p.pendingStats.FaultError = "ABA source P was not present exactly once"
		p.mu.Unlock()
	}
	if _, err := upstream.Put(ctx, &pb.PutRequest{Key: p.original.Key, Value: p.original.Value}); err != nil {
		return err
	}
	replacement, err := upstream.Range(ctx, &pb.RangeRequest{Key: p.original.Key})
	if err != nil {
		return err
	}
	p.mu.Lock()
	if len(replacement.Kvs) == 1 {
		p.pendingStats.Replacement = replacement.Kvs[0]
	} else {
		p.pendingStats.FaultError = "ABA replacement P was not readable exactly once"
	}
	p.mu.Unlock()
	return nil
}

func cleanupPendingFaultMounted(t *testing.T, ctx context.Context, s cleanupExecutorSetup, kernel *launcher.KernelBoundary, dir string, tlsConfig *tls.Config, ca, certificate, key []byte) {
	t.Helper()
	stopClock := cleanupExecutorLoaderClock(t, ctx, s)
	defer stopClock()
	manifest := cleanupExecutorLoaderFiles(t, s, dir, ca, certificate, key)
	raw, err := clientv3.New(clientv3.Config{Endpoints: []string{"https://127.0.0.1:2379"}, TLS: tlsConfig, DialTimeout: 2 * time.Second, Logger: zap.NewNop()})
	require.NoError(t, err)
	defer func() { require.NoError(t, raw.Close()) }()
	pending := cleanupPendingAutomaticBacklog(t, ctx, raw, s)
	proxy := newCleanupPendingFaultProxy(t, tlsConfig, s.Task, pending, s.AutomaticMode)
	defer proxy.close(t)
	wire, err := os.ReadFile(manifest)
	require.NoError(t, err)
	// Preserve uint64 identity bytes while editing the mode and proxy endpoint.
	var mounted map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wire, &mounted))
	mounted["version"] = json.RawMessage("2")
	var worker map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(mounted["worker"], &worker))
	require.NotEmpty(t, worker)
	worker["automatic_cleanup"] = json.RawMessage("true")
	mounted["worker"] = observerNativeJSON(t, worker)
	var etcd map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(mounted["etcd"], &etcd))
	require.NotEmpty(t, etcd)
	etcd["endpoints"] = observerNativeJSON(t, []string{proxy.endpoint()})
	mounted["etcd"] = observerNativeJSON(t, etcd)
	require.NoError(t, os.WriteFile(manifest, observerNativeJSON(t, mounted), 0600))
	d, err := deployment.OpenAutomaticLocalCleanup(ctx, manifest, kernel)
	require.NoError(t, err)
	defer d.Close()
	session := cleanupWorkerRoleSession(t, ctx, raw, s.Binding.Namespace, "cleanup-loader", d.Done())
	cleanupWorkerRoleSettled(t, ctx, raw, s, os.Getpid(), d.Done())
	cleanupPendingAutomaticArchive(t, dir)
	executionKey := fmt.Sprintf("%sp/%02x/tasks/%s/execution", s.Binding.Namespace, s.Task.Partition, s.Task.TaskID)
	execution := cleanupExecutorRecoveryPoint(t, ctx, raw, executionKey)
	release := cleanupExecutorRecoveryPoint(t, ctx, raw, proxy.prefix+"owner-release")
	physicalBefore := proxy.snapshot()
	select {
	case <-proxy.firstDone:
	case <-d.Done():
		if s.AutomaticMode != "automatic-pending-ack" {
			t.Fatalf("automatic ABA worker stopped before fault: %v", d.Wait(context.Background()))
		}
		select {
		case <-proxy.firstDone:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	first := proxy.pendingSnapshot()
	t.Logf("pending retirement native first mode=%s stats=%+v", s.AutomaticMode, first)
	require.Empty(t, first.FaultError)
	require.False(t, first.Overflow)
	cleanupPendingFaultRequest(t, first.Transactions[0], pending)
	if s.AutomaticMode == "automatic-pending-aba" {
		cleanupPendingFaultABA(t, ctx, raw, proxy, d, first)
	} else {
		cleanupPendingFaultACK(t, ctx, raw, proxy, d, first, session, manifest, kernel, s)
	}
	require.Equal(t, execution, cleanupExecutorRecoveryPoint(t, ctx, raw, executionKey), "retirement/restart must preserve the exact settled E")
	require.Equal(t, release, cleanupExecutorRecoveryPoint(t, ctx, raw, proxy.prefix+"owner-release"), "retirement/restart must preserve the original actual release")
	cleanupPendingFaultPhysical(t, physicalBefore, proxy.snapshot())
	cleanupPendingFaultLeases(t, ctx, raw, proxy, s.Binding.Namespace)
	require.NoError(t, kernel.ValidateCurrent())
	_, err = kernel.WorkspaceSyncWorkerReference()
	require.NoError(t, err)
	proxy.close(t)
	t.Logf("pending retirement native complete mode=%s stats=%+v", s.AutomaticMode, proxy.pendingSnapshot())
}

func cleanupPendingFaultRequest(t *testing.T, request cleanupPendingFaultTxn, pending *mvccpb.KeyValue) {
	t.Helper()
	require.Positive(t, request.Compares)
	require.LessOrEqual(t, request.Nodes, 64)
	require.LessOrEqual(t, request.RequestBytes, 128*1024)
	require.Equal(t, pending.CreateRevision, request.PendingCreate)
	require.Equal(t, pending.ModRevision, request.PendingMod)
	require.True(t, request.PendingLease)
	require.True(t, request.PendingValue)
}

func cleanupPendingFaultABA(t *testing.T, ctx context.Context, raw *clientv3.Client, proxy *cleanupPendingFaultProxy, d *deployment.LocalCleanupDeployment, first cleanupPendingFaultStats) {
	t.Helper()
	require.False(t, first.Transactions[0].Succeeded, "unchanged original CAS must reject the native ABA replacement")
	require.Zero(t, first.Transactions[0].Deleted)
	require.True(t, first.Survived)
	require.NotNil(t, first.Replacement)
	require.Greater(t, first.Replacement.CreateRevision, proxy.original.CreateRevision)
	require.Equal(t, proxy.original.Value, first.Replacement.Value)
	require.Equal(t, first.Replacement, cleanupExecutorRecoveryPoint(t, ctx, raw, string(proxy.original.Key)))
	select {
	case <-proxy.secondDone:
	case <-d.Done():
		t.Fatalf("ABA worker stopped before the fresh normal sweep: %v", d.Wait(context.Background()))
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	second := proxy.pendingSnapshot()
	require.Equal(t, 2, second.CASCount)
	require.False(t, second.Overflow)
	cleanupPendingFaultRequest(t, second.Transactions[1], first.Replacement)
	require.True(t, second.Transactions[1].Succeeded)
	require.EqualValues(t, 1, second.Transactions[1].Deleted)
	require.GreaterOrEqual(t, second.Transactions[1].Started.Sub(first.Transactions[0].Completed), 30*time.Second, "ABA may be retired only after the normal fresh sweep")
	absent, err := raw.Get(ctx, string(proxy.original.Key))
	require.NoError(t, err)
	require.Empty(t, absent.Kvs)
	closeErr := d.Close()
	t.Logf("automatic ABA worker actual Close=%v", closeErr)
	cleanupPendingAutomaticCloseResult(t, closeErr)
	cleanupPendingAutomaticCloseResult(t, d.Wait(context.Background()))
}

func cleanupPendingFaultACK(t *testing.T, ctx context.Context, raw *clientv3.Client, proxy *cleanupPendingFaultProxy, d *deployment.LocalCleanupDeployment, first cleanupPendingFaultStats, session *mvccpb.KeyValue, manifest string, kernel *launcher.KernelBoundary, s cleanupExecutorSetup) {
	t.Helper()
	require.True(t, first.Transactions[0].Succeeded)
	require.EqualValues(t, 1, first.Transactions[0].Deleted)
	require.Positive(t, first.Transactions[0].Revision)
	require.Equal(t, 1, first.AckCommitted)
	require.Equal(t, 1, first.HandlerJoined)
	require.True(t, first.DeadlinePresent)
	require.False(t, first.WithheldUntil.Before(first.Deadline), "response must remain withheld until the caller's original deadline")
	waitErr := d.Wait(ctx)
	t.Logf("automatic lost retirement ACK actual worker terminal=%v", waitErr)
	require.ErrorIs(t, waitErr, state.ErrOutcomeUnknown)
	closeErr := d.Close()
	t.Logf("automatic lost retirement ACK actual first Close=%v", closeErr)
	require.ErrorIs(t, closeErr, state.ErrOutcomeUnknown)
	require.Equal(t, closeErr, d.Close())
	committedAbsent, err := raw.Get(ctx, string(proxy.original.Key))
	require.NoError(t, err)
	require.Empty(t, committedAbsent.Kvs, "real committed retirement must remain absent before replacement startup")
	require.GreaterOrEqual(t, committedAbsent.Header.Revision, first.Transactions[0].Revision)
	cleanupPendingFaultLeases(t, ctx, raw, proxy, s.Binding.Namespace)
	second, err := deployment.OpenAutomaticLocalCleanup(ctx, manifest, kernel)
	require.NoError(t, err)
	defer second.Close()
	replacement := cleanupWorkerRoleSession(t, ctx, raw, s.Binding.Namespace, "cleanup-loader", second.Done())
	require.NotEqual(t, string(session.Key), string(replacement.Key))
	require.NotEqual(t, session.Lease, replacement.Lease)
	// A real normal round on the replacement worker must remain metadata-only;
	// no Task/P is resubmitted and no original physical stage may be replayed.
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-second.Done():
		t.Fatalf("replacement automatic worker stopped: %v", second.Wait(context.Background()))
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	absent, err := raw.Get(ctx, string(proxy.original.Key))
	require.NoError(t, err)
	require.Empty(t, absent.Kvs)
	secondClose := second.Close()
	t.Logf("automatic lost retirement ACK replacement actual Close=%v", secondClose)
	cleanupPendingAutomaticCloseResult(t, secondClose)
	cleanupPendingAutomaticCloseResult(t, second.Wait(context.Background()))
	require.Equal(t, 1, proxy.pendingSnapshot().CASCount, "P absent after the real commit must not be manufactured/retried")
}

func cleanupPendingFaultPhysical(t *testing.T, before, after cleanupExecutorProxyStats) {
	t.Helper()
	require.Equal(t, 2, before.ClaimPuts)
	require.Equal(t, 1, before.ArchivePuts)
	require.Equal(t, before.ClaimPuts, after.ClaimPuts)
	require.Equal(t, before.ArchivePuts, after.ArchivePuts)
	require.Equal(t, before.ClosePuts, after.ClosePuts)
	require.Equal(t, before.QuiescencePuts, after.QuiescencePuts)
	require.Equal(t, before.TerminationPuts, after.TerminationPuts)
	require.Equal(t, before.ProviderPuts, after.ProviderPuts)
	require.False(t, after.ObservationOverflow)
}

func cleanupPendingFaultLeases(t *testing.T, ctx context.Context, raw *clientv3.Client, proxy *cleanupPendingFaultProxy, namespace string) {
	t.Helper()
	sessions, err := raw.Get(ctx, namespace+"workers/sessions/", clientv3.WithPrefix(), clientv3.WithLimit(33))
	require.NoError(t, err)
	require.Empty(t, sessions.Kvs)
	for _, id := range proxy.snapshot().GrantIDs {
		ttl, err := raw.TimeToLive(ctx, clientv3.LeaseID(id))
		require.NoError(t, err)
		require.EqualValues(t, -1, ttl.TTL, "every actually granted lease must be gone after original Close")
	}
}
