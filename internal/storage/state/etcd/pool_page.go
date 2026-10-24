package etcd

import (
	"context"
	"encoding/hex"
	"math"
	"strings"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const maxPoolStatePage = 64

var _ state.FUSEPoolStatePageReader = (*FUSEPoolRepository)(nil)
var _ state.FUSEPoolInventoryReader = (*FUSEPoolRepository)(nil)

// ReadInventory reads only the pool's fixed-size aggregate. The six counts
// move with state indexes under the same transaction; a missing aggregate is
// an empty fresh pool, while a malformed or overflowing one fails closed.
func (r *FUSEPoolRepository) ReadInventory(ctx context.Context, poolKey string) (state.FUSEPoolInventory, error) {
	if r == nil || r.store == nil || !validPoolID(poolKey) {
		return state.FUSEPoolInventory{}, state.ErrFUSEPoolInvalidRecord
	}
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	read, err := r.store.client.Get(requestCtx, r.metadataKey(poolKey))
	if err != nil || len(read.Kvs) == 0 {
		return state.FUSEPoolInventory{}, err
	}
	metadata, err := decodePoolMetadata(read.Kvs[0].Value, poolKey)
	if err != nil || metadata.Total > math.MaxInt64 {
		return state.FUSEPoolInventory{}, state.ErrFUSEPoolCorrupt
	}
	return state.FUSEPoolInventory{
		Preparing: int64(metadata.Preparing), Prepared: int64(metadata.Prepared),
		Reserved: int64(metadata.Reserved), Binding: int64(metadata.Binding),
		Consumed: int64(metadata.Consumed), Cleanup: int64(metadata.Cleanup),
	}, nil
}

// ListStatePage reads at most 64 records from one pool state. The index and
// records are read at one MVCC revision; a mutation between pages may move a
// record behind the cursor, so maintenance must run again rather than treat
// this traversal as a global snapshot.
func (r *FUSEPoolRepository) ListStatePage(ctx context.Context, poolKey string, poolState state.FUSEPoolState, after string, limit int) ([]state.FUSEPoolRecord, string, error) {
	if r == nil || r.store == nil || !validPoolID(poolKey) || !validPoolState(poolState) ||
		limit < 1 || limit > maxPoolStatePage || !validPoolPageCursor(after) {
		return nil, "", state.ErrFUSEPoolInvalidRecord
	}

	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	prefix := r.poolBase(poolKey) + "state/" + string(poolState) + "/"
	start := prefix
	if after != "" {
		start += after + "\x00"
	}
	index, err := r.store.client.Get(requestCtx, start,
		clientv3.WithRange(clientv3.GetPrefixRangeEnd(prefix)), clientv3.WithLimit(int64(limit+1)))
	if err != nil {
		return nil, "", err
	}
	if len(index.Kvs) == 0 {
		return nil, "", nil
	}

	entries := index.Kvs
	more := len(entries) > limit
	if more {
		entries = entries[:limit]
	}
	reads := make([]clientv3.Op, 0, len(entries))
	for _, entry := range entries {
		id := string(entry.Value)
		if !validPoolID(id) || string(entry.Key) != r.stateKey(poolKey, id, poolState) {
			return nil, "", state.ErrFUSEPoolCorrupt
		}
		reads = append(reads, clientv3.OpGet(r.recordKey(id), clientv3.WithRev(index.Header.Revision)))
	}
	result, err := r.store.client.Txn(requestCtx).Then(reads...).Commit()
	if err != nil {
		return nil, "", err
	}
	if len(result.Responses) != len(entries) {
		return nil, "", state.ErrFUSEPoolCorrupt
	}

	records := make([]state.FUSEPoolRecord, 0, len(entries))
	for i, response := range result.Responses {
		rangeResult := response.GetResponseRange()
		if rangeResult == nil || len(rangeResult.Kvs) != 1 {
			return nil, "", state.ErrFUSEPoolCorrupt
		}
		record, err := decodePoolRecord(rangeResult.Kvs[0].Value, string(entries[i].Value))
		if err != nil || record.PoolKey != poolKey || record.State != poolState {
			return nil, "", state.ErrFUSEPoolCorrupt
		}
		records = append(records, record)
	}
	if !more {
		return records, "", nil
	}
	return records, strings.TrimPrefix(string(entries[len(entries)-1].Key), prefix), nil
}

func validPoolPageCursor(cursor string) bool {
	if cursor == "" {
		return true
	}
	if len(cursor) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(cursor)
	return err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == cursor
}
