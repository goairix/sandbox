package etcd

import (
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"testing"
	"time"
)

// Outgoing boundary accounting only: this unsealed model never returns native
// authority. Removing reservationSent would permit the forbidden second Put.
func TestTaskQuiesceMetadataAttemptNoResend(t *testing.T) {
	b, c, d := quiescenceBuilderFixture(t)
	c.birth = d.claims.Context.Current.ControlRevision
	c.parentCtx = context.Background()
	c.deadline = time.Now().Add(time.Minute)
	d.deadline = c.deadline
	b.clusterID = 7
	b.restoreEpoch = c.reference.Task.RestoreEpoch
	b.identityKey, _ = b.namespace.Key("identity")
	b.identityValue = "identity"
	b.restoreKey, _ = b.namespace.Key("restore")
	b.requestTimeout = time.Second
	key, _ := b.namespace.taskQuiescenceAttemptKey(c.reference.Task)
	writes, reads := 0, 0
	stop := errors.New("reservation reply lost")
	b.client = &clientv3.Client{KV: &quiescenceIssuerRecordingKV{commit: func(cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
		if len(yes) == 1 && yes[0].IsPut() {
			writes++
			require.Equal(t, key, string(yes[0].KeyBytes()))
			require.Equal(t, 49, len(cmps)+len(yes)+len(no))
			require.Len(t, cmps, 45)
			require.Equal(t, c.comparisons(), cmps[4:44])
			return nil, stop
		}
		reads++
		require.Equal(t, 50, len(cmps)+len(yes)+len(no))
		require.Len(t, yes, 3)
		require.Len(t, no, 3)
		header := &pb.ResponseHeader{ClusterId: 7, Revision: 100}
		r := &clientv3.TxnResponse{Header: header, Succeeded: true}
		for i, k := range []string{b.identityKey, b.restoreKey, key} {
			point := &pb.RangeResponse{Header: header}
			if i < 2 {
				value := b.identityValue
				if i == 1 {
					value = b.restoreEpoch
				}
				point.Count = 1
				point.Kvs = []*mvccpb.KeyValue{{Key: []byte(k), Value: []byte(value), CreateRevision: 1, ModRevision: 1}}
			}
			r.Responses = append(r.Responses, &pb.ResponseOp{Response: &pb.ResponseOp_ResponseRange{ResponseRange: point}})
		}
		return r, nil
	}}}
	err := b.reserveTaskQuiescenceAttempt(context.Background(), c, d)
	require.ErrorIs(t, err, stop)
	require.True(t, d.reservationSent)
	require.Equal(t, 1, writes)
	original := d.attemptReference
	for i := 0; i < 2; i++ {
		require.ErrorIs(t, b.reserveTaskQuiescenceAttempt(context.Background(), c, d), ErrOutcomeUnknown)
	}
	require.Equal(t, 1, writes)
	require.Equal(t, 3, reads)
	require.Equal(t, original, d.attemptReference)
	require.Zero(t, d.reservation.revision)
}
