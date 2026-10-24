package etcd

import (
	"context"
	"sort"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// ListPoolKeys reads the durable pool directory, not the per-sandbox state
// indexes. Its result scales with distinct configurations rather than the
// number of resident sandboxes under a configuration.
func (r *FUSEPoolRepository) ListPoolKeys(ctx context.Context) ([]string, error) {
	if r == nil || r.store == nil {
		return nil, state.ErrFUSEPoolInvalidRecord
	}
	prefix := r.store.prefix + "fusepool/v1/pool-index/"
	start := prefix
	var keys []string
	for {
		requestCtx, cancel := r.store.requestContext(ctx)
		read, err := r.store.client.Get(requestCtx, start,
			clientv3.WithRange(clientv3.GetPrefixRangeEnd(prefix)), clientv3.WithLimit(65))
		cancel()
		if err != nil {
			return nil, err
		}
		entries := read.Kvs
		more := len(entries) > 64
		if more {
			entries = entries[:64]
		}
		for _, entry := range entries {
			poolKey := string(entry.Value)
			if !validPoolID(poolKey) || string(entry.Key) != r.directoryKey(poolKey) {
				return nil, state.ErrFUSEPoolCorrupt
			}
			keys = append(keys, poolKey)
		}
		if !more {
			break
		}
		start = string(entries[len(entries)-1].Key) + "\x00"
	}
	sort.Strings(keys)
	for i := 1; i < len(keys); i++ {
		if keys[i] == keys[i-1] {
			return nil, state.ErrFUSEPoolCorrupt
		}
	}
	return keys, nil
}

// DrainRefillLocks is only for a release-wide drain after all API replicas
// have stopped. It removes exact leased lock keys for each durable pool entry;
// a partial failure leaves the directory so repeating the drain is safe.
func (r *FUSEPoolRepository) DrainRefillLocks(ctx context.Context) error {
	poolKeys, err := r.ListPoolKeys(ctx)
	if err != nil {
		return err
	}
	for _, poolKey := range poolKeys {
		requestCtx, cancel := r.store.requestContext(ctx)
		_, err := r.store.client.Delete(requestCtx, r.refillKey(poolKey))
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}
