package etcd

import (
	"context"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
)

// ListByPoolKey deliberately refuses the Redis-era whole-pool contract. The
// business callers use exact reads or state pages when this repository is
// installed; returning all consumed residents would make high-N requests
// depend on N and hide an unsafe fallback.
func (*FUSEPoolRepository) ListByPoolKey(context.Context, string) ([]state.FUSEPoolRecord, error) {
	return nil, state.ErrFUSEPoolUnboundedRead
}

// ServerTime cannot be implemented from etcd KV. Maintenance uses leased
// deadline keys instead; returning this process's clock would be unsafe.
func (*FUSEPoolRepository) ServerTime(context.Context) (time.Time, error) {
	return time.Time{}, state.ErrFUSEPoolNoServerTime
}
