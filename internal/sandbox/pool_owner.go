package sandbox

import (
	"context"
	"errors"

	"github.com/goairix/sandbox/internal/storage/state"
)

// refreshPoolOwner recovers expired membership under the same lock used for
// retirement. A transient Redis outage must pause a pool, not permanently stop
// every healthy API. Each owner key contains an unguessable instance token.
func refreshPoolOwner(ctx context.Context, store state.AtomicStore, ownerKey string, token []byte, lockKey, registryKey, poolKey string) error {
	ok, err := store.CompareAndSwap(ctx, ownerKey, token, token, ordinaryPoolLockTTL)
	if err != nil || ok {
		return err
	}
	return withPoolLock(ctx, store, lockKey, func(ctx context.Context) error {
		if registryKey != "" {
			if err := store.Set(ctx, registryKey, []byte(poolKey), 0); err != nil {
				return err
			}
		}
		ok, err := store.SetNX(ctx, ownerKey, token, ordinaryPoolLockTTL)
		if err != nil || ok {
			return err
		}
		ok, err = store.CompareAndSwap(ctx, ownerKey, token, token, ordinaryPoolLockTTL)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("pool owner lease token changed")
		}
		return nil
	})
}
