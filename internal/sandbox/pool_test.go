package sandbox

import (
	"context"
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
	mu        sync.Mutex
	created   int
	removed   int
	sandboxes map[string]*runtime.SandboxInfo
}

func newMockRuntime() *mockRuntime {
	return &mockRuntime{sandboxes: make(map[string]*runtime.SandboxInfo)}
}

func (m *mockRuntime) CreateSandbox(_ context.Context, spec runtime.SandboxSpec) (*runtime.SandboxInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.created++
	info := &runtime.SandboxInfo{
		ID:        spec.ID,
		RuntimeID: "container-" + spec.ID,
		State:     "running",
		CreatedAt: time.Now(),
	}
	m.sandboxes[spec.ID] = info
	return info, nil
}

func (m *mockRuntime) StartSandbox(_ context.Context, _ string) error { return nil }
func (m *mockRuntime) StopSandbox(_ context.Context, _ string) error  { return nil }

func (m *mockRuntime) RemoveSandbox(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed++
	delete(m.sandboxes, id)
	return nil
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

func (m *mockRuntime) Exec(context.Context, string, runtime.ExecRequest) (*runtime.ExecResult, error) {
	return &runtime.ExecResult{}, nil
}

func (m *mockRuntime) ExecStream(context.Context, string, runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
	return nil, nil
}

func (m *mockRuntime) UploadFile(_ context.Context, _ string, _ string, _ io.Reader) error {
	return nil
}

func (m *mockRuntime) DownloadFile(_ context.Context, _ string, _ string) (io.ReadCloser, error) {
	return nil, nil
}

func (m *mockRuntime) ListFiles(context.Context, string, string) ([]runtime.FileInfo, error) {
	return nil, nil
}

func TestPool_Acquire(t *testing.T) {
	rt := newMockRuntime()
	pool := NewPool(rt, PoolConfig{
		Language: LangPython,
		MinSize:  2,
		MaxSize:  10,
		Image:    "sandbox-python:latest",
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

func TestPool_Release(t *testing.T) {
	rt := newMockRuntime()
	pool := NewPool(rt, PoolConfig{
		Language: LangPython,
		MinSize:  2,
		MaxSize:  10,
		Image:    "sandbox-python:latest",
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
