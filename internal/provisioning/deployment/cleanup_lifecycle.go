package deployment

import "context"

// Done observes the original worker's physical completion, including accepted
// cleanup jobs and original session release. It does not cancel the worker or
// close deployment dependencies; callers must still call Close. An absent or
// unopened deployment returns nil rather than advertising successful startup.
func (d *LocalCleanupDeployment) Done() <-chan struct{} {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	w := d.worker
	d.mu.Unlock()
	if w == nil {
		return nil
	}
	return w.Done()
}

// Wait observes actual worker completion and returns its terminal result.
// Canceling ctx stops only this observation: it neither seals admission nor
// stops the worker, joins physical work, or closes deployment dependencies.
// Close remains required even after Wait reports completion or cancellation.
func (d *LocalCleanupDeployment) Wait(ctx context.Context) error {
	if d == nil || ctx == nil {
		return errConfiguration
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	w := d.worker
	d.mu.Unlock()
	if w == nil {
		return errConfiguration
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.Done():
		// Done is already closed: Stop cannot cancel live work here. It only
		// retrieves the original immutable result after the actual join.
		return w.Stop()
	}
}
