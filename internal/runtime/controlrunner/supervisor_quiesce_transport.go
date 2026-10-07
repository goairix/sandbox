package controlrunner

import (
	"bytes"
	"context"
	t "github.com/goairix/sandbox/internal/runtime/controltransport"
	"net"
)

// Called only by the genuine activated TLS handshake owner. Admission closure
// does not close management transport or cancel the finite registered worker.
func (s *Supervisor) dispatchTaskQuiesce(ctx context.Context, conn net.Conn, e t.TaskQuiesceEnvelope) error {
	s.mu.Lock()
	if s.closed || s.failed || s.failurePending.Load() || s.activation == nil || s.journal == nil {
		s.mu.Unlock()
		return ErrAdmissionClosed
	}
	a := s.activation
	b, i := a.Binding(), a.Identity()
	c := e.Context.Current
	if c.Namespace != b.Namespace || c.AuthorityID != b.AuthorityID || c.Target != b.Target || c.RestoreEpoch != b.RestoreEpoch || c.SandboxID != i.SandboxID || c.WorkspaceHash != i.WorkspaceHash || c.Generation != i.Generation || c.Runtime != i.Runtime || c.DataGateEpoch != a.DataGateEpoch() {
		s.mu.Unlock()
		return ErrInvalidConfiguration
	}
	issuer := a.IssuerCertificate()
	s.mu.Unlock()
	switch e.Purpose {
	case "task_quiesce_users":
		if !bytes.Equal(issuer, e.IssuerCertificate) {
			return ErrInvalidConfiguration
		}
		now, err := observeControlled(ctx, s.options.Clock)
		if err != nil {
			return err
		}
		evidence, err := s.options.Verifier.VerifyTaskUserQuiescenceTicket(e.Ticket, issuer, e.Context, now)
		if err != nil {
			return err
		}
		if evidence.Digest() != e.TicketDigest {
			return ErrInvalidConfiguration
		}
		owned, err := s.beginUserQuiescence(ctx, &authenticatedUserQuiescence{supervisor: s, evidence: evidence, originalCloseReceipt: bytes.Clone(e.OriginalCloseDataReceipt)})
		if err != nil {
			return err
		}
		wire, err := s.quiescenceAcceptedReceipt(ctx, owned)
		if err != nil {
			// Terminal persistence can win the race before the accepted signer,
			// which intentionally signs pending only. Consume the same producer's
			// fresh terminal query; never begin another attempt or revive failure.
			wire, err = s.queryUserQuiescence(ctx, e.Context, e.TicketDigest)
			if err != nil {
				return err
			}
			if err = ctx.Err(); err != nil {
				return err
			}
			return writeFiniteTransportEvent(ctx, conn, t.EventReceipt, wire)
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		return writeFiniteTransportEvent(ctx, conn, t.EventAccepted, wire)
	case "task_quiesce_users_query":
		wire, err := s.queryUserQuiescence(ctx, e.Context, e.TicketDigest)
		if err == nil {
			if err = ctx.Err(); err != nil {
				return err
			}
			return writeFiniteTransportEvent(ctx, conn, t.EventReceipt, wire)
		}
		// Pending attribution may be reported only through the original owner and
		// its final live signing boundary. Cold or failed history cannot sign it.
		s.mu.Lock()
		owned := s.quiescence
		if owned == nil || owned.evidence.Context() != e.Context || owned.evidence.Digest() != e.TicketDigest {
			s.mu.Unlock()
			return err
		}
		s.mu.Unlock()
		wire, err = s.quiescenceAcceptedReceipt(ctx, owned)
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		return writeFiniteTransportEvent(ctx, conn, t.EventAccepted, wire)
	default:
		return t.ErrFrame
	}
}
