package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	t "github.com/goairix/sandbox/internal/runtime/controltransport"
)

type TaskCloseDataDeliveryResult struct {
	Receipt *p.TaskDataClosedReceiptEvidence
}

// DeliverTaskCloseData sends the original committed command at most once. Any
// uncertainty retains its permanent intent; history never reconstructs dispatch.
func (b *Backend) DeliverTaskCloseData(ctx context.Context, prepared *PreparedTaskCloseData, destination *t.Destination) (TaskCloseDataDeliveryResult, error) {
	empty := TaskCloseDataDeliveryResult{}
	if b == nil || ctx == nil || prepared == nil || prepared.self != prepared || prepared.origin != b || prepared.claim == nil || prepared.draft == nil {
		return empty, ErrInvalidRecord
	}
	c, d := prepared.claim, prepared.draft
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.origin != b || c.self != c || c.parentCtx == nil || c.closeDraft != d || d.outcome != OutcomeCommitted || d.issuer == nil || !destination.MatchTaskClose(d.claims.Context, d.certificate) {
		return empty, ErrInvalidRecord
	}
	if d.deliveryAttempted {
		return empty, ErrDeliveryUnknown
	}
	request, finish := b.taskCloseContext(ctx, c, d)
	defer finish()
	bounded, cancel := context.WithTimeout(request, 5*time.Second)
	defer cancel()
	authorize := func() error {
		if err := b.authorizePreparedTaskClose(bounded, c, d); err != nil {
			return err
		}
		now, err := b.observePublicationClock(bounded)
		if err != nil {
			return err
		}
		if err = destination.VerifyTaskCloseRuntime(b.taskVerifier, d.claims.Context, d.claims.NotBefore, d.claims.NotAfter, now); err != nil {
			return err
		}
		return taskCloseLive(bounded, c, d)
	}
	if err := authorize(); err != nil {
		return empty, err
	}
	session, err := destination.Open(bounded)
	if err != nil {
		return empty, err
	}
	defer session.Close()
	if err = authorize(); err != nil {
		return empty, err
	}
	envelope := t.TaskCloseEnvelope{Version: 1, Purpose: "task_close_data", Context: d.claims.Context, TicketDigest: d.ticket.Digest(), Ticket: d.ticket.Wire(), IssuerCertificate: append([]byte(nil), d.certificate...)}
	// Repeat original intent/fences/deadline immediately before application bytes.
	if err = authorize(); err != nil {
		return empty, err
	}
	d.deliveryAttempted = true
	if err = session.SendTaskClose(bounded, envelope); err != nil {
		return empty, errors.Join(ErrDeliveryUnknown, err)
	}
	return b.readTaskCloseReceipt(bounded, session, destination, d.claims.Context, d.ticket.Digest(), d.claims.NotBefore, d.claims.NotAfter)
}

// QueryTaskCloseData is a history-only request. Neither a fresh nor a historical
// claim is reconstructed, and it cannot write or resend the close command.
func (b *Backend) QueryTaskCloseData(ctx context.Context, ref TaskCloseDataReference, destination *t.Destination) (TaskCloseDataDeliveryResult, error) {
	empty := TaskCloseDataDeliveryResult{}
	if b == nil || ctx == nil || ref.Validate() != nil || b.taskVerifier == nil {
		return empty, ErrInvalidRecord
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	entry, err := b.LoadTaskCloseData(bounded, ref.Task)
	if err != nil {
		return empty, err
	}
	if entry == nil || entry.Record == nil || entry.Reference != ref || entry.Outcome != OutcomeCommitted {
		return empty, ErrConflict
	}
	r := entry.Record
	issuer, err := b.LoadExecIssuer(bounded, r.IssuerCertificateID)
	if err != nil {
		return empty, err
	}
	if issuer == nil || issuer.Revision != r.IssuerRevision || issuer.Record.CertificateDigest != r.IssuerCertificateDigest || !destination.MatchTaskClose(r.Context, issuer.Record.Certificate) {
		return empty, ErrIdentityMismatch
	}
	// The stored strict record is immutable history; extracting its original
	// interval does not authenticate a fresh ticket or grant effect permission.
	var ticket struct {
		Claims    p.TaskCloseDataTicketClaims `json:"claims"`
		Signature []byte                      `json:"signature"`
	}
	if json.Unmarshal(r.Ticket, &ticket) != nil {
		return empty, ErrCorruptRecord
	}
	verify := func() error {
		now, err := b.observePublicationClock(bounded)
		if err != nil {
			return err
		}
		return destination.VerifyTaskCloseRuntime(b.taskVerifier, r.Context, ticket.Claims.NotBefore, ticket.Claims.NotAfter, now)
	}
	if err = verify(); err != nil {
		return empty, err
	}
	session, err := destination.Open(bounded)
	if err != nil {
		return empty, err
	}
	defer session.Close()
	if err = verify(); err != nil {
		return empty, err
	}
	if err = session.SendTaskClose(bounded, t.TaskCloseEnvelope{Version: 1, Purpose: "task_close_data_query", Context: r.Context, TicketDigest: r.TicketDigest}); err != nil {
		return empty, errors.Join(ErrDeliveryUnknown, err)
	}
	return b.readTaskCloseReceipt(bounded, session, destination, r.Context, r.TicketDigest, ticket.Claims.NotBefore, ticket.Claims.NotAfter)
}
func (b *Backend) readTaskCloseReceipt(ctx context.Context, s *t.Session, d *t.Destination, c p.TaskCloseDataContext, digest string, before, after time.Time) (TaskCloseDataDeliveryResult, error) {
	empty := TaskCloseDataDeliveryResult{}
	kind, wire, err := s.Read(ctx)
	if err != nil {
		return empty, errors.Join(ErrDeliveryUnknown, err)
	}
	if kind != t.EventReceipt {
		return empty, ErrDeliveryUnknown
	}
	now, err := b.observePublicationClock(ctx)
	if err != nil {
		return empty, errors.Join(ErrDeliveryUnknown, err)
	}
	proof, err := d.VerifyTaskCloseReceipt(b.taskVerifier, wire, c, digest, before, after, now)
	if err != nil {
		return empty, errors.Join(ErrDeliveryUnknown, err)
	}
	if err = ctx.Err(); err != nil {
		return empty, errors.Join(ErrDeliveryUnknown, err)
	}
	return TaskCloseDataDeliveryResult{Receipt: &proof}, nil
}
