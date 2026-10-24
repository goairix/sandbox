package etcd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// TestActiveResidentScale is opt-in because it writes N durable records to a
// dedicated test prefix. It measures the existing coordinator's full Scan
// cost; it is not a full sandbox, TTL, FUSE or multi-member capacity test.
func TestActiveResidentScale(t *testing.T) {
	rawCount := os.Getenv("TEST_ETCD_SCALE_N")
	if rawCount == "" {
		t.Skip("TEST_ETCD_SCALE_N is not set")
	}
	count, err := strconv.Atoi(rawCount)
	require.NoError(t, err)
	require.Greater(t, count, 0)
	require.LessOrEqual(t, count, 100000)

	store := testStore(t)
	repository, err := NewActiveSandboxRepository(store, "scale-cell")
	require.NoError(t, err)
	ctx := context.Background()
	endpoint := os.Getenv("TEST_ETCD_ENDPOINT")
	before, err := store.client.Status(ctx, endpoint)
	require.NoError(t, err)

	// A finite worker pool represents concurrent admissions without creating
	// one goroutine per resident sandbox or changing the repository's contract.
	jobs := make(chan int)
	var firstFailure error
	var failureMu sync.Mutex
	var workers sync.WaitGroup
	start := time.Now()
	for range 16 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for number := range jobs {
				id := fmt.Sprintf("sandbox-scale-%06d", number)
				record := validActiveRecord(id)
				if publishErr := repository.Publish(ctx, record); publishErr != nil {
					failureMu.Lock()
					if firstFailure == nil {
						firstFailure = fmt.Errorf("publish %s: %w", id, publishErr)
					}
					failureMu.Unlock()
					continue
				}
				if _, activateErr := repository.Activate(ctx, id, record.Revision, json.RawMessage(`{"ready":true}`)); activateErr != nil {
					failureMu.Lock()
					if firstFailure == nil {
						firstFailure = fmt.Errorf("activate %s: %w", id, activateErr)
					}
					failureMu.Unlock()
				}
			}
		}()
	}
	for number := range count {
		jobs <- number
	}
	close(jobs)
	workers.Wait()
	require.NoError(t, firstFailure)
	writeDuration := time.Since(start)

	scanStart := time.Now()
	var cursor uint64
	var scanned int
	for {
		page, scanErr := repository.Scan(ctx, cursor, 128)
		require.NoError(t, scanErr)
		scanned += len(page.Records)
		if page.Cursor == 0 {
			break
		}
		cursor = page.Cursor
	}
	scanDuration := time.Since(scanStart)
	require.Equal(t, count, scanned)

	reads := make([]time.Duration, 0, 100)
	for sample := range 100 {
		id := fmt.Sprintf("sandbox-scale-%06d", sample*count/100)
		readStart := time.Now()
		record, readErr := repository.Load(ctx, id)
		require.NoError(t, readErr)
		require.NotNil(t, record)
		reads = append(reads, time.Since(readStart))
	}
	sort.Slice(reads, func(i, j int) bool { return reads[i] < reads[j] })
	after, err := store.client.Status(ctx, endpoint)
	require.NoError(t, err)
	keys, err := store.client.Get(ctx, store.prefix, clientv3.WithPrefix(), clientv3.WithCountOnly())
	require.NoError(t, err)
	t.Logf("residents=%d keys=%d writes=%s scan=%s point_p95=%s db_delta_bytes=%d db_in_use_delta_bytes=%d",
		count, keys.Count, writeDuration, scanDuration, reads[94], after.DbSize-before.DbSize, after.DbSizeInUse-before.DbSizeInUse)
}
