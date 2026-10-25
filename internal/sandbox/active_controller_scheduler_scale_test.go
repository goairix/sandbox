package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage/state"
	etcdstate "github.com/goairix/sandbox/internal/storage/state/etcd"
)

// TestActiveControllerSchedulerScale measures the production per-controller
// goroutine and ticker against real etcd. The optional second Manager has its
// own client and instance identity, but does not start another API process.
// The test excludes Pod, workspace renewal, FUSE health, API traffic, and
// cleanup effects.
func TestActiveControllerSchedulerScale(t *testing.T) {
	rawCount := os.Getenv("TEST_ETCD_PRODUCT_CONTROLLER_SCALE_N")
	if rawCount == "" {
		t.Skip("TEST_ETCD_PRODUCT_CONTROLLER_SCALE_N is not set")
	}
	count, err := strconv.Atoi(rawCount)
	require.NoError(t, err)
	require.Greater(t, count, 0)
	require.LessOrEqual(t, count, 3000)
	replicas := 1
	if rawReplicas := os.Getenv("TEST_ETCD_PRODUCT_CONTROLLER_SCALE_REPLICAS"); rawReplicas != "" {
		replicas, err = strconv.Atoi(rawReplicas)
		require.NoError(t, err)
	}
	require.GreaterOrEqual(t, replicas, 1)
	require.LessOrEqual(t, replicas, 2)

	endpoint := os.Getenv("TEST_ETCD_ENDPOINT")
	require.NotEmpty(t, endpoint)
	prefix := "/controller-scheduler-scale/" + uuid.NewString() + "/"
	repositories := make([]state.ActiveSandboxRepository, 0, replicas)
	managers := make([]*Manager, 0, replicas)
	cancels := make([]context.CancelFunc, 0, replicas)
	// Clients share only the persisted prefix. Each sandbox is assigned one
	// Manager so this measures load distribution, not a takeover race.
	for replica := range replicas {
		store, storeErr := etcdstate.New(context.Background(), etcdstate.Options{
			Endpoints: []string{endpoint}, Prefix: prefix, Timeout: 3 * time.Second,
		})
		require.NoError(t, storeErr)
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		repository, repoErr := etcdstate.NewActiveSandboxRepository(store, "controller-scheduler-scale")
		require.NoError(t, repoErr)
		controlCtx, cancel := context.WithCancel(context.Background())
		repositories = append(repositories, repository)
		managers = append(managers, &Manager{
			config: ManagerConfig{RuntimeType: "kubernetes", InstanceID: fmt.Sprintf("api-%d", replica),
				ControllerTTL: 15 * time.Second, ControllerRenewInterval: 5 * time.Second},
			activeSandboxes: repository, controlCtx: controlCtx,
		})
		cancels = append(cancels, cancel)
	}
	controllers := make([]*activeController, 0, count)
	t.Cleanup(func() {
		for _, cancel := range cancels {
			cancel()
		}
		// Stop joins every goroutine and releases its exact leased key before
		// the client closes. The isolated namespace is deleted with the test
		// container, so a timeout remains a test failure rather than a retry.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer stopCancel()
		for _, controller := range controllers {
			require.NoError(t, controller.Stop(stopCtx))
		}
	})

	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	setupStart := time.Now()
	initialExpiries := make([]time.Time, 0, count)
	var snapshotBytes int
	for index := range count {
		replica := index % replicas
		repository := repositories[replica]
		manager := managers[replica]
		id := fmt.Sprintf("sandbox-scheduler-%06d", index)
		runtimeID := "pod-" + id
		runtimeUID := "uid-" + id
		snapshot, marshalErr := json.Marshal(map[string]any{
			"id": id, "runtime_id": runtimeID, "runtime_uid": runtimeUID,
			"config": map[string]any{
				"mode": "persistent", "workspace_path": "team/capacity/" + id,
				"workspace_mount_mode": "fuse", "timeout": 7200,
				"resources": map[string]string{"cpu": "500m", "memory": "512Mi"},
			},
			"workspace": map[string]any{"mount_type": "fuse", "mount_state": "ready"},
		})
		require.NoError(t, marshalErr)
		if index == 0 {
			snapshotBytes = len(snapshot)
		}
		now := time.Now().UTC()
		record := state.ActiveSandboxRecord{
			Version: 1, SandboxID: id, Phase: state.ActiveSandboxPublishing,
			Revision: 1, Generation: 1, RuntimeID: runtimeID, RuntimeUID: runtimeUID,
			Snapshot: snapshot, CreatedAt: now, UpdatedAt: now,
		}
		require.NoError(t, repository.Publish(context.Background(), record))
		_, err = repository.Activate(context.Background(), id, record.Revision, snapshot)
		require.NoError(t, err)
		controller, acquired, acquireErr := manager.acquireActiveController(context.Background(), &Sandbox{ID: id, activeGeneration: 1})
		require.NoError(t, acquireErr)
		require.True(t, acquired)
		controllers = append(controllers, controller)
		// The renewal goroutine may start before Acquire returns. Capture the
		// baseline under the same lock used by Fence to replace its lease.
		controller.mu.Lock()
		initialExpiries = append(initialExpiries, controller.lease.ExpiresAt)
		controller.mu.Unlock()
	}
	setupDuration := time.Since(setupStart)
	setupCompletedAt := time.Now().UTC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	setupGoroutines := runtime.NumGoroutine()

	// Holding beyond two product intervals observes the actual staggered
	// controller.run loops, not a synthetic synchronized renewal burst.
	time.Sleep(12 * time.Second)
	observedAt := time.Now()
	var advanced, live, lost int
	for index, controller := range controllers {
		controller.mu.Lock()
		if controller.lease.ExpiresAt.After(initialExpiries[index]) {
			advanced++
		}
		if controller.lease.ExpiresAt.After(observedAt) {
			live++
		}
		if controller.lost != nil {
			lost++
		}
		controller.mu.Unlock()
	}
	t.Logf("controllers=%d replicas=%d snapshot_bytes=%d setup=%s setup_completed_utc=%s observed_utc=%s "+
		"hold=12s advanced=%d live=%d lost=%d goroutines_after_setup=%d heap_alloc_delta_bytes=%d",
		count, replicas, snapshotBytes, setupDuration, setupCompletedAt.Format(time.RFC3339Nano),
		observedAt.UTC().Format(time.RFC3339Nano), advanced, live, lost, setupGoroutines,
		int64(after.HeapAlloc)-int64(before.HeapAlloc))
	require.Equal(t, count, advanced)
	require.Equal(t, count, live)
	require.Zero(t, lost)
}
