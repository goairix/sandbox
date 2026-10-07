package controlrunner

import (
	"context"
	"encoding/binary"
	"errors"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

func (s *Supervisor) renew(ctx context.Context, e *Execution, evidence controlprotocol.ExecRenewEvidence) error {
	if err := s.validateReceiver(); err != nil {
		return err
	}
	if nilValue(ctx) || e == nil || e.self != e || e.supervisor != s {
		return ErrUnavailable
	}
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	active := s.isActiveExecution(e)
	if !active {
		return ErrAdmissionClosed
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	live := func() bool { now, err := monitorMonotonic(); return err == nil && e.renewableLocked(now) }

	if !live() {
		return ErrUnavailable
	}
	before := e.record.AuthorityDeadline
	if err := s.journal.RenewAccepted(bounded, e.accepted, evidence); err != nil {
		if s.journal.Status().Poisoned {
			s.failurePending.Store(true)
			go s.isolate("renewal_persistence")
		}
		return err
	}
	fail := func(err error) error {
		e.state = "unknown"
		s.failurePending.Store(true)
		go s.isolate("renewal_delivery_unknown")
		return errors.Join(ErrExecutionUnknown, err)
	}
	if !live() {
		return fail(ErrUnavailable)
	}
	record, err := e.accepted.Snapshot()
	if err != nil {
		return fail(err)
	}
	deadline := e.authorityDeadlineNS
	if record.AuthorityDeadline.After(before) {
		deadline, err = s.absoluteDeadline(bounded, record.AuthorityDeadline)
		if err != nil {
			return fail(err)
		}
		deadline = min(deadline, e.commandDeadlineNS)
		deadline = max(deadline, e.authorityDeadlineNS)
	}
	if !live() {
		return fail(ErrUnavailable)
	}
	e.sequence++
	var data [16]byte
	binary.BigEndian.PutUint64(data[:8], e.sequence)
	binary.BigEndian.PutUint64(data[8:], uint64(deadline))
	d, _ := bounded.Deadline()
	if err = e.control.SetWriteDeadline(d); err != nil {
		return fail(err)
	}
	if err = writeMonitorFrame(e.control, monitorRenew, data[:]); err != nil {
		return fail(err)
	}
	select {
	case ack := <-e.ack:
		if ack.sequence != e.sequence || ack.deadline != deadline || bounded.Err() != nil || !live() {
			return fail(ErrUnavailable)
		}
		e.record = record
		e.authorityDeadlineNS = deadline
		mono, err := monitorMonotonic()
		if err != nil {
			return fail(err)
		}
		remaining := watchdogRemaining(mono, deadline, e.commandDeadlineNS)
		if remaining <= 0 {
			return fail(ErrUnavailable)
		}
		e.watchdogDeadline.Store(min(deadline, e.commandDeadlineNS) + int64(30*time.Second))
		e.watchdog.Reset(remaining)
		if err = e.result.SetReadDeadline(time.Now().Add(remaining)); err != nil {
			return fail(err)
		}
		return nil
	case <-e.waitDone:
		return fail(ErrUnavailable)
	case <-bounded.Done():
		return fail(bounded.Err())
	}
}

// Called while the original execution owner mutex is held. Atomic target failure
// remains observable even when isolation cannot acquire that same mutex.
func (e *Execution) renewableLocked(now int64) bool {
	if e.supervisor.failurePending.Load() || e.supervisor.usersClosed.Load() || e.state != "accepted" || e.rootPID <= 1 || e.terminalSeen.Load() || now >= e.authorityDeadlineNS || now >= e.commandDeadlineNS {
		return false
	}
	select {
	case <-e.waitDone:
		return false
	default:
		return true
	}
}

// Registry lookup uses the supervisor lock; mutable records belong exclusively
// to the execution owner lock.
func (s *Supervisor) isActiveExecution(e *Execution) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.admission && !s.closed && !s.failed && !s.failurePending.Load() && s.active[e.commandID] == e
}
