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

// TestActiveControllerRenewScale measures real etcd controller renewals. The
// optional repeated rounds share the same controllers and start five seconds
// apart when the preceding round fits. This opt-in test does not represent
// Pod, FUSE, or API capacity.
func TestActiveControllerRenewScale(t *testing.T) {
	rawCount := os.Getenv("TEST_ETCD_CONTROLLER_SCALE_N")
	if rawCount == "" {
		t.Skip("TEST_ETCD_CONTROLLER_SCALE_N is not set")
	}
	count, err := strconv.Atoi(rawCount)
	require.NoError(t, err)
	require.Greater(t, count, 0)
	require.LessOrEqual(t, count, 10000)
	rounds := 1
	if rawRounds := os.Getenv("TEST_ETCD_CONTROLLER_SCALE_ROUNDS"); rawRounds != "" {
		rounds, err = strconv.Atoi(rawRounds)
		require.NoError(t, err)
	}
	require.Greater(t, rounds, 0)
	require.LessOrEqual(t, rounds, 5)

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
		// measurement. The optional five-second pacing is a load model; it
		// does not use production's per-workspace renewal goroutines.
		acquired, ok, acquireErr := repository.AcquireController(ctx, lease, 5*time.Minute)
		require.NoError(t, acquireErr)
		require.True(t, ok)
		leases = append(leases, *acquired)
	}
	setupDuration := time.Since(setupStart)
	firstStart := time.Now()
	for round := range rounds {
		scheduled := firstStart.Add(time.Duration(round) * 5 * time.Second)
		if delay := time.Until(scheduled); delay > 0 {
			time.Sleep(delay)
		}
		started := time.Now()
		latencies, renewed, err := runControllerRenewRound(ctx, repository, leases)
		require.NoError(t, err)
		leases = renewed
		finished := time.Now()
		duration := finished.Sub(started)
		// A successful RPC round can still miss the next five-second tick.
		// Test leases are deliberately longer and do not make this pass.
		periodMet := !finished.After(scheduled.Add(5 * time.Second))
		t.Logf("controllers=%d snapshot_bytes=%d setup=%s round=%d/%d start_lag=%s renew_round=%s period_met=%t renew_per_second=%.1f p50=%s p95=%s p99=%s",
			count, snapshotBytes, setupDuration, round+1, rounds, started.Sub(scheduled), duration, periodMet,
			float64(count)/duration.Seconds(), latencies[(count-1)*50/100],
			latencies[(count-1)*95/100], latencies[(count-1)*99/100])
	}
}

func runControllerRenewRound(ctx context.Context, repository *ActiveSandboxRepository, leases []state.ActiveSandboxControllerLease) ([]time.Duration, []state.ActiveSandboxControllerLease, error) {
	jobs := make(chan int)
	durations := make(chan time.Duration, len(leases))
	errors := make(chan error, len(leases))
	// Each index is sent once; workers write distinct slots, then the next
	// round reads the completed slice only after workers.Wait returns.
	renewed := make([]state.ActiveSandboxControllerLease, len(leases))
	var workers sync.WaitGroup
	const workerCount = 16
	for range workerCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				lease := leases[index]
				started := time.Now()
				current, renewErr := repository.RenewController(ctx, lease, 5*time.Minute)
				durations <- time.Since(started)
				if renewErr != nil {
					errors <- fmt.Errorf("renew %s: %w", lease.SandboxID, renewErr)
					continue
				}
				if current == nil {
					errors <- fmt.Errorf("renew %s returned no controller", lease.SandboxID)
					continue
				}
				renewed[index] = *current
			}
		}()
	}
	for index := range leases {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	close(durations)
	close(errors)
	for renewErr := range errors {
		if renewErr != nil {
			return nil, nil, renewErr
		}
	}
	latencies := make([]time.Duration, 0, len(leases))
	for duration := range durations {
		latencies = append(latencies, duration)
	}
	if len(latencies) != len(leases) {
		return nil, nil, fmt.Errorf("measured %d renewals, expected %d", len(latencies), len(leases))
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	return latencies, renewed, nil
}
