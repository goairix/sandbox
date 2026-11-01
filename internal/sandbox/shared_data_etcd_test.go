package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage/state"
	etcdstate "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type coordinationCosts struct {
	stages                  sync.Map
	watchEvents, watchBytes atomic.Int64
}

func (c *coordinationCosts) observe(_ context.Context, op, stage, result string, _ time.Duration) {
	key := op + "/" + stage + "/" + result
	value, _ := c.stages.LoadOrStore(key, &atomic.Int64{})
	value.(*atomic.Int64).Add(1)
}
func (c *coordinationCosts) snapshot() map[string]int64 {
	out := map[string]int64{}
	c.stages.Range(func(k, v any) bool { out[k.(string)] = v.(*atomic.Int64).Load(); return true })
	return out
}

type coordinationEtcdFixture struct {
	store    *etcdstate.Store
	repo     *etcdstate.ActiveSandboxRepository
	client   *clientv3.Client
	prefix   string
	managers []*Manager
	rt       *sharedPoolRuntime
	costs    *coordinationCosts
}

func coordinationEndpoints(t testing.TB) []string {
	t.Helper()
	raw := os.Getenv("TEST_ETCD_ENDPOINTS")
	if raw == "" {
		raw = os.Getenv("TEST_ETCD_ENDPOINT")
	}
	if raw == "" {
		t.Skip("TEST_ETCD_ENDPOINT is not set")
	}
	return strings.Split(raw, ",")
}
func newCoordinationEtcdFixture(t testing.TB, supportsShared ...bool) *coordinationEtcdFixture {
	t.Helper()
	ctx := context.Background()
	costs := &coordinationCosts{}
	prefix := "/coordination-test/" + uuid.NewString() + "/"
	store, err := etcdstate.New(ctx, etcdstate.Options{Endpoints: coordinationEndpoints(t), Prefix: prefix, Timeout: 3 * time.Second, CoordinationStageObserver: costs.observe, CoordinationWatchObserver: func(_ context.Context, n, bytes int64) { costs.watchEvents.Add(n); costs.watchBytes.Add(bytes) }})
	require.NoError(t, err)
	repo, err := etcdstate.NewActiveSandboxRepository(store, "coordination-test")
	require.NoError(t, err)
	client, err := clientv3.New(clientv3.Config{Endpoints: coordinationEndpoints(t), DialTimeout: 3 * time.Second})
	require.NoError(t, err)
	f := &coordinationEtcdFixture{store: store, repo: repo, client: client, prefix: prefix, rt: newSharedPoolRuntime(), costs: costs}
	for i, capable := range supportsShared {
		var active state.ActiveSandboxRepository = repo
		if !capable {
			// Model older/unsupported repositories for mixed-version and baseline tests,
			// without a production configuration switch.
			active = struct{ state.ActiveSandboxRepository }{repo}
		}
		f.managers = append(f.managers, NewManager(f.rt, nil, nil, ManagerConfig{RuntimeType: "kubernetes", ActiveSandboxes: active, InstanceID: fmt.Sprintf("api-%d", i)}))
	}
	t.Cleanup(func() {
		for _, m := range f.managers {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			require.NoError(t, m.Stop(ctx))
			cancel()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err := client.Delete(ctx, prefix, clientv3.WithPrefix())
		require.NoError(t, err)
		require.NoError(t, store.Close())
		require.NoError(t, client.Close())
	})
	f.publish(t, "s")
	return f
}
func (f *coordinationEtcdFixture) publish(t testing.TB, id string) {
	t.Helper()
	now := time.Now()
	sb := &Sandbox{ID: id, RuntimeID: "pod-" + id, RuntimeUID: "uid-" + id, State: StateReady, Timeout: time.Hour, CreatedAt: now, UpdatedAt: now}
	snapshot, err := json.Marshal(sb)
	require.NoError(t, err)
	require.NoError(t, f.repo.Publish(context.Background(), state.ActiveSandboxRecord{Version: 1, SandboxID: id, Phase: state.ActiveSandboxPublishing, Revision: 1, Generation: 1, RuntimeID: sb.RuntimeID, RuntimeUID: sb.RuntimeUID, Snapshot: snapshot, CreatedAt: now, UpdatedAt: now}))
	_, err = f.repo.Activate(context.Background(), id, 1, snapshot)
	require.NoError(t, err)
	f.rt.mu.Lock()
	f.rt.sandboxes[sb.RuntimeID] = &runtime.SandboxInfo{RuntimeID: sb.RuntimeID, RuntimeUID: sb.RuntimeUID, State: "running"}
	f.rt.mu.Unlock()
}

func TestSharedDataEtcdMixedReplicaDestroy(t *testing.T) {
	f := newCoordinationEtcdFixture(t, true, false, true)
	var releases []func()
	for _, m := range f.managers {
		_, _, release, err := m.beginDistributedOperation(context.Background(), "s", state.ActiveOperationData)
		require.NoError(t, err)
		releases = append(releases, release)
		defer release()
	}
	_, live, _, err := f.repo.BeginDestroy(context.Background(), "s")
	require.NoError(t, err)
	require.EqualValues(t, 3, live)
	for _, m := range f.managers {
		_, _, _, err := m.beginDistributedOperation(context.Background(), "s", state.ActiveOperationData)
		require.Error(t, err)
	}
	for i := 0; i < 2; i++ {
		releases[i]()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	err = f.managers[0].Destroy(ctx, "s")
	cancel()
	require.Error(t, err)
	f.rt.mu.Lock()
	require.Zero(t, f.rt.removed)
	f.rt.mu.Unlock()
	releases[2]()
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, f.managers[0].Destroy(ctx, "s"))
	record, err := f.repo.Load(ctx, "s")
	require.NoError(t, err)
	require.Nil(t, record)
	f.rt.mu.Lock()
	require.Equal(t, 1, f.rt.removed)
	f.rt.mu.Unlock()
}

func TestSharedDataEtcdExclusiveWaitsForAllGroups(t *testing.T) {
	f := newCoordinationEtcdFixture(t, true, true, true)
	_, _, releaseA, err := f.managers[0].beginDistributedOperation(context.Background(), "s", state.ActiveOperationData)
	require.NoError(t, err)
	defer releaseA()
	_, _, releaseB, err := f.managers[1].beginDistributedOperation(context.Background(), "s", state.ActiveOperationData)
	require.NoError(t, err)
	defer releaseB()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		_, _, release, err := f.managers[2].beginDistributedWorkspaceOperation(ctx, "s")
		done <- err
		if err == nil {
			<-finished
			release()
		}
	}()
	defer close(finished)
	require.Eventually(t, func() bool {
		r, err := f.repo.Load(ctx, "s")
		return err == nil && r != nil && r.Phase == state.ActiveSandboxExclusive
	}, time.Second, 5*time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("exclusive admitted before groups drained: %v", err)
	default:
	}
	releaseA()
	live, err := f.repo.LiveOperations(ctx, "s")
	require.NoError(t, err)
	require.GreaterOrEqual(t, live, int64(2))
	releaseB()
	require.NoError(t, <-done)
	live, err = f.repo.LiveOperations(ctx, "s")
	require.NoError(t, err)
	require.EqualValues(t, 1, live)
}

func TestSharedDataEtcdOldExpiryStillCountsLiveLease(t *testing.T) {
	f := newCoordinationEtcdFixture(t, false)
	view, err := f.repo.OpenDataOperation(context.Background(), "s", "long-participant", 2*time.Second)
	require.NoError(t, err)
	for range 4 {
		timer := time.NewTimer(600 * time.Millisecond)
		<-timer.C
		_, _, err = f.repo.KeepAliveDataOperation(context.Background(), view.Lease)
		require.NoError(t, err)
	}
	require.True(t, time.Now().After(view.Lease.Operation.ExpiresAt))
	_, err = f.repo.ReadDataOperation(context.Background(), view.Lease, true)
	require.NoError(t, err)
	live, err := f.repo.LiveOperations(context.Background(), "s")
	require.NoError(t, err)
	require.EqualValues(t, 1, live)
	require.NoError(t, f.repo.EndOperation(context.Background(), view.Lease.Operation))
}

func TestSharedDataEtcdCrashAndLeaseLoss(t *testing.T) {
	f := newCoordinationEtcdFixture(t, true)
	_, opCtx, release, err := f.managers[0].beginDistributedOperation(context.Background(), "s", state.ActiveOperationData)
	require.NoError(t, err)
	defer release()
	r := f.managers[0].sharedData
	r.mu.Lock()
	g := r.groups["s"]
	r.mu.Unlock()
	g.mu.Lock()
	lease := g.lease
	g.mu.Unlock()
	_, err = f.client.Revoke(context.Background(), clientv3.LeaseID(lease.LeaseID))
	require.NoError(t, err)
	g.renew()
	require.Error(t, context.Cause(opCtx))
	record, err := f.repo.Load(context.Background(), "s")
	require.NoError(t, err)
	require.NotNil(t, record, "participant lease must never own durable sandbox state")
	successor, err := f.repo.OpenDataOperation(context.Background(), "s", "successor", 30*time.Second)
	require.NoError(t, err)
	require.NoError(t, f.repo.EndOperation(context.Background(), lease.Operation))
	_, err = f.repo.ReadDataOperation(context.Background(), successor.Lease, true)
	require.NoError(t, err)
	require.NoError(t, f.repo.EndOperation(context.Background(), successor.Lease.Operation))
}

func TestSharedDataEtcdWatchCompaction(t *testing.T) {
	f := newCoordinationEtcdFixture(t, true)
	_, _, release, err := f.managers[0].beginDistributedOperation(context.Background(), "s", state.ActiveOperationData)
	require.NoError(t, err)
	observed, err := f.repo.ObserveOperations(context.Background(), "s")
	require.NoError(t, err)
	_, err = f.client.Compact(context.Background(), observed.Revision)
	require.NoError(t, err)
	_, _, _, err = f.repo.BeginDestroy(context.Background(), "s")
	require.NoError(t, err)
	release()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, f.managers[0].waitDistributedOperations(ctx, "s", nil))
}

func TestSharedDataEtcdCrashProcessChild(t *testing.T) {
	prefix := os.Getenv("SANDBOX_COORDINATION_CRASH_PREFIX")
	if prefix == "" {
		t.Skip("subprocess helper")
	}
	store, err := etcdstate.New(context.Background(), etcdstate.Options{Endpoints: coordinationEndpoints(t), Prefix: prefix, Timeout: time.Second})
	require.NoError(t, err)
	repo, err := etcdstate.NewActiveSandboxRepository(store, "coordination-test")
	require.NoError(t, err)
	rt := newMockRuntime()
	rt.sandboxes["pod-s"] = &runtime.SandboxInfo{RuntimeID: "pod-s", RuntimeUID: "uid-s", State: "running"}
	m := NewManager(rt, nil, nil, ManagerConfig{RuntimeType: "kubernetes", ActiveSandboxes: repo, ActiveOperationTTL: 2 * time.Second, InstanceID: "crash-child"})
	_, _, _, err = m.beginDistributedOperation(context.Background(), "s", state.ActiveOperationData)
	require.NoError(t, err)
	fmt.Println("COORDINATION_READY")
	select {}
}
func TestSharedDataEtcdProcessCrashExpiresParticipant(t *testing.T) {
	f := newCoordinationEtcdFixture(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSharedDataEtcdCrashProcessChild$", "-test.timeout=20s")
	cmd.Env = append(os.Environ(), "SANDBOX_COORDINATION_CRASH_PREFIX="+f.prefix)
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	scan := bufio.NewScanner(out)
	ready := false
	for scan.Scan() {
		if scan.Text() == "COORDINATION_READY" {
			ready = true
			break
		}
	}
	require.True(t, ready, "crash child did not acquire participant")
	require.NoError(t, cmd.Process.Kill())
	require.Error(t, cmd.Wait())
	require.Eventually(t, func() bool { n, err := f.repo.LiveOperations(ctx, "s"); return err == nil && n == 0 }, 8*time.Second, 50*time.Millisecond)
	record, err := f.repo.Load(ctx, "s")
	require.NoError(t, err)
	require.NotNil(t, record)
}

func TestSharedDataEtcdQuorumLoss(t *testing.T) {
	members := strings.Split(os.Getenv("TEST_ETCD_MEMBERS"), ",")
	if len(members) != 3 {
		t.Skip("requires dedicated three-member fixture")
	}
	for _, member := range members {
		require.True(t, strings.HasPrefix(member, "sandbox-coordination-test-"), "refuse to stop external etcd")
	}
	f := newCoordinationEtcdFixture(t, true)
	restart := func() {
		for _, member := range members[1:] {
			_ = exec.Command("docker", "start", member).Run()
		}
		require.Eventually(t, func() bool { _, err := f.repo.Load(context.Background(), "s"); return err == nil }, 20*time.Second, 100*time.Millisecond)
	}
	t.Cleanup(restart)
	_, _, release, err := f.managers[0].beginDistributedOperation(context.Background(), "s", state.ActiveOperationData)
	require.NoError(t, err)
	release()
	require.NoError(t, exec.Command("docker", "stop", "-t", "1", members[2]).Run())
	_, _, release, err = f.managers[0].beginDistributedOperation(context.Background(), "s", state.ActiveOperationData)
	require.NoError(t, err)
	release()
	require.NoError(t, exec.Command("docker", "stop", "-t", "1", members[1]).Run())
	_, _, _, err = f.managers[0].beginDistributedOperation(context.Background(), "s", state.ActiveOperationData)
	require.Error(t, err, "no quorum must not authorize from local cache")
}

func TestSharedDataEtcdCapacityFallbackPreservesAllParticipants(t *testing.T) {
	f := newCoordinationEtcdFixture(t, true)
	for i := 1; i < 2048; i++ {
		f.publish(t, fmt.Sprintf("capacity-%d", i))
	}
	var releases []func()
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	for i := 0; i < 2048; i++ {
		id := "s"
		if i > 0 {
			id = fmt.Sprintf("capacity-%d", i)
		}
		_, _, release, err := f.managers[0].beginDistributedOperation(context.Background(), id, state.ActiveOperationData)
		require.NoError(t, err)
		releases = append(releases, release)
	}
	f.managers[0].sharedData.mu.Lock()
	groups := len(f.managers[0].sharedData.groups)
	f.managers[0].sharedData.mu.Unlock()
	require.Equal(t, 1024, groups)
	snapshot, err := f.client.Get(context.Background(), f.prefix, clientv3.WithPrefix())
	require.NoError(t, err)
	count := 0
	leases := map[int64]bool{}
	for _, kv := range snapshot.Kvs {
		if strings.Contains(string(kv.Key), "/operation/") {
			count++
			require.NotZero(t, kv.Lease)
			leases[kv.Lease] = true
		}
	}
	require.Equal(t, 2048, count, "capacity fallback must retain one participant per protected sandbox")
	require.Len(t, leases, 2048)
	t.Logf("COORDINATION_CAPACITY groups=%d namespace_keys=%d leased_operations=%d distinct_live_leases=%d", groups, len(snapshot.Kvs), count, len(leases))
}

func TestSharedDataEtcdMissingSandboxMatchesLegacy(t *testing.T) {
	f := newCoordinationEtcdFixture(t, false, true)
	for _, m := range f.managers {
		_, _, _, err := m.beginDistributedOperation(context.Background(), "missing", state.ActiveOperationData)
		require.ErrorIs(t, err, ErrSandboxNotFound, "enabling sharing must preserve not-found classification")
	}
}
