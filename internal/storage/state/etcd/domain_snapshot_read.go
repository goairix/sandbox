package etcd

import "context"

// SandboxSnapshotReadResult contains coherent metadata and caller-owned bytes.
// Neither field grants runtime health, TTL or operation admission authority.
type SandboxSnapshotReadResult struct {
	View     *SandboxReadView
	Snapshot *SandboxSnapshotRecord
}

func (b *Backend) ReadWorkspaceSnapshot(ctx context.Context, workspace WorkspaceIdentity) (*SandboxSnapshotReadResult, error) {
	call, err := b.registerSnapshotRead(ctx)
	if err != nil {
		return nil, err
	}
	defer call.finish()
	view, err := b.ReadWorkspaceView(call.context, workspace)
	return b.finishSnapshotRead(call, view, err)
}

func (b *Backend) ReadSandboxSnapshot(ctx context.Context, sandboxID string) (*SandboxSnapshotReadResult, error) {
	call, err := b.registerSnapshotRead(ctx)
	if err != nil {
		return nil, err
	}
	defer call.finish()
	view, err := b.ReadSandboxView(call.context, sandboxID)
	return b.finishSnapshotRead(call, view, err)
}

func (b *Backend) finishSnapshotRead(call *snapshotReadCall, view *SandboxReadView, err error) (*SandboxSnapshotReadResult, error) {
	if err != nil {
		return nil, err
	}
	var result *SandboxSnapshotReadResult
	if view != nil {
		control, placement := view.records.Control, view.records.Placement
		key := snapshotCacheKey{partition: placement.Partition, sandboxID: control.SandboxID, version: control.Snapshot.Version, digest: control.Snapshot.Digest}
		record, err := b.snapshotRecord(call, key, *placement)
		if err != nil {
			return nil, err
		}
		if record == nil {
			return nil, ErrCorruptRecord
		}
		if err := b.ConfirmSandboxView(call.context, view); err != nil {
			return nil, err
		}
		result = &SandboxSnapshotReadResult{View: view, Snapshot: record}
	}
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if b.archiveLife.closed {
		return nil, context.Canceled
	}
	if err := call.context.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (b *Backend) loadSnapshotRead(ctx context.Context, key snapshotCacheKey, expected SandboxPlacementRecord) (*SandboxSnapshotRecord, error) {
	keys, err := b.snapshotReadKeys(key)
	if err != nil {
		return nil, err
	}
	points, err := b.readSnapshotPoints(ctx, keys)
	if err != nil {
		return nil, err
	}
	placement, err := b.decodePlacement(points[1], key.sandboxID)
	if err != nil {
		return nil, err
	}
	if placement == nil || *placement != expected || readViewCanonical(points[1], placement) != nil || !enrolledImmutableKV(points[0]) {
		return nil, ErrCorruptRecord
	}
	record, err := decodeSnapshot(points[0], key.partition, key.sandboxID, SnapshotReference{Version: key.version, Digest: key.digest}, placement)
	if err != nil {
		return nil, err
	}
	if record == nil || readViewCanonical(points[0], record) != nil {
		return nil, ErrCorruptRecord
	}
	return record, nil
}
