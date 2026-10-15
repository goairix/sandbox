package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

func TestEnrolledPublicationOversizeBeforeRPC(t *testing.T) {
	// Count every outgoing native unary/stream RPC, including reads, rather
	// than merely counting Grant or calling the record encoder directly.
	var calls atomic.Int64
	client, err := clientv3.New(clientv3.Config{Endpoints: []string{"http://127.0.0.1:1"}, Logger: zap.NewNop(), DialOptions: []grpc.DialOption{
		grpc.WithUnaryInterceptor(func(context.Context, string, any, any, *grpc.ClientConn, grpc.UnaryInvoker, ...grpc.CallOption) error {
			calls.Add(1)
			return errors.New("unexpected native RPC")
		}),
		grpc.WithStreamInterceptor(func(context.Context, *grpc.StreamDesc, *grpc.ClientConn, string, grpc.Streamer, ...grpc.CallOption) (grpc.ClientStream, error) {
			calls.Add(1)
			return nil, errors.New("unexpected native stream RPC")
		}),
	}})
	require.NoError(t, err)
	defer client.Close()
	n, err := NewNamespace("/"+strings.Repeat("p", 252), strings.Repeat("s", 128), strings.Repeat("c", 128))
	require.NoError(t, err)
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	runtimeKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{8}, 32))
	issuerKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32))
	now := time.Now().UTC()
	nb, na := now.Add(-time.Minute), now.Add(5*time.Minute)
	var material controlprotocol.EnrolledRuntimeEvidence
	var verifier *controlprotocol.ManagementVerifier
	var reference CreationClaimReference
	var recordBytes int
	// Escaping is legitimate legacy wire expansion, not padding/unknown fields.
	// Find an independently valid signed legacy activation+delegate set whose
	// combined canonical companion exceeds the new record's stricter cap.
	for width := 32; width <= 128; width += 8 {
		large := strings.Repeat("<", width)
		binding := controlprotocol.TrustBinding{Namespace: n.Root(), AuthorityID: large, Target: large, RestoreEpoch: "restore"}
		v, err := controlprotocol.NewManagementVerifier(binding, []ed25519.PublicKey{root.Public().(ed25519.PublicKey)})
		require.NoError(t, err)
		runtime := controlprotocol.RuntimeReference{ID: large, UID: large, BootID: "11111111-1111-4111-8111-111111111111"}
		identity := controlprotocol.RuntimeIdentityContext{SandboxID: "sandbox", WorkspaceHash: strings.Repeat("a", 64), Generation: 1, Runtime: runtime}
		runtimeCertificate, err := controlprotocol.SignRuntimeIdentityCertificate(root, controlprotocol.RuntimeIdentityCertificateClaims{Version: 1, CertificateID: "22222222-2222-4222-8222-222222222222", Role: "runtime_receipt", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: runtimeKey.Public().(ed25519.PublicKey), NotBefore: nb, NotAfter: na, SandboxID: identity.SandboxID, WorkspaceHash: identity.WorkspaceHash, Generation: identity.Generation, Runtime: runtime})
		if err != nil {
			continue
		}
		issuerCertificate, err := controlprotocol.SignCommandIssuerCertificate(root, controlprotocol.CommandIssuerCertificateClaims{Version: 1, CertificateID: "33333333-3333-4333-8333-333333333333", Role: "command_issuer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: issuerKey.Public().(ed25519.PublicKey), NotBefore: nb, NotAfter: na})
		if err != nil {
			continue
		}
		rd, _ := snapshotDigest(runtimeCertificate)
		id, _ := snapshotDigest(issuerCertificate)
		activation, err := controlprotocol.SignTargetActivation(root, controlprotocol.TargetActivationClaims{Version: 1, Role: "target_activation", ActivationID: "44444444-4444-4444-8444-444444444444", Binding: binding, Identity: identity, DataGateEpoch: 1, UID: 1000, GID: 1000, ContractDigest: strings.Repeat("b", 64), RuntimeCertificateDigest: rd, IssuerCertificateDigest: id, NotBefore: nb, NotAfter: na})
		require.NoError(t, err)
		request := controlprotocol.NodeEnrollmentRequest{Version: 1, Purpose: "runtime_node_enrollment", RuntimeCertificate: runtimeCertificate, Activation: activation, IssuerCertificate: issuerCertificate}
		if _, err := controlprotocol.EncodeNodeEnrollmentRequest(request); err != nil {
			continue
		}
		_, err = v.VerifyNodeEnrollmentRequest(request, now)
		require.NoError(t, err, "legacy activation and both root-signed delegates must actually verify")
		receipt, err := json.Marshal(struct {
			Claims struct {
				Runtime controlprotocol.RuntimeReference `json:"runtime"`
			} `json:"claims"`
		}{Claims: struct {
			Runtime controlprotocol.RuntimeReference `json:"runtime"`
		}{runtime}})
		require.NoError(t, err)
		candidate := controlprotocol.EnrolledRuntimeEvidence{Request: request, ObserverCertificate: []byte(`{"shape":"input-only"}`), Receipt: receipt}
		d := RuntimeDispatchRecord{IntentID: "intent", SandboxID: identity.SandboxID, WorkspaceHash: identity.WorkspaceHash, RestoreEpoch: binding.RestoreEpoch, Generation: 1}
		_, companion := enrollmentRecords(d, candidate)
		wire, err := json.Marshal(companion)
		require.NoError(t, err)
		if len(wire) <= 8192 {
			continue
		}
		material, verifier, recordBytes = candidate, v, len(wire)
		reference = CreationClaimReference{IntentID: d.IntentID, SandboxID: d.SandboxID, WorkspaceHash: d.WorkspaceHash, RestoreEpoch: d.RestoreEpoch, Generation: d.Generation}
		break
	}
	require.Greater(t, recordBytes, 8192, "fixture must exceed the new companion cap within the valid legacy frame")
	b := &Backend{client: client, namespace: n, restoreEpoch: "restore", requestTimeout: 50 * time.Millisecond, archiveLife: &taskArchiveBackend{}, enrolledPublication: &EnrolledPublicationOptions{MaxActive: 1}, publicationVerifier: &controlprotocol.PublicationVerifier{}, enrolledVerifier: verifier}
	b.identityKey, _ = n.Key("meta", "identity")
	b.restoreKey, _ = n.Key("meta", "restore_epoch")
	c := &CreationClaim{origin: b, reference: reference, deadline: time.Now().Add(time.Minute)}
	calls.Store(0)
	_, err = b.PublishRuntimeEnrolled(context.Background(), c, []byte(`{}`), material)
	require.ErrorIs(t, err, ErrInvalidRecord)
	require.Zero(t, calls.Load(), "oversized final companion must refuse at the public entry before every native RPC")
	require.Nil(t, c.enrolledAttempt, "rejected input must not freeze a Stage attempt")
	t.Logf("verified legacy request accepted, new companion=%d bytes, native unary/stream calls=0", recordBytes)
}

func TestEnrolledPublicationOwners(t *testing.T) {
	b := &Backend{enrolledPublication: &EnrolledPublicationOptions{MaxActive: 1}, archiveLife: &taskArchiveBackend{}}
	o, err := b.registerEnrolledPublication(context.Background())
	require.NoError(t, err)
	_, err = b.registerEnrolledPublication(context.Background())
	require.Error(t, err)
	c := &CreationClaim{origin: b}
	ref := StageReference{StageID: enrolledPublicationStage, AttemptID: "original"}
	a, original, err := c.freezeEnrolledPublication("input", ref, o)
	require.NoError(t, err)
	require.True(t, original)
	got, original, err := c.freezeEnrolledPublication("input", StageReference{AttemptID: "replacement"}, o)
	require.NoError(t, err)
	require.False(t, original)
	require.Same(t, a, got)
	require.Equal(t, ref, got.reference)
	_, _, err = c.freezeEnrolledPublication("different", ref, o)
	require.ErrorIs(t, err, ErrIdempotencyConflict)
	closed := make(chan error, 1)
	go func() { closed <- b.Close() }()
	select {
	case <-o.context.Done():
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel original owner")
	}
	select {
	case <-closed:
		t.Fatal("Close returned before native owner joined")
	default:
	}
	_, err = b.registerEnrolledPublication(context.Background())
	require.Error(t, err)
	o.finish()
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Close did not join original owner")
	}
	require.NoError(t, b.Close())
}

func TestEnrolledPublicationCloseBlockedRPC(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	accepted := make(chan net.Conn, 1)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := l.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	client, err := clientv3.New(clientv3.Config{Endpoints: []string{"http://" + l.Addr().String()}, DialTimeout: time.Second, Logger: zap.NewNop()})
	require.NoError(t, err)
	b := &Backend{client: client, enrolledPublication: &EnrolledPublicationOptions{MaxActive: 1}, archiveLife: &taskArchiveBackend{}}
	o, err := b.registerEnrolledPublication(context.Background())
	require.NoError(t, err)
	rpcDone, release := make(chan struct{}), make(chan struct{})
	go func() { _, _ = client.Get(o.context, "/owned-enrolled/key"); close(rpcDone); <-release; o.finish() }()
	var conn net.Conn
	select {
	case conn = <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("actual gRPC connection not accepted")
	}
	defer conn.Close()
	closed := make(chan error, 1)
	go func() { closed <- b.Close() }()
	select {
	case <-rpcDone:
	case <-time.After(2 * time.Second):
		t.Fatal("actual blocked native RPC did not join")
	}
	select {
	case <-closed:
		t.Fatal("Close returned before original owner joined")
	default:
	}
	require.NoError(t, client.Ctx().Err(), "shared client closed before owner joined")
	close(release)
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not finish")
	}
	<-serverDone
}

func TestEnrolledPublicationProfileRefusal(t *testing.T) {
	legacy := &Backend{}
	_, err := legacy.PublishRuntimeEnrolled(context.Background(), nil, []byte(`{}`), controlprotocol.EnrolledRuntimeEvidence{})
	require.ErrorIs(t, err, ErrInvalidConfiguration)
	_, err = legacy.LoadRuntimePublicationEnrolled(context.Background(), WorkspaceIdentity{}, "intent")
	require.ErrorIs(t, err, ErrInvalidConfiguration)
	enrolled := &Backend{enrolledPublication: &EnrolledPublicationOptions{MaxActive: 1}}
	_, err = enrolled.PublishRuntime(context.Background(), nil, []byte(`{}`))
	require.ErrorIs(t, err, ErrInvalidConfiguration)
}

func TestEnrolledPublicationReadRejectsPartial(t *testing.T) {
	b := &Backend{}
	_, err := b.decodeEnrolledPublicationSnapshot(make([]*mvccpb.KeyValue, 14), make([]string, 14), WorkspaceIdentity{}, "intent", RuntimeEnrolledPublicationRecord{})
	require.Error(t, err)
}

func TestEnrolledPublicationCompactFailure(t *testing.T) {
	n := validOptions(t).Namespace
	b := &Backend{namespace: n, restoreEpoch: "epoch", clusterID: 1}
	l := StageAttemptLocator{Namespace: n.Root(), RequestID: "request", StageID: enrolledPublicationStage, AttemptID: "11111111-1111-4111-8111-111111111111", RestoreEpoch: "epoch"}
	ref := l.reference("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	_, key, err := b.stageKeys(ref)
	require.NoError(t, err)
	value, err := encodeReceipt(ref, OutcomeCommitted)
	require.NoError(t, err)
	makeReply := func() *clientv3.TxnResponse {
		return &clientv3.TxnResponse{Header: &pb.ResponseHeader{ClusterId: 1, Revision: 10}, Responses: []*pb.ResponseOp{{Response: &pb.ResponseOp_ResponseRange{ResponseRange: &pb.RangeResponse{Count: 1, Kvs: []*mvccpb.KeyValue{{Key: []byte(key), Value: []byte(value), Version: 1, CreateRevision: 9, ModRevision: 9}}}}}}}
	}
	_, err = b.enrolledCompactReceipt(makeReply(), ref)
	require.NoError(t, err)
	for name, mutate := range map[string]func(*clientv3.TxnResponse){
		"cluster": func(r *clientv3.TxnResponse) { r.Header.ClusterId++ },
		"header":  func(r *clientv3.TxnResponse) { r.Header.Revision = 0 },
		"kind": func(r *clientv3.TxnResponse) {
			r.Responses[0] = &pb.ResponseOp{Response: &pb.ResponseOp_ResponsePut{ResponsePut: &pb.PutResponse{}}}
		},
		"count": func(r *clientv3.TxnResponse) { r.Responses[0].GetResponseRange().Count++ },
		"more":  func(r *clientv3.TxnResponse) { r.Responses[0].GetResponseRange().More = true },
		"nested": func(r *clientv3.TxnResponse) {
			r.Responses[0].GetResponseRange().Header = &pb.ResponseHeader{Revision: 11}
		},
		"leased":  func(r *clientv3.TxnResponse) { r.Responses[0].GetResponseRange().Kvs[0].Lease = 1 },
		"version": func(r *clientv3.TxnResponse) { r.Responses[0].GetResponseRange().Kvs[0].Version = 2 },
		"mutable": func(r *clientv3.TxnResponse) { r.Responses[0].GetResponseRange().Kvs[0].ModRevision++ },
	} {
		t.Run(name, func(t *testing.T) {
			r := makeReply()
			mutate(r)
			_, err := b.enrolledCompactReceipt(r, ref)
			require.Error(t, err)
		})
	}
}

type enrolledUnknownLease struct {
	clientv3.Lease
	grants int
}

func (l *enrolledUnknownLease) Grant(context.Context, int64) (*clientv3.LeaseGrantResponse, error) {
	l.grants++
	return nil, errors.New("lost grant ACK")
}

func TestEnrolledPublicationGrantUnknown(t *testing.T) {
	n := validOptions(t).Namespace
	lease := &enrolledUnknownLease{}
	b := &Backend{namespace: n, restoreEpoch: "epoch", identityValue: "identity", requestTimeout: time.Second, client: &clientv3.Client{Lease: lease}}
	b.identityKey, _ = n.Key("meta", "identity")
	b.restoreKey, _ = n.Key("meta", "restore_epoch")
	l := StageAttemptLocator{Namespace: n.Root(), RequestID: "request", StageID: enrolledPublicationStage, AttemptID: "11111111-1111-4111-8111-111111111111", RestoreEpoch: "epoch"}
	m := Mutation{}
	for i := 0; i < 46; i++ {
		m.Comparisons = append(m.Comparisons, clientv3.Compare(clientv3.CreateRevision(n.Root()+fmt.Sprintf("cmp-%d", i)), "=", 0))
	}
	for i := 0; i < 8; i++ {
		m.Writes = append(m.Writes, Write{Key: n.Root() + fmt.Sprintf("write-%d", i), Value: []byte("value")})
	}
	owned, digest, err := prepareEnrolledMutation(n, m)
	require.NoError(t, err)
	s, cleanup, err := b.beginEnrolledStage(context.Background(), l.reference(digest), owned)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.Nil(t, s)
	require.NoError(t, cleanup)
	require.Equal(t, 1, lease.grants)
}
