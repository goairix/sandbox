package etcd

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const activeScanBatch = 64 // Below etcd's 128-operation transaction limit.

// Scan reads at most count index entries and their records from one MVCC
// revision. Its uint64 cursor is the last random index value returned. A
// concurrent new index below that cursor may appear in a later scan pass;
// callers must not treat one traversal as a frozen global inventory.
func (r *ActiveSandboxRepository) Scan(ctx context.Context, cursor uint64, count int64) (state.ActiveSandboxPage, error) {
	return r.scanCursorIndex(ctx, r.indexPrefix(), cursor, count, false)
}

// ScanWorkspace reads only records carrying a workspace or an unfinished
// workspace transition. The caller still reloads before any lifecycle effect.
func (r *ActiveSandboxRepository) ScanWorkspace(ctx context.Context, cursor uint64, count int64) (state.ActiveSandboxPage, error) {
	return r.scanCursorIndex(ctx, r.workspaceIndexPrefix(), cursor, count, true)
}

func (r *ActiveSandboxRepository) scanCursorIndex(ctx context.Context, prefix string, cursor uint64, count int64, workspace bool) (state.ActiveSandboxPage, error) {
	if count <= 0 || count > 1000 {
		return state.ActiveSandboxPage{}, state.ErrActiveSandboxCorrupt
	}
	if cursor == math.MaxUint64 {
		return state.ActiveSandboxPage{}, nil
	}
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	start := prefix
	if cursor != 0 {
		start = fmt.Sprintf("%s%016x", prefix, cursor+1)
	}
	indexes, err := r.store.client.Get(requestCtx, start,
		clientv3.WithRange(clientv3.GetPrefixRangeEnd(prefix)), clientv3.WithLimit(count+1))
	if err != nil {
		return state.ActiveSandboxPage{}, err
	}
	limit := len(indexes.Kvs)
	if limit > int(count) {
		limit = int(count)
	}
	page := state.ActiveSandboxPage{Records: make([]state.ActiveSandboxRecord, 0, limit)}
	for offset := 0; offset < limit; offset += activeScanBatch {
		end := min(offset+activeScanBatch, limit)
		ops := make([]clientv3.Op, 0, end-offset)
		cursors := make([]uint64, 0, end-offset)
		ids := make([]string, 0, end-offset)
		for _, index := range indexes.Kvs[offset:end] {
			key := string(index.Key)
			id := string(index.Value)
			if !strings.HasPrefix(key, prefix) || len(key) != len(prefix)+16 || !validActiveID(id) {
				return state.ActiveSandboxPage{}, state.ErrActiveSandboxCorrupt
			}
			position, err := strconv.ParseUint(key[len(prefix):], 16, 64)
			if err != nil || position == 0 {
				return state.ActiveSandboxPage{}, state.ErrActiveSandboxCorrupt
			}
			ops = append(ops, clientv3.OpGet(r.recordKey(id), clientv3.WithRev(indexes.Header.Revision)))
			cursors = append(cursors, position)
			ids = append(ids, id)
		}
		// Small batches avoid one RPC per resident sandbox while preserving
		// the index page's revision. A missing record at that revision means
		// the two-key publication invariant was broken.
		read, err := r.store.client.Txn(requestCtx).Then(ops...).Commit()
		if err != nil {
			return state.ActiveSandboxPage{}, err
		}
		if len(read.Responses) != len(ops) {
			return state.ActiveSandboxPage{}, state.ErrActiveSandboxCorrupt
		}
		for i, response := range read.Responses {
			get := response.GetResponseRange()
			if get == nil || len(get.Kvs) != 1 {
				return state.ActiveSandboxPage{}, state.ErrActiveSandboxCorrupt
			}
			envelope, err := decodeActiveEnvelope(get.Kvs[0].Value, ids[i])
			if err != nil || envelope.Cursor != cursors[i] {
				return state.ActiveSandboxPage{}, state.ErrActiveSandboxCorrupt
			}
			if workspace {
				key, err := r.lifecycleIndexKey(envelope)
				if err != nil || key != fmt.Sprintf("%s%016x", prefix, cursors[i]) {
					return state.ActiveSandboxPage{}, state.ErrActiveSandboxCorrupt
				}
			}
			page.Records = append(page.Records, envelope.Record)
			page.Cursor = cursors[i]
		}
	}
	if len(indexes.Kvs) <= limit {
		page.Cursor = 0
	}
	return page, nil
}

// Ping performs a bounded etcd read with the repository's request timeout.
func (r *ActiveSandboxRepository) Ping(ctx context.Context) error {
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	_, err := r.store.client.Get(requestCtx, r.indexPrefix(), clientv3.WithLimit(1))
	return err
}
