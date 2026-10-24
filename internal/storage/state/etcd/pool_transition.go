package etcd

import (
	"context"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
)

// Transition publishes the preparation and checkout edges implemented here.
// Unsupported edges fail explicitly without a partial record update.
func (r *FUSEPoolRepository) Transition(ctx context.Context, id string, from, to state.FUSEPoolState, token string, expectedRevision uint64) (*state.FUSEPoolRecord, error) {
	if r == nil || r.store == nil || !validPoolID(id) || !validPoolID(token) || expectedRevision == 0 {
		return nil, state.ErrFUSEPoolInvalidTransition
	}
	if (from == state.FUSEPoolReserved && to == state.FUSEPoolBinding) ||
		(from == state.FUSEPoolBinding && to == state.FUSEPoolConsumed) {
		return r.transitionCheckout(ctx, id, from, to, token, expectedRevision)
	}
	if from != state.FUSEPoolPreparing || to != state.FUSEPoolPrepared {
		return nil, state.ErrFUSEPoolInvalidTransition
	}
	return r.transitionPreparing(ctx, id, to, token, "", expectedRevision, 0)
}

// TransitionWithRefillLock publishes a prepared or cold-reserved runtime only
// while the original refill holder and preparation lease are both live.
func (r *FUSEPoolRepository) TransitionWithRefillLock(ctx context.Context, id string,
	from, to state.FUSEPoolState, token, refillToken string,
	expectedRevision uint64, reservationTTL time.Duration,
) (*state.FUSEPoolRecord, error) {
	if r == nil || r.store == nil || !validPoolID(id) || !validPoolID(token) ||
		!validPoolID(refillToken) || expectedRevision == 0 || from != state.FUSEPoolPreparing ||
		(to != state.FUSEPoolPrepared && to != state.FUSEPoolReserved) ||
		(to == state.FUSEPoolReserved) != (reservationTTL > 0) {
		return nil, state.ErrFUSEPoolInvalidTransition
	}
	return r.transitionPreparing(ctx, id, to, token, refillToken, expectedRevision, reservationTTL)
}
