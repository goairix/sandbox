package etcd

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
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
