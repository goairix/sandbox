package etcd

import (
	"context"
	"encoding/json"
	"errors"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	t "github.com/goairix/sandbox/internal/runtime/controltransport"
	"time"
)

// Accepted proves durable owned work only. Receipt is separately authenticated
// terminal local evidence. Every uncertain delivery retains the same reference.
type TaskUserQuiescenceDeliveryResult struct {
	Reference TaskQuiescenceReference
	Accepted  *p.TaskUserQuiescenceAcceptedEvidence
	Receipt   *p.TaskUserQuiescenceReceiptEvidence
}

func (b *Backend) DeliverTaskQuiescence(ctx context.Context, prepared *PreparedTaskUserQuiescence, destination *t.Destination) (TaskUserQuiescenceDeliveryResult, error) {
	result := TaskUserQuiescenceDeliveryResult{}
	if b == nil || ctx == nil || prepared == nil || prepared.self != prepared || prepared.origin != b || !b.validTaskClaim(prepared.claim) || prepared.draft == nil {
		return result, ErrInvalidRecord
	}
	c, d := prepared.claim, prepared.draft
	c.mu.Lock()
	defer c.mu.Unlock()
	result.Reference = d.reference
	if c.quiescenceDraft != d || d.outcome != OutcomeCommitted || d.issuer == nil || d.prior == nil || !destination.MatchTaskQuiesce(d.claims.Context, d.certificate) {
		return result, ErrInvalidRecord
	}
	if d.deliveryAttempted {
		return result, ErrDeliveryUnknown
	}
	request, finish := b.taskQuiescenceContext(ctx, c, d)
	defer finish()
	bounded, cancel := context.WithTimeout(request, 5*time.Second)
	defer cancel()
	authorize := func() error {
		if err := b.authorizePreparedTaskQuiescence(bounded, c, d); err != nil {
			return err
		}
		return b.verifyTaskQuiescenceRuntime(bounded, c, d, destination)
	}
	if err := authorize(); err != nil {
		return result, err
	}
	session, err := destination.Open(bounded)
	if err != nil {
		return result, err
	}
	defer session.Close()
	if err = authorize(); err != nil {
		return result, err
	}
	envelope := t.TaskQuiesceEnvelope{Version: 1, Purpose: "task_quiesce_users", Context: d.claims.Context, TicketDigest: d.ticket.Digest(), Ticket: d.ticket.Wire(), IssuerCertificate: append([]byte(nil), d.certificate...), OriginalCloseDataReceipt: append([]byte(nil), d.prior.proof...)}
	if err = authorize(); err != nil {
		return result, err
	}
	// Consume original send authority before any application byte can escape.
	d.deliveryAttempted = true
	if err = session.SendTaskQuiesce(bounded, envelope); err != nil {
		return result, errors.Join(ErrDeliveryUnknown, err)
	}
	return b.readTaskQuiescenceResult(bounded, session, destination, d.reference, d.claims.Context, d.ticket.Digest(), d.claims.NotBefore, d.claims.NotAfter)
}
func (b *Backend) QueryTaskQuiescence(ctx context.Context, task TaskReference, destination *t.Destination) (TaskUserQuiescenceDeliveryResult, error) {
	result := TaskUserQuiescenceDeliveryResult{}
	if b == nil || ctx == nil || b.taskQuiescenceVerifier == nil || b.authorityClock == nil || b.validateTaskReference(task) != nil {
		return result, ErrInvalidRecord
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	entry, err := b.LoadTaskQuiescence(bounded, task)
	if err != nil {
		return result, err
	}
	if entry == nil || entry.Record == nil || entry.Outcome != OutcomeCommitted {
		return result, ErrConflict
	}
	result.Reference = entry.Reference
	r := entry.Record
	issuer, err := b.LoadExecIssuer(bounded, r.IssuerCertificateID)
	if err != nil {
		return result, err
	}
	if issuer == nil || issuer.Revision != r.IssuerRevision || issuer.Record.CertificateDigest != r.IssuerCertificateDigest || !destination.MatchTaskQuiesce(r.Context, issuer.Record.Certificate) {
		return result, ErrIdentityMismatch
	}
	var ticket struct {
		Claims    p.TaskUserQuiescenceTicketClaims `json:"claims"`
		Signature []byte                           `json:"signature"`
	}
	if json.Unmarshal(r.Ticket, &ticket) != nil {
		return result, ErrCorruptRecord
	}
	verify := func() error {
		now, err := b.observePublicationClock(bounded)
		if err != nil {
			return err
		}
		if err = bounded.Err(); err != nil {
			return err
		}
		return destination.VerifyTaskQuiesceRuntime(b.taskQuiescenceVerifier, r.Context, ticket.Claims.NotBefore, ticket.Claims.NotAfter, now)
	}
	if err = verify(); err != nil {
		return result, err
	}
	session, err := destination.Open(bounded)
	if err != nil {
		return result, err
	}
	defer session.Close()
	if err = verify(); err != nil {
		return result, err
	}
	if err = session.SendTaskQuiesce(bounded, t.TaskQuiesceEnvelope{Version: 1, Purpose: "task_quiesce_users_query", Context: r.Context, TicketDigest: r.TicketDigest}); err != nil {
		return result, errors.Join(ErrDeliveryUnknown, err)
	}
	return b.readTaskQuiescenceResult(bounded, session, destination, entry.Reference, r.Context, r.TicketDigest, ticket.Claims.NotBefore, ticket.Claims.NotAfter)
}
func (b *Backend) readTaskQuiescenceResult(ctx context.Context, s *t.Session, d *t.Destination, ref TaskQuiescenceReference, c p.TaskUserQuiescenceContext, digest string, before, after time.Time) (TaskUserQuiescenceDeliveryResult, error) {
	result := TaskUserQuiescenceDeliveryResult{Reference: ref}
	kind, wire, err := s.Read(ctx)
	if err != nil {
		return result, errors.Join(ErrDeliveryUnknown, err)
	}
	if kind != t.EventAccepted && kind != t.EventReceipt {
		return result, ErrDeliveryUnknown
	}
	now, err := b.observePublicationClock(ctx)
	if err != nil {
		return result, errors.Join(ErrDeliveryUnknown, err)
	}
	if kind == t.EventAccepted {
		proof, err := d.VerifyTaskQuiesceAccepted(b.taskQuiescenceVerifier, wire, c, digest, before, after, now)
		if err != nil {
			return result, errors.Join(ErrDeliveryUnknown, err)
		}
		if err = ctx.Err(); err != nil {
			return result, errors.Join(ErrDeliveryUnknown, err)
		}
		result.Accepted = &proof
	} else {
		proof, err := d.VerifyTaskQuiesceReceipt(b.taskQuiescenceVerifier, wire, c, digest, before, after, now)
		if err != nil {
			return result, errors.Join(ErrDeliveryUnknown, err)
		}
		if err = ctx.Err(); err != nil {
			return result, errors.Join(ErrDeliveryUnknown, err)
		}
		result.Receipt = &proof
	}
	return result, nil
}
