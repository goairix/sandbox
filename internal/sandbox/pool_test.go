package sandbox

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/runtime"
)

// mockRuntime is a simple mock for testing pool logic.
type mockRuntime struct {
	mu             sync.Mutex
	created        int
	removed        int
	sandboxes      map[string]*runtime.SandboxInfo
	execContext    context.Context
	execRequest    runtime.ExecRequest
	execFunc       func(context.Context, string, runtime.ExecRequest) (*runtime.ExecResult, error)
	streamContext  context.Context
	streamRequest  runtime.ExecRequest
	execStreamFunc func(context.Context, string, runtime.ExecRequest) (<-chan runtime.StreamEvent, error)
	createdSpec    runtime.SandboxSpec
	uploadSize     int64
	prepareSeq     int
	prepareErr     error
	prepareEntered chan struct{}
	prepareRelease chan struct{}
	prepareSignal  sync.Once
	healthFailures map[string]error
	removeFailures map[string]error
	removedIDs     map[string]int
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
	m.mu.Unlock()
	return m.RemoveSandbox(ctx, runtimeID)
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

func (m *mockRuntime) ExecPipe(context.Context, string, []string, io.Reader) error {
	return nil
}

func (m *mockRuntime) UpdateLabels(context.Context, string, map[string]*string) error {
	return nil
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

func (m *mockRuntime) AuthorizeWorkspaceMount(context.Context, string, runtime.WorkspaceMountAuthorization) error {
	return nil
}

func (m *mockRuntime) WaitSandboxReady(context.Context, string) (*runtime.SandboxInfo, error) {
	return &runtime.SandboxInfo{State: "running"}, nil
}

func (m *mockRuntime) PreparedSandboxHealth(_ context.Context, id, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.healthFailures[id]
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

func (m *mockRuntime) WorkspaceHealth(context.Context, string) (*runtime.WorkspaceHealth, error) {
	return &runtime.WorkspaceHealth{Ready: true}, nil
}

func (m *mockRuntime) QuiesceWorkspace(context.Context, string) (runtime.WorkspaceQuiesceToken, error) {
	return runtime.WorkspaceQuiesceToken{}, nil
}

func (m *mockRuntime) ResumeWorkspace(context.Context, string, runtime.WorkspaceQuiesceToken) error {
	return nil
}

func (m *mockRuntime) FlushWorkspace(context.Context, string) error {
	return nil
}

func (m *mockRuntime) UploadFile(_ context.Context, _ string, _ string, size int64, _ io.Reader) error {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	return nil, nil
}

func (m *mockRuntime) UploadArchive(context.Context, string, string, io.Reader) error {
	return nil
}

func (m *mockRuntime) DownloadDir(context.Context, string, string) (io.ReadCloser, error) {
	return nil, nil
}

func (m *mockRuntime) UpdateNetwork(context.Context, string, bool, []string, bool) error {
	return nil
}

func (m *mockRuntime) RenameSandbox(context.Context, string, string) error {
	return nil
}

func (m *mockRuntime) ListSandboxes(_ context.Context, _ map[string]string) ([]runtime.SandboxInfo, error) {
	return nil, nil
}

func (m *mockRuntime) IsStateful() bool { return false }

func (m *mockRuntime) ListFilesRecursive(context.Context, string, string, int, int, int) (*runtime.FileListResult, error) {
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
