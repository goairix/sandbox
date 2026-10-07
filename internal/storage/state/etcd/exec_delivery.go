package etcd

import (
	"context"
	"errors"
	"time"

	protocol "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	t "github.com/goairix/sandbox/internal/runtime/controltransport"
)

// DeliverExecEffect carries only the original private draft, after fresh original
// fences on both sides of the TLS handshake. The capability mutex ends at ACK.
func (b *Backend) DeliverExecEffect(ctx context.Context, p *PreparedExecEffect, destination *t.Destination) (*ExecDeliveryHandle, error) {
	if b == nil || ctx == nil || p == nil || p.self != p || p.origin != b || p.capability == nil || p.capability.origin != b || p.capability.parentCtx == nil || p.draft == nil {
		return nil, ErrInvalidRecord
	}
	c, d := p.capability, p.draft
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.execDraft != d || d.outcome != OutcomeCommitted || d.issuer == nil || !destination.Match(d.claims.Context, d.issuer.Record.Certificate) {
		return nil, ErrInvalidRecord
	}
	bounded, cancel := b.execEffectContext(ctx, c, d)
	defer cancel()
	if err := b.authorizePreparedExecEffect(bounded, c, d); err != nil {
		return nil, err
	}
	if err := b.verifyExecDestination(bounded, destination, d); err != nil {
		return nil, err
	}
	s, err := destination.Open(bounded)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			s.Close()
		}
	}()
	if err = b.authorizePreparedExecEffect(bounded, c, d); err != nil {
		return nil, err
	}
	if err := b.verifyExecDestination(bounded, destination, d); err != nil {
		return nil, err
	}
	if err := b.verifyExecEffectTicket(bounded, c, d); err != nil {
		return nil, err
	}
	h := d.delivery
	if h != nil {
		if !h.valid() || h.destination != destination {
			return nil, ErrConflict
		}
		return h, ErrConflict
	}
	h = &ExecDeliveryHandle{origin: b, capability: c, draft: d, destination: destination, deadline: d.claims.NotAfter}
	h.self = h
	d.delivery = h
	envelope := t.StartEnvelope(d.claims.Context, d.descriptor, d.ticket.Wire(), d.issuer.Record.Certificate)
	if err = s.Send(bounded, envelope); err != nil {
		return h, errors.Join(ErrDeliveryUnknown, err)
	}
	kind, wire, err := s.Read(bounded)
	ackAt := time.Now()
	if err != nil {
		return h, errors.Join(ErrDeliveryUnknown, err)
	}
	receipt, err := h.observeReceipt(bounded, kind, wire)
	if err != nil {
		return h, err
	}
	if kind == t.EventAccepted && receipt.State() == "accepted" {
		h.startReader(s, ackAt)
		keep = true
	}
	return h, nil
}

func (b *Backend) verifyExecDestination(ctx context.Context, destination *t.Destination, d *execEffectDraft) error {
	now, err := b.observePublicationClock(ctx)
	if err != nil {
		return err
	}
	c := d.claims.Context
	return destination.VerifyRuntime(b.execVerifier, protocol.RuntimeIdentityContext{SandboxID: c.SandboxID, WorkspaceHash: c.WorkspaceHash, Generation: c.Generation, Runtime: c.Runtime}, now)
}
