package etcd

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/stretchr/testify/require"
)

func activeDueSnapshot(t *testing.T, createdAt time.Time, timeout time.Duration, workspace bool) json.RawMessage {
	t.Helper()
	payload := struct {
		CreatedAt time.Time     `json:"created_at"`
		Timeout   time.Duration `json:"timeout"`
		Workspace any           `json:"workspace,omitempty"`
	}{CreatedAt: createdAt, Timeout: timeout}
	if workspace {
		payload.Workspace = map[string]string{"mount_type": "sync"}
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return raw
}

func TestActiveDueIndexTracksOrdinaryLifecycle(t *testing.T) {
	store := testStore(t)
	repo, err := NewActiveSandboxRepository(store, "due-cell")
	require.NoError(t, err)
	ctx := context.Background()
	now := time.Now().UTC()
	record := validActiveRecord("sandbox-due")
	record.Snapshot = activeDueSnapshot(t, now, time.Hour, false)
	require.NoError(t, repo.Publish(ctx, record))

	page, err := repo.ScanDue(ctx, now.Add(2*time.Second), "", 10)
	require.NoError(t, err)
	require.Len(t, page.Records, 1, "a stranded publication must be discoverable")
	require.Equal(t, record.SandboxID, page.Records[0].SandboxID)

	active, err := repo.Activate(ctx, record.SandboxID, record.Revision, record.Snapshot)
	require.NoError(t, err)
	page, err = repo.ScanDue(ctx, now.Add(2*time.Second), "", 10)
	require.NoError(t, err)
	require.Empty(t, page.Records, "far-future ordinary records must not be scanned each tick")
	page, err = repo.ScanDue(ctx, now.Add(2*time.Hour), "", 10)
	require.NoError(t, err)
	require.Len(t, page.Records, 1)

	shortTTL := activeDueSnapshot(t, now, time.Second, false)
	active, err = repo.Update(ctx, record.SandboxID, active.Revision, shortTTL)
	require.NoError(t, err)
	page, err = repo.ScanDue(ctx, now.Add(2*time.Second), "", 10)
	require.NoError(t, err)
	require.Len(t, page.Records, 1, "a shortened TTL must be indexed with the record update")
	require.Equal(t, active.Revision, page.Records[0].Revision)

	destroying, _, won, err := repo.BeginDestroy(ctx, record.SandboxID)
	require.NoError(t, err)
	require.True(t, won)
	require.Equal(t, state.ActiveSandboxDestroying, destroying.Phase)
	page, err = repo.ScanDue(ctx, time.Now().Add(2*time.Second), "", 10)
	require.NoError(t, err)
	require.Len(t, page.Records, 1, "durable destruction must remain discoverable")

	require.NoError(t, repo.Delete(ctx, record.SandboxID, destroying.Revision, destroying.Generation))
	page, err = repo.ScanDue(ctx, now.Add(2*time.Hour), "", 10)
	require.NoError(t, err)
	require.Empty(t, page.Records)
}

func TestActiveWorkspaceIndexTracksSnapshotTransition(t *testing.T) {
	store := testStore(t)
	repo, err := NewActiveSandboxRepository(store, "workspace-cell")
	require.NoError(t, err)
	ctx := context.Background()
	now := time.Now().UTC()
	record := validActiveRecord("sandbox-workspace-index")
	record.Snapshot = activeDueSnapshot(t, now, time.Hour, true)
	require.NoError(t, repo.Publish(ctx, record))
	active, err := repo.Activate(ctx, record.SandboxID, record.Revision, record.Snapshot)
	require.NoError(t, err)

	page, err := repo.ScanWorkspace(ctx, 0, 10)
	require.NoError(t, err)
	require.Len(t, page.Records, 1)
	due, err := repo.ScanDue(ctx, now.Add(2*time.Hour), "", 10)
	require.NoError(t, err)
	require.Empty(t, due.Records, "workspace recovery belongs to its separate index")

	_, err = repo.Update(ctx, record.SandboxID, active.Revision, activeDueSnapshot(t, now, time.Hour, false))
	require.NoError(t, err)
	page, err = repo.ScanWorkspace(ctx, 0, 10)
	require.NoError(t, err)
	require.Empty(t, page.Records)
	due, err = repo.ScanDue(ctx, now.Add(2*time.Hour), "", 10)
	require.NoError(t, err)
	require.Len(t, due.Records, 1)
}

func TestActiveDueIndexTracksExclusiveMutationAndCheckpoint(t *testing.T) {
	store := testStore(t)
	repo, err := NewActiveSandboxRepository(store, "exclusive-index-cell")
	require.NoError(t, err)
	ctx := context.Background()
	now := time.Now().UTC()
	record := validActiveRecord("sandbox-exclusive-index")
	record.Snapshot = activeDueSnapshot(t, now, time.Hour, false)
	require.NoError(t, repo.Publish(ctx, record))
	active, err := repo.Activate(ctx, record.SandboxID, record.Revision, record.Snapshot)
	require.NoError(t, err)

	exclusive, operation, err := repo.BeginOperation(ctx, record.SandboxID, "exclusive-index", state.ActiveOperationExclusive, 10*time.Second)
	require.NoError(t, err)
	page, err := repo.ScanDue(ctx, time.Now().Add(2*time.Second), "", 10)
	require.NoError(t, err)
	require.Len(t, page.Records, 1)
	require.Equal(t, exclusive.Revision, page.Records[0].Revision)

	// A mutation can attach a workspace while the record is exclusive. Its
	// discovery key must move with the exact snapshot write, not at EndOperation.
	workspace := activeDueSnapshot(t, now, time.Hour, true)
	updated, err := repo.UpdateOperation(ctx, *operation, exclusive.Revision, workspace)
	require.NoError(t, err)
	page, err = repo.ScanDue(ctx, time.Now().Add(2*time.Second), "", 10)
	require.NoError(t, err)
	require.Empty(t, page.Records)
	workspaces, err := repo.ScanWorkspace(ctx, 0, 10)
	require.NoError(t, err)
	require.Len(t, workspaces.Records, 1)
	require.Equal(t, updated.Revision, workspaces.Records[0].Revision)

	require.NoError(t, repo.EndOperation(ctx, *operation))
	current, err := repo.Load(ctx, record.SandboxID)
	require.NoError(t, err)
	require.Equal(t, state.ActiveSandboxActive, current.Phase)
	require.Equal(t, active.Revision+3, current.Revision)

	destroying, _, won, err := repo.BeginDestroy(ctx, record.SandboxID)
	require.NoError(t, err)
	require.True(t, won)
	checkpointed, err := repo.Checkpoint(ctx, record.SandboxID, destroying.Revision, "runtime_stopped")
	require.NoError(t, err)
	workspaces, err = repo.ScanWorkspace(ctx, 0, 10)
	require.NoError(t, err)
	require.Len(t, workspaces.Records, 1)
	require.Equal(t, checkpointed.Revision, workspaces.Records[0].Revision)
	require.NoError(t, repo.Delete(ctx, record.SandboxID, checkpointed.Revision, checkpointed.Generation))
	workspaces, err = repo.ScanWorkspace(ctx, 0, 10)
	require.NoError(t, err)
	require.Empty(t, workspaces.Records)
}

func TestActiveDueIndexPagesPastEarlierCandidate(t *testing.T) {
	store := testStore(t)
	repo, err := NewActiveSandboxRepository(store, "due-pages-cell")
	require.NoError(t, err)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, id := range []string{"sandbox-page-a", "sandbox-page-b", "sandbox-page-c"} {
		record := validActiveRecord(id)
		record.Snapshot = activeDueSnapshot(t, now, time.Hour, false)
		require.NoError(t, repo.Publish(ctx, record))
	}

	var ids []string
	cursor := ""
	for {
		page, err := repo.ScanDue(ctx, time.Now().Add(2*time.Second), cursor, 1)
		require.NoError(t, err)
		for _, record := range page.Records {
			ids = append(ids, record.SandboxID)
		}
		if page.Cursor == "" {
			break
		}
		cursor = page.Cursor
	}
	require.ElementsMatch(t, []string{"sandbox-page-a", "sandbox-page-b", "sandbox-page-c"}, ids)
}

func TestActiveDueIndexResolvesLostReplyAndRejectsStaleRevision(t *testing.T) {
	store := testStore(t)
	repo, err := NewActiveSandboxRepository(store, "due-ack-cell")
	require.NoError(t, err)
	ctx := context.Background()
	now := time.Now().UTC()
	record := validActiveRecord("sandbox-due-ack")
	record.Snapshot = activeDueSnapshot(t, now, time.Hour, false)
	require.NoError(t, repo.Publish(ctx, record))

	realKV := store.client.KV
	fault := &lostReplyKV{KV: realKV}
	fault.drop.Store(true)
	store.client.KV = fault
	_, err = repo.Activate(ctx, record.SandboxID, record.Revision, record.Snapshot)
	store.client.KV = realKV
	require.ErrorContains(t, err, "reply lost after commit")

	current, err := repo.Load(ctx, record.SandboxID)
	require.NoError(t, err)
	require.Equal(t, state.ActiveSandboxActive, current.Phase)
	near, err := repo.ScanDue(ctx, now.Add(2*time.Second), "", 10)
	require.NoError(t, err)
	require.Empty(t, near.Records)
	future, err := repo.ScanDue(ctx, now.Add(2*time.Hour), "", 10)
	require.NoError(t, err)
	require.Len(t, future.Records, 1)
	require.Equal(t, current.Revision, future.Records[0].Revision)

	_, err = repo.Update(ctx, record.SandboxID, record.Revision, activeDueSnapshot(t, now, time.Second, false))
	require.ErrorIs(t, err, state.ErrActiveSandboxConflict)
	near, err = repo.ScanDue(ctx, now.Add(2*time.Second), "", 10)
	require.NoError(t, err)
	require.Empty(t, near.Records)
}
