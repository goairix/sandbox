package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestBeginOperationUnknownReplies(t *testing.T) {
	for _, phase := range []string{"grant", "known grant", "guard", "admit"} {
		for _, cleanupFails := range []bool{false, true} {
			t.Run(phase+map[bool]string{false: "", true: " cleanup failure"}[cleanupFails], func(t *testing.T) {
				b, raw, in, _ := operationControlFixture(t, "plain")
				ctx := context.Background()
				var original clientv3.LeaseID
				var observed OperationReference
				writes := 0
				lease := &creationFaultLease{Lease: b.client.Lease, grant: func(r *clientv3.LeaseGrantResponse, e error) (*clientv3.LeaseGrantResponse, error) {
					require.NoError(t, e)
					original = r.ID
					if phase == "grant" {
						return nil, context.DeadlineExceeded
					}
					if phase == "known grant" {
						return r, context.DeadlineExceeded
					}
					return r, e
				}}
				cleanupErr := errors.New("owned operation cleanup failed")
				if cleanupFails {
					lease.revoke = func(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
						require.NoError(t, ctx.Err())
						_, bounded := ctx.Deadline()
						require.True(t, bounded)
						require.Equal(t, original, id)
						return nil, cleanupErr
					}
				}
				b.client.Lease = lease
				t.Cleanup(func() {
					b.client.Lease = lease.Lease
					if original > 0 {
						_, _ = raw.Revoke(context.Background(), original)
					}
				})
				b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
					if len(ops) == 0 || !ops[0].IsPut() {
						return false
					}
					writes++
					var record OperationRecord
					require.NoError(t, json.Unmarshal(ops[0].ValueBytes(), &record))
					observed = record.Reference
					return phase == "guard" && len(ops) == 1 || phase == "admit" && len(ops) > 1
				}, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
					require.NoError(t, e)
					require.True(t, r.Succeeded)
					return nil, context.DeadlineExceeded
				}}
				result, err := b.BeginOperation(ctx, BeginOperationInput{SandboxID: in.SandboxID, RequestID: "unknown", Kind: OperationData})
				require.ErrorIs(t, err, ErrOutcomeUnknown)
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.Equal(t, OperationUnknown, result.Outcome)
				require.Nil(t, result.Capability)
				require.NotEmpty(t, result.Reference.OperationID)
				require.NotEmpty(t, result.Reference.Digest)
				if phase == "grant" {
					require.Zero(t, result.Reference.LeaseID)
					require.Zero(t, writes)
					require.Zero(t, lease.revokes.Load())
				} else {
					require.Equal(t, int64(original), result.Reference.LeaseID)
					require.Equal(t, int64(1), lease.revokes.Load())
					if phase == "known grant" {
						require.Zero(t, writes)
					} else {
						require.Equal(t, observed, result.Reference)
					}
					if phase == "guard" {
						require.Equal(t, 1, writes)
					}
				}
				if cleanupFails && phase != "grant" {
					require.ErrorIs(t, result.GuardCleanupError, cleanupErr)
					require.NotErrorIs(t, err, cleanupErr)
				} else {
					require.NoError(t, result.GuardCleanupError)
				}
				if cleanupFails && phase == "admit" {
					token, _, receipt, _, e := b.namespace.operationKeys(result.Reference)
					require.NoError(t, e)
					got, e := b.readDomain(ctx, token, receipt)
					require.NoError(t, e)
					require.NoError(t, validateOperationCompletion(got[0], got[1]))
				}
			})
		}
	}
}

func TestBeginOperationCannotDeliverCancelledOrRevoked(t *testing.T) {
	for _, phase := range []string{"grant", "guard", "admit", "revoked after admit"} {
		t.Run(phase, func(t *testing.T) {
			b, raw, in, _ := operationControlFixture(t, "plain")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var original clientv3.LeaseID
			lease := &creationFaultLease{Lease: b.client.Lease, grant: func(r *clientv3.LeaseGrantResponse, e error) (*clientv3.LeaseGrantResponse, error) {
				require.NoError(t, e)
				original = r.ID
				if phase == "grant" {
					cancel()
				}
				return r, e
			}}
			b.client.Lease = lease
			b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
				if len(ops) == 0 || !ops[0].IsPut() {
					return false
				}
				return phase == "guard" && len(ops) == 1 || strings.Contains(phase, "admit") && len(ops) > 1
			}, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
				require.NoError(t, e)
				require.True(t, r.Succeeded)
				if phase == "revoked after admit" {
					_, e = raw.Revoke(context.Background(), original)
					require.NoError(t, e)
				} else {
					cancel()
				}
				return r, e
			}}
			result, err := b.BeginOperation(ctx, BeginOperationInput{SandboxID: in.SandboxID, RequestID: "canceled", Kind: OperationData})
			require.Error(t, err)
			require.Nil(t, result.Capability)
			if strings.Contains(phase, "admit") {
				require.Equal(t, OperationCommitted, result.Outcome)
			}
			require.Equal(t, int64(1), lease.revokes.Load())
			require.NoError(t, result.GuardCleanupError)
		})
	}
}
