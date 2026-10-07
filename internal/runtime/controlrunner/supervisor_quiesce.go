package controlrunner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/controltarget"
	"os"
	"time"
)

// finishExecution belongs to the original start/result owner. All callbacks,
// pipe users and the sole cmd.Wait finish before retirement is published. The
// admission snapshot and ownerDone close share s.mu, leaving no retiring gap.
func (s *Supervisor) finishExecution(e *Execution) {
	e.cancel()
	if e.watchdogDone != nil {
		<-e.watchdogDone
	}
	if e.request != nil {
		e.request.Close()
	}
	if e.result != nil {
		e.result.Close()
	}
	if e.control != nil {
		e.control.Close()
	}
	<-e.waitDone
	close(e.output)
	s.mu.Lock()
	close(e.ownerDone)
	if s.active[e.commandID] == e {
		delete(s.active, e.commandID)
	}
	s.publishActiveLocked()
	s.mu.Unlock()
}

// Constructed only by the activated management transport in Task4.
type authenticatedUserQuiescence struct {
	supervisor           *Supervisor
	evidence             p.TaskUserQuiescenceEvidence
	originalCloseReceipt []byte
}
type userQuiescenceAttempt struct {
	self                       *userQuiescenceAttempt
	supervisor                 *Supervisor
	evidence                   p.TaskUserQuiescenceEvidence
	accepted                   *controltarget.AcceptedUserQuiescence
	executions                 []*Execution // immutable original not-yet-owner-joined set
	started, deadline          time.Time
	context                    context.Context
	cancel                     context.CancelFunc
	timer                      *isolationDeadline
	done                       chan struct{}
	ready                      chan struct{} // original pending owner finished; never caller-owned
	pendingErr                 error         // immutable once ready closes
	acceptedWire, terminalWire []byte        // s.mu protected; copied when exposed
	err                        error
	success                    bool
}

func (s *Supervisor) beginUserQuiescence(ctx context.Context, request *authenticatedUserQuiescence) (*userQuiescenceAttempt, error) {
	if err := s.validateReceiver(); err != nil {
		return nil, err
	}
	if nilValue(ctx) || request == nil || request.supervisor != s {
		return nil, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !s.mu.TryLock() {
		return nil, ErrUnavailable
	}
	a, err := s.registerUserQuiescenceLocked(ctx, request)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	// The caller borrows only this wait. Persistence and cleanup belong to the
	// same already-registered coordinator, even if this context expires first.
	select {
	case <-a.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if a.pendingErr != nil {
		return nil, a.pendingErr
	}
	return a, nil
}

// Caller holds the admission mutex. This fixes ownership and the barrier only;
// no pending journal IO can run until the single registered worker starts.
func (s *Supervisor) registerUserQuiescenceLocked(ctx context.Context, request *authenticatedUserQuiescence) (*userQuiescenceAttempt, error) {
	if s.closed || s.failed || s.failurePending.Load() || s.activation == nil || s.journal == nil {
		return nil, ErrUnavailable
	}
	now, err := observeControlled(ctx, s.options.Clock)
	if err != nil {
		return nil, err
	}
	evidence, err := s.options.Verifier.VerifyTaskUserQuiescenceTicket(request.evidence.Wire(), s.activation.IssuerCertificate(), request.evidence.Context(), now)
	if err != nil {
		return nil, err
	}
	receiptDigest := sha256.Sum256(request.originalCloseReceipt)
	if len(request.originalCloseReceipt) == 0 || len(request.originalCloseReceipt) > 8192 || hex.EncodeToString(receiptDigest[:]) != evidence.Context().CloseDataReceiptDigest {
		return nil, ErrUnavailable
	}
	if evidence.Digest() != request.evidence.Digest() {
		return nil, ErrUnavailable
	}
	if a := s.quiescence; a != nil {
		if a.evidence.Context() != evidence.Context() || a.evidence.Digest() != evidence.Digest() || !a.evidence.NotBefore().Equal(evidence.NotBefore()) || !a.evidence.NotAfter().Equal(evidence.NotAfter()) {
			return nil, ErrUnavailable
		}
		if a.err != nil {
			return nil, a.err
		}
		return a, nil
	}
	if len(s.active) > 64 || s.usersClosed.Load() {
		return nil, ErrUnavailable
	}
	// D1: capture/arm before the first pending IO. Permanent closure also wins
	// against an original start owner which has not crossed beginStartLocked.
	t0 := time.Now()
	a := &userQuiescenceAttempt{supervisor: s, evidence: evidence, started: t0, deadline: t0.Add(31 * time.Second), done: make(chan struct{}), ready: make(chan struct{})}
	a.self = a
	a.context, a.cancel = context.WithDeadline(context.Background(), a.deadline)
	s.usersClosed.Store(true)
	s.admission = false
	for _, e := range s.active {
		a.executions = append(a.executions, e)
	}
	s.quiescence = a
	a.timer = s.armIsolationDeadline(a.deadline, "user_quiescence_deadline")
	go s.runUserQuiescence(a, bytes.Clone(request.originalCloseReceipt))
	return a, nil
}
func (s *Supervisor) runUserQuiescence(a *userQuiescenceAttempt, closeReceipt []byte) {
	defer a.cancel()
	defer close(a.done)
	fail := func(err error) {
		s.failurePending.Store(true)
		s.armIsolationDeadline(a.deadline, "user_quiescence_deadline")
		s.mu.Lock()
		a.err = errors.Join(ErrExecutionUnknown, err)
		a.terminalWire = nil
		s.mu.Unlock()
		go s.isolate("user_quiescence_unknown")
	}
	// Preserve the original journal/key serialization under the admission lock.
	// Finite request entries refuse contention; only this registered coordinator
	// owns pending IO and may wait beyond the caller's original deadline.
	s.mu.Lock()
	if s.closed || s.failed || s.failurePending.Load() || a.context.Err() != nil || s.quiescence != a {
		s.mu.Unlock()
		fail(ErrUnavailable)
		a.pendingErr = a.err
		close(a.ready)
		return
	}
	accepted, err := s.journal.AcceptUserQuiescence(a.context, a.evidence, closeReceipt)
	a.accepted = accepted
	if err == nil && (s.closed || s.failed || s.failurePending.Load() || a.context.Err() != nil || s.quiescence != a) {
		err = ErrUnavailable
	}
	if err == nil {
		a.acceptedWire, err = s.journal.SignUserQuiescenceAccepted(a.context, accepted, s.key)
	}
	if err == nil && (a.context.Err() != nil || s.failurePending.Load()) {
		err = ErrUnavailable
	}
	if err != nil {
		a.acceptedWire = nil
		s.mu.Unlock()
		fail(err)
		a.pendingErr = a.err
		close(a.ready)
		return
	}
	s.mu.Unlock()
	close(a.ready)
	if err := s.stopOriginalExecutions(a.context, a.executions, a.deadline); err != nil {
		fail(err)
		return
	}
	entries := make([]controltarget.QuiescedExecution, 0, len(a.executions))
	for _, e := range a.executions {
		select {
		case <-e.ownerDone:
		case <-a.context.Done():
			fail(a.context.Err())
			return
		}
		select {
		case <-e.waitDone:
		default:
			fail(ErrExecutionUnknown)
			return
		}
		e.mu.Lock()
		entry := controltarget.QuiescedExecution{Accepted: e.accepted}
		if !e.startCommitted && e.state == "never_spawned" && e.waitErr == nil {
			entry.Disposition = "never_spawned"
		} else if e.startCommitted && e.state == "local_terminal" && e.waitErr == nil && e.terminalSeen.Load() {
			entry.Disposition = "local_terminal"
			entry.TerminalReceipt = bytes.Clone(e.resultReceipt)
		}
		e.mu.Unlock()
		if entry.Disposition == "" {
			fail(ErrExecutionUnknown)
			return
		}
		entries = append(entries, entry)
	}
	observation, err := s.options.Kernel.ObserveNoUserDescendants(a.context)
	if err != nil {
		fail(err)
		return
	}
	s.mu.Lock()
	if s.closed || s.failed || s.failurePending.Load() || a.context.Err() != nil || s.quiescence != a {
		s.mu.Unlock()
		fail(ErrUnavailable)
		return
	}
	wire, err := s.journal.CompleteUserQuiescence(a.context, a.accepted, observation, entries, s.key)
	// This success transition and joining the original deadline owner must both
	// fit the same pre-IO deadline. Readable terminal bytes cannot revive a loser.
	if err == nil && !s.failurePending.Load() && a.context.Err() == nil && a.timer.succeed() && !s.failurePending.Load() && time.Now().Before(a.deadline) {
		a.terminalWire = bytes.Clone(wire)
		a.success = true
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	fail(errors.Join(ErrUnavailable, err))
}

// stopOriginalExecutions gives the entire immutable set one aggregate second.
// Busy owner locks, blocked writes or exhausted budget fail conservatively.
func (s *Supervisor) stopOriginalExecutions(ctx context.Context, executions []*Execution, absolute time.Time) error {
	end := time.Now().Add(time.Second)
	if absolute.Before(end) {
		end = absolute
	}
	for _, e := range executions {
		select {
		case <-e.ownerDone:
			continue
		default:
		}
		if ctx.Err() != nil || !time.Now().Before(end) || !e.mu.TryLock() {
			return ErrExecutionUnknown
		}
		e.closeRequested = true
		var err error
		if e.control != nil && e.startCommitted && e.state == "accepted" {
			err = e.control.SetWriteDeadline(end)
			if err == nil {
				err = writeMonitorFrame(e.control, monitorCancel, nil)
			}
		}
		e.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}
func (s *Supervisor) queryUserQuiescence(ctx context.Context, expected p.TaskUserQuiescenceContext, digest string) (wire []byte, err error) {
	if err = s.validateReceiver(); err != nil {
		return nil, err
	}
	if nilValue(ctx) {
		return nil, ErrUnavailable
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if !s.mu.TryLock() {
		return nil, ErrExecutionUnknown
	}
	defer s.mu.Unlock()
	a := s.quiescence
	if s.closed || s.failed || s.failurePending.Load() || s.activation == nil || a == nil || a.self != a || a.supervisor != s || !a.success || a.err != nil || !s.usersClosed.Load() || a.evidence.Context() != expected || a.evidence.Digest() != digest {
		return nil, ErrExecutionUnknown
	}
	select {
	case <-a.done:
	default:
		return nil, ErrExecutionUnknown
	}
	// Historical success gets a fresh query context/census/certificate, never a
	// renewed cleanup timer or the original now-expired coordinator context.
	observation, err := s.options.Kernel.ObserveNoUserDescendants(bounded)
	if err != nil {
		return nil, err
	}
	wire, err = s.journal.SignUserQuiescenceReceipt(bounded, expected, digest, observation, s.key)
	if bounded.Err() != nil || s.failurePending.Load() {
		return nil, ErrExecutionUnknown
	}
	return wire, err
}
func (s *Supervisor) armIsolationDeadline(at time.Time, reason string) *isolationDeadline {
	s.deadlineMu.Lock()
	defer s.deadlineMu.Unlock()
	d := s.isolationDeadline
	if d != nil {
		d.mu.Lock()
		stopped := d.state == 2
		d.mu.Unlock()
		if !stopped {
			d.shorten(at)
			return d
		}
	}
	d = newIsolationDeadline(at, func() {
		s.failurePending.Store(true)
		var active []*Execution
		if view := s.activeView.Load(); view != nil {
			active = *view
		}
		s.emitIsolation(reason, active)
		os.Exit(70)
	})
	s.isolationDeadline = d
	return d
}

// The later transport must use this final locked copy boundary, never expose
// cached accepted bytes after async failure, cancellation or absolute expiry.
func (s *Supervisor) quiescenceAcceptedReceipt(ctx context.Context, a *userQuiescenceAttempt) ([]byte, error) {
	if nilValue(ctx) {
		return nil, ErrUnavailable
	}
	if a == nil || a.self != a || a.supervisor != s || a.ready == nil {
		return nil, ErrExecutionUnknown
	}
	select {
	case <-a.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if !s.mu.TryLock() {
		return nil, ErrExecutionUnknown
	}
	defer s.mu.Unlock()
	if a == nil || a.self != a || a.supervisor != s || s.quiescence != a || s.closed || s.failed || s.activation == nil || s.failurePending.Load() || a.err != nil || ctx.Err() != nil || !time.Now().Before(a.deadline) {
		return nil, ErrExecutionUnknown
	}
	if len(a.acceptedWire) == 0 {
		return nil, ErrExecutionUnknown
	}
	wire, err := s.journal.SignUserQuiescenceAccepted(ctx, a.accepted, s.key)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil || s.failurePending.Load() || !time.Now().Before(a.deadline) {
		return nil, ErrExecutionUnknown
	}
	return wire, nil
}
