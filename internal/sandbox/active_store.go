package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/goairix/sandbox/internal/storage/state"
)

const (
	activeSandboxRecordVersion = 1
	activeOperationTTL         = 30 * time.Second
	activeOperationEndTimeout  = 2 * time.Second
)

func (m *Manager) distributedStateEnabled() bool {
	return m.config.RuntimeType == "kubernetes" && m.activeSandboxes != nil
}

func marshalActiveSandbox(sb *Sandbox) ([]byte, error) {
	if sb == nil || sb.ID == "" || sb.RuntimeID == "" || sb.RuntimeUID == "" {
		return nil, state.ErrActiveSandboxCorrupt
	}
	snapshot := cloneSandbox(sb)
	return json.Marshal(&snapshot)
}

func decodeActiveSandbox(record *state.ActiveSandboxRecord, expectedID string) (*Sandbox, error) {
	if record == nil {
		return nil, fmt.Errorf("%w: %s", ErrSandboxNotFound, expectedID)
	}
	if err := record.Validate(); err != nil || record.SandboxID != expectedID || record.Phase != state.ActiveSandboxActive {
		return nil, fmt.Errorf("%w: %s", ErrSandboxNotReady, expectedID)
	}
	var sb Sandbox
	if err := json.Unmarshal(record.Snapshot, &sb); err != nil || sb.ID != record.SandboxID ||
		sb.RuntimeID != record.RuntimeID || sb.RuntimeUID != record.RuntimeUID {
		return nil, fmt.Errorf("%w: %s", ErrSandboxNotReady, expectedID)
	}
	return &sb, nil
}

func (m *Manager) publishActiveSandbox(ctx context.Context, sb *Sandbox) error {
	if !m.distributedStateEnabled() {
		return nil
	}
	snapshot, err := marshalActiveSandbox(sb)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	record := state.ActiveSandboxRecord{
		Version: activeSandboxRecordVersion, SandboxID: sb.ID,
		Phase: state.ActiveSandboxPublishing, Revision: 1, Generation: 1,
		RuntimeID: sb.RuntimeID, RuntimeUID: sb.RuntimeUID, Snapshot: snapshot,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := m.activeSandboxes.Publish(ctx, record); err != nil {
		current, loadErr := m.activeSandboxes.Load(context.WithoutCancel(ctx), sb.ID)
		if loadErr != nil || current == nil || current.Phase != state.ActiveSandboxPublishing ||
			current.Generation != record.Generation || current.RuntimeID != record.RuntimeID ||
			current.RuntimeUID != record.RuntimeUID || !bytes.Equal(current.Snapshot, record.Snapshot) {
			return errors.Join(fmt.Errorf("publish active sandbox: %w", err), loadErr)
		}
		record = *current
	}
	active, activateErr := m.activeSandboxes.Activate(ctx, sb.ID, record.Revision, snapshot)
	if activateErr == nil && active != nil {
		return nil
	}
	current, loadErr := m.activeSandboxes.Load(context.WithoutCancel(ctx), sb.ID)
	if loadErr == nil && current != nil && current.Phase == state.ActiveSandboxActive &&
		current.Generation == record.Generation && current.RuntimeID == record.RuntimeID &&
		current.RuntimeUID == record.RuntimeUID && bytes.Equal(current.Snapshot, snapshot) {
		return nil
	}
	return errors.Join(fmt.Errorf("activate sandbox state: %w", activateErr), loadErr)
}

func (m *Manager) beginDistributedOperation(ctx context.Context, id string, kind state.ActiveOperationKind) (*Sandbox, func(), error) {
	token := uuid.NewString()
	record, operation, err := m.activeSandboxes.BeginOperation(ctx, id, token, kind, activeOperationTTL)
	if err != nil {
		if errors.Is(err, state.ErrActiveSandboxAdmissionClosed) {
			return nil, nil, fmt.Errorf("%w: %s", ErrSandboxNotReady, id)
		}
		return nil, nil, err
	}
	if record == nil || operation == nil {
		return nil, nil, fmt.Errorf("%w: %s", ErrSandboxNotFound, id)
	}
	sb, err := decodeActiveSandbox(record, id)
	if err == nil {
		var infoRuntimeUID string
		info, inspectErr := m.runtime.GetSandbox(ctx, sb.RuntimeID)
		if info != nil {
			infoRuntimeUID = info.RuntimeUID
		}
		if inspectErr != nil || info == nil || info.State != "running" || info.RuntimeID != sb.RuntimeID || infoRuntimeUID != sb.RuntimeUID {
			err = errors.Join(fmt.Errorf("%w: runtime identity changed for %s", ErrSandboxNotReady, id), inspectErr)
		}
	}
	if err != nil {
		endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), activeOperationEndTimeout)
		_ = m.activeSandboxes.EndOperation(endCtx, *operation)
		cancel()
		return nil, nil, err
	}
	var released bool
	var releaseMu sync.Mutex
	release := func() {
		releaseMu.Lock()
		defer releaseMu.Unlock()
		if released {
			return
		}
		released = true
		endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), activeOperationEndTimeout)
		_ = m.activeSandboxes.EndOperation(endCtx, *operation)
		cancel()
	}
	return sb, release, nil
}
