//go:build linux && (amd64 || arm64)

package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

const maxDrainProcesses = 4096

// Drain requires the one trusted user spawn to have completed, no new trusted
// spawns, no unrelated children, and exclusive ownership of every wait. Only
// actual __WALL ECHILD plus a fresh boundary check produces local evidence. Any
// attempted operational failure poisons the handle and requires later PID1
// escalation; it never authorizes End or releases workspace ownership.
func (m *MonitorBoundary) Drain(ctx context.Context) (LocalDrainObservation, error) {
	return m.drainWith(ctx, drainCalls{
		validate: m.validateCurrentLocked,
		wait:     func(status *unix.WaitStatus) (int, error) { return unix.Wait4(-1, status, unix.WNOHANG|unix.WALL, nil) },
		scan:     m.killDirectChildren,
	})
}

// Fixed production calls above; this narrow per-call seam permits deterministic
// faults in the synchronous state machine without global mutable syscall hooks.
type drainCalls struct {
	validate func() error
	wait     func(*unix.WaitStatus) (int, error)
	scan     func(context.Context) error
}

func (m *MonitorBoundary) drainWith(ctx context.Context, calls drainCalls) (observation LocalDrainObservation, result error) {
	if err := validateDrainContext(ctx); err != nil {
		return observation, err
	}
	if err := m.validateReceiver(os.Getpid()); err != nil {
		return observation, err
	}
	if !m.mu.TryLock() {
		return observation, fmt.Errorf("%w: monitor operation active", ErrKernelUnavailable)
	}
	defer m.mu.Unlock()
	if m.poisoned {
		return observation, fmt.Errorf("%w: monitor poisoned", ErrUnsafeKernel)
	}
	m.drainStarted = true
	defer func() {
		if result != nil {
			m.poisoned = true
			observation = LocalDrainObservation{}
		}
	}()
	if err := calls.validate(); err != nil {
		return observation, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return observation, err
		}
		var status unix.WaitStatus
		pid, err := calls.wait(&status)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.ECHILD) {
			if err = calls.validate(); err != nil {
				return observation, err
			}
			if err = ctx.Err(); err != nil {
				return observation, err
			}
			return LocalDrainObservation{MonitorPID: m.pid, Reaped: append([]ProcessExit(nil), m.reaped...)}, nil
		}
		if err != nil {
			return observation, unavailable("drain wait4", err)
		}
		if pid > 0 {
			if !status.Exited() && !status.Signaled() {
				return observation, fmt.Errorf("%w: nonterminal wait status", ErrUnsafeKernel)
			}
			if len(m.reaped) >= maxDrainProcesses {
				return observation, fmt.Errorf("%w: drain reap limit", ErrUnsafeKernel)
			}
			m.reaped = append(m.reaped, ProcessExit{PID: pid, WaitStatus: uint32(status)})
			continue
		}
		if pid != 0 {
			return observation, fmt.Errorf("%w: invalid wait PID", ErrUnsafeKernel)
		}
		if err = calls.scan(ctx); err != nil && !vanishedDrainProcess(err) {
			return observation, err
		}
		// Only pending cleanup allocates a timer; there is no detached worker.
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return observation, ctx.Err()
		case <-timer.C:
		}
	}
}

func vanishedDrainProcess(err error) bool {
	return errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ESRCH)
}
