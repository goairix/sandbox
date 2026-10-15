package etcd

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTaskWorkspaceSyncRecoveryOwner(t *testing.T) {
	s := &taskArchiveStore{limits: taskArchiveLimits{Jobs: 1}, recovery: &taskArchiveRecoveryStore{}}
	s.self = s
	s.recovery.slots[0].diagnostic = taskArchiveRecoveryDiagnostic{Occupied: true, State: 3}
	r, err := s.registerRecovery(context.Background(), 0)
	require.NoError(t, err)
	require.Nil(t, r.index, "a registered waiter allocates no archive index or metadata buffers")
	_, err = s.registerRecovery(context.Background(), 0)
	require.ErrorIs(t, err, ErrConflict)
	require.NoError(t, r.begin(context.Background()))
	joined := make(chan error, 1)
	go func() { joined <- r.Close() }()
	select {
	case <-joined:
		t.Fatal("Close crossed a real operation registration")
	case <-time.After(20 * time.Millisecond):
	}
	r.end()
	require.NoError(t, <-joined)
	require.Nil(t, s.recovery.slots[0].owner)
	require.True(t, s.recovery.slots[0].diagnostic.Occupied)
}

func TestTaskWorkspaceSyncRecoveryRejectedProfiles(t *testing.T) {
	for _, b := range []*Backend{nil, {}, {archiveStore: &taskArchiveStore{}}} {
		r, err := b.RecoverTaskWorkspaceSyncLocal(context.Background(), nil, 0, nil, nil)
		require.Error(t, err)
		require.False(t, r.RegularFilesComplete)
		require.Zero(t, r.CompletedFiles)
	}
}
