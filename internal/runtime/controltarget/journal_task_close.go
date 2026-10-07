package controltarget

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

// CloseData durably attributes one same-epoch admission closure on the original
// live handle. Accepted executions remain untouched for the later drain owner.
// Every uncertain operation closes local admission and returns no close proof.
func (j *Journal) CloseData(ctx context.Context, e controlprotocol.TaskCloseDataEvidence) (*TaskDataCloseRecord, error) {
	if j == nil || j.self != j {
		return nil, ErrJournalUnavailable
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.checkLocked(ctx, true); err != nil {
		return nil, err
	}
	r := TaskDataCloseRecord{Version: 1, State: "pending", Context: e.Context(), TicketDigest: e.Digest(), NotBefore: e.NotBefore(), NotAfter: e.NotAfter()}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if err := j.taskCloseBinding(r.Context); err != nil {
		return nil, err
	}
	if err := j.taskCloseInstalledLocked(); err != nil {
		return nil, err
	}
	if err := j.taskCloseIssuerLocked(r.Context); err != nil {
		return nil, err
	}
	previous, err := j.readTaskCloseLocked(ctx)
	if err != nil {
		return nil, j.poison(err)
	}
	if (previous == nil) != (j.taskCloseBytes == 0) {
		return nil, j.poison(ErrConflict)
	}
	if previous != nil && !sameTaskClose(*previous, r) {
		return nil, ErrConflict
	}
	if err = j.authenticateTaskCloseLocked(ctx, e); err != nil {
		return nil, j.poison(err)
	}
	if previous != nil && previous.State == "data_closed" {
		if j.gate.GateState != "closed" {
			return nil, j.poison(ErrConflict)
		}
		return previous, nil
	}
	pending, err := encodeTaskDataClose(r)
	if err != nil {
		return nil, err
	}
	closed := r
	closed.State = "data_closed"
	terminal, err := encodeTaskDataClose(closed)
	if err != nil {
		return nil, err
	}
	gate := j.gate
	gate.GateState = "closed"
	gateWire, err := encodeGateManifest(gate)
	if err != nil {
		return nil, err
	}
	// Pending stays present during both replacements. Reserve the maximum of
	// actual pending+gate-temp and pending+terminal-temp bytes, and both slots.
	peak := j.logicalBytes
	extra := uint64(1)
	if previous == nil {
		peak += int64(len(pending))
		extra++
	}
	peakGate := peak + int64(len(gateWire))
	peakTerminal := peak + int64(len(gateWire)) - j.manifestBytes + int64(len(terminal))
	if peakTerminal > peakGate {
		peakGate = peakTerminal
	}
	if !j.accountingKnown {
		return nil, ErrJournalUnavailable
	}
	if peakGate > j.maxBytes || peakGate*100 >= j.maxBytes*85 || j.contentFilesLocked()+extra > maxJournalContentFiles {
		return nil, ErrCapacity
	}
	j.gate.GateState = "closed"
	if previous == nil {
		if err = j.persistTaskCloseLocked(ctx, pending); err != nil {
			return nil, j.poison(err)
		}
	}
	if err = j.persistClosedGateLocked(ctx); err != nil {
		return nil, j.poison(err)
	}
	if err = j.authenticateTaskCloseLocked(ctx, e); err != nil {
		return nil, j.poison(err)
	}
	if err = j.persistTaskCloseLocked(ctx, terminal); err != nil {
		return nil, j.poison(err)
	}
	if err = j.authenticateTaskCloseLocked(ctx, e); err != nil {
		return nil, j.poison(err)
	}
	return &closed, nil
}

// The original installed activation was authenticated at installation. Its
// expiry is not a cleanup cutoff. Keep its independent birth and tuple pinned,
// and freshly authenticate the ORIGINAL certificates rather than renewing or
// reconstructing activation authority from historical disk bytes.
func (j *Journal) taskCloseInstalledLocked() error {
	if !j.fresh || j.birth == nil || j.verifier == nil || j.activation == nil {
		return ErrJournalUnavailable
	}
	a := j.activation
	b := a.Birth()
	i := a.Identity()
	binding := a.Binding()
	want := j.gate.Identity
	if b.BootID != j.birth.BootID || b.UID != j.birth.UID || b.GID != j.birth.GID || b.NetworkAllowed != j.birth.NetworkAllowed || b.ContractDigest != j.birth.ContractDigest || !bytes.Equal(b.RuntimePublicKey, j.birth.RuntimePublicKey) || i.SandboxID != want.SandboxID || i.WorkspaceHash != want.WorkspaceHash || i.Generation != want.Generation || i.Runtime != want.Runtime || binding.Namespace != want.Namespace || binding.AuthorityID != want.AuthorityID || binding.Target != want.Target || binding.RestoreEpoch != want.RestoreEpoch || a.DataGateEpoch() != j.gate.DataGateEpoch {
		return ErrIdentityMismatch
	}
	return nil
}
func (j *Journal) taskCloseIssuerLocked(c controlprotocol.TaskCloseDataContext) error {
	// Digest is over the normalized original certificate copied into activation.
	if digestJournalBytes(j.activation.IssuerCertificate()) != c.IssuerCertificateDigest {
		return ErrIdentityMismatch
	}
	return nil
}
func (j *Journal) authenticateTaskCloseLocked(ctx context.Context, e controlprotocol.TaskCloseDataEvidence) error {
	if err := j.taskCloseInstalledLocked(); err != nil {
		return err
	}
	now, err := j.authorityNow(ctx)
	if err != nil {
		return err
	}
	a := j.activation
	verified, err := j.verifier.VerifyTaskCloseDataTicket(e.Wire(), a.IssuerCertificate(), e.Context(), now)
	if err != nil {
		return err
	}
	if verified.Digest() != e.Digest() || !verified.NotBefore().Equal(e.NotBefore()) || !verified.NotAfter().Equal(e.NotAfter()) {
		return ErrInvalidRecord
	}
	identity := a.Identity()
	runtime, err := j.verifier.VerifyRuntimeIdentityCertificate(a.RuntimeCertificate(), identity, now)
	if err != nil {
		return err
	}
	if !bytes.Equal(runtime.PublicKey(), j.birth.RuntimePublicKey) || e.NotBefore().Before(runtime.NotBefore()) || e.NotAfter().After(runtime.NotAfter()) {
		return ErrIdentityMismatch
	}
	return ctx.Err()
}

// LookupDataClose is bounded structural diagnostics, including on poisoned or
// cold handles and after ticket expiry. Pending is not proof; even data_closed
// can follow a rename whose directory fsync failed. A transport must never sign
// this snapshot as durable closure from a poisoned/accounting-unknown/cold
// handle. Absence is only absence, not proof that a command was never sent.
func (j *Journal) LookupDataClose(ctx context.Context, expected controlprotocol.TaskCloseDataContext, ticketDigest string) (*TaskDataCloseRecord, error) {
	if j == nil || j.self != j {
		return nil, ErrJournalUnavailable
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.checkLocked(ctx, false); err != nil {
		return nil, err
	}
	if err := validateJournalTaskCloseContext(expected); err != nil {
		return nil, err
	}
	if !journalHash(ticketDigest) {
		return nil, ErrInvalidRecord
	}
	if err := j.taskCloseBinding(expected); err != nil {
		return nil, err
	}
	r, err := j.readTaskCloseLocked(ctx)
	if err != nil || r == nil {
		return r, err
	}
	if r.Context != expected || r.TicketDigest != ticketDigest {
		return nil, ErrConflict
	}
	return r, nil
}

func digestJournalBytes(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
