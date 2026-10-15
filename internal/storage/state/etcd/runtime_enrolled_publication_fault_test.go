package etcd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
)

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
