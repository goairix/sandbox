package etcd

import (
	"context"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"strings"
	"testing"
	"time"
)

// Invalid private pins and complete protocol bytes must fail before any Grant.
func TestTaskQuiesceMetadataReservedStagePreflight(t *testing.T) {
	for _, fault := range []string{"zero-pin", "foreign-pin", "65", "bytes", "protocol-bytes"} {
		t.Run(fault, func(t *testing.T) {
			b, c, d := quiescenceBuilderFixture(t)
			b.restoreEpoch = c.reference.Task.RestoreEpoch
			b.identityKey, _ = b.namespace.Key("identity")
			b.restoreKey, _ = b.namespace.Key("restore")
			b.identityValue = "identity"
			l := StageAttemptLocator{Namespace: b.namespace.Root(), Partition: c.reference.Task.Partition, RequestID: c.reference.ClaimID, StageID: "task_quiesce_users", AttemptID: uuid.NewString(), RestoreEpoch: b.restoreEpoch}
			key, _ := b.namespace.taskQuiescenceAttemptKey(c.reference.Task)
			pin := taskQuiescenceReservationPin{key: key, revision: 40}
			lease := &creationFaultLease{}
			b.client = &clientv3.Client{Lease: lease}
			if fault == "zero-pin" {
				pin.revision = 0
			}
			if fault == "foreign-pin" {
				pin.key, _ = b.namespace.Key("other")
			}
			if fault == "protocol-bytes" {
				b.identityValue = strings.Repeat("x", maxRecordBytes+1)
			}
			s, err := b.beginReservedTaskQuiescenceStage(context.Background(), l, pin, time.Second, func(got StageAttemptLocator) (Mutation, error) {
				require.Equal(t, l, got)
				m, e := b.buildTaskQuiescence(c, d, got)
				if e != nil {
					return m, e
				}
				if fault == "65" {
					m.Comparisons = append(m.Comparisons, clientv3.Compare(clientv3.ModRevision(key), "=", 0))
				}
				if fault == "bytes" {
					for i := 0; i < 8; i++ {
						m.Comparisons[i*4] = clientv3.Compare(clientv3.Value(c.fences[i].key), "=", strings.Repeat("x", maxRecordBytes))
					}
				}
				return m, nil
			})
			require.ErrorIs(t, err, ErrInvalidMutation)
			require.Nil(t, s)
			require.Zero(t, lease.grants.Load())
			require.Zero(t, lease.revokes.Load())
		})
	}
}
