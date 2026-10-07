//go:build linux && (amd64 || arm64)

package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

type rootCalls struct {
	drainCalls
	root func(int) error
	now  func() (int64, error)
}

func monotonicNS() (int64, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0, err
	}
	return ts.Nano(), nil
}

// NewRootLifecycle stores an absolute namespace monotonic deadline; startup and
// queue delay cannot reset it. It does not certify authority or spawn a child.
func NewRootLifecycle(initialDeadlineNS int64) (*RootLifecycle, error) {
	now, err := monotonicNS()
	if err != nil {
		return nil, err
	}
	if initialDeadlineNS <= 0 || initialDeadlineNS-now > int64(30*time.Second) {
		return nil, fmt.Errorf("%w: lifecycle deadline outside bound", ErrUnsafeKernel)
	}
	return newRootLifecycle(initialDeadlineNS), nil
}
func (m *MonitorBoundary) rootCalls() rootCalls {
	return rootCalls{drainCalls: drainCalls{validate: m.validateCurrentLocked, wait: func(s *unix.WaitStatus) (int, error) { return unix.Wait4(-1, s, unix.WNOHANG|unix.WALL, nil) }, scan: m.killDirectChildren}, root: m.validateRootChild, now: monotonicNS}
}

// SuperviseRoot observes exactly one already-completed trusted spawn. The caller
// must own all child waits and perform no further spawn. There is no Cmd.Wait.
func (m *MonitorBoundary) SuperviseRoot(ctx context.Context, rootPID int) (RootExitObservation, error) {
	return m.superviseRootWith(ctx, rootPID, nil, m.rootCalls())
}
func (m *MonitorBoundary) SuperviseRootWithLifecycle(ctx context.Context, rootPID int, life *RootLifecycle) (RootExitObservation, error) {
	if life == nil {
		return RootExitObservation{}, ErrKernelUnavailable
	}
	return m.superviseRootWith(ctx, rootPID, life, m.rootCalls())
}
func (m *MonitorBoundary) superviseRootWith(ctx context.Context, rootPID int, life *RootLifecycle, calls rootCalls) (observation RootExitObservation, result error) {
	if err := validateRootContext(ctx); err != nil {
		return observation, err
	}
	if err := m.validateReceiver(os.Getpid()); err != nil {
		return observation, err
	}
	if rootPID <= 1 || rootPID == m.pid {
		return observation, fmt.Errorf("%w: invalid root PID", ErrUnsafeKernel)
	}
	if !m.mu.TryLock() {
		return observation, ErrKernelUnavailable
	}
	defer m.mu.Unlock()
	if m.poisoned || m.drainStarted {
		return observation, fmt.Errorf("%w: root wait already used", ErrUnsafeKernel)
	}
	m.drainStarted = true
	defer func() {
		if result != nil {
			m.poisoned = true
			observation = RootExitObservation{}
		}
	}()
	if err := calls.validate(); err != nil {
		return observation, err
	}
	if err := calls.root(rootPID); err != nil {
		return observation, err
	}
	if life != nil {
		if err := life.bind(); err != nil {
			return observation, err
		}
		defer life.stop()
	}
	reason := "root_exit"
waitRoot:
	for {
		if err := ctx.Err(); err != nil {
			reason = "command_canceled"
			if errors.Is(err, context.DeadlineExceeded) {
				reason = "command_timeout"
			}
			break
		}
		if life != nil {
			now, err := calls.now()
			if err != nil {
				return observation, err
			}
			if now >= life.deadline {
				reason = "authority_expired"
				break
			}
		}
		var status unix.WaitStatus
		pid, err := calls.wait(&status)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.ECHILD) {
			return observation, fmt.Errorf("%w: root status missing", ErrUnsafeKernel)
		}
		if err != nil {
			return observation, unavailable("root wait4", err)
		}
		if pid > 0 {
			if !status.Exited() && !status.Signaled() {
				return observation, fmt.Errorf("%w: nonterminal root wait status", ErrUnsafeKernel)
			}
			if len(m.reaped) >= maxDrainProcesses {
				return observation, fmt.Errorf("%w: combined reap limit", ErrUnsafeKernel)
			}
			m.reaped = append(m.reaped, ProcessExit{PID: pid, WaitStatus: uint32(status)})
			if pid == rootPID {
				break
			}
			continue
		}
		if pid != 0 {
			return observation, fmt.Errorf("%w: invalid root wait PID", ErrUnsafeKernel)
		}
		if life != nil {
			select {
			case r := <-life.requests:
				now, e := calls.now()
				if e != nil {
					return observation, e
				}
				if ctx.Err() != nil || now >= life.deadline {
					r.result <- ErrKernelUnavailable
					reason = "authority_expired"
					break waitRoot
				}
				if e = r.ctx.Err(); e != nil {
					r.result <- e
					continue
				}
				if r.deadline < life.deadline || r.deadline <= now || r.deadline-now > int64(30*time.Second) {
					r.result <- fmt.Errorf("%w: renewal deadline outside bound", ErrUnsafeKernel)
					continue
				}
				// No callback/IO in the sole owner. Recheck old authority immediately
				// before this nonblocking buffered response linearizes the extension.
				now, e = calls.now()
				if e != nil {
					return observation, e
				}
				if ctx.Err() != nil || now >= life.deadline || r.ctx.Err() != nil {
					r.result <- ErrKernelUnavailable
					if ctx.Err() != nil || now >= life.deadline {
						reason = "authority_expired"
						break waitRoot
					}
					continue
				}
				life.deadline = r.deadline
				r.result <- nil
			default:
			}
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	if life != nil {
		life.stop()
	}
	// Cleanup has its own bounded fresh budget, independent of command/authority
	// cancellation; the same private lock and wait owner remain held throughout.
	cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	drain, err := m.drainLocked(cleanup, calls.drainCalls)
	if err != nil {
		return observation, err
	}
	found := false
	var raw uint32
	for _, exit := range drain.Reaped {
		if exit.PID == rootPID {
			if found {
				return observation, fmt.Errorf("%w: repeated root status", ErrUnsafeKernel)
			}
			found = true
			raw = exit.WaitStatus
		}
	}
	if !found {
		return observation, fmt.Errorf("%w: root status absent after drain", ErrUnsafeKernel)
	}
	return RootExitObservation{RootPID: rootPID, RootWaitStatus: raw, Drain: drain, Reason: reason}, nil
}

func (m *MonitorBoundary) validateRootChild(pid int) (result error) {
	fd, err := unix.Open("/proc/"+strconv.Itoa(pid), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, unix.Close(fd)) }()
	data, err := readDrainFile(fd, "status", maxStatusBytes)
	if err != nil {
		return err
	}
	s, err := parseDrainStatus(data, pid)
	if err != nil {
		return err
	}
	group, err := readDrainFile(fd, "cgroup", maxCgroupBytes)
	if err != nil {
		return err
	}
	return m.checkDrainChild(s, group)
}
