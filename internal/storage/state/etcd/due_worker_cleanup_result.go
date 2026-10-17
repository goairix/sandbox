package etcd

import (
	"context"
	"errors"
	"fmt"
)

// One worker ledger and one immutable terminal copy share this fixed charge
// inside the existing automatic 4MiB reservation. No extra Task identity or
// dynamic cache capacity is admitted. The former 3,670,016-byte maximum plus
// this 1KiB remains below 4MiB, including the fixed join/error object overhead.
const cleanupResultReservedBytes = 1024

// These are static identities, not retained dependency error graphs. Unknown
// dependencies remain a nonnil failure even when none of these flags match.
var cleanupResultCauses = [...]error{
	context.Canceled, context.DeadlineExceeded, ErrOutcomeUnknown,
	ErrInvalidConfiguration, ErrIdentityMismatch, ErrConflict, ErrGuardExpired,
	ErrInvalidMutation, ErrCorruptReceipt, ErrSnapshotReadCapacity, ErrInvalidRecord,
}

// cleanupJobResult retains only one copied diagnostic and cumulative classes.
// It never owns the completed job, its Task, or an arbitrary error/Unwrap graph.
// Successful recovery leaves this history intact until the worker is discarded.
type cleanupJobResult struct {
	message  [256]byte
	length   uint16
	flags    uint16
	failures uint64
}

func (r *cleanupJobResult) Error() string {
	return fmt.Sprintf("automatic cleanup failures=%d: %s", r.failures, r.message[:r.length])
}

// Is preserves a fixed set of cause identities without retaining the original
// error graph. This error is itself a non-cancellation leaf: mixed canceled
// failures must never be mistaken for an ordinary requested shutdown.
func (r *cleanupJobResult) Is(target error) bool {
	for i, cause := range cleanupResultCauses {
		if target == cause {
			return r.flags&(1<<i) != 0
		}
	}
	return false
}

// Only a tree whose every actual leaf is context.Canceled is clean shutdown.
// errors.Is alone would incorrectly discard Join(Canceled, dependencyError).
func cleanupOnlyCanceled(err error) bool {
	if err == context.Canceled {
		return true
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		seen := false
		for _, child := range wrapped.Unwrap() {
			if child == nil {
				continue
			}
			seen = true
			if !cleanupOnlyCanceled(child) {
				return false
			}
		}
		return seen
	case interface{ Unwrap() error }:
		return cleanupOnlyCanceled(wrapped.Unwrap())
	default:
		return false
	}
}

// Capture only after the original job/callbacks have actually returned. The
// fixed array copies the first 256 bytes rather than retaining a large string's
// backing storage; later publication owns no dependency error reference.
func cleanupResultFailure(err error) cleanupJobResult {
	if err == nil || cleanupOnlyCanceled(err) {
		return cleanupJobResult{}
	}
	r := cleanupJobResult{failures: 1}
	r.length = uint16(copy(r.message[:], err.Error()))
	for i, cause := range cleanupResultCauses {
		if errors.Is(err, cause) {
			r.flags |= 1 << i
		}
	}
	return r
}

// Requires archiveLife.mu, in the same publication as registry removal/done.
// No failure cancels the actor: other jobs and later reconciliation continue.
func (w *DueWorker) recordCleanupResultLocked(result cleanupJobResult) {
	if !w.options.AutomaticCleanup || result.failures == 0 {
		return
	}
	r := &w.cleanupResults
	if r.failures == 0 {
		r.message, r.length = result.message, result.length
	}
	if r.failures != ^uint64(0) {
		r.failures++
	}
	r.flags |= result.flags
	w.status.CleanupFailures = r.failures
	w.status.LastError = dueWorkerError(r)
}

// Requires archiveLife.mu. The terminal result owns one immutable fixed copy;
// it never points at a live mutable ledger or grows an errors.Join history.
func (w *DueWorker) cleanupResultErrorLocked() error {
	if w.cleanupResults.failures == 0 {
		return nil
	}
	result := w.cleanupResults
	return &result
}
