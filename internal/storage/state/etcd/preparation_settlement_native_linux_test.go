//go:build linux && (amd64 || arm64)

package etcd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// These are actual TLS etcd transactions and test-issued protocol signatures.
// Kernel provenance belongs to TestColdCreateServiceNative's formal launcher.
func TestColdPreparationSettlementNative(t *testing.T) {
	for _, fault := range []string{"commit-ACK", "guard-ACK"} {
		t.Run(fault, func(t *testing.T) {
			f := newEnrolledNativePublicationFixture(t, false, true)
			ctx, b, c := context.Background(), f.b, f.claim
			published, err := b.PublishRuntimeEnrolled(ctx, c, f.proof, f.enrollment)
			require.NoError(t, err)
			require.Equal(t, OutcomeCommitted, published.Outcome)
			original, err := b.LoadColdPreparation(ctx, c.workspace, c.reference.IntentID)
			require.NoError(t, err)
			key, _, _ := b.namespace.coldPreparationKeys(c.reference.Partition, c.reference.IntentID)
			trace := &coldTraceKV{KV: b.client.KV}
			matches := 0
			b.client.KV = &faultKV{KV: trace, match: func(ops []clientv3.Op) bool {
				for _, op := range ops {
					k := string(op.KeyBytes())
					if !op.IsPut() {
						continue
					}
					var guard struct {
						StageReference
						LeaseID int64 `json:"lease_id"`
					}
					isGuard := strings.Contains(k, "/attempts/") && strings.HasSuffix(k, "/guard") && json.Unmarshal(op.ValueBytes(), &guard) == nil && guard.StageID == "cold_preparation_settle" && guard.Namespace == b.namespace.Root() && guard.LeaseID > 0
					if fault == "commit-ACK" && k == key || fault == "guard-ACK" && isGuard {
						return true
					}
				}
				return false
			}, after: func(response *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				require.NoError(t, err)
				require.True(t, response.Succeeded)
				matches++
				return nil, context.DeadlineExceeded
			}}
			unknown, err := b.SettleColdPreparation(ctx, c.workspace, c.reference.IntentID)
			require.ErrorIs(t, err, ErrOutcomeUnknown)
			require.Equal(t, OutcomeUnknown, unknown.Outcome)
			require.NotEqual(t, StageReference{}, unknown.Reference, "exact attempt survives Grant/guard/commit unknown")
			require.Equal(t, 1, matches)
			other, err := New(ctx, f.options)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, other.Close()) })
			otherTrace := &coldTraceKV{KV: other.client.KV}
			other.client.KV = otherTrace
			outcome, err := other.ResolveStage(ctx, unknown.Reference)
			require.NoError(t, err)
			if fault == "guard-ACK" {
				require.Equal(t, OutcomeAborted, outcome)
				held, err := other.LoadColdPreparation(ctx, c.workspace, c.reference.IntentID)
				require.NoError(t, err)
				require.Equal(t, original, held, "unknown admission did not return either slot")
			} else {
				require.Equal(t, OutcomeCommitted, outcome)
			}
			settled, err := other.SettleColdPreparation(ctx, c.workspace, c.reference.IntentID)
			require.NoError(t, err)
			require.Equal(t, OutcomeCommitted, settled.Outcome)
			require.Equal(t, int32(1), trace.nodes[39].Load()+otherTrace.nodes[39].Load(), "one real 30+5+4 settlement transaction")
			if fault == "commit-ACK" {
				require.Equal(t, unknown.Reference, settled.Reference)
			}
			// Reuse the returned prep slot under another quota partition and owner.
			w, err := NewWorkspaceIdentity("s3", f.options.Identity.StorageID, "bucket", uuid.NewString()+"/")
			require.NoError(t, err)
			input := AcquireIntentInput{Workspace: w, Principal: "cold-reuse", IdempotencyKey: uuid.NewString(), RequestID: uuid.NewString(), IntentID: uuid.NewString(), SandboxID: uuid.NewString(), SnapshotVersion: uuid.NewString(), Payload: json.RawMessage(`{"reuse":true}`), Now: time.Now().UTC(), TTL: time.Hour}
			acquired, err := other.AcquireIntent(ctx, input)
			require.NoError(t, err)
			require.Equal(t, OutcomeCommitted, acquired.Outcome)
			next, err := other.ClaimCreation(ctx, w, input.IntentID, "next", 30*time.Second)
			require.NoError(t, err)
			t.Cleanup(func() { _ = other.ReleaseCreationClaim(context.Background(), next) })
			reserved, err := other.ReserveColdPreparation(ctx, next, 8)
			require.NoError(t, err)
			require.Equal(t, original.Preparation.ID, reserved.Record.Preparation.ID)
			require.Equal(t, original.Preparation.Epoch+1, reserved.Record.Preparation.Epoch)
			replayed, err := other.SettleColdPreparation(ctx, c.workspace, c.reference.IntentID)
			require.NoError(t, err)
			require.Equal(t, settled.Reference, replayed.Reference)
			current, err := other.LoadColdPreparation(ctx, w, input.IntentID)
			require.NoError(t, err)
			require.Equal(t, reserved.Record, current)
			coldNativeQuotaHeld(t, other, f.raw, *original)
		})
	}
	t.Run("missing-index-expired-certificates-history-only", func(t *testing.T) {
		f := newEnrolledNativePublicationFixture(t, false, true)
		ctx, b, c := context.Background(), f.b, f.claim
		published, err := b.PublishRuntimeEnrolled(ctx, c, f.proof, f.enrollment)
		require.NoError(t, err)
		require.Equal(t, OutcomeCommitted, published.Outcome)
		original, err := b.LoadColdPreparation(ctx, c.workspace, c.reference.IntentID)
		require.NoError(t, err)
		index, err := b.namespace.runtimeIndexKey(published.Entry.Record.Runtime.UID)
		require.NoError(t, err)
		deleted, err := f.raw.Delete(ctx, index)
		require.NoError(t, err)
		require.Equal(t, int64(1), deleted.Deleted)
		// This is controlled index absence, not a death or safe-release proof.
		b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
			return ClockObservation{UTC: time.Now().UTC().Add(24 * time.Hour)}, nil
		})
		_, err = b.LoadRuntimePublicationEnrolled(ctx, c.workspace, c.reference.IntentID)
		require.Error(t, err, "public current index loader remains strict")
		settled, err := b.SettleColdPreparation(ctx, c.workspace, c.reference.IntentID)
		require.NoError(t, err)
		require.Equal(t, OutcomeCommitted, settled.Outcome)
		require.Equal(t, published.Entry, settled.Entry)
		coldNativeQuotaHeld(t, b, f.raw, *original)
	})
	t.Run("tampered-retained-publication-rejected", func(t *testing.T) {
		f := newEnrolledNativePublicationFixture(t, false, true)
		ctx, b, c := context.Background(), f.b, f.claim
		published, err := b.PublishRuntimeEnrolled(ctx, c, f.proof, f.enrollment)
		require.NoError(t, err)
		require.Equal(t, OutcomeCommitted, published.Outcome)
		original, err := b.LoadColdPreparation(ctx, c.workspace, c.reference.IntentID)
		require.NoError(t, err)
		journal, _, _ := b.namespace.runtimePublicationKeys(c.reference.Partition, c.reference.IntentID)
		tampered := published.Entry.Record
		tampered.NodeUID = uuid.NewString()
		wire, err := json.Marshal(tampered)
		require.NoError(t, err)
		_, err = f.raw.Put(ctx, journal, string(wire))
		require.NoError(t, err)
		_, err = b.SettleColdPreparation(ctx, c.workspace, c.reference.IntentID)
		require.ErrorIs(t, err, ErrCorruptRecord)
		held, err := b.LoadColdPreparation(ctx, c.workspace, c.reference.IntentID)
		require.NoError(t, err)
		require.Equal(t, original, held)
		coldNativeQuotaHeld(t, b, f.raw, *original)
	})
}

func coldNativeQuotaHeld(t *testing.T, b *Backend, raw *clientv3.Client, reservation ColdPreparationRecord) {
	t.Helper()
	key, free, err := b.namespace.coldSlotKeys(reservation.Quota)
	require.NoError(t, err)
	response, err := raw.Get(context.Background(), key)
	require.NoError(t, err)
	require.Len(t, response.Kvs, 1)
	var slot coldSlotRecord
	require.NoError(t, coldDecode(response.Kvs[0], &slot))
	require.True(t, slot.Occupied)
	require.Equal(t, reservation.Owner, slot.Owner)
	require.Equal(t, reservation.Quota, slot.Slot)
	response, err = raw.Get(context.Background(), free)
	require.NoError(t, err)
	require.Empty(t, response.Kvs)
}
