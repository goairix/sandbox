package sandbox

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/runtime"
)

// mockRuntime is a simple mock for testing pool logic.
type mockRuntime struct {
	mu                sync.Mutex
	created           int
	removed           int
	sandboxes         map[string]*runtime.SandboxInfo
	execContext       context.Context
	execRequest       runtime.ExecRequest
	execFunc          func(context.Context, string, runtime.ExecRequest) (*runtime.ExecResult, error)
	streamContext     context.Context
	streamRequest     runtime.ExecRequest
	execStreamFunc    func(context.Context, string, runtime.ExecRequest) (<-chan runtime.StreamEvent, error)
	execPipeCalls     int
	createdSpec       runtime.SandboxSpec
	uploadSize        int64
	uploadCalls       int
	listFiles         []runtime.FileInfo
	listRecursiveFunc func(page, pageSize int) *runtime.FileListResult
	reservedFileCount int
	quiesceCalls      int
	flushCalls        int
	resumeCalls       int
	quiesceErr        error
	flushErr          error
	resumeErr         error
	quiesceToken      runtime.WorkspaceQuiesceToken
	workspaceOps      []string
	prepareSeq        int
	prepareErr        error
	prepareEntered    chan struct{}
	prepareRelease    chan struct{}
	prepareSignal     sync.Once
	healthFailures    map[string]error
	removeFailures    map[string]error
	removedIDs        map[string]int
	removeEntered     chan string
	removeRelease     chan struct{}
	downloadDirErr    error
	listedSandboxes   []runtime.SandboxInfo
	listLabels        map[string]string
	updateLabelsErr   error
	updateLabelsID    string
	updateLabelsCalls int
	updatedLabels     map[string]*string
}

func newMockRuntime() *mockRuntime {
	return &mockRuntime{
		sandboxes:      make(map[string]*runtime.SandboxInfo),
		healthFailures: make(map[string]error),
		removeFailures: make(map[string]error),
		removedIDs:     make(map[string]int),
	}
}

func (m *mockRuntime) CreateSandbox(_ context.Context, spec runtime.SandboxSpec) (*runtime.SandboxInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.created++
	m.createdSpec = spec
	info := &runtime.SandboxInfo{
		ID:         spec.ID,
		RuntimeID:  "container-" + spec.ID,
		RuntimeUID: "container-" + spec.ID,
		State:      "running",
		CreatedAt:  time.Now(),
	}
	m.sandboxes[info.RuntimeID] = info
	return info, nil
}

func (m *mockRuntime) lastCreatedSpec() runtime.SandboxSpec {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createdSpec
}

func (m *mockRuntime) StartSandbox(_ context.Context, _ string) error { return nil }
func (m *mockRuntime) StopSandbox(_ context.Context, _ string) error  { return nil }

func (m *mockRuntime) RemoveSandbox(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.removeFailures[id]; err != nil {
		return err
	}
	m.removed++
	m.removedIDs[id]++
	delete(m.sandboxes, id)
	return nil
}

func (m *mockRuntime) RemovePreparedSandbox(ctx context.Context, runtimeID, runtimeUID string) error {
	m.mu.Lock()
	info, ok := m.sandboxes[runtimeID]
	if ok && info.RuntimeUID != runtimeUID {
		m.mu.Unlock()
		return runtime.ErrNotFound
	}
	entered, release := m.removeEntered, m.removeRelease
	m.mu.Unlock()
	if entered != nil {
		select {
		case entered <- runtimeID:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return m.RemoveSandbox(ctx, runtimeID)
}

func (m *mockRuntime) blockPreparedRemovals(count int) (<-chan string, chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeEntered = make(chan string, count)
	m.removeRelease = make(chan struct{})
	return m.removeEntered, m.removeRelease
}

func (m *mockRuntime) GetSandbox(_ context.Context, id string) (*runtime.SandboxInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	info, ok := m.sandboxes[id]
	if !ok {
		return nil, nil
	}
	return info, nil
}

func (m *mockRuntime) Exec(ctx context.Context, id string, req runtime.ExecRequest) (*runtime.ExecResult, error) {
	m.mu.Lock()
	m.execContext = ctx
	m.execRequest = req
	execFunc := m.execFunc
	m.mu.Unlock()
	if execFunc != nil {
		return execFunc(ctx, id, req)
	}
	return &runtime.ExecResult{}, nil
}

func (m *mockRuntime) ExecStream(ctx context.Context, id string, req runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
	m.mu.Lock()
	m.streamContext = ctx
	m.streamRequest = req
	execStreamFunc := m.execStreamFunc
	m.mu.Unlock()
	if execStreamFunc != nil {
		return execStreamFunc(ctx, id, req)
	}
	ch := make(chan runtime.StreamEvent)
	close(ch)
	return ch, nil
}

func (m *mockRuntime) lastExec() (context.Context, runtime.ExecRequest) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.execContext, m.execRequest
}

func (m *mockRuntime) lastStream() (context.Context, runtime.ExecRequest) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.streamContext, m.streamRequest
}

func (m *mockRuntime) ExecPipe(_ context.Context, _ string, _ []string, input io.Reader) error {
	_, err := io.Copy(io.Discard, input)
	m.mu.Lock()
	m.execPipeCalls++
	m.mu.Unlock()
	return err
}

func (m *mockRuntime) UpdateLabels(_ context.Context, id string, labels map[string]*string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateLabelsCalls++
	m.updateLabelsID = id
	m.updatedLabels = labels
	return m.updateLabelsErr
}

func (m *mockRuntime) PrepareSandbox(ctx context.Context, spec runtime.SandboxSpec) (*runtime.SandboxInfo, error) {
	m.mu.Lock()
	if m.prepareErr != nil {
		err := m.prepareErr
		m.mu.Unlock()
		return nil, err
	}
	entered, release := m.prepareEntered, m.prepareRelease
	m.mu.Unlock()
	if entered != nil {
		m.prepareSignal.Do(func() { close(entered) })
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	m.mu.Lock()
	m.prepareSeq++
	seq := m.prepareSeq
	m.created++
	m.createdSpec = spec
	info := &runtime.SandboxInfo{
		ID:         spec.ID,
		RuntimeID:  fmt.Sprintf("prepared-runtime-%d", seq),
		RuntimeUID: fmt.Sprintf("prepared-uid-%d", seq),
		State:      "running",
		CreatedAt:  time.Now(),
	}
	m.sandboxes[info.RuntimeID] = info
	m.mu.Unlock()
	return info, nil
}

func (m *mockRuntime) blockPrepare() (<-chan struct{}, chan<- struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prepareEntered = make(chan struct{})
	m.prepareRelease = make(chan struct{})
	return m.prepareEntered, m.prepareRelease
}

func (m *mockRuntime) AuthorizeWorkspaceMount(context.Context, runtime.RuntimeRef, runtime.WorkspaceMountAuthorization) error {
	return nil
}

func (m *mockRuntime) WaitSandboxReady(_ context.Context, ref runtime.RuntimeRef, _ int64) (*runtime.SandboxInfo, error) {
	return &runtime.SandboxInfo{RuntimeID: ref.ID, RuntimeUID: ref.UID, State: "running"}, nil
}

func (m *mockRuntime) PreparedSandboxHealth(_ context.Context, ref runtime.RuntimeRef, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.healthFailures[ref.ID]
}

func (m *mockRuntime) failPreparedHealth(id string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.healthFailures[id] = err
}

func (m *mockRuntime) failRemove(id string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeFailures[id] = err
}

func (m *mockRuntime) wasRemoved(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.removedIDs[id] > 0
}

func (m *mockRuntime) prepareCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.prepareSeq
}

func (m *mockRuntime) WorkspaceHealth(context.Context, runtime.RuntimeRef) (*runtime.WorkspaceHealth, error) {
	return &runtime.WorkspaceHealth{Ready: true}, nil
}

func (m *mockRuntime) QuiesceWorkspace(context.Context, runtime.RuntimeRef, int64) (runtime.WorkspaceQuiesceToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.quiesceCalls++
	m.workspaceOps = append(m.workspaceOps, "quiesce")
	if m.quiesceErr != nil {
		return runtime.WorkspaceQuiesceToken{}, m.quiesceErr
	}
	if m.quiesceToken.Opaque != "" {
		return m.quiesceToken, nil
	}
	return runtime.WorkspaceQuiesceToken{RuntimeUID: "prepared-uid-1", Generation: 1, Opaque: "token-1"}, nil
}

func (m *mockRuntime) ResumeWorkspace(context.Context, runtime.RuntimeRef, runtime.WorkspaceQuiesceToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resumeCalls++
	m.workspaceOps = append(m.workspaceOps, "resume")
	return m.resumeErr
}

func (m *mockRuntime) FlushWorkspace(context.Context, runtime.RuntimeRef, int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flushCalls++
	m.workspaceOps = append(m.workspaceOps, "flush")
	return m.flushErr
}

func (m *mockRuntime) UploadFile(_ context.Context, _ string, _ string, size int64, _ io.Reader) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.uploadCalls++
	m.uploadSize = size
	return nil
}

func (m *mockRuntime) lastUploadSize() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.uploadSize
}

func (m *mockRuntime) DownloadFile(_ context.Context, _ string, _ string) (io.ReadCloser, error) {
	return nil, nil
}

func (m *mockRuntime) ListFiles(context.Context, string, string) ([]runtime.FileInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]runtime.FileInfo(nil), m.listFiles...), nil
}

func (m *mockRuntime) UploadArchive(context.Context, string, string, io.Reader) error {
	return nil
}

func (m *mockRuntime) DownloadDir(context.Context, string, string) (io.ReadCloser, error) {
	m.mu.Lock()
	err := m.downloadDirErr
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return io.NopCloser(strings.NewReader("")), nil
}

func (m *mockRuntime) UpdateNetwork(context.Context, string, bool, []string, bool) error {
	return nil
}

func (m *mockRuntime) UpdateFUSENetwork(context.Context, runtime.RuntimeRef, bool, []string, bool) error {
	return nil
}

func (m *mockRuntime) RenameSandbox(context.Context, string, string) error {
	return nil
}

func (m *mockRuntime) ListSandboxes(_ context.Context, labels map[string]string) ([]runtime.SandboxInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listLabels = make(map[string]string, len(labels))
	for key, value := range labels {
		m.listLabels[key] = value
	}
	return append([]runtime.SandboxInfo(nil), m.listedSandboxes...), nil
}

func (m *mockRuntime) IsStateful() bool { return false }

func (m *mockRuntime) ListFilesRecursive(_ context.Context, _, _ string, _ int, page, pageSize int) (*runtime.FileListResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listRecursiveFunc != nil {
		return m.listRecursiveFunc(page, pageSize), nil
	}
	return nil, nil
}
func (m *mockRuntime) ReadFileLines(context.Context, string, string, int, int) (*runtime.FileLineResult, error) {
	return nil, nil
}
func (m *mockRuntime) EditFile(context.Context, string, string, string, string, bool) error {
	return nil
}
func (m *mockRuntime) EditFileLines(context.Context, string, string, int, int, string) error {
	return nil
}
func (m *mockRuntime) GlobInfo(context.Context, string, string) ([]runtime.FileContent, error) {
	return nil, nil
}
func (m *mockRuntime) GlobFiles(context.Context, string, string, string, int, int) (*runtime.FileListResult, error) {
	return nil, nil
}
func (m *mockRuntime) DownloadFiles(context.Context, string, []string) ([]runtime.FileContent, error) {
	return nil, nil
}

func (m *mockRuntime) FileExists(_ context.Context, _ string, _ string) error {
	return nil
}

func (m *mockRuntime) CountReservedFiles(context.Context, string, string, int, string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reservedFileCount, nil
}

func (m *mockRuntime) ReadFileContent(_ context.Context, _ string, _ string) (io.ReadCloser, error) {
	return nil, nil
}

var _ runtime.Runtime = (*mockRuntime)(nil)

func TestPool_Acquire(t *testing.T) {
	rt := newMockRuntime()
	pool := NewPool(rt, PoolConfig{
		MinSize: 2,
		MaxSize: 10,
		Image:   "sandbox:latest",
	})

	ctx := context.Background()

	// Warm up pool
	pool.WarmUp(ctx)
	time.Sleep(100 * time.Millisecond) // let async creation finish

	assert.Equal(t, 2, pool.Size())

	// Acquire one
	info, err := pool.Acquire(ctx)
	require.NoError(t, err)
	assert.NotEmpty(t, info.ID)
	assert.Equal(t, 1, pool.Size())
}

func TestPoolCreateWarmPropagatesTmpDisk(t *testing.T) {
	rt := newMockRuntime()
	pool := NewPool(rt, PoolConfig{
		Image:   "sandbox:latest",
		TmpDisk: "150Mi",
	})

	_, err := pool.createWarm(context.Background())

	require.NoError(t, err)
	assert.Equal(t, "150Mi", rt.lastCreatedSpec().TmpDisk)
}

func TestPool_Release(t *testing.T) {
	rt := newMockRuntime()
	pool := NewPool(rt, PoolConfig{
		MinSize: 2,
		MaxSize: 10,
		Image:   "sandbox:latest",
	})

	ctx := context.Background()
	pool.WarmUp(ctx)
	time.Sleep(100 * time.Millisecond)

	info, err := pool.Acquire(ctx)
	require.NoError(t, err)

	// Release should destroy (not return to pool — used containers are dirty)
	pool.Release(ctx, info.ID)

	rt.mu.Lock()
	assert.GreaterOrEqual(t, rt.removed, 1)
	rt.mu.Unlock()
}
