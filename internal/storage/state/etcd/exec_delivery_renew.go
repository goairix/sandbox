package etcd

import (
	"context"
	"errors"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	t "github.com/goairix/sandbox/internal/runtime/controltransport"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// ExecRenewIssuer is optional: existing start-only providers remain compatible.
// It signs only the fixed renewal purpose for the exact selected certificate.
type ExecRenewIssuer interface {
	SignRenew(context.Context, string, p.ExecRenewTicketClaims) ([]byte, error)
}

func (b *Backend) RenewExecDelivery(ctx context.Context, h *ExecDeliveryHandle) error {
	if b == nil || ctx == nil || !h.valid() || h.origin != b {
		return ErrInvalidRecord
	}
	h.mu.Lock()
	if !h.accepted || h.terminal || h.unknown || h.closed || !h.pending.IsZero() || h.renewing {
		h.mu.Unlock()
		return ErrConflict
	}
	h.renewing = true
	h.mu.Unlock()
	defer func() { h.mu.Lock(); h.renewing = false; h.mu.Unlock() }()
	issuer, ok := b.execIssuer.(ExecRenewIssuer)
	if !ok {
		return ErrInvalidConfiguration
	}
	c, d := h.capability, h.draft
	// This is the existing original-Lease renewal; no reconstructed capability or
	// replacement Lease is introduced and its irreversible-loss semantics remain.
	if err := b.RenewOperation(ctx, c); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	bounded, cancel := b.requestContext(ctx)
	defer cancel()
	stop := context.AfterFunc(c.parentCtx, cancel)
	defer stop()
	if err := b.authorizeExecRenewal(bounded, c, d); err != nil {
		return err
	}
	if err := b.verifyExecDestination(bounded, h.destination, d); err != nil {
		return err
	}
	s, err := h.destination.Open(bounded)
	if err != nil {
		return err
	}
	defer s.Close()
	if err = b.authorizeExecRenewal(bounded, c, d); err != nil {
		return err
	}
	if err := b.verifyExecDestination(bounded, h.destination, d); err != nil {
		return err
	}
	now, err := b.observePublicationClock(bounded)
	anchor := time.Now()
	if err != nil {
		return err
	}
	identity, err := b.execVerifier.VerifyCommandIssuerCertificate(d.issuer.Record.Certificate, now)
	if err != nil {
		return err
	}
	before := now.Add(-2 * time.Second)
	if identity.NotBefore().After(before) {
		before = identity.NotBefore()
	}
	after := now.Add(-time.Second).Add(c.deadline.Sub(anchor))
	for _, limit := range []time.Time{before.Add(30 * time.Second), c.record.ExpiresAt, identity.NotAfter()} {
		if limit.Before(after) {
			after = limit
		}
	}
	if !now.Add(time.Second).Before(after) || now.Add(-time.Second).Before(before) {
		return ErrGuardExpired
	}
	claims := p.ExecRenewTicketClaims{Version: 1, Purpose: "operation_exec_renew", Context: d.claims.Context, DescriptorDigest: d.descriptor.Digest(), NotBefore: before, NotAfter: after}
	wire, err := issuer.SignRenew(bounded, d.issuer.Record.CertificateDigest, claims)
	if err != nil {
		return err
	}
	wire = append([]byte(nil), wire...)
	if err = b.authorizeExecRenewal(bounded, c, d); err != nil {
		return err
	}
	if err := b.verifyExecDestination(bounded, h.destination, d); err != nil {
		return err
	}
	now, err = b.observePublicationClock(bounded)
	if err != nil {
		return err
	}
	evidence, err := b.execVerifier.VerifyExecRenewTicket(wire, d.issuer.Record.Certificate, d.claims.Context, d.descriptor.Digest(), now)
	if err != nil {
		return err
	}
	if !evidence.NotBefore().Equal(before) || !evidence.NotAfter().Equal(after) {
		return ErrInvalidRecord
	}
	if err := c.admissionLive(); err != nil {
		return err
	}
	if err := bounded.Err(); err != nil {
		return err
	}
	h.mu.Lock()
	if !h.accepted || h.terminal || h.unknown || h.closed || !h.pending.IsZero() || !after.After(h.deadline) {
		h.mu.Unlock()
		return ErrConflict
	}
	h.pending = after
	h.mu.Unlock()
	if err = s.Send(bounded, t.Envelope{Version: 1, Purpose: "exec_renew", Context: d.claims.Context, DescriptorDigest: d.descriptor.Digest(), Ticket: wire, IssuerCertificate: d.issuer.Record.Certificate}); err != nil {
		return errors.Join(ErrDeliveryUnknown, err)
	}
	kind, result, err := s.Read(bounded)
	if err != nil {
		return errors.Join(ErrDeliveryUnknown, err)
	}
	receipt, err := h.observeReceipt(bounded, kind, result)
	if err != nil {
		return err
	}
	if kind != t.EventAccepted || receipt.State() != "accepted" || !receipt.AuthorityDeadline().Equal(after) {
		return ErrDeliveryUnknown
	}
	return nil
}

// Renewal uses current original capability fences and committed immutable
// effect identity; the historical Start interval never becomes a new Start.
func (b *Backend) authorizeExecRenewal(ctx context.Context, c *OperationCapability, d *execEffectDraft) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.admissionLive(); err != nil {
		return err
	}
	entry, err := b.LoadExecEffect(ctx, d.reference)
	if err != nil {
		return err
	}
	if entry == nil || entry.Record == nil || entry.Outcome != OutcomeCommitted {
		return ErrCorruptRecord
	}
	value, err := encodeExecEffectRecord(*entry.Record)
	if err != nil || value != d.recordValue {
		return ErrCorruptRecord
	}
	checks, err := b.execEffectComparisons(c, d)
	if err != nil {
		return err
	}
	checks = append(b.baseComparisons(), checks...)
	response, err := b.client.Txn(ctx).If(checks...).Then(clientv3.OpGet(b.identityKey)).Else(clientv3.OpGet(b.identityKey)).Commit()
	if err != nil {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	if err = b.validateFenceCheckResponse(response); err != nil {
		return err
	}
	if !response.Succeeded {
		return ErrConflict
	}
	now, err := b.observePublicationClock(ctx)
	if err != nil {
		return err
	}
	identity, err := b.execVerifier.VerifyCommandIssuerCertificate(d.issuer.Record.Certificate, now)
	if err != nil {
		return err
	}
	if identity.Digest() != d.issuer.Record.CertificateDigest || identity.CertificateID() != d.issuer.Record.CertificateID {
		return ErrInvalidRecord
	}
	if !now.Add(time.Second).Before(c.record.ExpiresAt) {
		return ErrGuardExpired
	}
	return c.admissionLive()
}
