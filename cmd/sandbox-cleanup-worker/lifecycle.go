package main

import (
	"context"
	"errors"
)

var errCleanupWorkerStopped = errors.New("cleanup worker stopped unexpectedly")

// This private seam controls process exit ordering. The only production caller
// supplies the mounted deployment created with the genuine current boundary;
// this interface neither constructs nor authorizes a physical cleanup worker.
type cleanupWorkerLifecycle interface {
	Done() <-chan struct{}
	Wait(context.Context) error
	Close() error
}

func awaitCleanupWorker(ctx context.Context, worker cleanupWorkerLifecycle) (err error) {
	if worker == nil {
		return errCleanupWorkerStopped
	}
	// Close seals admission, joins physical work and then closes dependencies.
	// No signal, worker failure or observation result may bypass this join.
	shutdownRequested := false
	defer func() {
		closeErr := worker.Close()
		if shutdownRequested && cleanupWorkerCancellationOnly(closeErr) {
			closeErr = nil
		}
		err = errors.Join(err, closeErr)
	}()
	if ctx == nil || worker.Done() == nil {
		return errCleanupWorkerStopped
	}

	select {
	case <-ctx.Done():
		shutdownRequested = true
		return nil
	case <-worker.Done():
		terminal := worker.Wait(context.Background())
		if ctx.Err() != nil && (terminal == nil || cleanupWorkerCancellationOnly(terminal)) {
			shutdownRequested = true
			return nil
		}
		// The service has no successful finite-work completion mode. Even a
		// clean actor exit without a shutdown signal must fail the process.
		return errors.Join(errCleanupWorkerStopped, terminal)
	}
}

// Only a requested shutdown may accept cancellation after the actual join.
// Walk every leaf: errors.Is(Canceled) would also accept a joined unknown
// physical outcome or dependency error and incorrectly report a clean exit.
func cleanupWorkerCancellationOnly(err error) bool {
	if err == nil {
		return false
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		children := wrapped.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !cleanupWorkerCancellationOnly(child) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return cleanupWorkerCancellationOnly(wrapped.Unwrap())
	default:
		return err == context.Canceled
	}
}
