package controlrunner

import (
	"bytes"
	"context"
	"net"

	t "github.com/goairix/sandbox/internal/runtime/controltransport"
)

// Only the activated TLS handler reaches this dispatch. The admission mutex
// orders closure against accepts without destroying preaccepted execution owners.
func (s *Supervisor) dispatchTaskClose(ctx context.Context, conn net.Conn, e t.TaskCloseEnvelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.failed || s.failurePending.Load() || s.activation == nil || s.journal == nil {
		return ErrAdmissionClosed
	}
	a := s.activation
	b, i := a.Binding(), a.Identity()
	c := e.Context
	if c.Namespace != b.Namespace || c.AuthorityID != b.AuthorityID || c.Target != b.Target || c.RestoreEpoch != b.RestoreEpoch || c.SandboxID != i.SandboxID || c.WorkspaceHash != i.WorkspaceHash || c.Generation != i.Generation || c.Runtime != i.Runtime || c.DataGateEpoch != a.DataGateEpoch() {
		return ErrInvalidConfiguration
	}
	now, err := observeControlled(ctx, s.options.Clock)
	if err != nil {
		return err
	}
	issuer, err := s.options.Verifier.VerifyCommandIssuerCertificate(a.IssuerCertificate(), now)
	if err != nil {
		return err
	}
	if c.IssuerCertificateID != issuer.CertificateID() || c.IssuerCertificateDigest != issuer.Digest() {
		return ErrInvalidConfiguration
	}
	switch e.Purpose {
	case "task_close_data":
		if !bytes.Equal(e.IssuerCertificate, a.IssuerCertificate()) {
			return ErrInvalidConfiguration
		}
		evidence, err := s.options.Verifier.VerifyTaskCloseDataTicket(e.Ticket, e.IssuerCertificate, c, now)
		if err != nil {
			return err
		}
		if evidence.Digest() != e.TicketDigest {
			return ErrInvalidConfiguration
		}
		s.admission = false
		if _, err = s.journal.CloseData(ctx, evidence); err != nil {
			return err
		}
	case "task_close_data_query":
		// History performs no close, no activation renewal and no state transition.
	default:
		return t.ErrFrame
	}
	wire, err := s.journal.SignDataCloseReceipt(ctx, c, e.TicketDigest, s.key)
	if err != nil {
		return err
	}
	if ctx.Err() != nil || s.failurePending.Load() {
		return ErrUnavailable
	}
	return writeTransportEvent(conn, t.EventReceipt, wire)
}
