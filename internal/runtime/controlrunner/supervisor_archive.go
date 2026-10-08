package controlrunner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/controltarget"
	tr "github.com/goairix/sandbox/internal/runtime/controltransport"
)

// This original registration precedes every pending journal or source IO.
// done joins the source, terminal signing, completion IO and TLS close_notify.
type workspaceArchiveAttempt struct {
	self             *workspaceArchiveAttempt
	supervisor       *Supervisor
	evidence         p.TaskWorkspaceArchiveEvidence
	prior            *userQuiescenceAttempt
	accepted         *controltarget.AcceptedWorkspaceArchive
	context          context.Context
	cancel           context.CancelFunc
	deadline         time.Time
	timer            *isolationDeadline
	done             chan struct{}
	terminal         []byte // s.mu: genuine local signed terminal, independent of delivery
	err, deliveryErr error  // s.mu
	joinedSuccess    bool   // s.mu: published by handler only after physical worker/timer join
}

func (s *Supervisor) registerWorkspaceArchive(ctx, session context.Context, conn net.Conn, e tr.TaskWorkspaceArchiveEnvelope) (*workspaceArchiveAttempt, error) {
	if ctx.Err() != nil || session.Err() != nil || !s.mu.TryLock() {
		return nil, ErrUnavailable
	}
	defer s.mu.Unlock()
	q := s.quiescence
	if s.closed || s.closeStarted.Load() || s.failed || s.failurePending.Load() || s.activation == nil || s.journal == nil || q == nil || q.self != q || q.supervisor != s || !q.success || q.err != nil || !s.usersClosed.Load() || !bytes.Equal(q.terminalWire, e.OriginalQuiescenceReceipt) {
		return nil, ErrUnavailable
	}
	select {
	case <-q.done:
	default:
		return nil, ErrUnavailable
	}
	if s.archive != nil {
		return nil, ErrUnavailable
	}
	if !bytes.Equal(e.IssuerCertificate, s.activation.IssuerCertificate()) {
		return nil, ErrUnavailable
	}
	started := time.Now()
	now, err := observeControlled(ctx, s.options.Clock)
	if err != nil {
		return nil, err
	}
	evidence, err := s.options.Verifier.VerifyTaskWorkspaceArchiveTicket(e.Ticket, e.IssuerCertificate, e.Context, now)
	if err != nil || evidence.Digest() != e.TicketDigest || e.Context.QuiescenceContext != q.evidence.Context() || e.Context.QuiescenceTicketDigest != q.evidence.Digest() {
		return nil, errors.Join(ErrUnavailable, err)
	}
	// observeControlled permits at most one second of verified uncertainty.
	// Capture monotonic start before observation, never after pending IO.
	deadline := started.Add(evidence.SessionNotAfter().Sub(now) - time.Second)
	if !time.Now().Before(deadline) {
		return nil, ErrUnavailable
	}
	a := &workspaceArchiveAttempt{supervisor: s, evidence: evidence, prior: q, deadline: deadline, done: make(chan struct{})}
	a.self = a
	a.context, a.cancel = context.WithDeadline(session, deadline)
	s.archive = a
	a.timer = s.armIsolationDeadline(deadline, "workspace_archive_deadline")
	key := bytes.Clone(s.key)
	go s.runWorkspaceArchive(a, conn, bytes.Clone(e.OriginalQuiescenceReceipt), key)
	return a, nil
}

func (s *Supervisor) runWorkspaceArchive(a *workspaceArchiveAttempt, conn net.Conn, receipt []byte, key ed25519.PrivateKey) {
	defer func() {
		clear(key)
		a.cancel()
		close(a.done)
	}()
	fail := func(err error) {
		s.mu.Lock()
		a.err = errors.Join(ErrExecutionUnknown, err)
		s.mu.Unlock()
		s.failurePending.Store(true)
		s.armIsolationDeadline(a.deadline, "workspace_archive_deadline")
		go s.isolate("workspace_archive_unknown")
	}
	conn.(interface{ sealArchiveDeadline(time.Time) }).sealArchiveDeadline(a.deadline)
	if err := conn.SetDeadline(a.deadline); err != nil {
		fail(err)
		return
	}
	accepted, err := s.journal.AcceptWorkspaceArchive(a.context, a.evidence, a.prior.accepted, receipt)
	if err != nil {
		fail(err)
		return
	}
	a.accepted = accepted
	bound, err := accepted.Deadline()
	if err != nil {
		fail(err)
		return
	}
	if bound.Before(a.deadline) {
		// The original externally visible bound is never extended.
		a.timer.shorten(bound)
		conn.(interface{ sealArchiveDeadline(time.Time) }).sealArchiveDeadline(bound)
		if err = conn.SetDeadline(bound); err != nil {
			fail(err)
			return
		}
	}
	observation, err := s.options.Kernel.ObserveNoUserDescendants(a.context)
	if err != nil {
		fail(err)
		return
	}
	source, err := s.options.Kernel.OpenWorkspaceArchiveAsUser(a.context, observation, s.birth.UID, s.birth.GID)
	if err != nil {
		fail(err)
		return
	}
	wire, err := s.journal.StreamWorkspaceArchive(a.context, accepted, source, archiveFrameWriter{ctx: a.context, conn: conn}, key)
	err = errors.Join(err, source.Close())
	if err != nil {
		fail(err)
		return
	}
	s.mu.Lock()
	a.terminal = bytes.Clone(wire)
	s.mu.Unlock()
	err = tr.WriteTaskWorkspaceArchiveFrame(conn, tr.ArchiveComplete, wire)
	if err == nil {
		if half, ok := conn.(interface{ CloseWrite() error }); ok {
			err = half.CloseWrite()
		} else {
			err = ErrUnavailable
		}
	}
	s.mu.Lock()
	a.deliveryErr = err
	s.mu.Unlock()
}

// Called by the original handler after the worker's cleanup and physical join.
// Its transport borrower remains registered through this final arbitration.
func (s *Supervisor) finishWorkspaceArchive(a *workspaceArchiveAttempt) {
	s.mu.Lock()
	if a.err != nil || s.closeStarted.Load() {
		s.mu.Unlock()
		return
	}
	s.deadlineMu.Lock()
	if s.closeStarted.Load() {
		s.deadlineMu.Unlock()
		s.mu.Unlock()
		return
	}
	a.timer.mu.Lock()
	deadline := a.timer.deadline
	a.timer.mu.Unlock()
	joined := !s.failurePending.Load() && a.timer.succeed() && !s.failurePending.Load() && time.Now().Before(deadline)
	if joined {
		a.joinedSuccess = true
	} else {
		a.err = ErrExecutionUnknown
		s.failurePending.Store(true)
	}
	s.deadlineMu.Unlock()
	s.mu.Unlock()
	if !joined {
		s.armIsolationDeadline(deadline, "workspace_archive_deadline")
		go s.isolate("workspace_archive_unknown")
	}
}

func (s *Supervisor) queryWorkspaceArchive(ctx context.Context, e tr.TaskWorkspaceArchiveEnvelope) ([]byte, error) {
	if ctx.Err() != nil || !s.mu.TryLock() {
		return nil, ErrUnavailable
	}
	a := s.archive
	if s.closed || s.closeStarted.Load() || s.failed || s.failurePending.Load() || a == nil || a.self != a || a.supervisor != s || a.evidence.Context() != e.Context || a.evidence.Digest() != e.TicketDigest || a.err != nil || !a.joinedSuccess || len(a.terminal) == 0 {
		s.mu.Unlock()
		return nil, ErrUnavailable
	}
	select {
	case <-a.done:
	default:
		s.mu.Unlock()
		return nil, ErrUnavailable
	}
	accepted, key, journal := a.accepted, bytes.Clone(s.key), s.journal
	s.mu.Unlock()
	defer clear(key)
	o, err := s.options.Kernel.ObserveNoUserDescendants(ctx)
	if err != nil {
		return nil, err
	}
	wire, err := journal.SignWorkspaceArchiveReceipt(ctx, accepted, o, key)
	if ctx.Err() != nil || s.failurePending.Load() {
		return nil, ErrUnavailable
	}
	return wire, err
}
