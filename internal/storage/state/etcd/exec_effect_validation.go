package etcd

import (
	"context"
	"fmt"
	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"time"
)

type execEffectDraft struct {
	descriptor             controlprotocol.ExecutionDescriptor
	deadline               time.Time
	commandID              string
	issuer                 *CommandIssuerEntry
	issuerKey, issuerValue string
	claims                 controlprotocol.ExecStartTicketClaims
	ticket                 controlprotocol.ExecStartEvidence
	reference              ExecEffectReference
	recordValue            string
	stage                  *Stage
	outcome                Outcome
}

// execEffectContext links a call to the original admission parent and the
// draft's monotonic deadline. Its AfterFunc is removed on every return path.
func (b *Backend) execEffectContext(ctx context.Context, c *OperationCapability, d *execEffectDraft) (context.Context, context.CancelFunc) {
	bounded, cancel := context.WithDeadline(ctx, d.deadline)
	stop := context.AfterFunc(c.parentCtx, cancel)
	request, finish := b.requestContext(bounded)
	return request, func() { stop(); finish(); cancel() }
}
func execEffectLive(ctx context.Context, c *OperationCapability, d *execEffectDraft) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.admissionLive(); err != nil {
		return err
	}
	if !time.Now().Before(d.deadline) {
		return ErrGuardExpired
	}
	return nil
}

func (b *Backend) signExecEffect(ctx context.Context, c *OperationCapability, d *execEffectDraft) error {
	bounded, cancel := b.execEffectContext(ctx, c, d)
	defer cancel()
	if err := execEffectLive(bounded, c, d); err != nil {
		return err
	}
	if d.issuer == nil {
		entry, err := b.RegisterExecIssuer(bounded)
		if err != nil {
			return err
		}
		// Preserve the first registered identity even if the next observation fails.
		d.issuer = entry
		d.issuerKey, err = b.namespace.commandIssuerKey(entry.Record.CertificateID)
		if err != nil {
			return err
		}
		d.issuerValue, err = encodeCommandIssuerRecord(entry.Record)
		if err != nil {
			return err
		}
	}
	if err := execEffectLive(bounded, c, d); err != nil {
		return err
	}
	now, err := b.observePublicationClock(bounded)
	anchor := time.Now() // Must follow the completed trusted UTC observation.
	if err != nil {
		return err
	}
	if err := execEffectLive(bounded, c, d); err != nil {
		return err
	}
	issuer, err := b.execVerifier.VerifyCommandIssuerCertificate(d.issuer.Record.Certificate, now)
	if err != nil {
		return fmt.Errorf("%w: exec issuer: %v", ErrInvalidRecord, err)
	}
	if issuer.CertificateID() != d.issuer.Record.CertificateID || issuer.Digest() != d.issuer.Record.CertificateDigest {
		return ErrInvalidRecord
	}
	if d.claims.Version == 0 {
		before := now.Add(-2 * time.Second)
		if issuer.NotBefore().After(before) {
			before = issuer.NotBefore()
		}
		after := now.Add(-time.Second).Add(d.deadline.Sub(anchor))
		for _, limit := range []time.Time{before.Add(30 * time.Second), c.record.ExpiresAt, issuer.NotAfter()} {
			if limit.Before(after) {
				after = limit
			}
		}
		if now.Add(-time.Second).Before(before) || !now.Add(time.Second).Before(after) {
			return ErrGuardExpired
		}
		r := c.record
		ref := r.Reference
		expected := controlprotocol.ExecStartContext{Namespace: b.namespace.Root(), AuthorityID: b.publicationAuthorityID, Target: b.publicationTarget, RestoreEpoch: b.restoreEpoch, IssuerCertificateID: issuer.CertificateID(), IssuerCertificateDigest: issuer.Digest(), CommandID: d.commandID, OperationID: ref.OperationID, RequestID: ref.RequestID, OperationDigest: ref.Digest, SandboxID: ref.SandboxID, WorkspaceHash: r.WorkspaceHash, Generation: r.Generation, DataGateEpoch: r.DataGateEpoch, ControlRevision: r.ControlRevision, AdmissionRevision: c.admitRevision, LeaseID: ref.LeaseID, Runtime: controlprotocol.RuntimeReference(r.Runtime), ExpiresAt: r.ExpiresAt}
		d.claims = controlprotocol.ExecStartTicketClaims{Version: 1, Purpose: "operation_exec_start", Context: expected, DescriptorDigest: d.descriptor.Digest(), NotBefore: before, NotAfter: after}
	}
	if d.ticket.Digest() != "" {
		return b.verifyExecEffectTicket(bounded, c, d)
	}
	wire, err := b.execIssuer.SignStart(bounded, d.issuer.Record.CertificateDigest, d.claims)
	wire = append([]byte(nil), wire...)
	if err != nil {
		return fmt.Errorf("exec start signer: %w", err)
	}
	if err := execEffectLive(bounded, c, d); err != nil {
		return err
	}
	now, err = b.observePublicationClock(bounded)
	if err != nil {
		return err
	}
	if err := execEffectLive(bounded, c, d); err != nil {
		return err
	}
	evidence, err := b.execVerifier.VerifyExecStartTicket(wire, d.issuer.Record.Certificate, d.claims.Context, d.descriptor, now)
	if err != nil {
		return fmt.Errorf("%w: exec start ticket: %v", ErrInvalidRecord, err)
	}
	if !evidence.NotBefore().Equal(d.claims.NotBefore) || !evidence.NotAfter().Equal(d.claims.NotAfter) {
		return ErrInvalidRecord
	}
	d.ticket = evidence
	return nil
}
func (b *Backend) verifyExecEffectTicket(ctx context.Context, c *OperationCapability, d *execEffectDraft) error {
	now, err := b.observePublicationClock(ctx)
	if err != nil {
		return err
	}
	if err := execEffectLive(ctx, c, d); err != nil {
		return err
	}
	_, err = b.execVerifier.VerifyExecStartTicket(d.ticket.Wire(), d.issuer.Record.Certificate, d.claims.Context, d.descriptor, now)
	if err != nil {
		return fmt.Errorf("%w: exec start ticket: %v", ErrInvalidRecord, err)
	}
	return nil
}
