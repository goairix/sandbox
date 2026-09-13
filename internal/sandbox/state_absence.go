package sandbox

import (
	"context"

	"github.com/goairix/sandbox/internal/storage/state"
)

type stateAbsenceConfirmer interface {
	ConfirmAbsence(context.Context, string) (bool, error)
}

// Persistent Redis confirms absence atomically with a replication barrier;
// legacy/local stores retain their original read-only semantics.
func confirmStateAbsence(ctx context.Context, store state.Store, key string) (bool, error) {
	if confirmer, ok := store.(stateAbsenceConfirmer); ok {
		return confirmer.ConfirmAbsence(ctx, key)
	}
	raw, err := store.Get(ctx, key)
	return raw == nil, err
}
