package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Returning the Begin aggregate directly must fail this regression: each result
// API owns the cleanup diagnostic while returning only the primary Begin cause.
func TestCreationStageGuardInitAndCleanupFailures(t *testing.T) {
	for _, operation := range []string{"acquire", "dispatch"} {
		t.Run(operation, func(t *testing.T) {
			b, raw := integrationBackend(t)
			input := acquisitionInput(t, "begin-cleanup")
			ctx := context.Background()
			var claim *CreationClaim
			var otherLease clientv3.LeaseID
			if operation == "dispatch" {
				_, err := b.AcquireIntent(ctx, input)
				require.NoError(t, err)
				claim, err = b.ClaimCreation(ctx, input.Workspace, input.IntentID, "worker", 30*time.Second)
				require.NoError(t, err)
				otherLease = clientv3.LeaseID(claim.Reference().LeaseID)
			} else {
				lease, err := raw.Grant(ctx, 30)
				require.NoError(t, err)
				otherLease = lease.ID
			}
			defer func() { _, err := raw.Revoke(ctx, otherLease); require.NoError(t, err) }()
			before, err := raw.Get(ctx, b.namespace.Root(), clientv3.WithPrefix())
			require.NoError(t, err)
			caller, cancel := context.WithCancel(ctx)
			defer cancel()
			var guard struct {
				StageReference
				LeaseID int64 `json:"lease_id"`
			}
			var guardKey string
			b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
				matched := len(ops) == 1 && ops[0].IsPut() && strings.Contains(string(ops[0].KeyBytes()), "/attempts/")
				if matched {
					guardKey = string(ops[0].KeyBytes())
					require.NoError(t, json.Unmarshal(ops[0].ValueBytes(), &guard))
				}
				return matched
			}, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
				require.NoError(t, e)
				require.True(t, r.Succeeded)
				cancel()
				return nil, context.DeadlineExceeded
			}}
			cleanupFailure := errors.New("failed original creation stage revoke")
			var revoked clientv3.LeaseID
			lease := &creationFaultLease{Lease: b.client.Lease, revoke: func(cleanup context.Context, id clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
				require.ErrorIs(t, caller.Err(), context.Canceled)
				require.NoError(t, cleanup.Err())
				deadline, bounded := cleanup.Deadline()
				require.True(t, bounded)
				require.Positive(t, time.Until(deadline))
				require.LessOrEqual(t, time.Until(deadline), b.requestTimeout)
				require.Equal(t, clientv3.LeaseID(guard.LeaseID), id)
				require.NotEqual(t, otherLease, id)
				revoked = id
				return nil, cleanupFailure
			}}
			b.client.Lease = lease
			defer func() {
				b.client.Lease = lease.Lease
				if revoked != 0 {
					_, err := raw.Revoke(ctx, revoked)
					require.NoError(t, err)
				}
			}()
			var primary, cleanup error
			var reference StageReference
			switch operation {
			case "acquire":
				result, err := b.AcquireIntent(caller, input)
				primary, cleanup = err, result.GuardCleanupError
				require.Equal(t, OutcomeUnknown, result.Outcome)
				require.Nil(t, result.Owner)
				require.Empty(t, result.Disposition)
				require.Equal(t, StageReference{}, result.Reference)
				require.Equal(t, CreationRequestRecord{}, result.Request)
			case "dispatch":
				result, err := b.DeclareRuntimeDispatch(caller, claim, dispatchInputForDeclaration())
				primary, cleanup, reference = err, result.GuardCleanupError, result.Reference
				require.Equal(t, OutcomeUnknown, result.Outcome)
				require.Nil(t, result.Entry)
				require.Empty(t, result.Disposition)
				require.NotEmpty(t, reference.Digest)
				require.Equal(t, guard.StageReference, reference)
			}
			require.ErrorIs(t, primary, ErrOutcomeUnknown)
			require.ErrorIs(t, primary, context.DeadlineExceeded)
			require.ErrorIs(t, cleanup, cleanupFailure)
			require.NotErrorIs(t, primary, cleanupFailure)
			require.NotContains(t, primary.Error(), cleanupFailure.Error())
			require.Equal(t, int64(1), lease.grants.Load())
			require.Equal(t, int64(1), lease.revokes.Load())
			require.NotZero(t, revoked)
			ttl, err := raw.TimeToLive(ctx, otherLease)
			require.NoError(t, err)
			require.Positive(t, ttl.TTL, "cleanup must preserve the unrelated original Lease")
			after, err := raw.Get(ctx, b.namespace.Root(), clientv3.WithPrefix())
			require.NoError(t, err)
			require.Len(t, after.Kvs, len(before.Kvs)+1, "only the REAL guard-init write may exist")
			for i, kv := range after.Kvs {
				if string(kv.Key) == guardKey {
					require.Equal(t, guard.LeaseID, kv.Lease)
					after.Kvs = append(after.Kvs[:i], after.Kvs[i+1:]...)
					break
				}
			}
			require.Equal(t, before.Kvs, after.Kvs, "failed Begin cannot change any business value or receipt")
			if operation == "dispatch" {
				fresh := freshDispatchBackend(t, b, raw)
				outcome, err := fresh.ResolveStage(ctx, reference)
				require.NoError(t, err)
				require.Equal(t, OutcomeAborted, outcome)
				entry, err := fresh.LoadRuntimeDispatch(ctx, input.Workspace, input.IntentID)
				require.NoError(t, err)
				require.Nil(t, entry)
			}
		})
	}
}
