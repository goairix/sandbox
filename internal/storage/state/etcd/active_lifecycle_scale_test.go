package etcd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// TestActiveResidentIndexScale is an opt-in storage measurement, not a sandbox
// capacity gate. It writes ordinary records with TTL and a representative
// nonempty configuration, then measures idle and concentrated-due discovery.
func TestActiveResidentIndexScale(t *testing.T) {
	rawCount := os.Getenv("TEST_ETCD_INDEX_SCALE_N")
	if rawCount == "" {
		t.Skip("TEST_ETCD_INDEX_SCALE_N is not set")
	}
	count, err := strconv.Atoi(rawCount)
	require.NoError(t, err)
	require.Greater(t, count, 0)
	require.LessOrEqual(t, count, 100000)

	store := testStore(t)
	repository, err := NewActiveSandboxRepository(store, "resident-index-scale")
	require.NoError(t, err)
	ctx := context.Background()
	before, err := store.client.Status(ctx, os.Getenv("TEST_ETCD_ENDPOINT"))
	require.NoError(t, err)

	var workers sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	jobs := make(chan int)
	writeStart := time.Now()
	for range 16 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for number := range jobs {
				id := fmt.Sprintf("sandbox-resident-%06d", number)
				record := validActiveRecord(id)
				snapshot, writeErr := json.Marshal(struct {
					CreatedAt  time.Time     `json:"created_at"`
					Timeout    time.Duration `json:"timeout"`
					Config     any           `json:"config"`
					RuntimeID  string        `json:"runtime_id"`
					RuntimeUID string        `json:"runtime_uid"`
				}{
					CreatedAt: record.CreatedAt,
					Timeout:   2 * time.Hour,
					Config: map[string]any{
						"mode": "persistent", "image": "sandbox-runtime:benchmark",
						"resources": map[string]any{"cpu": 1, "memory_mb": 512},
						"network":   map[string]any{"enabled": true, "allow_internet": false},
					},
					RuntimeID: record.RuntimeID, RuntimeUID: record.RuntimeUID,
				})
				record.Snapshot = snapshot
				if writeErr == nil {
					writeErr = repository.Publish(ctx, record)
				}
				if writeErr == nil {
					_, writeErr = repository.Activate(ctx, id, record.Revision, record.Snapshot)
				}
				if writeErr != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("record %s: %w", id, writeErr)
					}
					errMu.Unlock()
				}
			}
		}()
	}
	for number := range count {
		jobs <- number
	}
	close(jobs)
	workers.Wait()
	require.NoError(t, firstErr)
	writeDuration := time.Since(writeStart)

	idleStart := time.Now()
	idle, err := repository.ScanDue(ctx, time.Now(), "", 128)
	require.NoError(t, err)
	require.Empty(t, idle.Records)
	idleDuration := time.Since(idleStart)

	dueStart := time.Now()
	var dueCount int
	var dueCursor string
	for {
		page, scanErr := repository.ScanDue(ctx, time.Now().Add(3*time.Hour), dueCursor, 128)
		require.NoError(t, scanErr)
		dueCount += len(page.Records)
		if page.Cursor == "" {
			break
		}
		dueCursor = page.Cursor
	}
	require.Equal(t, count, dueCount)
	dueDuration := time.Since(dueStart)

	// The indexed coordinator still performs one complete recovery inventory
	// before accepting traffic after process startup.
	startupStart := time.Now()
	var startupCount int
	var startupCursor uint64
	for {
		page, scanErr := repository.Scan(ctx, startupCursor, 128)
		require.NoError(t, scanErr)
		startupCount += len(page.Records)
		if page.Cursor == 0 {
			break
		}
		startupCursor = page.Cursor
	}
	require.Equal(t, count, startupCount)
	startupDuration := time.Since(startupStart)

	after, err := store.client.Status(ctx, os.Getenv("TEST_ETCD_ENDPOINT"))
	require.NoError(t, err)
	keys, err := store.client.Get(ctx, store.prefix, clientv3.WithPrefix(), clientv3.WithCountOnly())
	require.NoError(t, err)
	t.Logf("residents=%d keys=%d write=%s idle_due_scan=%s concentrated_due_scan=%s startup_full_scan=%s db_delta_bytes=%d",
		count, keys.Count, writeDuration, idleDuration, dueDuration, startupDuration, after.DbSize-before.DbSize)
}
