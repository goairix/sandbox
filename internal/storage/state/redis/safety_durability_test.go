package redis

import (
	"context"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestAtomicStoreMutationsAndNoopsRejectMissingReplicaAck(t *testing.T) {
	skipIfNoRedis(t)
	for _, tc := range []struct {
		name    string
		initial *string
		run     func(*Store, string) error
	}{
		{"set", nil, func(s *Store, k string) error { return s.Set(context.Background(), k, []byte("next"), 0) }},
		{"setnx", nil, func(s *Store, k string) error {
			_, err := s.SetNX(context.Background(), k, []byte("next"), 0)
			return err
		}},
		{"setnx conflict", ptrSafetyString("existing"), func(s *Store, k string) error {
			_, err := s.SetNX(context.Background(), k, []byte("next"), 0)
			return err
		}},
		{"delete", ptrSafetyString("existing"), func(s *Store, k string) error { return s.Delete(context.Background(), k) }},
		{"delete absent", nil, func(s *Store, k string) error { return s.Delete(context.Background(), k) }},
		{"increment", nil, func(s *Store, k string) error {
			_, err := s.Increment(context.Background(), k)
			return err
		}},
		{"cas", ptrSafetyString("existing"), func(s *Store, k string) error {
			_, err := s.CompareAndSwap(context.Background(), k, []byte("existing"), []byte("next"), 0)
			return err
		}},
		{"cas noop", nil, func(s *Store, k string) error {
			_, err := s.CompareAndSwap(context.Background(), k, nil, nil, 0)
			return err
		}},
		{"compare delete", ptrSafetyString("existing"), func(s *Store, k string) error {
			_, err := s.CompareAndDelete(context.Background(), k, []byte("existing"))
			return err
		}},
		{"compare delete noop", nil, func(s *Store, k string) error { _, err := s.CompareAndDelete(context.Background(), k, nil); return err }},
		{"confirm absent", nil, func(s *Store, k string) error {
			_, err := s.ConfirmAbsence(context.Background(), k)
			return err
		}},
		{"confirm empty value", ptrSafetyString(""), func(s *Store, k string) error {
			_, err := s.ConfirmAbsence(context.Background(), k)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			key := "safety-test:{" + uuid.NewString() + "}:state"
			t.Cleanup(func() { _ = s.client.Del(ctx, key).Err() })
			if tc.initial != nil {
				require.NoError(t, s.client.Set(ctx, key, *tc.initial, 0).Err())
			}
			s.durability, s.ackReplicas, s.ackTimeout = DurabilityReplicaAck, 1, time.Millisecond
			require.ErrorIs(t, tc.run(s, key), state.ErrDurabilityUnconfirmed)
			if tc.name == "confirm empty value" {
				value, err := s.client.Get(ctx, key).Result()
				require.NoError(t, err)
				require.Empty(t, value, "absence proof must not delete an empty/corrupt state value")
			}
		})
	}
}

func ptrSafetyString(value string) *string { return &value }

func TestSafetyScriptPreservesResultWhenReplicaAckFails(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	s.durability, s.ackReplicas, s.ackTimeout = DurabilityReplicaAck, 1, time.Millisecond
	key := "safety-test:{" + uuid.NewString() + "}:record"
	cmd, err := s.runSafetyScript(context.Background(), confirmAbsenceScript, []string{key})
	require.ErrorIs(t, err, state.ErrDurabilityUnconfirmed)
	result, commandErr := cmd.Int64()
	require.NoError(t, commandErr)
	require.EqualValues(t, 1, result, "a durability failure must not discard Lua's ambiguous result")
}

func TestActiveSandboxAckFailureRetainsOperationAndControllerCapabilities(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	ctx := context.Background()
	repo, err := NewActiveSandboxRepository(s, "safety-capabilities-"+uuid.NewString())
	require.NoError(t, err)
	initial := activeRecordForTest("sandbox-safety-capabilities")
	require.NoError(t, repo.Publish(ctx, initial))
	active, err := repo.Activate(ctx, initial.SandboxID, initial.Revision, initial.Snapshot)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.forceDelete(ctx, initial.SandboxID) })
	s.durability, s.ackReplicas, s.ackTimeout = DurabilityReplicaAck, 1, time.Millisecond
	record, operation, err := repo.BeginOperation(ctx, initial.SandboxID, "operation-a", state.ActiveOperationMutation, time.Second)
	require.ErrorIs(t, err, state.ErrDurabilityUnconfirmed)
	require.NotNil(t, record)
	require.NotNil(t, operation, "cleanup needs the ambiguous operation capability")
	updated, err := repo.UpdateOperation(ctx, *operation, record.Revision, active.Snapshot)
	require.ErrorIs(t, err, state.ErrDurabilityUnconfirmed)
	require.NotNil(t, updated)
	renewed, err := repo.RenewOperation(ctx, *operation, time.Second)
	require.ErrorIs(t, err, state.ErrDurabilityUnconfirmed)
	require.NotNil(t, renewed)
	require.ErrorIs(t, repo.EndOperation(ctx, *renewed), state.ErrDurabilityUnconfirmed)
	lease := state.ActiveSandboxControllerLease{SandboxID: initial.SandboxID, Token: "controller-a", InstanceID: "api-a", Generation: updated.Generation}
	controller, won, err := repo.AcquireController(ctx, lease, time.Second)
	require.ErrorIs(t, err, state.ErrDurabilityUnconfirmed)
	require.True(t, won)
	require.NotNil(t, controller, "cleanup needs the ambiguous controller capability")
	controller, err = repo.RenewController(ctx, *controller, time.Second)
	require.ErrorIs(t, err, state.ErrDurabilityUnconfirmed)
	require.NotNil(t, controller)
	destroying, _, _, err := repo.BeginDestroy(ctx, initial.SandboxID)
	require.ErrorIs(t, err, state.ErrDurabilityUnconfirmed)
	require.NotNil(t, destroying)
	_, _, retryWon, err := repo.BeginDestroy(ctx, initial.SandboxID)
	require.False(t, retryWon)
	require.ErrorIs(t, err, state.ErrDurabilityUnconfirmed, "an idempotent no-op must not discard its failed barrier ACK")
	checkpoint, err := repo.CheckpointController(ctx, *controller, destroying.Revision, "removed")
	require.ErrorIs(t, err, state.ErrDurabilityUnconfirmed)
	require.NotNil(t, checkpoint)
	require.ErrorIs(t, repo.DeleteController(ctx, *controller, checkpoint.Revision), state.ErrDurabilityUnconfirmed)
}
