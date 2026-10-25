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

	"github.com/goairix/sandbox/internal/storage/state"
)

// TestActiveControllerRenewScale measures one burst of real etcd controller
// renewals. It is opt-in and does not represent Pod, FUSE, or API capacity.
func TestActiveControllerRenewScale(t *testing.T) {
	rawCount := os.Getenv("TEST_ETCD_CONTROLLER_SCALE_N")
	if rawCount == "" {
		t.Skip("TEST_ETCD_CONTROLLER_SCALE_N is not set")
	}
	count, err := strconv.Atoi(rawCount)
	require.NoError(t, err)
	require.Greater(t, count, 0)
	require.LessOrEqual(t, count, 10000)

	store := testStore(t)
	repository, err := NewActiveSandboxRepository(store, "controller-renew-scale")
	require.NoError(t, err)
	ctx := context.Background()
	leases := make([]state.ActiveSandboxControllerLease, 0, count)
	var snapshotBytes int
	setupStart := time.Now()

	for number := range count {
		id := fmt.Sprintf("sandbox-controller-%06d", number)
		record := validActiveRecord(id)
		// The store treats this JSON as opaque. Use a nonempty workspace-shaped
		// snapshot so the renewal's authoritative record read pays for bytes.
		record.Snapshot, err = json.Marshal(map[string]any{
			"id": id, "runtime_id": record.RuntimeID, "runtime_uid": record.RuntimeUID,
			"config": map[string]any{
				"mode": "persistent", "timeout": 7200,
				"resources":      map[string]string{"cpu": "500m", "memory": "512Mi", "disk": "1Gi"},
				"network":        map[string]any{"enabled": true, "block_private": true},
				"workspace_path": "team/capacity/" + id, "workspace_mount_mode": "fuse",
			},
			"workspace": map[string]any{
				"root_path": "team/capacity/" + id, "mount_type": "fuse", "mount_state": "ready",
				"fuse_preparation_id": "preparation-" + id,
				"owner": map[string]any{
					"sandbox_id": id, "runtime_id": record.RuntimeID, "runtime_uid": record.RuntimeUID,
					"provider": "minio", "mount_type": "fuse", "generation": 1,
				},
			},
		})
		require.NoError(t, err)
		if number == 0 {
			snapshotBytes = len(record.Snapshot)
		}
		require.NoError(t, repository.Publish(ctx, record))
		_, err = repository.Activate(ctx, id, record.Revision, record.Snapshot)
		require.NoError(t, err)

		lease := state.ActiveSandboxControllerLease{
			SandboxID: id, Token: "controller-" + id, InstanceID: "api-a", PodUID: "api-pod-a",
			Generation: record.Generation,
		}
		// The five-minute lease keeps sequential setup from expiring before
		// the measured round. Production's five-second renewal interval is
		// not simulated by this burst experiment.
		acquired, ok, acquireErr := repository.AcquireController(ctx, lease, 5*time.Minute)
		require.NoError(t, acquireErr)
		require.True(t, ok)
		leases = append(leases, *acquired)
	}
	setupDuration := time.Since(setupStart)

	jobs := make(chan state.ActiveSandboxControllerLease)
	durations := make(chan time.Duration, count)
	errors := make(chan error, count)
	var workers sync.WaitGroup
	const workerCount = 16
	for range workerCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for lease := range jobs {
				started := time.Now()
				_, renewErr := repository.RenewController(ctx, lease, 5*time.Minute)
				durations <- time.Since(started)
				if renewErr != nil {
					errors <- fmt.Errorf("renew %s: %w", lease.SandboxID, renewErr)
				}
			}
		}()
	}
	renewStart := time.Now()
	for _, lease := range leases {
		jobs <- lease
	}
	close(jobs)
	workers.Wait()
	renewDuration := time.Since(renewStart)
	close(durations)
	close(errors)
	for renewErr := range errors {
		require.NoError(t, renewErr)
	}
	latencies := make([]time.Duration, 0, count)
	for duration := range durations {
		latencies = append(latencies, duration)
	}
	require.Len(t, latencies, count)
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	t.Logf("controllers=%d snapshot_bytes=%d setup=%s renew_round=%s renew_per_second=%.1f p50=%s p95=%s p99=%s",
		count, snapshotBytes, setupDuration, renewDuration, float64(count)/renewDuration.Seconds(),
		latencies[(len(latencies)-1)*50/100], latencies[(len(latencies)-1)*95/100],
		latencies[(len(latencies)-1)*99/100])
}
