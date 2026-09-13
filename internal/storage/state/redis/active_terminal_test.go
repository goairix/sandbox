package redis

import (
	"context"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestActiveAbsentDestroyRequiresReplicaAcknowledgement(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	s.durability, s.ackReplicas, s.ackTimeout = DurabilityReplicaAck, 1, time.Millisecond
	repo, err := NewActiveSandboxRepository(s, "active-absent-"+uuid.NewString())
	require.NoError(t, err)
	record, _, _, err := repo.BeginDestroy(context.Background(), "sandbox-absent")
	require.Nil(t, record)
	require.ErrorIs(t, err, state.ErrDurabilityUnconfirmed)
	require.ErrorIs(t, repo.ConfirmRecordAbsence(context.Background(), "sandbox-absent"), state.ErrDurabilityUnconfirmed)
	require.ErrorIs(t, repo.ConfirmRecordAbsence(context.Background(), ""), state.ErrActiveSandboxCorrupt)
}
