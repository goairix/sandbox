package launcher

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

// RootExitObservation attributes one exact already-spawned root's actual raw
// terminal wait status only after complete local drain. It grants no start or End.
type RootExitObservation struct {
	RootPID        int
	RootWaitStatus uint32
	Drain          LocalDrainObservation
	Reason         string
}

// RootLifecycle coordinates the sole wait owner of one already-spawned root.
// It is process-local, cannot be copied/reused, and grants no start authority.
// The caller may deliver only durably accepted absolute monotonic deadlines.
type RootLifecycle struct {
	mu                          sync.Mutex
	self                        *RootLifecycle
	pid                         int
	deadline                    int64
	registered, running, closed bool
	requests                    chan rootRenewRequest
	done                        chan struct{}
	ready                       chan struct{}
}
type rootRenewRequest struct {
	ctx      context.Context
	deadline int64
	result   chan error
}

func newRootLifecycle(deadline int64) *RootLifecycle {
	l := &RootLifecycle{pid: os.Getpid(), deadline: deadline, requests: make(chan rootRenewRequest, 1), done: make(chan struct{}), ready: make(chan struct{})}
	l.self = l
	return l
}
func (l *RootLifecycle) validateReceiver() error {
	if l == nil || l.self == nil {
		return ErrKernelUnavailable
	}
	if l.self != l || l.pid != os.Getpid() {
		return fmt.Errorf("%w: copied lifecycle or another process", ErrUnsafeKernel)
	}
	return nil
}
func (l *RootLifecycle) bind() error {
	if err := l.validateReceiver(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.registered || l.closed {
		return fmt.Errorf("%w: reused root lifecycle", ErrUnsafeKernel)
	}
	l.registered = true
	l.running = true
	close(l.ready)
	return nil
}
func (l *RootLifecycle) stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.running = false
		l.closed = true
		close(l.done)
	}
}

// Renew never starts a worker. The sole wait owner processes one bounded queued
// request and linearizes success before any subsequent cleanup transition.
func (l *RootLifecycle) Renew(ctx context.Context, deadlineNS int64) error {
	if err := l.validateReceiver(); err != nil {
		return err
	}
	if ctx == nil {
		return ErrKernelUnavailable
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > time.Second {
		return fmt.Errorf("%w: renewal caller needs <=1second deadline", ErrUnsafeKernel)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	running := l.registered && l.running && !l.closed
	l.mu.Unlock()
	if !running {
		return ErrKernelUnavailable
	}
	r := rootRenewRequest{ctx: ctx, deadline: deadlineNS, result: make(chan error, 1)}
	select {
	case <-l.done:
		return ErrKernelUnavailable
	case l.requests <- r:
	default:
		return fmt.Errorf("%w: renewal queue full", ErrKernelUnavailable)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-r.result:
		if e := ctx.Err(); e != nil {
			return e
		}
		return err
	case <-l.done:
		// A successful response linearized before the owner closed remains valid.
		select {
		case err := <-r.result:
			if e := ctx.Err(); e != nil {
				return e
			}
			return err
		default:
			return ErrKernelUnavailable
		}
	}
}

func validateRootContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil root context", ErrUnsafeKernel)
	}
	d, ok := ctx.Deadline()
	if !ok || time.Until(d) > 3600*time.Second {
		return fmt.Errorf("%w: root context requires <=3600second deadline", ErrUnsafeKernel)
	}
	// A structurally valid but canceled command still requires fresh cleanup.
	return nil
}

// WaitRegistered observes ownership of an already-spawned root. It grants no
// spawn or continued-liveness authority; closed ownership wins over readiness.
func (l *RootLifecycle) WaitRegistered(ctx context.Context) error {
	if err := l.validateReceiver(); err != nil {
		return err
	}
	if ctx == nil {
		return ErrKernelUnavailable
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > time.Second {
		return ErrUnsafeKernel
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-l.done:
		return ErrKernelUnavailable
	case <-l.ready:
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !l.registered || !l.running || l.closed {
		return ErrKernelUnavailable
	}
	return nil
}
