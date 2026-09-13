package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage"
	"github.com/goairix/sandbox/internal/storage/state"
)

const ephemeralLifecycleKeyPrefix = "sandbox:ephemeral:v1:"

var (
	ErrEphemeralLifecycleConflict = errors.New("ephemeral lifecycle changed")
	ErrEphemeralLifecycleInvalid  = errors.New("invalid ephemeral lifecycle")
)

type EphemeralFinalizationState string

const (
	EphemeralActive          EphemeralFinalizationState = "active"
	EphemeralFinalizing      EphemeralFinalizationState = "finalizing"
	EphemeralRemovingRuntime EphemeralFinalizationState = "removing-runtime"
	EphemeralReleasingLease  EphemeralFinalizationState = "releasing-lease"
)

type EphemeralLifecycleRecord struct {
	Version          int                        `json:"version"`
	SandboxID        string                     `json:"sandbox_id"`
	RuntimeID        string                     `json:"runtime_id"`
	RuntimeUID       string                     `json:"runtime_uid"`
	WorkspacePath    string                     `json:"workspace_path"`
	MountType        WorkspaceMountType         `json:"mount_type"`
	Owner            WorkspaceOwner             `json:"owner"`
	PreparationID    string                     `json:"preparation_id,omitempty"`
	PoolKey          string                     `json:"pool_key,omitempty"`
	ReservationToken string                     `json:"reservation_token,omitempty"`
	PoolRevision     uint64                     `json:"pool_revision,omitempty"`
	State            EphemeralFinalizationState `json:"state"`
	Revision         uint64                     `json:"revision"`
	UpdatedAt        time.Time                  `json:"updated_at"`
}

type EphemeralLifecycleStore struct {
	store state.AtomicStore
}

func NewEphemeralLifecycleStore(store state.AtomicStore) *EphemeralLifecycleStore {
	return &EphemeralLifecycleStore{store: store}
}

func (s *EphemeralLifecycleStore) Create(ctx context.Context, record EphemeralLifecycleRecord) error {
	if s == nil || s.store == nil || validateEphemeralLifecycle(record) != nil {
		return ErrEphemeralLifecycleInvalid
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal ephemeral lifecycle: %w", err)
	}
	created, err := s.store.SetNX(ctx, ephemeralLifecycleKeyPrefix+record.SandboxID, raw, 0)
	if err != nil {
		current, verifyErr := s.store.Get(context.WithoutCancel(ctx), ephemeralLifecycleKeyPrefix+record.SandboxID)
		if verifyErr == nil && bytes.Equal(current, raw) {
			return nil
		}
		return errors.Join(ErrEphemeralLifecycleConflict, err, verifyErr)
	}
	if !created {
		return ErrEphemeralLifecycleConflict
	}
	return nil
}

func (s *EphemeralLifecycleStore) Load(ctx context.Context, sandboxID string) (*EphemeralLifecycleRecord, error) {
	if s == nil || s.store == nil || validateOpaqueText(sandboxID, false) != nil {
		return nil, ErrEphemeralLifecycleInvalid
	}
	raw, err := s.store.Get(ctx, ephemeralLifecycleKeyPrefix+sandboxID)
	if err != nil {
		return nil, fmt.Errorf("load ephemeral lifecycle: %w", err)
	}
	if raw == nil {
		return nil, ErrSandboxNotFound
	}
	record, err := decodeEphemeralLifecycle(raw)
	if err != nil || record.SandboxID != sandboxID {
		return nil, ErrEphemeralLifecycleInvalid
	}
	return &record, nil
}

func (s *EphemeralLifecycleStore) List(ctx context.Context) ([]EphemeralLifecycleRecord, error) {
	if s == nil || s.store == nil {
		return nil, ErrEphemeralLifecycleInvalid
	}
	keys, err := s.store.Keys(ctx, ephemeralLifecycleKeyPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("list ephemeral lifecycles: %w", err)
	}
	records := make([]EphemeralLifecycleRecord, 0, len(keys))
	for _, key := range keys {
		id := strings.TrimPrefix(key, ephemeralLifecycleKeyPrefix)
		if id == key || id == "" {
			return nil, ErrEphemeralLifecycleInvalid
		}
		record, err := s.Load(ctx, id)
		if err != nil {
			return nil, err
		}
		records = append(records, *record)
	}
	return records, nil
}

func (s *EphemeralLifecycleStore) Transition(ctx context.Context, sandboxID string, expectedRevision uint64, nextState EphemeralFinalizationState) (*EphemeralLifecycleRecord, error) {
	if s == nil || s.store == nil || expectedRevision == 0 {
		return nil, ErrEphemeralLifecycleInvalid
	}
	key := ephemeralLifecycleKeyPrefix + sandboxID
	raw, err := s.store.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("load ephemeral lifecycle transition: %w", err)
	}
	if raw == nil {
		return nil, ErrSandboxNotFound
	}
	record, err := decodeEphemeralLifecycle(raw)
	if err != nil || record.SandboxID != sandboxID || record.Revision != expectedRevision || !validEphemeralTransition(record.State, nextState) {
		return nil, ErrEphemeralLifecycleConflict
	}
	record.State = nextState
	record.Revision++
	record.UpdatedAt = time.Now().UTC()
	nextRaw, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("marshal ephemeral lifecycle transition: %w", err)
	}
	swapped, err := s.store.CompareAndSwap(ctx, key, raw, nextRaw, 0)
	if err != nil {
		current, verifyErr := s.store.Get(context.WithoutCancel(ctx), key)
		if verifyErr == nil && bytes.Equal(current, nextRaw) {
			return &record, nil
		}
		return nil, errors.Join(ErrEphemeralLifecycleConflict, err, verifyErr)
	}
	if !swapped {
		return nil, ErrEphemeralLifecycleConflict
	}
	return &record, nil
}

func (s *EphemeralLifecycleStore) RemoveExact(ctx context.Context, record EphemeralLifecycleRecord) error {
	if s == nil || s.store == nil || validateEphemeralLifecycle(record) != nil {
		return ErrEphemeralLifecycleInvalid
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal exact ephemeral lifecycle: %w", err)
	}
	deleted, err := s.store.CompareAndDelete(ctx, ephemeralLifecycleKeyPrefix+record.SandboxID, raw)
	if err != nil {
		return errors.Join(ErrEphemeralLifecycleConflict, err)
	}
	if deleted {
		return nil
	}
	current, getErr := s.store.Get(ctx, ephemeralLifecycleKeyPrefix+record.SandboxID)
	if getErr != nil {
		return errors.Join(ErrEphemeralLifecycleConflict, getErr)
	}
	if current == nil {
		return nil
	}
	return ErrEphemeralLifecycleConflict
}

func (s *EphemeralLifecycleStore) Exists(ctx context.Context, sandboxID string) bool {
	if s == nil || s.store == nil {
		return false
	}
	exists, err := s.store.Exists(ctx, ephemeralLifecycleKeyPrefix+sandboxID)
	return err == nil && exists
}

func validEphemeralTransition(current, next EphemeralFinalizationState) bool {
	return (current == EphemeralActive && next == EphemeralFinalizing) ||
		(current == EphemeralFinalizing && next == EphemeralRemovingRuntime) ||
		(current == EphemeralRemovingRuntime && next == EphemeralReleasingLease)
}

func validateEphemeralLifecycle(record EphemeralLifecycleRecord) error {
	if record.Version != 1 || record.Revision == 0 || record.UpdatedAt.IsZero() ||
		validateOpaqueText(record.SandboxID, false) != nil || validateOpaqueText(record.RuntimeID, false) != nil ||
		validateOpaqueText(record.RuntimeUID, false) != nil || validateOpaqueText(record.WorkspacePath, false) != nil ||
		validateStoredOwner(record.Owner) != nil || record.Owner.SandboxID != record.SandboxID ||
		record.Owner.RuntimeID != record.RuntimeID || record.Owner.RuntimeUID != record.RuntimeUID || record.Owner.MountType != record.MountType {
		return ErrEphemeralLifecycleInvalid
	}
	if _, err := storage.BuildWorkspacePrefix("", record.WorkspacePath); err != nil {
		return ErrEphemeralLifecycleInvalid
	}
	switch record.State {
	case EphemeralActive, EphemeralFinalizing, EphemeralRemovingRuntime, EphemeralReleasingLease:
	default:
		return ErrEphemeralLifecycleInvalid
	}
	switch record.MountType {
	case WorkspaceMountSync:
		if record.Owner.MountAttempt != 0 || record.PreparationID != "" || record.PoolKey != "" || record.ReservationToken != "" || record.PoolRevision != 0 {
			return ErrEphemeralLifecycleInvalid
		}
	case WorkspaceMountFUSE:
		if record.Owner.MountAttempt != 1 || record.PreparationID == "" || record.PoolKey == "" || record.ReservationToken == "" || record.PoolRevision == 0 {
			return ErrEphemeralLifecycleInvalid
		}
	default:
		return ErrEphemeralLifecycleInvalid
	}
	return nil
}

func decodeEphemeralLifecycle(raw []byte) (EphemeralLifecycleRecord, error) {
	var record EphemeralLifecycleRecord
	if len(raw) == 0 || len(raw) > workspaceLeaseMaxRecordBytes {
		return record, ErrEphemeralLifecycleInvalid
	}
	allowed := make(map[string]struct{})
	t := reflect.TypeOf(record)
	for i := 0; i < t.NumField(); i++ {
		allowed[strings.Split(t.Field(i).Tag.Get("json"), ",")[0]] = struct{}{}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return record, ErrEphemeralLifecycleInvalid
	}
	seen := make(map[string]struct{})
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return record, ErrEphemeralLifecycleInvalid
		}
		if _, ok := allowed[key]; !ok {
			return record, ErrEphemeralLifecycleInvalid
		}
		if _, duplicate := seen[key]; duplicate {
			return record, ErrEphemeralLifecycleInvalid
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if decoder.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return record, ErrEphemeralLifecycleInvalid
		}
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return record, ErrEphemeralLifecycleInvalid
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return record, ErrEphemeralLifecycleInvalid
	}
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil || validateEphemeralLifecycle(record) != nil {
		return EphemeralLifecycleRecord{}, ErrEphemeralLifecycleInvalid
	}
	return record, nil
}

func ephemeralRecordFromSandbox(sb *Sandbox) (EphemeralLifecycleRecord, error) {
	if sb == nil || sb.Config.Mode != ModeEphemeral || sb.Workspace == nil || sb.Workspace.Owner.Generation <= 0 {
		return EphemeralLifecycleRecord{}, ErrEphemeralLifecycleInvalid
	}
	record := EphemeralLifecycleRecord{
		Version: 1, SandboxID: sb.ID, RuntimeID: sb.RuntimeID, RuntimeUID: sb.RuntimeUID,
		WorkspacePath: sb.Workspace.RootPath, MountType: sb.Workspace.MountType, Owner: sb.Workspace.Owner,
		State: EphemeralActive, Revision: 1, UpdatedAt: time.Now().UTC(),
	}
	if sb.Workspace.MountType == WorkspaceMountFUSE {
		record.PreparationID = sb.Workspace.FUSEPreparationID
		record.PoolKey = sb.Workspace.FUSEPoolKey
		record.ReservationToken = sb.Workspace.FUSEReservationToken
		record.PoolRevision = sb.Workspace.FUSERecordRevision
	}
	if err := validateEphemeralLifecycle(record); err != nil {
		return EphemeralLifecycleRecord{}, err
	}
	return record, nil
}

func (m *Manager) createWorkspaceLifecycle(ctx context.Context, sb *Sandbox) (*EphemeralLifecycleRecord, error) {
	if sb == nil {
		return nil, ErrSandboxNotReady
	}
	if sb.Config.Mode == ModePersistent {
		if m.sessions == nil {
			return nil, ErrSandboxNotReady
		}
		return nil, m.sessions.Save(ctx, sb)
	}
	if sb.Config.Mode != ModeEphemeral || m.ephemeral == nil {
		return nil, ErrSandboxNotReady
	}
	record, err := ephemeralRecordFromSandbox(sb)
	if err != nil {
		return nil, err
	}
	if err := m.ephemeral.Create(ctx, record); err != nil {
		return nil, err
	}
	return &record, nil
}

func (m *Manager) verifyWorkspaceLifecycle(ctx context.Context, sb *Sandbox) error {
	if sb == nil || sb.Workspace == nil {
		return ErrSandboxNotReady
	}
	if sb.Config.Mode == ModePersistent {
		if m.sessions == nil {
			return ErrSandboxNotReady
		}
		return m.sessions.Save(ctx, sb)
	}
	if m.ephemeral == nil {
		return ErrSandboxNotReady
	}
	record, err := m.ephemeral.Load(ctx, sb.ID)
	if err != nil {
		return err
	}
	if record == nil || record.RuntimeID != sb.RuntimeID || record.RuntimeUID != sb.RuntimeUID || record.MountType != sb.Workspace.MountType ||
		record.Owner.Generation != sb.Workspace.Owner.Generation {
		return ErrEphemeralLifecycleConflict
	}
	return nil
}

func (m *Manager) removeWorkspaceLifecycle(ctx context.Context, sb *Sandbox, ephemeralRecord *EphemeralLifecycleRecord) error {
	if sb == nil || sb.Workspace == nil {
		return ErrSandboxNotReady
	}
	if sb.Config.Mode == ModePersistent {
		if m.sessions == nil {
			return nil
		}
		if sb.Workspace.MountType == WorkspaceMountFUSE {
			return m.sessions.RemoveMatchingFUSESession(ctx, sb.ID, sb.RuntimeID, sb.RuntimeUID, sb.Workspace.FUSEPreparationID, sb.Workspace.LeaseGeneration)
		}
		return m.sessions.RemoveMatchingRuntime(ctx, sb)
	}
	if m.ephemeral == nil {
		return ErrEphemeralLifecycleInvalid
	}
	if ephemeralRecord == nil {
		if !m.ephemeral.Exists(ctx, sb.ID) {
			return nil
		}
		return ErrEphemeralLifecycleConflict
	}
	return m.ephemeral.RemoveExact(ctx, *ephemeralRecord)
}

// finalizeEphemeralLifecycles resumes private cleanup records at startup. It
// never publishes them into m.sandboxes, so one-shot executors cannot become
// user-visible persistent sandboxes after an API restart.
func (m *Manager) finalizeEphemeralLifecycles(ctx context.Context) error {
	if m.ephemeral == nil {
		return nil
	}
	records, err := m.ephemeral.List(ctx)
	if err != nil {
		return err
	}
	for i := range records {
		record := records[i]
		if record.State == EphemeralActive {
			next, err := m.ephemeral.Transition(ctx, record.SandboxID, record.Revision, EphemeralFinalizing)
			if err != nil {
				return err
			}
			record = *next
		}
		switch record.MountType {
		case WorkspaceMountSync:
			if err := m.finalizeRecoveredEphemeralSync(ctx, &record); err != nil {
				return fmt.Errorf("finalize ephemeral sync %q: %w", record.SandboxID, err)
			}
		case WorkspaceMountFUSE:
			if err := m.finalizeRecoveredEphemeralFUSE(ctx, &record); err != nil {
				return fmt.Errorf("finalize ephemeral FUSE %q: %w", record.SandboxID, err)
			}
		default:
			return ErrEphemeralLifecycleInvalid
		}
	}
	return nil
}

func (m *Manager) finalizeRecoveredEphemeralSync(ctx context.Context, record *EphemeralLifecycleRecord) error {
	if m.config.WorkspaceCoordinator == nil || m.fsMeta == nil || record == nil {
		return ErrSandboxCleanupPending
	}
	lease, err := m.config.WorkspaceCoordinator.Restore(ctx, record.Owner)
	if err != nil {
		return errors.Join(ErrSandboxCleanupPending, err)
	}
	renewal, err := m.config.WorkspaceCoordinator.StartRenewal(context.Background(), lease, nil)
	if err != nil {
		return errors.Join(ErrSandboxCleanupPending, err)
	}
	defer renewal.Stop()
	if record.State == EphemeralFinalizing {
		info, err := m.runtime.GetSandbox(ctx, record.RuntimeID)
		if err != nil || info == nil || info.RuntimeUID != record.RuntimeUID || info.State != "running" {
			return errors.Join(ErrSandboxCleanupPending, ErrSandboxNotReady, err)
		}
		scoped, err := storage.NewScopedFS(m.filesystem, record.WorkspacePath)
		if err != nil {
			return errors.Join(ErrSandboxCleanupPending, err)
		}
		if err := m.fullSyncFromContainer(ctx, scoped, record.RuntimeID, nil); err != nil {
			return errors.Join(ErrSandboxCleanupPending, err)
		}
		next, err := m.ephemeral.Transition(ctx, record.SandboxID, record.Revision, EphemeralRemovingRuntime)
		if err != nil {
			return errors.Join(ErrSandboxCleanupPending, err)
		}
		record = next
	}
	if record.State == EphemeralRemovingRuntime {
		if err := m.runtime.RemoveSandbox(ctx, record.RuntimeID); err != nil {
			return errors.Join(ErrSandboxCleanupPending, err)
		}
		next, err := m.ephemeral.Transition(ctx, record.SandboxID, record.Revision, EphemeralReleasingLease)
		if err != nil {
			return errors.Join(ErrSandboxCleanupPending, err)
		}
		record = next
	}
	if record.State != EphemeralReleasingLease {
		return ErrEphemeralLifecycleInvalid
	}
	if err := m.config.WorkspaceCoordinator.Release(ctx, lease, runtime.TerminationEvidence{}); err != nil {
		return errors.Join(ErrSandboxCleanupPending, err)
	}
	return m.ephemeral.RemoveExact(ctx, *record)
}

func (m *Manager) finalizeRecoveredEphemeralFUSE(ctx context.Context, record *EphemeralLifecycleRecord) error {
	if m.fusePool == nil || m.config.WorkspaceCoordinator == nil || record == nil {
		return ErrSandboxCleanupPending
	}
	records, err := m.fusePool.repo.ListByPoolKey(ctx, record.PoolKey)
	if err != nil {
		return errors.Join(ErrSandboxCleanupPending, err)
	}
	var poolRecord *state.FUSEPoolRecord
	for i := range records {
		candidate := records[i]
		if candidate.PreparationID == record.PreparationID && candidate.RuntimeID == record.RuntimeID && candidate.RuntimeUID == record.RuntimeUID &&
			candidate.ReservationToken == record.ReservationToken {
			poolRecord = &candidate
			break
		}
	}
	if poolRecord == nil {
		return errors.Join(ErrSandboxCleanupPending, state.ErrFUSEPoolCorrupt)
	}
	lease, err := m.config.WorkspaceCoordinator.Restore(ctx, record.Owner)
	if err != nil {
		return errors.Join(ErrSandboxCleanupPending, err)
	}
	renewal, err := m.config.WorkspaceCoordinator.StartRenewal(context.Background(), lease, nil)
	if err != nil {
		return errors.Join(ErrSandboxCleanupPending, err)
	}
	sb := &Sandbox{
		ID: record.SandboxID, Config: SandboxConfig{Mode: ModeEphemeral, WorkspacePath: record.WorkspacePath, WorkspaceMountMode: WorkspaceMountFUSE},
		State: StateDestroying, RuntimeID: record.RuntimeID, RuntimeUID: record.RuntimeUID,
		Workspace: &WorkspaceInfo{
			RootPath: record.WorkspacePath, MountType: WorkspaceMountFUSE, Owner: record.Owner, LeaseGeneration: record.Owner.Generation,
			FUSEPreparationID: record.PreparationID, FUSEPoolKey: record.PoolKey, FUSEReservationToken: record.ReservationToken, FUSERecordRevision: record.PoolRevision,
		},
	}
	_, cancel := context.WithCancel(context.Background())
	lifecycle := &fuseSandboxLifecycle{
		sandboxID: record.SandboxID, sandbox: sb, gate: newOperationGate(false), lease: lease, renewal: renewal,
		record: *poolRecord, cancel: cancel, ephemeralRecord: record, finalizingPersisted: true, gateClosed: true,
	}
	if record.State == EphemeralRemovingRuntime || record.State == EphemeralReleasingLease {
		lifecycle.quiesceAttempted = true
		lifecycle.quiesced = true
		lifecycle.flushAttempted = true
	}
	if record.State == EphemeralReleasingLease {
		lifecycle.runtimeRemoved = true
	}
	m.teardownFUSESandbox(lifecycle, ErrSandboxCleanupPending)
	lifecycle.teardownMu.Lock()
	done := lifecycle.teardownDone
	lifecycle.teardownMu.Unlock()
	if !done {
		return ErrSandboxCleanupPending
	}
	return nil
}

func (m *Manager) finalizeRecoveredPersistentFUSE(ctx context.Context, sb *Sandbox) error {
	if sb == nil || sb.Workspace == nil || sb.State != StateDestroying || m.fusePool == nil ||
		m.config.WorkspaceCoordinator == nil || m.sessions == nil {
		return ErrSandboxCleanupPending
	}
	workspace := sb.Workspace
	records, err := m.fusePool.repo.ListByPoolKey(ctx, workspace.FUSEPoolKey)
	if err != nil {
		return errors.Join(ErrSandboxCleanupPending, err)
	}
	var poolRecord *state.FUSEPoolRecord
	for i := range records {
		candidate := records[i]
		if candidate.PreparationID == workspace.FUSEPreparationID && candidate.RuntimeID == sb.RuntimeID && candidate.RuntimeUID == sb.RuntimeUID &&
			candidate.ReservationToken == workspace.FUSEReservationToken {
			poolRecord = &candidate
			break
		}
	}
	if poolRecord == nil {
		return errors.Join(ErrSandboxCleanupPending, state.ErrFUSEPoolCorrupt)
	}
	lease, err := m.config.WorkspaceCoordinator.Restore(ctx, workspace.Owner)
	if err != nil {
		return errors.Join(ErrSandboxCleanupPending, err)
	}
	renewal, err := m.config.WorkspaceCoordinator.StartRenewal(context.Background(), lease, nil)
	if err != nil {
		return errors.Join(ErrSandboxCleanupPending, err)
	}
	_, cancel := context.WithCancel(context.Background())
	lifecycle := &fuseSandboxLifecycle{
		sandboxID: sb.ID, sandbox: sb, gate: newOperationGate(false), lease: lease, renewal: renewal,
		record: *poolRecord, cancel: cancel, finalizingPersisted: true, gateClosed: true,
	}
	m.teardownFUSESandbox(lifecycle, ErrSandboxCleanupPending)
	lifecycle.teardownMu.Lock()
	done := lifecycle.teardownDone
	lifecycle.teardownMu.Unlock()
	if !done {
		return ErrSandboxCleanupPending
	}
	return nil
}
