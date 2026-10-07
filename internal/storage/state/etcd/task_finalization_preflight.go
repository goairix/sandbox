package etcd

import (
	"context"

	t "github.com/goairix/sandbox/internal/runtime/controltransport"
)

// RevalidateTaskQuiescence freshly checks the original local prerequisite using
// current authenticated terminal queries around a new whole-operations-prefix
// native observation. Success applies only to this invocation. A future effect
// producer must still fence its own intent transaction and every external send;
// this read-only check does not settle effects, authorize release, or return a
// reusable authority object.
func (b *Backend) RevalidateTaskQuiescence(ctx context.Context, c *TaskClaim, prepared *PreparedTaskQuiescence, destination *t.Destination) error {
	if !b.validTaskClaim(c) || ctx == nil || prepared == nil || prepared.self != prepared || prepared.origin != b || prepared.claim != c || prepared.claimReference != c.reference || prepared.emptyRevision <= 0 || prepared.deadline.IsZero() || len(prepared.receipt.Wire()) == 0 || prepared.pin.intent.create <= 0 || prepared.pin.intent.mod <= 0 || prepared.pin.receipt.create <= 0 || prepared.pin.receipt.mod <= 0 || prepared.pin.reservation.revision <= 0 {
		return ErrInvalidRecord
	}
	_, err := b.prepareAfterDualDrain(ctx, c, destination, prepared)
	return err
}
