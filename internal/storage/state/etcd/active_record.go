package etcd

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

var activeIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

// ActiveSandboxRepository owns durable lifecycle records within one scope.
// Production wiring follows separate business-path verification.
type ActiveSandboxRepository struct {
	store *Store
	scope string
}

var (
	_ state.ActiveSandboxRepository                   = (*ActiveSandboxRepository)(nil)
	_ state.ActiveSandboxLifecycleIndexRepository     = (*ActiveSandboxRepository)(nil)
	_ state.ActiveSandboxCheckpointRepository         = (*ActiveSandboxRepository)(nil)
	_ state.ActiveSandboxCleanupCompletionRepository  = (*ActiveSandboxRepository)(nil)
	_ state.ActiveSandboxConditionalDestroyRepository = (*ActiveSandboxRepository)(nil)
	_ state.FUSEPoolBoundActiveRepository             = (*ActiveSandboxRepository)(nil)
)

type activeEnvelope struct {
	Cursor uint64                    `json:"cursor"`
	Record state.ActiveSandboxRecord `json:"record"`
}

// NewActiveSandboxRepository binds a lifecycle repository to an isolated scope.
func NewActiveSandboxRepository(store *Store, scope string) (*ActiveSandboxRepository, error) {
	if store == nil || strings.TrimSpace(scope) == "" {
		return nil, state.ErrActiveSandboxCorrupt
	}
	digest := sha256.Sum256([]byte(scope))
	return &ActiveSandboxRepository{store: store, scope: hex.EncodeToString(digest[:16])}, nil
}

func (r *ActiveSandboxRepository) recordKey(id string) string {
	return r.store.prefix + "active/" + r.scope + "/record/" + id
}

func (r *ActiveSandboxRepository) indexKey(cursor uint64) string {
	return fmt.Sprintf("%s%016x", r.indexPrefix(), cursor)
}

func (r *ActiveSandboxRepository) indexPrefix() string {
	return r.store.prefix + "active/" + r.scope + "/index/"
}

func validActiveID(id string) bool {
	return activeIDPattern.MatchString(id)
}

func decodeActiveEnvelope(raw []byte, id string) (activeEnvelope, error) {
	var envelope activeEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Cursor == 0 || envelope.Record.SandboxID != id {
		return activeEnvelope{}, state.ErrActiveSandboxCorrupt
	}
	if err := envelope.Record.Validate(); err != nil {
		return activeEnvelope{}, err
	}
	return envelope, nil
}

func randomActiveCursor() (uint64, error) {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(bytes[:]), nil
}

// Publish inserts the record and its scan index in one transaction. A random
// 64-bit cursor avoids a shared increment hot key when many sandboxes publish.
// The cursor is private storage metadata and never grants lifecycle authority.
func (r *ActiveSandboxRepository) Publish(ctx context.Context, record state.ActiveSandboxRecord) error {
	return r.publish(ctx, record, nil)
}

func (r *ActiveSandboxRepository) PublishFUSEBound(ctx context.Context, record state.ActiveSandboxRecord, binding state.FUSEPoolPublication) error {
	return r.publish(ctx, record, &binding)
}

func (r *ActiveSandboxRepository) publish(ctx context.Context, record state.ActiveSandboxRecord, binding *state.FUSEPoolPublication) error {
	if record.Validate() != nil || record.Phase != state.ActiveSandboxPublishing || !validActiveID(record.SandboxID) {
		return state.ErrActiveSandboxCorrupt
	}
	if binding != nil && (record.RuntimeID != binding.RuntimeID || record.RuntimeUID != binding.RuntimeUID) {
		return state.ErrFUSEPoolConflict
	}
	key := r.recordKey(record.SandboxID)
	for range 4 {
		cursor, err := randomActiveCursor()
		if err != nil {
			return err
		}
		if cursor == 0 {
			continue
		}
		index := r.indexKey(cursor)
		envelope := activeEnvelope{Cursor: cursor, Record: record}
		indexCompares, indexWrites, err := r.lifecycleIndexMutation(nil, &envelope)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(envelope)
		if err != nil {
			return err
		}
		requestCtx, cancel := r.store.requestContext(ctx)
		compares := append([]clientv3.Cmp{clientv3.Compare(clientv3.Version(key), "=", 0), clientv3.Compare(clientv3.Version(index), "=", 0)}, indexCompares...)
		if binding != nil {
			poolConditions, _, conditionErr := r.store.fusePublicationConditions(requestCtx, *binding)
			if conditionErr != nil {
				cancel()
				return conditionErr
			}
			compares = append(compares, poolConditions...)
		}
		writes := append([]clientv3.Op{clientv3.OpPut(key, string(raw)), clientv3.OpPut(index, record.SandboxID)}, indexWrites...)
		if binding != nil {
			// The active record and removal of the orphan scan marker commit
			// together. A lost reply cannot leave an unprotected runtime.
			pool := NewFUSEPoolRepository(r.store)
			pendingKey := pool.unpublishedKey(binding.PoolKey, binding.PreparationID)
			pending, readErr := r.store.client.Get(requestCtx, pendingKey)
			if readErr != nil {
				cancel()
				return readErr
			}
			if len(pending.Kvs) != 0 {
				if len(pending.Kvs) != 1 || string(pending.Kvs[0].Value) != binding.PreparationID {
					cancel()
					return state.ErrFUSEPoolCorrupt
				}
				compares = append(compares, clientv3.Compare(clientv3.ModRevision(pendingKey), "=", pending.Kvs[0].ModRevision))
				writes = append(writes, clientv3.OpDelete(pendingKey))
			}
		}
		response, err := r.store.client.Txn(requestCtx).
			If(compares...).
			Then(writes...).
			Else(clientv3.OpGet(key), clientv3.OpGet(index)).Commit()
		cancel()
		if err != nil {
			// A lost response is unknown. Regenerating a cursor here could
			// create a second index for an already published record.
			return err
		}
		if response.Succeeded {
			return nil
		}
		// The Else reads share the failed transaction's snapshot. A later
		// separate read could miss a concurrently deleted record and turn
		// one conflicting Publish into a second publication.
		if len(response.Responses) != 2 || response.Responses[0].GetResponseRange() == nil || response.Responses[1].GetResponseRange() == nil {
			return state.ErrActiveSandboxCorrupt
		}
		if len(response.Responses[0].GetResponseRange().Kvs) != 0 {
			return state.ErrActiveSandboxConflict
		}
		if len(response.Responses[1].GetResponseRange().Kvs) == 0 {
			return state.ErrActiveSandboxCorrupt
		}
		// An unrelated record occupied the random index. Pick a new
		// cursor without changing the requested lifecycle record.
	}
	return state.ErrActiveSandboxConflict
}

// Load returns nil for a missing record and rejects corrupt storage bytes.
func (r *ActiveSandboxRepository) Load(ctx context.Context, id string) (*state.ActiveSandboxRecord, error) {
	if !validActiveID(id) {
		return nil, state.ErrActiveSandboxCorrupt
	}
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	response, err := r.store.client.Get(requestCtx, r.recordKey(id))
	if err != nil || len(response.Kvs) == 0 {
		return nil, err
	}
	envelope, err := decodeActiveEnvelope(response.Kvs[0].Value, id)
	if err != nil {
		return nil, err
	}
	return &envelope.Record, nil
}

// Activate transitions a publishing record to active at the exact business
// revision. A missing record retains the original nil, nil contract.
func (r *ActiveSandboxRepository) Activate(ctx context.Context, id string, expectedRevision uint64, snapshot json.RawMessage) (*state.ActiveSandboxRecord, error) {
	return r.changeRecord(ctx, id, expectedRevision, state.ActiveSandboxPublishing, state.ActiveSandboxActive, snapshot)
}

// Update changes an active snapshot at the exact business revision.
func (r *ActiveSandboxRepository) Update(ctx context.Context, id string, expectedRevision uint64, snapshot json.RawMessage) (*state.ActiveSandboxRecord, error) {
	return r.changeRecord(ctx, id, expectedRevision, state.ActiveSandboxActive, state.ActiveSandboxActive, snapshot)
}

func (r *ActiveSandboxRepository) changeRecord(ctx context.Context, id string, expectedRevision uint64, from, to state.ActiveSandboxPhase, snapshot json.RawMessage) (*state.ActiveSandboxRecord, error) {
	if !validActiveID(id) || expectedRevision == 0 || !json.Valid(snapshot) {
		return nil, state.ErrActiveSandboxCorrupt
	}
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	key := r.recordKey(id)
	read, err := r.store.client.Get(requestCtx, key)
	if err != nil || len(read.Kvs) == 0 {
		return nil, err
	}
	envelope, err := decodeActiveEnvelope(read.Kvs[0].Value, id)
	if err != nil {
		return nil, err
	}
	if envelope.Record.Revision != expectedRevision || envelope.Record.Phase != from {
		return nil, state.ErrActiveSandboxConflict
	}
	previous := envelope

	envelope.Record.Phase = to
	envelope.Record.Revision++
	envelope.Record.Snapshot = append(json.RawMessage(nil), snapshot...)
	envelope.Record.UpdatedAt = time.Now().UTC()
	indexCompares, indexWrites, err := r.lifecycleIndexMutation(&previous, &envelope)
	if err != nil {
		return nil, err
	}
	next, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	compares := append([]clientv3.Cmp{clientv3.Compare(clientv3.ModRevision(key), "=", read.Kvs[0].ModRevision)}, indexCompares...)
	writes := append([]clientv3.Op{clientv3.OpPut(key, string(next))}, indexWrites...)
	written, err := r.store.client.Txn(requestCtx).
		If(compares...).Then(writes...).Commit()
	if err != nil {
		return nil, err
	}
	if !written.Succeeded {
		return nil, state.ErrActiveSandboxConflict
	}
	return &envelope.Record, nil
}
