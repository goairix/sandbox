package etcd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// lifecycleIndexKey derives a discovery key only from the durable record.
// The index is never authority for expiry or cleanup. Workspace snapshots use
// their own index because controller recovery has a different cost and cadence.
func (r *ActiveSandboxRepository) lifecycleIndexKey(envelope activeEnvelope) (string, error) {
	var snapshot struct {
		CreatedAt           time.Time       `json:"created_at"`
		Timeout             time.Duration   `json:"timeout"`
		Workspace           json.RawMessage `json:"workspace"`
		WorkspaceTransition string          `json:"workspace_transition"`
	}
	if err := json.Unmarshal(envelope.Record.Snapshot, &snapshot); err != nil {
		return "", state.ErrActiveSandboxCorrupt
	}
	if (len(snapshot.Workspace) > 0 && !bytes.Equal(snapshot.Workspace, []byte("null"))) || snapshot.WorkspaceTransition != "" {
		return fmt.Sprintf("%s%016x", r.workspaceIndexPrefix(), envelope.Cursor), nil
	}

	var due time.Time
	switch envelope.Record.Phase {
	case state.ActiveSandboxPublishing, state.ActiveSandboxExclusive, state.ActiveSandboxDestroying, state.ActiveSandboxCleanupPending:
		// The coordinator still applies the publication grace and all cleanup
		// fences. These phases must remain discoverable after a process crash.
		due = envelope.Record.UpdatedAt
	case state.ActiveSandboxActive:
		if snapshot.Timeout <= 0 {
			return "", nil
		}
		if snapshot.CreatedAt.IsZero() {
			return "", state.ErrActiveSandboxCorrupt
		}
		due = snapshot.CreatedAt.Add(snapshot.Timeout)
	default:
		return "", state.ErrActiveSandboxCorrupt
	}
	if due.IsZero() || due.UnixNano() < 0 {
		return "", state.ErrActiveSandboxCorrupt
	}
	return fmt.Sprintf("%s%019d/%016x", r.dueIndexPrefix(), due.UnixNano(), envelope.Cursor), nil
}

func (r *ActiveSandboxRepository) dueIndexPrefix() string {
	return r.store.prefix + "active/" + r.scope + "/due/"
}

func (r *ActiveSandboxRepository) workspaceIndexPrefix() string {
	return r.store.prefix + "active/" + r.scope + "/workspace/"
}

// lifecycleIndexMutation belongs in the same etcd transaction as the record
// write. Comparing the old value prevents removal of another publication; a
// failed or unknown commit must never be repaired by an unfenced second write.
func (r *ActiveSandboxRepository) lifecycleIndexMutation(before, after *activeEnvelope) ([]clientv3.Cmp, []clientv3.Op, error) {
	var oldKey, newKey string
	var err error
	if before != nil {
		oldKey, err = r.lifecycleIndexKey(*before)
		if err != nil {
			return nil, nil, err
		}
	}
	if after != nil {
		newKey, err = r.lifecycleIndexKey(*after)
		if err != nil {
			return nil, nil, err
		}
	}
	if oldKey == newKey {
		if oldKey != "" {
			return []clientv3.Cmp{clientv3.Compare(clientv3.Value(oldKey), "=", before.Record.SandboxID)}, nil, nil
		}
		return nil, nil, nil
	}

	var compares []clientv3.Cmp
	var writes []clientv3.Op
	if oldKey != "" {
		compares = append(compares, clientv3.Compare(clientv3.Value(oldKey), "=", before.Record.SandboxID))
		writes = append(writes, clientv3.OpDelete(oldKey))
	}
	if newKey != "" {
		compares = append(compares, clientv3.Compare(clientv3.Version(newKey), "=", 0))
		writes = append(writes, clientv3.OpPut(newKey, after.Record.SandboxID))
	}
	return compares, writes, nil
}
