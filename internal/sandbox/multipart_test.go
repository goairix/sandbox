package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// inMemoryStore is a simple in-memory state.Store for testing.
type inMemoryStore struct {
	data map[string][]byte
}

func newInMemoryStore() *inMemoryStore {
	return &inMemoryStore{data: make(map[string][]byte)}
}

func (s *inMemoryStore) Set(_ context.Context, key string, value []byte, _ time.Duration) error {
	s.data[key] = value
	return nil
}
func (s *inMemoryStore) Get(_ context.Context, key string) ([]byte, error) {
	return s.data[key], nil
}
func (s *inMemoryStore) Delete(_ context.Context, key string) error {
	delete(s.data, key)
	return nil
}
func (s *inMemoryStore) Exists(_ context.Context, key string) (bool, error) {
	_, ok := s.data[key]
	return ok, nil
}
func (s *inMemoryStore) SetNX(_ context.Context, key string, value []byte, _ time.Duration) (bool, error) {
	if _, ok := s.data[key]; ok {
		return false, nil
	}
	s.data[key] = value
	return true, nil
}
func (s *inMemoryStore) Keys(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

// newTestManagerWithStore creates a Manager with an in-memory multipart store, no pool warm-up.
func newTestManagerWithStore(t *testing.T) (*Manager, *inMemoryStore) {
	t.Helper()
	rt := newMockRuntime()
	mgr := NewManager(rt, nil, nil, ManagerConfig{
		PoolConfig:     PoolConfig{MinSize: 0, MaxSize: 0, Image: "sandbox:latest"},
		DefaultTimeout: 30,
	})
	store := newInMemoryStore()
	mgr.SetMultipartStore(store)

	// Register a fake sandbox directly to avoid pool dependency.
	sb := &Sandbox{
		ID:        "test-sb",
		RuntimeID: "container-test-sb",
		State:     StateReady,
		Config:    SandboxConfig{Mode: ModeEphemeral},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	mgr.mu.Lock()
	mgr.sandboxes[sb.ID] = sb
	mgr.mu.Unlock()

	return mgr, store
}

func TestInitMultipartUpload(t *testing.T) {
	mgr, store := newTestManagerWithStore(t)

	uploadID, err := mgr.InitMultipartUpload(context.Background(), "test-sb", "/workspace/big.bin", 3)
	require.NoError(t, err)
	assert.NotEmpty(t, uploadID)

	// State should be persisted in Redis store.
	key := multipartKey("test-sb", uploadID)
	data, err := store.Get(context.Background(), key)
	require.NoError(t, err)
	assert.NotNil(t, data)
}

func TestUploadChunk_OutOfOrder(t *testing.T) {
	mgr, _ := newTestManagerWithStore(t)

	uploadID, err := mgr.InitMultipartUpload(context.Background(), "test-sb", "/workspace/big.bin", 3)
	require.NoError(t, err)

	// chunk_index=1 before chunk_index=0 should fail.
	_, _, err = mgr.UploadChunk(context.Background(), "test-sb", uploadID, 1, strings.NewReader("data"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected chunk_index 0")
}

func TestCompleteMultipartUpload_Incomplete(t *testing.T) {
	mgr, _ := newTestManagerWithStore(t)

	uploadID, err := mgr.InitMultipartUpload(context.Background(), "test-sb", "/workspace/big.bin", 3)
	require.NoError(t, err)

	_, _, err = mgr.CompleteMultipartUpload(context.Background(), "test-sb", uploadID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "incomplete upload")
}

func TestGetMultipartStatus(t *testing.T) {
	mgr, _ := newTestManagerWithStore(t)

	uploadID, err := mgr.InitMultipartUpload(context.Background(), "test-sb", "/workspace/big.bin", 5)
	require.NoError(t, err)

	st, err := mgr.GetMultipartStatus(context.Background(), "test-sb", uploadID)
	require.NoError(t, err)
	assert.Equal(t, uploadID, st.UploadID)
	assert.Equal(t, "/workspace/big.bin", st.DestPath)
	assert.Equal(t, 5, st.TotalChunks)
	assert.Equal(t, 0, st.ReceivedChunks)
}

func TestCancelMultipartUpload(t *testing.T) {
	mgr, store := newTestManagerWithStore(t)

	uploadID, err := mgr.InitMultipartUpload(context.Background(), "test-sb", "/workspace/big.bin", 3)
	require.NoError(t, err)

	err = mgr.CancelMultipartUpload(context.Background(), "test-sb", uploadID)
	require.NoError(t, err)

	// State should be removed from store.
	key := multipartKey("test-sb", uploadID)
	data, err := store.Get(context.Background(), key)
	require.NoError(t, err)
	assert.Nil(t, data)
}

func TestGetMultipartStatus_NotFound(t *testing.T) {
	mgr, _ := newTestManagerWithStore(t)

	_, err := mgr.GetMultipartStatus(context.Background(), "test-sb", "nonexistent-id")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "upload not found")
}
