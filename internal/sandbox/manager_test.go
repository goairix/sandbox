package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage"
	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/goairix/sandbox/internal/telemetry/metrics"
)

type fuseManagerRuntime struct {
	*mockRuntime
	mu               sync.Mutex
	events           []string
	authorizations   []runtime.WorkspaceMountAuthorization
	waitReadyEntered chan struct{}
	allowReady       chan struct{}
	waitReadyOnce    sync.Once
	waitReadyErr     error
	networkErr       error
	health           runtime.WorkspaceHealth
}

func newFUSEManagerRuntime() *fuseManagerRuntime {
	return &fuseManagerRuntime{
		mockRuntime: newFUSEMockRuntime(),
		health: runtime.WorkspaceHealth{
			Ready: true, MountType: "fuse", RuntimeUID: "prepared-uid-1", Generation: 1,
		},
	}
}

func newBlockingFUSEManagerRuntime() *fuseManagerRuntime {
	rt := newFUSEManagerRuntime()
	rt.waitReadyEntered = make(chan struct{})
	rt.allowReady = make(chan struct{})
	return rt
}

func (r *fuseManagerRuntime) recordEvent(event string) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
}

func (r *fuseManagerRuntime) AuthorizeWorkspaceMount(_ context.Context, _ string, auth runtime.WorkspaceMountAuthorization) error {
	r.mu.Lock()
	r.events = append(r.events, "authorize")
	r.authorizations = append(r.authorizations, auth)
	r.mu.Unlock()
	return nil
}

func (r *fuseManagerRuntime) UpdateNetwork(_ context.Context, _ string, _ bool, _ []string, _ bool) error {
	r.recordEvent("network")
	return r.networkErr
}

func (r *fuseManagerRuntime) WaitSandboxReady(ctx context.Context, id string) (*runtime.SandboxInfo, error) {
	r.recordEvent("ready")
	if r.waitReadyEntered != nil {
		r.waitReadyOnce.Do(func() { close(r.waitReadyEntered) })
		select {
		case <-r.allowReady:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if r.waitReadyErr != nil {
		return nil, r.waitReadyErr
	}
	info, err := r.GetSandbox(ctx, id)
	if err != nil || info == nil {
		return nil, fmt.Errorf("prepared sandbox missing")
	}
	copy := *info
	copy.State = "running"
	return &copy, nil
}

func (r *fuseManagerRuntime) WorkspaceHealth(_ context.Context, id string) (*runtime.WorkspaceHealth, error) {
	r.mu.Lock()
	health := r.health
	r.mu.Unlock()
	info, _ := r.GetSandbox(context.Background(), id)
	if info != nil && health.RuntimeUID == "" {
		health.RuntimeUID = info.RuntimeUID
	}
	return &health, nil
}

func (r *fuseManagerRuntime) ConfirmTerminated(_ context.Context, _, runtimeUID string) (runtime.TerminationEvidence, error) {
	return runtime.TerminationEvidence{RuntimeUID: runtimeUID, GracefulUnmount: true, ProcessExited: true}, nil
}

type fuseMarkerClient struct {
	mu     sync.Mutex
	events *[]string
	exists bool
	err    error
}

type ambiguousSessionStore struct {
	*atomicMemoryStore
	mu       sync.Mutex
	failNext bool
}

type blockingSessionSetStore struct {
	*atomicMemoryStore
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingSessionSetStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if strings.HasPrefix(key, sandboxSessionKeyPrefix) {
		s.once.Do(func() {
			close(s.entered)
			<-s.release
		})
	}
	return s.atomicMemoryStore.Set(ctx, key, value, ttl)
}

type consumeFailStore struct{ *atomicMemoryStore }

func (s *consumeFailStore) CompareAndSwap(ctx context.Context, key string, oldValue, newValue []byte, ttl time.Duration) (bool, error) {
	if strings.HasPrefix(key, workspaceOwnerKeyPrefix) {
		var owner WorkspaceOwner
		if json.Unmarshal(newValue, &owner) == nil && owner.MountAttempt == 1 {
			return false, errors.New("mount attempt CAS failed")
		}
	}
	return s.atomicMemoryStore.CompareAndSwap(ctx, key, oldValue, newValue, ttl)
}

type bindRenewFailStore struct {
	*atomicMemoryStore
	mu       sync.Mutex
	failNext bool
}

type transitionReplyFailRepository struct {
	state.FUSEPoolRepository
	failTo      state.FUSEPoolState
	afterCommit bool
	err         error
}

func (r *transitionReplyFailRepository) Transition(ctx context.Context, preparationID string, from, to state.FUSEPoolState, token string, expectedRevision uint64) (*state.FUSEPoolRecord, error) {
	if to != r.failTo {
		return r.FUSEPoolRepository.Transition(ctx, preparationID, from, to, token, expectedRevision)
	}
	if !r.afterCommit {
		return nil, r.err
	}
	if _, err := r.FUSEPoolRepository.Transition(ctx, preparationID, from, to, token, expectedRevision); err != nil {
		return nil, err
	}
	return nil, r.err
}

func (s *bindRenewFailStore) CompareAndSwap(ctx context.Context, key string, oldValue, newValue []byte, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	fail := s.failNext && strings.HasPrefix(key, workspaceLeaseKeyPrefix)
	if fail {
		s.failNext = false
	}
	s.mu.Unlock()
	if fail {
		return false, errors.New("renew failed after owner bind")
	}
	swapped, err := s.atomicMemoryStore.CompareAndSwap(ctx, key, oldValue, newValue, ttl)
	if err == nil && swapped && strings.HasPrefix(key, workspaceOwnerKeyPrefix) {
		var owner WorkspaceOwner
		if json.Unmarshal(newValue, &owner) == nil && owner.RuntimeUID != "" && owner.MountAttempt == 0 {
			s.mu.Lock()
			s.failNext = true
			s.mu.Unlock()
		}
	}
	return swapped, err
}

func (s *ambiguousSessionStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := s.atomicMemoryStore.Set(ctx, key, value, ttl); err != nil {
		return err
	}
	s.mu.Lock()
	fail := s.failNext
	s.failNext = false
	s.mu.Unlock()
	if fail && strings.HasPrefix(key, sandboxSessionKeyPrefix) {
		return errors.New("connection lost after session commit")
	}
	return nil
}

func (c *fuseMarkerClient) PutEmptyObject(_ context.Context, _ string, _ storage.RootMarkerOptions) error {
	if c.events != nil {
		c.mu.Lock()
		*c.events = append(*c.events, "marker-put")
		c.mu.Unlock()
	}
	return c.err
}

func (c *fuseMarkerClient) HeadObject(context.Context, string) (bool, error) {
	if c.err != nil {
		return false, c.err
	}
	return c.exists, nil
}

func newFUSETestManager(t *testing.T, rt *fuseManagerRuntime) (*Manager, string, *memoryFUSEPoolRepository, *atomicMemoryStore) {
	t.Helper()
	repo := newMemoryFUSEPoolRepository()
	store := newAtomicMemoryStore()
	spec := fixedFUSESpec("pool-key")
	pool := NewFUSEPool(rt, repo, fusePoolConfig(), spec)
	profile, err := storage.RootMarkerProfileByID(storage.RootMarkerProfileMinIO)
	require.NoError(t, err)
	mgr := NewManager(rt, nil, &storage.FileSystemMeta{
		Provider: storage.ProviderMinIO, Bucket: "sandbox", SubPath: "workspaces", StorageIdentity: "storage-primary",
	}, ManagerConfig{
		WorkspaceMode: "fuse", FUSEPool: pool,
		WorkspaceCoordinator:  NewWorkspaceCoordinator(store, time.Minute, 10*time.Second),
		WorkspaceObjectClient: &fuseMarkerClient{exists: true}, WorkspaceMarkerProfile: profile,
		FUSEHealthInterval: time.Hour,
	})
	mgr.SetSessionStore(NewSessionStore(store, time.Hour))
	require.NoError(t, mgr.fusePool.WarmUp(context.Background()))
	records, err := repo.ListByPoolKey(context.Background(), "pool-key")
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, state.FUSEPoolPrepared, records[0].State)
	return mgr, records[0].RuntimeID, repo, store
}

func TestManagerCreateFUSEUsesPreparedRuntimeAndPublishesAfterProbe(t *testing.T) {
	rt := newFUSEManagerRuntime()
	mgr, preparedID, _, _ := newFUSETestManager(t, rt)

	sb, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	assert.Equal(t, preparedID, sb.RuntimeID)
	require.NotNil(t, sb.Workspace)
	assert.Equal(t, WorkspaceMountFUSE, sb.Workspace.MountType)
	assert.Equal(t, WorkspaceMountReady, sb.Workspace.MountState)
	rt.mu.Lock()
	assert.Equal(t, []string{"authorize", "network", "ready"}, rt.events)
	assert.Len(t, rt.authorizations, 1)
	rt.mu.Unlock()
	_, err = mgr.Exec(context.Background(), sb.ID, runtime.ExecRequest{Command: "true"})
	require.NoError(t, err)
}

func TestManagerCreateFUSEIsInvisibleUntilReady(t *testing.T) {
	rt := newBlockingFUSEManagerRuntime()
	mgr, _, _, _ := newFUSETestManager(t, rt)
	done := make(chan error, 1)
	go func() {
		_, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
		done <- err
	}()
	<-rt.waitReadyEntered
	mgr.mu.RLock()
	assert.Empty(t, mgr.sandboxes)
	assert.Empty(t, mgr.operationGates)
	mgr.mu.RUnlock()
	close(rt.allowReady)
	require.NoError(t, <-done)
}

func TestManagerStopCancelsAndWaitsForInFlightFUSECreate(t *testing.T) {
	rt := newBlockingFUSEManagerRuntime()
	mgr, preparedID, _, _ := newFUSETestManager(t, rt)
	createDone := make(chan error, 1)
	go func() {
		_, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
		createDone <- err
	}()
	<-rt.waitReadyEntered

	stopDone := make(chan struct{})
	go func() {
		mgr.Stop(context.Background())
		close(stopDone)
	}()

	require.Error(t, <-createDone)
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("Stop did not wait for and finish the in-flight FUSE transaction")
	}
	assert.True(t, rt.wasRemoved(preparedID))
	mgr.mu.RLock()
	assert.Empty(t, mgr.sandboxes)
	assert.Empty(t, mgr.operationGates)
	assert.Empty(t, mgr.fuseInFlight)
	mgr.mu.RUnlock()
	_, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/b"})
	require.ErrorIs(t, err, ErrSandboxNotReady)
}

type managerStreamRuntime struct {
	*mockRuntime
	stream chan runtime.StreamEvent
	reader io.ReadCloser
	batch  []runtime.FileContent
}

func (r *managerStreamRuntime) ExecStream(context.Context, string, runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
	return r.stream, nil
}

func (r *managerStreamRuntime) DownloadFile(context.Context, string, string) (io.ReadCloser, error) {
	return r.reader, nil
}

func (r *managerStreamRuntime) DownloadFiles(context.Context, string, []string) ([]runtime.FileContent, error) {
	return r.batch, nil
}

type signalReadCloser struct {
	closed chan struct{}
	once   sync.Once
}

func newSignalReadCloser() *signalReadCloser       { return &signalReadCloser{closed: make(chan struct{})} }
func (*signalReadCloser) Read([]byte) (int, error) { return 0, io.EOF }
func (r *signalReadCloser) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

func newGatedManager(rt runtime.Runtime) (*Manager, *operationGate) {
	mgr := NewManager(rt, nil, nil, ManagerConfig{})
	gate := newOperationGate(true)
	mgr.sandboxes["sandbox-test"] = &Sandbox{ID: "sandbox-test", RuntimeID: "runtime-test", RuntimeUID: "uid-test", State: StateReady, Workspace: &WorkspaceInfo{MountType: WorkspaceMountFUSE}}
	mgr.operationGates["sandbox-test"] = gate
	return mgr, gate
}

func TestManagerExecStreamHoldsGateUntilStreamEnds(t *testing.T) {
	rt := &managerStreamRuntime{mockRuntime: newMockRuntime(), stream: make(chan runtime.StreamEvent)}
	mgr, gate := newGatedManager(rt)
	out, err := mgr.ExecStream(context.Background(), "sandbox-test", runtime.ExecRequest{Command: "true"})
	require.NoError(t, err)

	drained := make(chan error, 1)
	go func() { drained <- gate.CloseAndWait(context.Background()) }()
	require.Eventually(t, func() bool {
		_, execErr := mgr.Exec(context.Background(), "sandbox-test", runtime.ExecRequest{Command: "true"})
		return errors.Is(execErr, ErrSandboxNotReady)
	}, time.Second, time.Millisecond)
	select {
	case <-drained:
		t.Fatal("gate drained before stream completion")
	default:
	}
	close(rt.stream)
	for range out {
	}
	require.NoError(t, <-drained)
}

func TestManagerDownloadReaderHoldsGateUntilTerminalRead(t *testing.T) {
	reader := newSignalReadCloser()
	rt := &managerStreamRuntime{mockRuntime: newMockRuntime(), reader: reader}
	mgr, gate := newGatedManager(rt)
	rc, err := mgr.DownloadFile(context.Background(), "sandbox-test", "/workspace/a")
	require.NoError(t, err)

	drained := make(chan error, 1)
	go func() { drained <- gate.CloseAndWait(context.Background()) }()
	require.Eventually(t, func() bool {
		probeRelease, err := gate.Acquire()
		if err == nil {
			probeRelease()
		}
		return errors.Is(err, ErrSandboxNotReady)
	}, time.Second, time.Millisecond)
	select {
	case <-drained:
		t.Fatal("gate drained before reader completion")
	default:
	}
	buf := make([]byte, 1)
	_, err = rc.Read(buf)
	require.ErrorIs(t, err, io.EOF)
	require.NoError(t, <-drained)
	require.NoError(t, rc.Close())
	select {
	case <-reader.closed:
	case <-time.After(time.Second):
		t.Fatal("underlying reader was not closed")
	}
}

func TestManagerDownloadFilesHoldsOneGateReferenceUntilEveryReaderEnds(t *testing.T) {
	one, two := newSignalReadCloser(), newSignalReadCloser()
	rt := &managerStreamRuntime{mockRuntime: newMockRuntime(), batch: []runtime.FileContent{{Path: "a", Content: one}, {Path: "b", Content: two}}}
	mgr, gate := newGatedManager(rt)
	files, err := mgr.DownloadFiles(context.Background(), "sandbox-test", []string{"a", "b"})
	require.NoError(t, err)

	drained := make(chan error, 1)
	go func() { drained <- gate.CloseAndWait(context.Background()) }()
	require.Eventually(t, func() bool {
		probeRelease, err := gate.Acquire()
		if err == nil {
			probeRelease()
		}
		return errors.Is(err, ErrSandboxNotReady)
	}, time.Second, time.Millisecond)
	require.NoError(t, files[0].Content.Close())
	select {
	case <-drained:
		t.Fatal("first reader released the batch reference")
	default:
	}
	require.NoError(t, files[1].Content.Close())
	require.NoError(t, <-drained)
}

func TestManagerDirectFUSESessionLoadDoesNotOpenGate(t *testing.T) {
	store := newAtomicMemoryStore()
	sessions := NewSessionStore(store, time.Hour)
	sb := &Sandbox{ID: "persistent", RuntimeID: "runtime", RuntimeUID: "uid"}
	sb.Config.Mode = ModePersistent
	sb.Workspace = &WorkspaceInfo{MountType: WorkspaceMountFUSE, MountState: WorkspaceMountReady}
	require.NoError(t, sessions.Save(context.Background(), sb))
	mgr := NewManager(newMockRuntime(), nil, nil, ManagerConfig{WorkspaceMode: "fuse"})
	mgr.SetSessionStore(sessions)

	_, err := mgr.Get(context.Background(), sb.ID)
	require.ErrorIs(t, err, ErrSandboxNotReady)
	mgr.mu.RLock()
	_, loaded := mgr.sandboxes[sb.ID]
	_, gated := mgr.operationGates[sb.ID]
	mgr.mu.RUnlock()
	assert.False(t, loaded)
	assert.False(t, gated)
}

func TestManagerCreateFUSELeaseConflictReturnsPristineShell(t *testing.T) {
	rt := newFUSEManagerRuntime()
	mgr, preparedID, repo, _ := newFUSETestManager(t, rt)
	prefix, err := storage.BuildWorkspacePrefix("workspaces", "team/a")
	require.NoError(t, err)
	other, err := mgr.config.WorkspaceCoordinator.Acquire(context.Background(), WorkspaceLeaseRequest{
		Provider: "minio", StorageIdentity: "storage-primary", Bucket: "sandbox", Prefix: prefix, SandboxID: "other",
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = mgr.config.WorkspaceCoordinator.Release(context.Background(), other, runtime.TerminationEvidence{})
	})

	_, err = mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.ErrorIs(t, err, ErrWorkspaceLeased)
	require.Eventually(t, func() bool {
		records, listErr := repo.ListByPoolKey(context.Background(), "pool-key")
		if listErr != nil {
			return false
		}
		for _, record := range records {
			if record.RuntimeID == preparedID && record.State == state.FUSEPoolPrepared {
				return true
			}
		}
		return false
	}, time.Second, time.Millisecond)
	assert.False(t, rt.wasRemoved(preparedID))
}

func TestManagerCreateFUSEUnconfirmedAcquireCompensationDestroysShell(t *testing.T) {
	rt := newFUSEManagerRuntime()
	mgr, preparedID, repo, store := newFUSETestManager(t, rt)
	prefix, err := storage.BuildWorkspacePrefix("workspaces", "team/a")
	require.NoError(t, err)
	keys, err := workspaceStateKeys(WorkspaceLeaseRequest{
		Provider: "minio", StorageIdentity: "storage-primary", Bucket: "sandbox", Prefix: prefix,
	})
	require.NoError(t, err)
	store.failSetNXAfterWriting(keys.owner, errors.New("owner create reply lost"))
	store.failCompareAndDeleteBeforeWriting(keys.owner, errors.New("owner compensation unavailable"))

	_, err = mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.ErrorIs(t, err, ErrWorkspaceAcquireCleanupUnconfirmed)
	assert.True(t, rt.wasRemoved(preparedID))
	records, listErr := repo.ListByPoolKey(context.Background(), "pool-key")
	require.NoError(t, listErr)
	for _, record := range records {
		assert.NotEqual(t, preparedID, record.RuntimeID)
	}
}

func TestManagerCreateFUSERejectsResourcesOutsidePreparedPoolSpec(t *testing.T) {
	rt := newFUSEManagerRuntime()
	mgr, preparedID, repo, _ := newFUSETestManager(t, rt)

	_, err := mgr.Create(context.Background(), SandboxConfig{
		Mode:          ModePersistent,
		WorkspacePath: "team/a",
		Resources:     ResourceLimits{Memory: "2Gi"},
	})
	require.ErrorIs(t, err, ErrInvalidFUSEPoolConfig)
	assert.False(t, rt.wasRemoved(preparedID))
	records, listErr := repo.ListByPoolKey(context.Background(), "pool-key")
	require.NoError(t, listErr)
	require.Len(t, records, 1)
	assert.Equal(t, state.FUSEPoolPrepared, records[0].State)
}

func TestManagerCreateFUSEUserNetworkFailureDestroysBoundShell(t *testing.T) {
	rt := newFUSEManagerRuntime()
	rt.networkErr = errors.New("network policy rejected")
	mgr, preparedID, repo, _ := newFUSETestManager(t, rt)

	_, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.ErrorContains(t, err, "network policy rejected")
	assert.True(t, rt.wasRemoved(preparedID))
	records, listErr := repo.ListByPoolKey(context.Background(), "pool-key")
	require.NoError(t, listErr)
	for _, record := range records {
		assert.NotEqual(t, preparedID, record.RuntimeID)
	}
	mgr.mu.RLock()
	assert.Empty(t, mgr.sandboxes)
	assert.Empty(t, mgr.operationGates)
	mgr.mu.RUnlock()
}

func TestManagerCreateFUSEMountAttemptFailureDestroysReservedBoundShell(t *testing.T) {
	rt := newFUSEManagerRuntime()
	mgr, preparedID, repo, store := newFUSETestManager(t, rt)
	mgr.config.WorkspaceCoordinator = NewWorkspaceCoordinator(&consumeFailStore{atomicMemoryStore: store}, time.Minute, 10*time.Second)

	_, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.ErrorContains(t, err, "mount attempt CAS failed")
	assert.True(t, rt.wasRemoved(preparedID))
	records, listErr := repo.ListByPoolKey(context.Background(), "pool-key")
	require.NoError(t, listErr)
	for _, record := range records {
		assert.NotEqual(t, preparedID, record.RuntimeID)
	}
	ownerKeys, keyErr := store.Keys(context.Background(), workspaceOwnerKeyPrefix+"*")
	require.NoError(t, keyErr)
	assert.Empty(t, ownerKeys)
}

func TestManagerCreateFUSEBindCommittedThenRenewFailedStillDestroysShell(t *testing.T) {
	rt := newFUSEManagerRuntime()
	mgr, preparedID, _, store := newFUSETestManager(t, rt)
	mgr.config.WorkspaceCoordinator = NewWorkspaceCoordinator(&bindRenewFailStore{atomicMemoryStore: store}, time.Minute, 10*time.Second)

	_, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.ErrorContains(t, err, "renew failed after owner bind")
	assert.True(t, rt.wasRemoved(preparedID))
}

func TestManagerCreateFUSEBindingTransitionFailureNeverDereferencesNilRecord(t *testing.T) {
	rt := newFUSEManagerRuntime()
	mgr, preparedID, repo, _ := newFUSETestManager(t, rt)
	mgr.fusePool.repo = &transitionReplyFailRepository{
		FUSEPoolRepository: repo,
		failTo:             state.FUSEPoolBinding,
		err:                errors.New("binding transition unavailable"),
	}

	assert.NotPanics(t, func() {
		_, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
		require.ErrorContains(t, err, "binding transition unavailable")
	})
	assert.True(t, rt.wasRemoved(preparedID))
}

func TestManagerCreateFUSEBindingTransitionReplyLossClaimsCommittedRecord(t *testing.T) {
	rt := newFUSEManagerRuntime()
	mgr, preparedID, repo, _ := newFUSETestManager(t, rt)
	mgr.fusePool.repo = &transitionReplyFailRepository{
		FUSEPoolRepository: repo,
		failTo:             state.FUSEPoolBinding,
		afterCommit:        true,
		err:                errors.New("binding transition reply lost"),
	}

	_, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.ErrorContains(t, err, "binding transition reply lost")
	assert.True(t, rt.wasRemoved(preparedID))
	require.Eventually(t, func() bool {
		records, listErr := repo.ListByPoolKey(context.Background(), "pool-key")
		if listErr != nil {
			return false
		}
		for _, record := range records {
			if record.RuntimeID == preparedID {
				return false
			}
		}
		return true
	}, time.Second, time.Millisecond)
}

func TestManagerCreateFUSEConsumedTransitionFailuresClaimExactRecord(t *testing.T) {
	for _, afterCommit := range []bool{false, true} {
		t.Run(fmt.Sprintf("after_commit_%t", afterCommit), func(t *testing.T) {
			rt := newFUSEManagerRuntime()
			mgr, preparedID, repo, _ := newFUSETestManager(t, rt)
			mgr.fusePool.repo = &transitionReplyFailRepository{
				FUSEPoolRepository: repo,
				failTo:             state.FUSEPoolConsumed,
				afterCommit:        afterCommit,
				err:                errors.New("consumed transition failed"),
			}

			_, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
			require.ErrorContains(t, err, "consumed transition failed")
			assert.True(t, rt.wasRemoved(preparedID))
			require.Eventually(t, func() bool {
				records, listErr := repo.ListByPoolKey(context.Background(), "pool-key")
				if listErr != nil {
					return false
				}
				for _, record := range records {
					if record.RuntimeID == preparedID {
						return false
					}
				}
				return true
			}, time.Second, time.Millisecond)
		})
	}
}

func TestManagerCreateFUSESessionSaveFailureDestroysWithoutOpeningGate(t *testing.T) {
	rt := newFUSEManagerRuntime()
	mgr, preparedID, _, store := newFUSETestManager(t, rt)
	store.failNext("Set", errors.New("redis write failed"))

	_, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.ErrorContains(t, err, "redis write failed")
	assert.True(t, rt.wasRemoved(preparedID))
	mgr.mu.RLock()
	assert.Empty(t, mgr.sandboxes)
	assert.Empty(t, mgr.operationGates)
	mgr.mu.RUnlock()
}

func TestManagerCreateFUSEPreBindCleanupRetriesWithoutLosingPoolAnchor(t *testing.T) {
	rt := newFUSEManagerRuntime()
	mgr, preparedID, repo, store := newFUSETestManager(t, rt)
	mgr.config.WorkspaceObjectClient = &fuseMarkerClient{err: errors.New("marker unavailable")}
	store.failNext("CompareAndDelete", errors.New("owner delete temporarily unavailable"))

	_, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.ErrorContains(t, err, "marker unavailable")
	require.Eventually(t, func() bool {
		ownerKeys, keyErr := store.Keys(context.Background(), workspaceOwnerKeyPrefix+"*")
		if keyErr != nil || len(ownerKeys) != 0 {
			return false
		}
		records, listErr := repo.ListByPoolKey(context.Background(), "pool-key")
		if listErr != nil {
			return false
		}
		for _, record := range records {
			if record.RuntimeID == preparedID {
				return record.State == state.FUSEPoolPrepared
			}
		}
		return false
	}, time.Second, 5*time.Millisecond)
	assert.False(t, rt.wasRemoved(preparedID))
}

func TestManagerCreateFUSECompensatesAmbiguousSessionPublicationExactly(t *testing.T) {
	rt := newFUSEManagerRuntime()
	mgr, preparedID, _, store := newFUSETestManager(t, rt)
	ambiguous := &ambiguousSessionStore{atomicMemoryStore: store, failNext: true}
	mgr.sessions = NewSessionStore(ambiguous, time.Hour)

	_, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.ErrorContains(t, err, "connection lost after session commit")
	assert.True(t, rt.wasRemoved(preparedID))
	keys, listErr := store.Keys(context.Background(), sandboxSessionKeyPrefix+"*")
	require.NoError(t, listErr)
	assert.Empty(t, keys)
}

func TestManagerCreateFUSELeaseLossDuringSessionSaveNeverPublishes(t *testing.T) {
	rt := newFUSEManagerRuntime()
	mgr, preparedID, _, store := newFUSETestManager(t, rt)
	mgr.config.WorkspaceCoordinator = NewWorkspaceCoordinator(store, 200*time.Millisecond, 5*time.Millisecond)
	blocking := &blockingSessionSetStore{
		atomicMemoryStore: store,
		entered:           make(chan struct{}),
		release:           make(chan struct{}),
	}
	mgr.sessions = NewSessionStore(blocking, time.Hour)
	createDone := make(chan error, 1)
	go func() {
		_, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
		createDone <- err
	}()
	<-blocking.entered
	leaseKeys, err := store.Keys(context.Background(), workspaceLeaseKeyPrefix+"*")
	require.NoError(t, err)
	require.Len(t, leaseKeys, 1)
	require.NoError(t, store.Delete(context.Background(), leaseKeys[0]))
	require.Eventually(t, func() bool {
		mgr.mu.RLock()
		defer mgr.mu.RUnlock()
		for _, claim := range mgr.fuseInFlight {
			if claim.lost {
				return true
			}
		}
		return false
	}, time.Second, time.Millisecond)
	close(blocking.release)
	require.ErrorIs(t, <-createDone, ErrWorkspaceLeaseLost)
	assert.True(t, rt.wasRemoved(preparedID))
	mgr.mu.RLock()
	assert.Empty(t, mgr.sandboxes)
	assert.Empty(t, mgr.operationGates)
	mgr.mu.RUnlock()
	sessionKeys, err := store.Keys(context.Background(), sandboxSessionKeyPrefix+"*")
	require.NoError(t, err)
	assert.Empty(t, sessionKeys)
}

func TestManagerFUSETeardownRetriesTransientExactRemoval(t *testing.T) {
	rt := newFUSEManagerRuntime()
	mgr, _, _, _ := newFUSETestManager(t, rt)
	sb, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	mgr.mu.RLock()
	lifecycle := mgr.fuseLifecycles[sb.ID]
	mgr.mu.RUnlock()
	require.NotNil(t, lifecycle)
	rt.failRemove(sb.RuntimeID, errors.New("runtime temporarily unavailable"))

	mgr.teardownFUSESandbox(lifecycle, errors.New("unhealthy"))
	assert.False(t, rt.wasRemoved(sb.RuntimeID))
	rt.failRemove(sb.RuntimeID, nil)
	mgr.teardownFUSESandbox(lifecycle, errors.New("retry"))

	assert.True(t, rt.wasRemoved(sb.RuntimeID))
	mgr.mu.RLock()
	_, exists := mgr.sandboxes[sb.ID]
	mgr.mu.RUnlock()
	assert.False(t, exists)
}

func TestManagerStopWaitsForScheduledFUSETeardownRetry(t *testing.T) {
	rt := newFUSEManagerRuntime()
	mgr, _, _, _ := newFUSETestManager(t, rt)
	sb, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	mgr.mu.RLock()
	lifecycle := mgr.fuseLifecycles[sb.ID]
	mgr.mu.RUnlock()
	rt.failRemove(sb.RuntimeID, errors.New("runtime temporarily unavailable"))
	mgr.scheduleFUSETeardown(lifecycle, errors.New("unhealthy"))
	require.Eventually(t, func() bool {
		_, err := mgr.Exec(context.Background(), sb.ID, runtime.ExecRequest{Command: "true"})
		return errors.Is(err, ErrSandboxNotReady)
	}, time.Second, time.Millisecond)

	stopDone := make(chan struct{})
	go func() {
		mgr.Stop(context.Background())
		close(stopDone)
	}()
	select {
	case <-stopDone:
		t.Fatal("Stop returned while the scheduled exact cleanup was incomplete")
	case <-time.After(30 * time.Millisecond):
	}

	rt.failRemove(sb.RuntimeID, nil)
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("Stop did not wait for the scheduled exact cleanup retry")
	}
	assert.True(t, rt.wasRemoved(sb.RuntimeID))
}

func TestManagerFUSETeardownUsesPublishedSessionRevisionAfterExecMutation(t *testing.T) {
	rt := newFUSEManagerRuntime()
	mgr, _, _, store := newFUSETestManager(t, rt)
	sb, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	_, err = mgr.Exec(context.Background(), sb.ID, runtime.ExecRequest{Command: "true"})
	require.NoError(t, err)
	_, err = mgr.UpdateTTL(context.Background(), sb.ID, 300)
	require.NoError(t, err)
	mgr.mu.RLock()
	lifecycle := mgr.fuseLifecycles[sb.ID]
	mgr.mu.RUnlock()

	mgr.teardownFUSESandbox(lifecycle, errors.New("unhealthy"))
	keys, listErr := store.Keys(context.Background(), sandboxSessionKeyPrefix+"*")
	require.NoError(t, listErr)
	assert.Empty(t, keys)
}

func TestManagerFUSETeardownCompensatesLeaseDeleteReplyLoss(t *testing.T) {
	rt := newFUSEManagerRuntime()
	mgr, _, _, store := newFUSETestManager(t, rt)
	sb, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	mgr.mu.RLock()
	lifecycle := mgr.fuseLifecycles[sb.ID]
	mgr.mu.RUnlock()
	store.failCompareAndDeleteAfterWriting(lifecycle.lease.Key, errors.New("connection lost after delete"))

	mgr.teardownFUSESandbox(lifecycle, errors.New("unhealthy"))

	keys, listErr := store.Keys(context.Background(), sandboxSessionKeyPrefix+"*")
	require.NoError(t, listErr)
	assert.Empty(t, keys)
	assert.True(t, lifecycle.leaseReleased)
}

func TestManagerFUSEWatcherClosesGateBeforeBlockedStreamTeardown(t *testing.T) {
	rt := &fuseManagerRuntime{mockRuntime: newFUSEMockRuntime(), health: runtime.WorkspaceHealth{Ready: true, MountType: "fuse", RuntimeUID: "prepared-uid-1", Generation: 1}}
	stream := make(chan runtime.StreamEvent)
	rt.execStreamFunc = func(context.Context, string, runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
		return stream, nil
	}
	mgr, _, _, _ := newFUSETestManager(t, rt)
	mgr.config.FUSEHealthInterval = time.Millisecond
	sb, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	out, err := mgr.ExecStream(context.Background(), sb.ID, runtime.ExecRequest{Command: "sleep"})
	require.NoError(t, err)
	rt.mu.Lock()
	rt.health.Ready = false
	rt.mu.Unlock()

	require.Eventually(t, func() bool {
		_, execErr := mgr.Exec(context.Background(), sb.ID, runtime.ExecRequest{Command: "true"})
		return errors.Is(execErr, ErrSandboxNotReady)
	}, time.Second, time.Millisecond)
	assert.False(t, rt.wasRemoved(sb.RuntimeID), "teardown must wait for the live stream reference")
	close(stream)
	for range out {
	}
	require.Eventually(t, func() bool { return rt.wasRemoved(sb.RuntimeID) }, time.Second, time.Millisecond)
}

func TestManagerFUSEHealthCheckRejectsIdentityAndRestartDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*runtime.WorkspaceHealth)
	}{
		{name: "runtime_uid", mutate: func(h *runtime.WorkspaceHealth) { h.RuntimeUID = "different-uid" }},
		{name: "generation", mutate: func(h *runtime.WorkspaceHealth) { h.Generation++ }},
		{name: "restart_count", mutate: func(h *runtime.WorkspaceHealth) { h.RestartCount = 1 }},
		{name: "restart_marker", mutate: func(h *runtime.WorkspaceHealth) { h.RestartDetected = true }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := newFUSEManagerRuntime()
			mgr, _, _, _ := newFUSETestManager(t, rt)
			mgr.config.FUSEHealthInterval = time.Hour
			sb, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
			require.NoError(t, err)
			rt.mu.Lock()
			tt.mutate(&rt.health)
			rt.mu.Unlock()
			mgr.mu.RLock()
			lifecycle := mgr.fuseLifecycles[sb.ID]
			mgr.mu.RUnlock()
			require.ErrorIs(t, mgr.checkFUSELifecycle(context.Background(), lifecycle), ErrSandboxNotReady)
			mgr.teardownFUSESandbox(lifecycle, ErrSandboxNotReady)
			lifecycle.teardownMu.Lock()
			assert.True(t, lifecycle.teardownDone)
			lifecycle.teardownMu.Unlock()
			assert.True(t, rt.wasRemoved(sb.RuntimeID))
		})
	}
}

func TestManagerGetReturnsDeepCopyOfFUSESandbox(t *testing.T) {
	rt := newFUSEManagerRuntime()
	mgr, _, _, _ := newFUSETestManager(t, rt)
	sb, err := mgr.Create(context.Background(), SandboxConfig{
		Mode:          ModePersistent,
		WorkspacePath: "team/a",
		Network:       NetworkConfig{Whitelist: []string{"allowed.example"}},
		Dependencies:  []Dependency{{Name: "requests", Manager: "pip"}},
	})
	require.NoError(t, err)

	got, err := mgr.Get(context.Background(), sb.ID)
	require.NoError(t, err)
	got.Config.Network.Whitelist[0] = "mutated.example"
	got.Config.Dependencies[0].Name = "mutated"
	got.Workspace.RootPath = "mutated"

	mgr.mu.RLock()
	internal := mgr.sandboxes[sb.ID]
	assert.Equal(t, "allowed.example", internal.Config.Network.Whitelist[0])
	assert.Equal(t, "requests", internal.Config.Dependencies[0].Name)
	assert.Equal(t, "team/a", internal.Workspace.RootPath)
	mgr.mu.RUnlock()
}

func TestManagerRejectsLegacyWorkspaceOperationsForFUSESandbox(t *testing.T) {
	rt := newFUSEManagerRuntime()
	mgr, _, _, _ := newFUSETestManager(t, rt)
	sb, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)

	require.ErrorIs(t, mgr.MountWorkspace(context.Background(), sb.ID, "team/b", nil), ErrFUSEWorkspaceOperationUnsupported)
	require.ErrorIs(t, mgr.UnmountWorkspace(context.Background(), sb.ID), ErrFUSEWorkspaceOperationUnsupported)
	require.ErrorIs(t, mgr.SyncWorkspace(context.Background(), sb.ID, "from_container", nil), ErrFUSEWorkspaceOperationUnsupported)

	info, err := mgr.GetWorkspaceInfo(context.Background(), sb.ID)
	require.NoError(t, err)
	require.NotNil(t, info)
	assert.Equal(t, WorkspaceMountFUSE, info.MountType)
	assert.Equal(t, "team/a", info.RootPath)
}

func TestManagerAutoSyncDoesNotRestoreUnverifiedFUSESession(t *testing.T) {
	rt := newFUSEManagerRuntime()
	mgr, _, _, _ := newFUSETestManager(t, rt)
	stale := &Sandbox{
		ID:         "sandbox-stale-fuse",
		Config:     SandboxConfig{Mode: ModePersistent},
		State:      StateReady,
		RuntimeID:  "runtime-stale",
		RuntimeUID: "uid-stale",
		Workspace:  &WorkspaceInfo{MountType: WorkspaceMountFUSE},
	}
	require.NoError(t, mgr.sessions.Save(context.Background(), stale))

	mgr.autoSyncOnce()

	mgr.mu.RLock()
	_, loaded := mgr.sandboxes[stale.ID]
	_, gated := mgr.operationGates[stale.ID]
	mgr.mu.RUnlock()
	assert.False(t, loaded)
	assert.False(t, gated)
}

func TestManagerFUSEReconcileRetainsLiveBindingTransaction(t *testing.T) {
	rt := newBlockingFUSEManagerRuntime()
	mgr, _, repo, _ := newFUSETestManager(t, rt)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := mgr.Create(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
		done <- err
	}()
	<-rt.waitReadyEntered
	records, err := repo.ListByPoolKey(context.Background(), "pool-key")
	require.NoError(t, err)
	var binding state.FUSEPoolRecord
	for _, record := range records {
		if record.State == state.FUSEPoolBinding {
			binding = record
		}
	}
	require.NotEmpty(t, binding.PreparationID)
	require.NoError(t, mgr.fusePool.Reconcile(context.Background()))
	assert.False(t, rt.wasRemoved(binding.RuntimeID))
	cancel()
	require.Error(t, <-done)
	assert.True(t, rt.wasRemoved(binding.RuntimeID))
}

func TestManagerUploadFilePropagatesExactSize(t *testing.T) {
	rt := newMockRuntime()
	mgr := newExecTestManager(rt, ManagerConfig{})

	require.NoError(t, mgr.UploadFile(context.Background(), "sandbox-test", "/workspace/a.txt", 5, strings.NewReader("hello")))
	assert.Equal(t, int64(5), rt.lastUploadSize())
}

func newExecTestManager(rt *mockRuntime, cfg ManagerConfig) *Manager {
	mgr := NewManager(rt, nil, nil, cfg)
	mgr.sandboxes["sandbox-test"] = &Sandbox{
		ID:        "sandbox-test",
		RuntimeID: "runtime-test",
		State:     StateReady,
		Config: SandboxConfig{
			Network: NetworkConfig{Enabled: true},
		},
	}
	return mgr
}

func TestManagerCreatePropagatesTmpDiskToDirectRuntime(t *testing.T) {
	rt := newMockRuntime()
	mgr := NewManager(rt, nil, nil, ManagerConfig{
		PoolConfig: PoolConfig{
			Image:         "sandbox:latest",
			Memory:        "256Mi",
			MemoryRequest: "128Mi",
			CPU:           "500m",
			CPURequest:    "100m",
			Disk:          "1Gi",
		},
	})

	_, err := mgr.Create(context.Background(), SandboxConfig{
		Mode: ModeEphemeral,
		Resources: ResourceLimits{
			TmpDisk: "200Mi",
		},
	})

	require.NoError(t, err)
	spec := rt.lastCreatedSpec()
	assert.Equal(t, "200Mi", spec.TmpDisk)
	assert.Equal(t, "256Mi", spec.Memory)
	assert.Equal(t, "128Mi", spec.MemoryRequest)
	assert.Equal(t, "500m", spec.CPU)
	assert.Equal(t, "100m", spec.CPURequest)
	assert.Equal(t, "1Gi", spec.Disk)
}

func TestManagerBuildSpecUsesConfiguredTmpDiskDefault(t *testing.T) {
	mgr := NewManager(newMockRuntime(), nil, nil, ManagerConfig{
		PoolConfig: PoolConfig{
			Image:   "sandbox:latest",
			TmpDisk: "150Mi",
		},
	})

	spec := mgr.buildSpec("sandbox-test", SandboxConfig{})

	assert.Equal(t, "150Mi", spec.TmpDisk)
}

func captureExecMetricStatus(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	meter := provider.Meter(t.Name())
	counter, err := meter.Int64Counter("sandbox.exec.total")
	require.NoError(t, err)
	duration, err := meter.Float64Histogram("sandbox.exec.duration")
	require.NoError(t, err)

	oldCounter := metrics.SandboxExecTotal
	oldDuration := metrics.SandboxExecDuration
	metrics.SandboxExecTotal = counter
	metrics.SandboxExecDuration = duration
	t.Cleanup(func() {
		metrics.SandboxExecTotal = oldCounter
		metrics.SandboxExecDuration = oldDuration
		require.NoError(t, provider.Shutdown(context.Background()))
	})
	return reader
}

func readExecMetricStatus(t *testing.T, reader *sdkmetric.ManualReader) string {
	t.Helper()
	var resourceMetrics metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &resourceMetrics))
	for _, scopeMetrics := range resourceMetrics.ScopeMetrics {
		for _, metric := range scopeMetrics.Metrics {
			if metric.Name != "sandbox.exec.total" {
				continue
			}
			sum, ok := metric.Data.(metricdata.Sum[int64])
			require.True(t, ok)
			require.Len(t, sum.DataPoints, 1)
			status, ok := sum.DataPoints[0].Attributes.Value(attribute.Key("status"))
			require.True(t, ok)
			return status.AsString()
		}
	}
	t.Fatal("sandbox.exec.total metric not found")
	return ""
}

func captureEndedSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	oldProvider := otel.GetTracerProvider()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(oldProvider)
		require.NoError(t, provider.Shutdown(context.Background()))
	})
	return recorder
}

func TestManagerResolveExecTimeout(t *testing.T) {
	mgr := NewManager(newMockRuntime(), nil, nil, ManagerConfig{
		ExecTimeoutSeconds:    30,
		MaxExecTimeoutSeconds: 600,
	})

	tests := []struct {
		name      string
		requested int
		want      int
		wantErr   bool
	}{
		{name: "default", requested: 0, want: 30},
		{name: "request override", requested: 300, want: 300},
		{name: "exceeds maximum", requested: 601, wantErr: true},
		{name: "negative", requested: -1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mgr.resolveExecTimeout(tt.requested)
			if tt.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, ErrInvalidExecTimeout)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestManagerResolveExecTimeoutWithoutMaximum(t *testing.T) {
	mgr := NewManager(newMockRuntime(), nil, nil, ManagerConfig{
		ExecTimeoutSeconds: 30,
	})

	got, err := mgr.resolveExecTimeout(601)

	require.NoError(t, err)
	assert.Equal(t, 601, got)
}

func TestManagerExecPropagatesEffectiveTimeout(t *testing.T) {
	tests := []struct {
		name      string
		requested int
		want      int
	}{
		{name: "default timeout", requested: 0, want: 30},
		{name: "request timeout", requested: 300, want: 300},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := newMockRuntime()
			mgr := newExecTestManager(rt, ManagerConfig{
				ExecTimeoutSeconds:    30,
				MaxExecTimeoutSeconds: 600,
			})

			_, err := mgr.Exec(context.Background(), "sandbox-test", runtime.ExecRequest{
				Command: "true",
				Timeout: tt.requested,
			})

			require.NoError(t, err)
			_, gotReq := rt.lastExec()
			assert.Equal(t, tt.want, gotReq.Timeout)
		})
	}
}

func TestManagerExecRejectsInvalidTimeout(t *testing.T) {
	rt := newMockRuntime()
	mgr := newExecTestManager(rt, ManagerConfig{
		ExecTimeoutSeconds:    30,
		MaxExecTimeoutSeconds: 600,
	})

	_, err := mgr.Exec(context.Background(), "sandbox-test", runtime.ExecRequest{
		Command: "true",
		Timeout: 601,
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidExecTimeout)
	ctx, _ := rt.lastExec()
	assert.Nil(t, ctx)
}

func TestManagerExecTimeout(t *testing.T) {
	rt := newMockRuntime()
	rt.execFunc = func(ctx context.Context, _ string, _ runtime.ExecRequest) (*runtime.ExecResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	mgr := newExecTestManager(rt, ManagerConfig{
		ExecTimeoutSeconds:    1,
		MaxExecTimeoutSeconds: 600,
	})

	_, err := mgr.Exec(context.Background(), "sandbox-test", runtime.ExecRequest{Command: "sleep"})

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrExecTimeout)
	sb, getErr := mgr.Get(context.Background(), "sandbox-test")
	require.NoError(t, getErr)
	assert.Equal(t, StateError, sb.State)
}

func TestManagerExecRuntimeDeadlineExceededIsTimeout(t *testing.T) {
	tests := []struct {
		name       string
		requested  int
		wantStatus string
	}{
		{name: "default", requested: 0, wantStatus: "timeout_default"},
		{name: "request", requested: 300, wantStatus: "timeout_request"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metricReader := captureExecMetricStatus(t)
			rt := newMockRuntime()
			rt.execFunc = func(context.Context, string, runtime.ExecRequest) (*runtime.ExecResult, error) {
				return nil, context.DeadlineExceeded
			}
			mgr := newExecTestManager(rt, ManagerConfig{
				ExecTimeoutSeconds:    30,
				MaxExecTimeoutSeconds: 600,
			})

			_, err := mgr.Exec(context.Background(), "sandbox-test", runtime.ExecRequest{
				Command: "sleep",
				Timeout: tt.requested,
			})

			require.Error(t, err)
			assert.ErrorIs(t, err, ErrExecTimeout)
			assert.Equal(t, tt.wantStatus, readExecMetricStatus(t, metricReader))
		})
	}
}

func TestManagerExecParentDeadlineIsNotExecutionTimeout(t *testing.T) {
	metricReader := captureExecMetricStatus(t)
	rt := newMockRuntime()
	rt.execFunc = func(context.Context, string, runtime.ExecRequest) (*runtime.ExecResult, error) {
		return nil, context.DeadlineExceeded
	}
	mgr := newExecTestManager(rt, ManagerConfig{
		ExecTimeoutSeconds:    30,
		MaxExecTimeoutSeconds: 600,
	})
	parent, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	_, err := mgr.Exec(parent, "sandbox-test", runtime.ExecRequest{Command: "sleep"})

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotErrorIs(t, err, ErrExecTimeout)
	assert.Equal(t, "caller_cancelled", readExecMetricStatus(t, metricReader))
}

func TestManagerExecUnlimitedTimeoutDoesNotCancelRuntime(t *testing.T) {
	rt := newMockRuntime()
	rt.execFunc = func(ctx context.Context, _ string, _ runtime.ExecRequest) (*runtime.ExecResult, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return &runtime.ExecResult{}, nil
	}
	mgr := newExecTestManager(rt, ManagerConfig{})

	_, err := mgr.Exec(context.Background(), "sandbox-test", runtime.ExecRequest{Command: "true"})

	require.NoError(t, err)
	_, gotReq := rt.lastExec()
	assert.Zero(t, gotReq.Timeout)
}

func TestManagerExecUnlimitedRuntimeDeadlineExceededIsNotTimeout(t *testing.T) {
	metricReader := captureExecMetricStatus(t)
	rt := newMockRuntime()
	rt.execFunc = func(context.Context, string, runtime.ExecRequest) (*runtime.ExecResult, error) {
		return nil, context.DeadlineExceeded
	}
	mgr := newExecTestManager(rt, ManagerConfig{})

	_, err := mgr.Exec(context.Background(), "sandbox-test", runtime.ExecRequest{Command: "sleep"})

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotErrorIs(t, err, ErrExecTimeout)
	assert.Equal(t, "error", readExecMetricStatus(t, metricReader))
}

func TestManagerExecCallerCancellationIsNotTimeout(t *testing.T) {
	rt := newMockRuntime()
	rt.execFunc = func(ctx context.Context, _ string, _ runtime.ExecRequest) (*runtime.ExecResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	mgr := newExecTestManager(rt, ManagerConfig{
		ExecTimeoutSeconds:    30,
		MaxExecTimeoutSeconds: 600,
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := mgr.Exec(ctx, "sandbox-test", runtime.ExecRequest{Command: "true"})

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, ErrExecTimeout)
}

func TestManagerExecStreamContextLivesUntilStreamCompletion(t *testing.T) {
	rt := newMockRuntime()
	runtimeEvents := make(chan runtime.StreamEvent)
	rt.execStreamFunc = func(context.Context, string, runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
		return runtimeEvents, nil
	}
	mgr := newExecTestManager(rt, ManagerConfig{
		ExecTimeoutSeconds:    30,
		MaxExecTimeoutSeconds: 600,
	})

	out, err := mgr.ExecStream(context.Background(), "sandbox-test", runtime.ExecRequest{Command: "true"})
	require.NoError(t, err)
	runtimeCtx, gotReq := rt.lastStream()
	assert.Equal(t, 30, gotReq.Timeout)
	select {
	case <-runtimeCtx.Done():
		t.Fatal("stream context was canceled when ExecStream returned")
	default:
	}

	close(runtimeEvents)
	for range out {
	}
	require.Eventually(t, func() bool {
		select {
		case <-runtimeCtx.Done():
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
}

func TestManagerExecStreamTimeoutEmitsErrorAndCloses(t *testing.T) {
	rt := newMockRuntime()
	rt.execStreamFunc = func(context.Context, string, runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
		return make(chan runtime.StreamEvent), nil
	}
	mgr := newExecTestManager(rt, ManagerConfig{
		ExecTimeoutSeconds:    1,
		MaxExecTimeoutSeconds: 600,
	})

	out, err := mgr.ExecStream(context.Background(), "sandbox-test", runtime.ExecRequest{Command: "sleep"})
	require.NoError(t, err)

	select {
	case event, ok := <-out:
		require.True(t, ok)
		assert.Equal(t, runtime.StreamError, event.Type)
		assert.Equal(t, ErrExecTimeout.Error(), event.Content)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stream timeout event")
	}
	_, ok := <-out
	assert.False(t, ok)
	sb, getErr := mgr.Get(context.Background(), "sandbox-test")
	require.NoError(t, getErr)
	assert.Equal(t, StateError, sb.State)
}

func TestManagerExecStreamNormalCompletionSetsIdle(t *testing.T) {
	rt := newMockRuntime()
	runtimeEvents := make(chan runtime.StreamEvent, 1)
	runtimeEvents <- runtime.StreamEvent{Type: runtime.StreamDone, Content: "0"}
	close(runtimeEvents)
	rt.execStreamFunc = func(context.Context, string, runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
		return runtimeEvents, nil
	}
	mgr := newExecTestManager(rt, ManagerConfig{
		ExecTimeoutSeconds:    30,
		MaxExecTimeoutSeconds: 600,
	})

	out, err := mgr.ExecStream(context.Background(), "sandbox-test", runtime.ExecRequest{Command: "true"})
	require.NoError(t, err)
	var got []runtime.StreamEvent
	for event := range out {
		got = append(got, event)
	}

	assert.Equal(t, []runtime.StreamEvent{{Type: runtime.StreamDone, Content: "0"}}, got)
	sb, getErr := mgr.Get(context.Background(), "sandbox-test")
	require.NoError(t, getErr)
	assert.Equal(t, StateIdle, sb.State)
}

func TestManagerExecStreamInitializationFailureCancelsContext(t *testing.T) {
	rt := newMockRuntime()
	rt.execStreamFunc = func(context.Context, string, runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
		return nil, errors.New("stream init failed")
	}
	mgr := newExecTestManager(rt, ManagerConfig{
		ExecTimeoutSeconds:    30,
		MaxExecTimeoutSeconds: 600,
	})

	_, err := mgr.ExecStream(context.Background(), "sandbox-test", runtime.ExecRequest{Command: "true"})

	require.EqualError(t, err, "stream init failed")
	runtimeCtx, _ := rt.lastStream()
	select {
	case <-runtimeCtx.Done():
	default:
		t.Fatal("stream context was not canceled after initialization failed")
	}
}

func TestManagerExecStreamRuntimeDeadlineExceededIsTimeout(t *testing.T) {
	metricReader := captureExecMetricStatus(t)
	rt := newMockRuntime()
	rt.execStreamFunc = func(context.Context, string, runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
		return nil, context.DeadlineExceeded
	}
	mgr := newExecTestManager(rt, ManagerConfig{
		ExecTimeoutSeconds:    30,
		MaxExecTimeoutSeconds: 600,
	})

	_, err := mgr.ExecStream(context.Background(), "sandbox-test", runtime.ExecRequest{
		Command: "sleep",
		Timeout: 300,
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrExecTimeout)
	assert.Equal(t, "timeout_request", readExecMetricStatus(t, metricReader))
}

func TestManagerExecStreamUnlimitedTimeoutDoesNotCancelRuntime(t *testing.T) {
	rt := newMockRuntime()
	runtimeEvents := make(chan runtime.StreamEvent)
	rt.execStreamFunc = func(context.Context, string, runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
		return runtimeEvents, nil
	}
	mgr := newExecTestManager(rt, ManagerConfig{})

	out, err := mgr.ExecStream(context.Background(), "sandbox-test", runtime.ExecRequest{Command: "true"})
	require.NoError(t, err)
	runtimeCtx, gotReq := rt.lastStream()
	assert.Zero(t, gotReq.Timeout)
	select {
	case <-runtimeCtx.Done():
		t.Fatal("unlimited stream context was canceled when ExecStream returned")
	default:
	}

	close(runtimeEvents)
	for range out {
	}
}

func TestManagerExecStreamUnlimitedRuntimeDeadlineExceededIsNotTimeout(t *testing.T) {
	metricReader := captureExecMetricStatus(t)
	rt := newMockRuntime()
	rt.execStreamFunc = func(context.Context, string, runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
		return nil, context.DeadlineExceeded
	}
	mgr := newExecTestManager(rt, ManagerConfig{})

	_, err := mgr.ExecStream(context.Background(), "sandbox-test", runtime.ExecRequest{Command: "sleep"})

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotErrorIs(t, err, ErrExecTimeout)
	assert.Equal(t, "error", readExecMetricStatus(t, metricReader))
}

func TestManagerExecStreamFullOutputBufferStillFinalizesTimeout(t *testing.T) {
	recorder := captureEndedSpans(t)
	rt := newMockRuntime()
	runtimeEvents := make(chan runtime.StreamEvent, 65)
	for i := 0; i < cap(runtimeEvents); i++ {
		runtimeEvents <- runtime.StreamEvent{Type: runtime.StreamStdout, Content: "output"}
	}
	rt.execStreamFunc = func(context.Context, string, runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
		return runtimeEvents, nil
	}
	mgr := newExecTestManager(rt, ManagerConfig{ExecTimeoutSeconds: 1})

	out, err := mgr.ExecStream(context.Background(), "sandbox-test", runtime.ExecRequest{Command: "sleep"})
	require.NoError(t, err)
	t.Cleanup(func() {
		close(runtimeEvents)
		for range out {
		}
	})

	require.Eventually(t, func() bool {
		for _, span := range recorder.Ended() {
			if span.Name() == "sandbox.Manager.ExecStream" {
				return true
			}
		}
		return false
	}, 3*time.Second, 10*time.Millisecond, "stream did not finalize while output buffer was full")

	var got []runtime.StreamEvent
	for event := range out {
		got = append(got, event)
	}
	require.NotEmpty(t, got)
	assert.Equal(t, runtime.StreamEvent{Type: runtime.StreamError, Content: ErrExecTimeout.Error()}, got[len(got)-1])
}

func TestManagerExecStreamDoneWinsDeadlineRace(t *testing.T) {
	rt := newMockRuntime()
	rt.execStreamFunc = func(ctx context.Context, _ string, _ runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
		<-ctx.Done()
		events := make(chan runtime.StreamEvent, 1)
		events <- runtime.StreamEvent{Type: runtime.StreamDone, Content: "0"}
		close(events)
		return events, nil
	}
	mgr := newExecTestManager(rt, ManagerConfig{ExecTimeoutSeconds: 1})

	out, err := mgr.ExecStream(context.Background(), "sandbox-test", runtime.ExecRequest{Command: "true"})
	require.NoError(t, err)
	var got []runtime.StreamEvent
	for event := range out {
		got = append(got, event)
	}

	assert.Equal(t, []runtime.StreamEvent{{Type: runtime.StreamDone, Content: "0"}}, got)
	sb, getErr := mgr.Get(context.Background(), "sandbox-test")
	require.NoError(t, getErr)
	assert.Equal(t, StateIdle, sb.State)
}

func TestManagerExecStreamRuntimeErrorIsImmediateUniqueTerminal(t *testing.T) {
	rt := newMockRuntime()
	runtimeEvents := make(chan runtime.StreamEvent, 1)
	runtimeEvents <- runtime.StreamEvent{Type: runtime.StreamError, Content: "runtime failed"}
	rt.execStreamFunc = func(context.Context, string, runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
		return runtimeEvents, nil
	}
	mgr := newExecTestManager(rt, ManagerConfig{ExecTimeoutSeconds: 1})

	out, err := mgr.ExecStream(context.Background(), "sandbox-test", runtime.ExecRequest{Command: "false"})
	require.NoError(t, err)
	event, ok := <-out
	require.True(t, ok)
	assert.Equal(t, runtime.StreamEvent{Type: runtime.StreamError, Content: "runtime failed"}, event)
	select {
	case _, ok = <-out:
		assert.False(t, ok)
	case <-time.After(250 * time.Millisecond):
		t.Fatal("stream did not close immediately after runtime error terminal")
	}
	sb, getErr := mgr.Get(context.Background(), "sandbox-test")
	require.NoError(t, getErr)
	assert.Equal(t, StateError, sb.State)
}

func TestManager_CreateEphemeralSandbox(t *testing.T) {
	rt := newMockRuntime()
	mgr := NewManager(rt, nil, nil, ManagerConfig{
		PoolConfig:     PoolConfig{MinSize: 2, MaxSize: 10, Image: "sandbox:latest"},
		DefaultTimeout: 30,
	})

	ctx := context.Background()
	mgr.Start(ctx)
	defer mgr.Stop(ctx)

	time.Sleep(100 * time.Millisecond)

	sb, err := mgr.Create(ctx, SandboxConfig{
		Mode:    ModeEphemeral,
		Timeout: 30,
	})
	require.NoError(t, err)
	assert.Equal(t, StateReady, sb.State)
	assert.Equal(t, ModeEphemeral, sb.Config.Mode)
}

func TestManager_CreatePersistentSandbox(t *testing.T) {
	rt := newMockRuntime()
	mgr := NewManager(rt, nil, nil, ManagerConfig{
		PoolConfig:     PoolConfig{MinSize: 1, MaxSize: 5, Image: "sandbox:latest"},
		DefaultTimeout: 60,
	})

	ctx := context.Background()
	mgr.Start(ctx)
	defer mgr.Stop(ctx)

	time.Sleep(100 * time.Millisecond)

	sb, err := mgr.Create(ctx, SandboxConfig{
		Mode:    ModePersistent,
		Timeout: 60,
	})
	require.NoError(t, err)
	assert.Equal(t, StateReady, sb.State)
	assert.Equal(t, ModePersistent, sb.Config.Mode)

	// Should be retrievable by ID
	got, err := mgr.Get(ctx, sb.ID)
	require.NoError(t, err)
	assert.Equal(t, sb.ID, got.ID)
}

func TestManager_Destroy(t *testing.T) {
	rt := newMockRuntime()
	mgr := NewManager(rt, nil, nil, ManagerConfig{
		PoolConfig:     PoolConfig{MinSize: 1, MaxSize: 5, Image: "sandbox:latest"},
		DefaultTimeout: 30,
	})

	ctx := context.Background()
	mgr.Start(ctx)
	defer mgr.Stop(ctx)

	time.Sleep(100 * time.Millisecond)

	sb, err := mgr.Create(ctx, SandboxConfig{
		Mode: ModeEphemeral,
	})
	require.NoError(t, err)

	err = mgr.Destroy(ctx, sb.ID)
	require.NoError(t, err)

	_, err = mgr.Get(ctx, sb.ID)
	assert.Error(t, err)
}
