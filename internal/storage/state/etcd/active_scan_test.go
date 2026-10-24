package etcd

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage/state"
)

func TestScanPagesActiveRecordsWithoutDuplicates(t *testing.T) {
	s := testStore(t)
	repository, err := NewActiveSandboxRepository(s, "cell-a")
	require.NoError(t, err)
	ctx := context.Background()
	ids := []string{"sandbox-scan-a", "sandbox-scan-b", "sandbox-scan-c"}
	for _, id := range ids {
		require.NoError(t, repository.Publish(ctx, validActiveRecord(id)))
	}
	seen := make(map[string]bool)
	cursor := uint64(0)
	for {
		page, err := repository.Scan(ctx, cursor, 2)
		require.NoError(t, err)
		require.LessOrEqual(t, len(page.Records), 2)
		for _, record := range page.Records {
			require.False(t, seen[record.SandboxID])
			seen[record.SandboxID] = true
		}
		if page.Cursor == 0 {
			break
		}
		require.NotEqual(t, cursor, page.Cursor)
		cursor = page.Cursor
	}
	require.Len(t, seen, len(ids))
	for _, id := range ids {
		require.True(t, seen[id])
	}
	_, err = repository.Scan(ctx, 0, 0)
	require.ErrorIs(t, err, state.ErrActiveSandboxCorrupt)
	require.NoError(t, repository.Ping(ctx))
}

func TestScanReadsMoreThanOneRecordBatch(t *testing.T) {
	s := testStore(t)
	repository, err := NewActiveSandboxRepository(s, "cell-a")
	require.NoError(t, err)
	ctx := context.Background()
	for i := range activeScanBatch + 1 {
		require.NoError(t, repository.Publish(ctx, validActiveRecord(fmt.Sprintf("sandbox-batch-%03d", i))))
	}
	page, err := repository.Scan(ctx, 0, activeScanBatch+1)
	require.NoError(t, err)
	require.Len(t, page.Records, activeScanBatch+1)
	require.Zero(t, page.Cursor)
}
