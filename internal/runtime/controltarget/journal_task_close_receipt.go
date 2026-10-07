package controltarget

import (
	"bytes"
	"context"
	"crypto/ed25519"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

// SignDataCloseReceipt signs only the original durable close on a clean live
// installation. The journal mutex covers lookup, authentication, and signing,
// including concurrent execution-terminal persistence or journal shutdown.
// This fixed-purpose method neither stores the key nor accepts caller records.
// A receipt certifies admission closure only, never execution drain or release.
func (j *Journal) SignDataCloseReceipt(ctx context.Context, expected p.TaskCloseDataContext, digest string, key ed25519.PrivateKey) ([]byte, error) {
	if j == nil || j.self != j {
		return nil, ErrJournalUnavailable
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.checkLocked(ctx, true); err != nil {
		return nil, err
	}
	if !j.accountingKnown || j.gate.GateState != "closed" {
		return nil, ErrJournalUnavailable
	}
	if err := j.taskCloseInstalledLocked(); err != nil {
		return nil, err
	}
	if err := validateJournalTaskCloseContext(expected); err != nil {
		return nil, err
	}
	if !journalHash(digest) || len(key) != ed25519.PrivateKeySize {
		return nil, ErrInvalidRecord
	}
	if !bytes.Equal(key.Public().(ed25519.PublicKey), j.birth.RuntimePublicKey) {
		return nil, ErrIdentityMismatch
	}
	if err := j.taskCloseIssuerLocked(expected); err != nil {
		return nil, err
	}
	r, err := j.readTaskCloseLocked(ctx)
	if err != nil {
		return nil, err
	}
	if r == nil || r.State != "data_closed" || r.Context != expected || r.TicketDigest != digest {
		return nil, ErrConflict
	}
	if err = j.authenticateTaskCloseReceiptLocked(ctx, *r); err != nil {
		return nil, err
	}
	wire, err := p.SignTaskDataClosedReceipt(key, p.TaskDataClosedReceiptClaims{Version: 1, State: "data_closed", Context: r.Context, TicketDigest: r.TicketDigest, NotBefore: r.NotBefore, NotAfter: r.NotAfter})
	if err != nil {
		return nil, err
	}
	// A late clock/cancellation never returns the already-computed signature.
	if err = j.authenticateTaskCloseReceiptLocked(ctx, *r); err != nil {
		return nil, err
	}
	return wire, nil
}
func (j *Journal) authenticateTaskCloseReceiptLocked(ctx context.Context, r TaskDataCloseRecord) error {
	now, err := j.authorityNow(ctx)
	if err != nil {
		return err
	}
	cert, err := j.verifier.VerifyRuntimeIdentityCertificate(j.activation.RuntimeCertificate(), j.activation.Identity(), now)
	if err != nil {
		return err
	}
	if !bytes.Equal(cert.PublicKey(), j.birth.RuntimePublicKey) || r.NotBefore.Before(cert.NotBefore()) || r.NotAfter.After(cert.NotAfter()) {
		return ErrIdentityMismatch
	}
	return ctx.Err()
}
