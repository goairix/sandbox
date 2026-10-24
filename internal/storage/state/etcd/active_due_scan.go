package etcd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// ScanDue reads one bounded page of ordinary lifecycle candidates due by
// dueBefore. A candidate can become stale before the caller acts: expiry,
// ownership and physical cleanup remain guarded by the authoritative record.
func (r *ActiveSandboxRepository) ScanDue(ctx context.Context, dueBefore time.Time, cursor string, count int64) (state.ActiveSandboxDuePage, error) {
	if count <= 0 || count > 1000 || dueBefore.IsZero() || dueBefore.UnixNano() < 0 {
		return state.ActiveSandboxDuePage{}, state.ErrActiveSandboxCorrupt
	}
	prefix := r.dueIndexPrefix()
	if cursor != "" && (!strings.HasPrefix(cursor, prefix) || len(cursor) != len(prefix)+36) {
		return state.ActiveSandboxDuePage{}, state.ErrActiveSandboxCorrupt
	}
	start := prefix
	if cursor != "" {
		start = cursor + "\x00"
	}
	end := fmt.Sprintf("%s%019d/", prefix, dueBefore.UnixNano()+1)
	if start >= end {
		return state.ActiveSandboxDuePage{}, nil
	}

	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	indexes, err := r.store.client.Get(requestCtx, start, clientv3.WithRange(end), clientv3.WithLimit(count+1))
	if err != nil {
		return state.ActiveSandboxDuePage{}, err
	}
	limit := min(len(indexes.Kvs), int(count))
	page := state.ActiveSandboxDuePage{Records: make([]state.ActiveSandboxRecord, 0, limit)}
	for offset := 0; offset < limit; offset += activeScanBatch {
		batch := indexes.Kvs[offset:min(offset+activeScanBatch, limit)]
		ops := make([]clientv3.Op, 0, len(batch))
		for _, index := range batch {
			if len(index.Key) != len(prefix)+36 || !strings.HasPrefix(string(index.Key), prefix) || !validActiveID(string(index.Value)) {
				return state.ActiveSandboxDuePage{}, state.ErrActiveSandboxCorrupt
			}
			ops = append(ops, clientv3.OpGet(r.recordKey(string(index.Value)), clientv3.WithRev(indexes.Header.Revision)))
		}
		read, err := r.store.client.Txn(requestCtx).Then(ops...).Commit()
		if err != nil {
			return state.ActiveSandboxDuePage{}, err
		}
		if len(read.Responses) != len(batch) {
			return state.ActiveSandboxDuePage{}, state.ErrActiveSandboxCorrupt
		}
		for i, response := range read.Responses {
			get := response.GetResponseRange()
			if get == nil || len(get.Kvs) != 1 {
				return state.ActiveSandboxDuePage{}, state.ErrActiveSandboxCorrupt
			}
			envelope, err := decodeActiveEnvelope(get.Kvs[0].Value, string(batch[i].Value))
			if err != nil {
				return state.ActiveSandboxDuePage{}, err
			}
			key, err := r.lifecycleIndexKey(envelope)
			if err != nil || key != string(batch[i].Key) {
				return state.ActiveSandboxDuePage{}, state.ErrActiveSandboxCorrupt
			}
			page.Records = append(page.Records, envelope.Record)
			page.Cursor = key
		}
	}
	if len(indexes.Kvs) <= limit {
		page.Cursor = ""
	}
	return page, nil
}
