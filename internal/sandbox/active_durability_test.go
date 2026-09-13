package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/stretchr/testify/require"
)

type committedButUnconfirmedRepository struct {
	*memoryActiveRepository
	failStage string
	failure   error
}

func (r *committedButUnconfirmedRepository) ConfirmRecordAbsence(context.Context, string) error {
	return state.ErrDurabilityUnconfirmed
}

func TestActiveCleanupAbsentRecordRequiresDurabilityConfirmation(t *testing.T) {
	managers, _, _ := distributedSyncManagers(t)
	m := managers[0]
	m.activeSandboxes = &committedButUnconfirmedRepository{memoryActiveRepository: newMemoryActiveRepository()}
	require.ErrorIs(t, m.completeActiveSandboxCleanup(context.Background(), "sandbox-absent", nil), state.ErrDurabilityUnconfirmed)
}

func (r *committedButUnconfirmedRepository) Publish(ctx context.Context, record state.ActiveSandboxRecord) error {
	if err := r.memoryActiveRepository.Publish(ctx, record); err != nil {
		return err
	}
	if r.failStage == "publish" {
		return r.failure
	}
	return nil
}

func (r *committedButUnconfirmedRepository) Activate(ctx context.Context, id string, revision uint64, snapshot json.RawMessage) (*state.ActiveSandboxRecord, error) {
	record, err := r.memoryActiveRepository.Activate(ctx, id, revision, snapshot)
	if err == nil && r.failStage == "activate" {
		return record, r.failure
	}
	return record, err
}

func (r *committedButUnconfirmedRepository) UpdateOperation(ctx context.Context, op state.ActiveSandboxOperation, revision uint64, snapshot json.RawMessage) (*state.ActiveSandboxRecord, error) {
	record, err := r.memoryActiveRepository.UpdateOperation(ctx, op, revision, snapshot)
	if err == nil && r.failStage == "update" {
		if !errors.Is(r.failure, state.ErrDurabilityUnconfirmed) {
			r.failStage = ""
		}
		return record, r.failure
	}
	return record, err
}

func TestActivePublicationReadbackDoesNotProveDurabilityAcknowledgement(t *testing.T) {
	for _, stage := range []string{"publish", "activate"} {
		t.Run(stage, func(t *testing.T) {
			managers, _, _ := distributedSyncManagers(t)
			m := managers[0]
			m.activeSandboxes = &committedButUnconfirmedRepository{newMemoryActiveRepository(), stage, state.ErrDurabilityUnconfirmed}
			sb := &Sandbox{ID: "sandbox-durability", RuntimeID: "runtime-a", RuntimeUID: "uid-a", State: StateReady, CreatedAt: time.Now(), Config: SandboxConfig{Mode: ModePersistent}}
			require.ErrorIs(t, m.publishActiveSandbox(context.Background(), sb), state.ErrDurabilityUnconfirmed)
		})
	}
}

func TestWorkspaceUpdateReadbackDistinguishesReplyLossFromDurabilityFailure(t *testing.T) {
	for _, failure := range []error{state.ErrDurabilityUnconfirmed, errors.New("reply lost")} {
		t.Run(failure.Error(), func(t *testing.T) {
			managers, _, _ := distributedSyncManagers(t)
			m := managers[0]
			ctx := context.Background()
			sb, err := m.Create(ctx, SandboxConfig{Mode: ModePersistent})
			require.NoError(t, err)
			m.activeSandboxes = &committedButUnconfirmedRepository{m.activeSandboxes.(*memoryActiveRepository), "update", failure}
			snapshot, opCtx, release, err := m.beginDistributedWorkspaceOperation(ctx, sb.ID)
			require.NoError(t, err)
			defer release()
			snapshot.Config.Network.Enabled = true
			err = m.persistActiveSandboxUpdate(opCtx, snapshot)
			if errors.Is(failure, state.ErrDurabilityUnconfirmed) {
				require.ErrorIs(t, err, failure)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
