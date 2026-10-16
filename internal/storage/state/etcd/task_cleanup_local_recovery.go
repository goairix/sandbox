package etcd

import "time"

// Accepted is not terminal quiescence. Wait using only the original read-only
// query, without resending a command or constructing another Prepared value.
// Any unknown/error retains the original pending reference; only a verified
// terminal receipt permits the following dual-drain preparation.
func (b *Backend) waitCleanupQuiescence(j *cleanupLocalJob, result TaskUserQuiescenceDeliveryResult) error {
	for result.Receipt == nil {
		if result.Accepted == nil {
			return ErrDeliveryUnknown
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-j.ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return j.ctx.Err()
		case <-timer.C:
		}
		var err error
		result, err = b.QueryTaskQuiescence(j.ctx, j.task, j.target.destination)
		if err != nil {
			return err
		}
	}
	return nil
}
