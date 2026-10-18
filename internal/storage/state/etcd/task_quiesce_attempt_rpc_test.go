package etcd

import (
	"context"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"testing"
	"time"
)

// Outgoing boundary accounting only: this unsealed model never returns native
// authority. Candidate identity is fixed across absent reads; no standalone
// permanent Put is permitted before its original Stage business submission.
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
	b.client = &clientv3.Client{KV: &quiescenceIssuerRecordingKV{commit: func(cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
		if len(yes) == 1 && yes[0].IsPut() {
			writes++
			t.Fatal("candidate preparation must not send a standalone reservation Put")
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
	require.NoError(t, err)
	require.True(t, d.reservationSent)
	require.Zero(t, writes)
	original := d.attemptReference
	for i := 0; i < 2; i++ {
		require.NoError(t, b.reserveTaskQuiescenceAttempt(context.Background(), c, d))
	}
	require.Zero(t, writes)
	require.Equal(t, 3, reads)
	require.Equal(t, original, d.attemptReference)
	require.Zero(t, d.reservation.revision)
}

func TestTaskQuiesceMetadataAttemptHistoryAssociation(t *testing.T) {
	record := quiesceRecordFixture(t)
	for _, fault := range []string{"valid", "missing", "same-birth", "future-birth", "task", "claim", "command", "locator", "destroying"} {
		t.Run(fault, func(t *testing.T) {
			reservation := &TaskQuiescenceAttemptEntry{Revision: 40, Record: &TaskQuiescenceAttemptRecord{Version: 1, Task: record.Task, Claim: record.Claim, DestroyingRevision: record.Context.Current.ControlRevision, CommandID: record.Context.Current.CommandID, Attempt: record.Attempt}}
			switch fault {
			case "missing":
				reservation = nil
			case "same-birth":
				reservation.Revision = 50
			case "future-birth":
				reservation.Revision = 51
			case "task":
				reservation.Record.Task.Generation++
			case "claim":
				reservation.Record.Claim.WorkerID = "other"
			case "command":
				reservation.Record.CommandID = "other"
			case "locator":
				reservation.Record.Attempt.AttemptID = "other"
			case "destroying":
				reservation.Record.DestroyingRevision++
			}
			require.Equal(t, fault == "valid" || fault == "same-birth", taskQuiescenceHistoryReservation(record, 50, reservation))
		})
	}
}
