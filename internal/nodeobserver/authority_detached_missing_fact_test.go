package nodeobserver

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// This bounded native protocol fixture contains only the initial three points.
// Its real ticket signature is permission to investigate the fixed task, not
// signed source history, a valid settlement writer, or physical kill evidence.
// Missing fact replies always remain native Count=0, never a forged success.
type detachedMissingFactKV struct {
	pb.UnimplementedKVServer
	mu       sync.Mutex
	values   map[string]*mvccpb.KeyValue
	requests []*pb.TxnRequest
}

func (s *detachedMissingFactKV) Txn(_ context.Context, request *pb.TxnRequest) (*pb.TxnResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, request)
	if len(request.Compare) != 0 || len(request.Failure) != 0 || len(request.Success) == 0 || len(request.Success) > 4 || request.Size() > 128<<10 {
		return nil, status.Error(codes.InvalidArgument, "test reader requires bounded fixed point transactions")
	}
	response := &pb.TxnResponse{Header: &pb.ResponseHeader{ClusterId: 7, MemberId: 1, Revision: 100, RaftTerm: 1}, Succeeded: true}
	for _, op := range request.Success {
		point := op.GetRequestRange()
		if point == nil || len(point.Key) == 0 || len(point.RangeEnd) != 0 {
			return nil, status.Error(codes.InvalidArgument, "test reader requires point operations")
		}
		rangeResponse := &pb.RangeResponse{}
		if kv := s.values[string(point.Key)]; kv != nil {
			copy := *kv
			copy.Key, copy.Value = bytes.Clone(kv.Key), bytes.Clone(kv.Value)
			rangeResponse.Kvs, rangeResponse.Count = []*mvccpb.KeyValue{&copy}, 1
		}
		response.Responses = append(response.Responses, &pb.ResponseOp{Response: &pb.ResponseOp_ResponseRange{ResponseRange: rangeResponse}})
	}
	return response, nil
}

func detachedMissingFactClient(t *testing.T, values map[string]*mvccpb.KeyValue) (*clientv3.Client, *detachedMissingFactKV) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	native := &detachedMissingFactKV{values: values}
	server := grpc.NewServer()
	pb.RegisterKVServer(server, native)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
		<-done
	})
	client, err := clientv3.New(clientv3.Config{Endpoints: []string{"http://" + listener.Addr().String()}, DialTimeout: time.Second, MaxCallSendMsgSize: 128 << 10, MaxCallRecvMsgSize: 256 << 10})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	return client, native
}

// The behavior under test is the real reader's emitted fixed native request.
// A persisted detached intent must never be passed to the local key wrapper or
// replaced by archive/local-complete absence. No positive source is fabricated.
func TestNodeObserverDetachedReservationRequiresNativeSettlement(t *testing.T) {
	now := time.Now().UTC()
	binding := p.TrustBinding{Namespace: "/observer/detached/v1/", AuthorityID: "test", Target: "node", RestoreEpoch: "restore"}
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	issuerKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{8}, ed25519.SeedSize))
	verifier, err := p.NewManagementVerifier(binding, []ed25519.PublicKey{root.Public().(ed25519.PublicKey)})
	require.NoError(t, err)
	issuerID := uuid.NewString()
	issuerWire, err := p.SignCommandIssuerCertificate(root, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: issuerID, Role: "command_issuer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: issuerKey.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(5 * time.Minute)})
	require.NoError(t, err)
	issuer, err := verifier.VerifyCommandIssuerCertificate(issuerWire, now)
	require.NoError(t, err)
	current := p.TaskCloseDataContext{Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch,
		IssuerCertificateID: issuerID, IssuerCertificateDigest: digest(issuerWire), CommandID: uuid.NewString(), TaskID: uuid.NewString(), TaskDigest: strings.Repeat("a", 64), ClaimID: uuid.NewString(), WorkerID: "worker", SandboxID: uuid.NewString(), WorkspaceHash: strings.Repeat("ab", 32), Generation: 1, DataGateEpoch: 1, ControlRevision: 10, ClaimCreateRevision: 20, LeaseID: 21,
		Runtime: p.RuntimeReference{ID: "plain-pod", UID: uuid.NewString(), BootID: uuid.NewString()},
	}
	stamp := p.RecordStamp{CreateRevision: 10, ModRevision: 10, SHA256: strings.Repeat("a", 64)}
	intent := p.TaskRuntimeTerminationIntent{Version: 2, Phase: "reserved", Current: current, Partition: 0xab, TaskBirth: 10,
		CreationIntentID: uuid.NewString(), EnrollmentDigest: strings.Repeat("b", 64), ObserverCertificateDigest: strings.Repeat("c", 64), NodeUID: uuid.NewString(), NodeBootID: uuid.NewString(), Action: "kill_pid1_and_observe_cgroup_empty",
		Authority: p.TerminationAuthorityBindings{Task: stamp, CleanupIntent: stamp, TaskLink: stamp, Placement: stamp, Control: stamp, Owner: stamp, Fence: stamp, RuntimeIndex: stamp, Quiescence: stamp, QuiescenceReceipt: stamp, QuiescenceReservation: stamp, FinalSync: stamp, Issuer: stamp,
			QuiescenceStage: p.TerminationQuiescenceStage{RequestID: "request", StageID: "stage", AttemptID: uuid.NewString(), Digest: strings.Repeat("d", 64)}},
		DetachedEffects: &p.DetachedRuntimeEffectBindings{Version: 1, Profile: "detached_enrolled_plain", ContractDigest: strings.Repeat("e", 64), Settlement: p.RecordStamp{CreateRevision: 30, ModRevision: 30, SHA256: strings.Repeat("f", 64)}},
	}
	intentWire, err := p.EncodeTaskRuntimeTerminationIntent(intent)
	require.NoError(t, err)
	intentKey := fmt.Sprintf("%sp/ab/tasks/%s/runtime-termination", binding.Namespace, current.TaskID)
	settlementKey := fmt.Sprintf("%sp/ab/tasks/%s/detached-noeffect", binding.Namespace, current.TaskID)
	identity := []byte(`{"schema_version":1}`)
	point := func(key string, value []byte, revision int64) *mvccpb.KeyValue {
		return &mvccpb.KeyValue{Key: []byte(key), Value: value, CreateRevision: revision, ModRevision: revision, Version: 1}
	}
	client, native := detachedMissingFactClient(t, map[string]*mvccpb.KeyValue{
		binding.Namespace + "meta/identity":      point(binding.Namespace+"meta/identity", identity, 1),
		binding.Namespace + "meta/restore_epoch": point(binding.Namespace+"meta/restore_epoch", []byte(binding.RestoreEpoch), 1),
		intentKey:                                point(intentKey, intentWire, 40),
	})
	claims := p.TaskRuntimeTerminationTicketClaims{Version: 1, Purpose: "task_runtime_terminate", Current: current, Partition: 0xab,
		IntentCreateRevision: 40, IntentModRevision: 40, IntentDigest: digest(intentWire), EnrollmentDigest: intent.EnrollmentDigest, ObserverCertificateDigest: intent.ObserverCertificateDigest, NodeUID: intent.NodeUID, NodeBootID: intent.NodeBootID,
		NotBefore: now.Add(-2 * time.Second), NotAfter: now.Add(15 * time.Second),
	}
	ticket, err := p.SignTaskRuntimeTerminationTicket(issuerKey, issuer, claims)
	require.NoError(t, err)
	_, err = verifier.VerifyTaskRuntimeTerminationTicket(ticket, issuerWire, claims, now)
	require.NoError(t, err, "the negative must reach native authority after genuine ticket authentication")
	reader := &authorityReader{client: client, verifier: verifier, clock: &journalCutoffClock{utc: now}, binding: binding, identity: identity, clusterID: 7, timeout: time.Second}
	enrollment := authorityEnrollment{Identity: p.RuntimeIdentityContext{SandboxID: current.SandboxID, WorkspaceHash: current.WorkspaceHash, Generation: current.Generation, Runtime: current.Runtime}, CreationIntentID: intent.CreationIntentID, EnrollmentDigest: intent.EnrollmentDigest, ObserverCertificateDigest: intent.ObserverCertificateDigest, NodeUID: intent.NodeUID, NodeBootID: intent.NodeBootID}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snapshot, err := reader.load(ctx, ticket, issuerWire, enrollment, time.Now().Add(5*time.Second))
	require.Error(t, err, "an actual missing settlement must never authorize effects")
	require.Nil(t, snapshot)

	// Inspect the native boundary after the reader joined its actual calls.
	// This checks emitted protocol keys, not an asserted mock call count.
	native.mu.Lock()
	defer native.mu.Unlock()
	readSettlement := false
	for _, request := range native.requests {
		for _, op := range request.Success {
			key := string(op.GetRequestRange().Key)
			require.NotContains(t, key, "workspace-archive-command")
			require.NotContains(t, key, "workspace-sync-local-complete")
			readSettlement = readSettlement || key == settlementKey
		}
	}
	require.True(t, readSettlement, "persisted v2 must select its real fixed settlement point before rejecting missing authority")
}
