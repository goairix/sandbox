package sandbox

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage/state"
)

type memoryActiveRepository struct {
	mu      sync.Mutex
	records map[string]state.ActiveSandboxRecord
	ops     map[string]map[string]state.ActiveSandboxOperation
}

func newMemoryActiveRepository() *memoryActiveRepository {
	return &memoryActiveRepository{records: map[string]state.ActiveSandboxRecord{}, ops: map[string]map[string]state.ActiveSandboxOperation{}}
}

func (r *memoryActiveRepository) Publish(_ context.Context, record state.ActiveSandboxRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.records[record.SandboxID]; ok {
		return state.ErrActiveSandboxConflict
	}
	r.records[record.SandboxID] = record
	return nil
}
func (r *memoryActiveRepository) Load(_ context.Context, id string) (*state.ActiveSandboxRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.records[id]
	if !ok {
		return nil, nil
	}
	copy := record
	copy.Snapshot = append([]byte(nil), record.Snapshot...)
	return &copy, nil
}
func (r *memoryActiveRepository) Activate(_ context.Context, id string, revision uint64, snapshot json.RawMessage) (*state.ActiveSandboxRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.records[id]
	if !ok {
		return nil, nil
	}
	if record.Revision != revision || record.Phase != state.ActiveSandboxPublishing {
		return nil, state.ErrActiveSandboxConflict
	}
	record.Phase = state.ActiveSandboxActive
	record.Revision++
	record.Snapshot = append([]byte(nil), snapshot...)
	record.UpdatedAt = time.Now()
	r.records[id] = record
	return &record, nil
}
func (r *memoryActiveRepository) Update(_ context.Context, id string, revision uint64, snapshot json.RawMessage) (*state.ActiveSandboxRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.records[id]
	if !ok {
		return nil, nil
	}
	if record.Revision != revision || record.Phase != state.ActiveSandboxActive {
		return nil, state.ErrActiveSandboxConflict
	}
	record.Revision++
	record.Snapshot = append([]byte(nil), snapshot...)
	record.UpdatedAt = time.Now()
	r.records[id] = record
	return &record, nil
}
func (r *memoryActiveRepository) BeginOperation(_ context.Context, id, token string, kind state.ActiveOperationKind, ttl time.Duration) (*state.ActiveSandboxRecord, *state.ActiveSandboxOperation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.records[id]
	if !ok {
		return nil, nil, nil
	}
	if record.Phase != state.ActiveSandboxActive {
		return nil, nil, state.ErrActiveSandboxAdmissionClosed
	}
	if r.ops[id] == nil {
		r.ops[id] = map[string]state.ActiveSandboxOperation{}
	}
	op := state.ActiveSandboxOperation{SandboxID: id, Token: token, Generation: record.Generation, Kind: kind, ExpiresAt: time.Now().Add(ttl)}
	r.ops[id][token] = op
	copy := record
	return &copy, &op, nil
}
func (r *memoryActiveRepository) RenewOperation(_ context.Context, op state.ActiveSandboxOperation, ttl time.Duration) (*state.ActiveSandboxOperation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, ok := r.ops[op.SandboxID][op.Token]
	if !ok || current.Generation != op.Generation {
		return nil, state.ErrActiveSandboxStaleToken
	}
	current.ExpiresAt = time.Now().Add(ttl)
	r.ops[op.SandboxID][op.Token] = current
	return &current, nil
}
func (r *memoryActiveRepository) EndOperation(_ context.Context, op state.ActiveSandboxOperation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.ops[op.SandboxID], op.Token)
	return nil
}
func (r *memoryActiveRepository) BeginDestroy(_ context.Context, id string) (*state.ActiveSandboxRecord, int64, bool, error) {
	return nil, 0, false, nil
}
func (r *memoryActiveRepository) LiveOperations(_ context.Context, id string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return int64(len(r.ops[id])), nil
}
func (r *memoryActiveRepository) Checkpoint(_ context.Context, id string, revision uint64, checkpoint string) (*state.ActiveSandboxRecord, error) {
	return nil, nil
}
func (r *memoryActiveRepository) Delete(_ context.Context, id string, revision uint64, generation int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.records, id)
	delete(r.ops, id)
	return nil
}
func (r *memoryActiveRepository) AcquireController(_ context.Context, lease state.ActiveSandboxControllerLease, ttl time.Duration) (*state.ActiveSandboxControllerLease, bool, error) {
	return nil, false, nil
}
func (r *memoryActiveRepository) RenewController(_ context.Context, lease state.ActiveSandboxControllerLease, ttl time.Duration) (*state.ActiveSandboxControllerLease, error) {
	return nil, state.ErrActiveSandboxStaleToken
}
func (r *memoryActiveRepository) ReleaseController(_ context.Context, lease state.ActiveSandboxControllerLease) error {
	return nil
}
func (r *memoryActiveRepository) Scan(_ context.Context, cursor uint64, count int64) (state.ActiveSandboxPage, error) {
	return state.ActiveSandboxPage{}, nil
}
func (r *memoryActiveRepository) Ping(context.Context) error { return nil }

func TestKubernetesActivePublicationAllowsCrossReplicaOrdinaryEphemeralOperations(t *testing.T) {
	rt := newMockRuntime()
	repository := newMemoryActiveRepository()
	cfg := ManagerConfig{RuntimeType: "kubernetes", ActiveSandboxes: repository, InstanceID: "api-a", PoolConfig: PoolConfig{Image: "sandbox:latest"}}
	creator := NewManager(rt, nil, nil, cfg)
	cfg.InstanceID = "api-b"
	peer := NewManager(rt, nil, nil, cfg)

	sb, err := creator.Create(context.Background(), SandboxConfig{Mode: ModeEphemeral, Network: NetworkConfig{Enabled: true}})
	require.NoError(t, err)
	record, err := repository.Load(context.Background(), sb.ID)
	require.NoError(t, err)
	require.NotNil(t, record)
	assert.Equal(t, state.ActiveSandboxActive, record.Phase)
	assert.Equal(t, sb.RuntimeUID, record.RuntimeUID)

	got, err := peer.Get(context.Background(), sb.ID)
	require.NoError(t, err)
	assert.Equal(t, sb.RuntimeID, got.RuntimeID)
	result, err := peer.Exec(context.Background(), sb.ID, runtime.ExecRequest{Command: "echo shared"})
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
}
