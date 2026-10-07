package controltarget

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

// This schema checks only consistency of already authenticated opaque input.
// It neither verifies current authority nor turns historical evidence into a
// launch capability. The input ticket is never persisted.
var journalEvidenceSchema = journalObject(map[string]*journalSchema{
	"claims": journalObject(map[string]*journalSchema{
		"version": journalNumberField, "purpose": journalStringField,
		"context": journalContextSchema, "descriptor_digest": journalStringField,
		"not_before": journalTimeField, "not_after": journalTimeField,
	}),
	"signature": journalStringField,
})

func recordFromEvidence(e controlprotocol.ExecStartEvidence) (ExecJournalRecord, error) {
	r := ExecJournalRecord{Version: 1, State: "unknown", Context: e.Context(), DescriptorDigest: e.DescriptorDigest(), TicketDigest: e.Digest(), NotBefore: e.NotBefore(), NotAfter: e.NotAfter()}
	if err := r.Validate(); err != nil {
		return ExecJournalRecord{}, err
	}
	wire := e.Wire()
	if len(wire) == 0 || len(wire) > 4096 {
		return ExecJournalRecord{}, fmt.Errorf("%w: missing or oversized execution evidence", ErrInvalidRecord)
	}
	digest := sha256.Sum256(wire)
	if hex.EncodeToString(digest[:]) != r.TicketDigest {
		return ExecJournalRecord{}, fmt.Errorf("%w: execution evidence digest", ErrInvalidRecord)
	}
	var envelope struct {
		Claims    controlprotocol.ExecStartTicketClaims `json:"claims"`
		Signature []byte                                `json:"signature"`
	}
	if err := decodeJournalWire(wire, journalEvidenceSchema, &envelope); err != nil {
		return ExecJournalRecord{}, err
	}
	c := envelope.Claims
	if c.Version != 1 || c.Purpose != "operation_exec_start" || c.Context != r.Context || c.DescriptorDigest != r.DescriptorDigest || c.NotBefore != r.NotBefore || c.NotAfter != r.NotAfter || len(envelope.Signature) != ed25519.SignatureSize {
		return ExecJournalRecord{}, fmt.Errorf("%w: inconsistent execution evidence", ErrInvalidRecord)
	}
	// controlprotocol canonical wire uses json.Marshal's HTML escaping;
	// persisted journal records use their own exact opaque-character codec.
	canonical, err := json.Marshal(envelope)
	if err != nil {
		return ExecJournalRecord{}, fmt.Errorf("%w: encode evidence: %w", ErrInvalidRecord, err)
	}
	if !bytes.Equal(wire, canonical) {
		return ExecJournalRecord{}, fmt.Errorf("%w: noncanonical execution evidence", ErrInvalidRecord)
	}
	return r, nil
}

// RecordUnknown durably records passive diagnostic evidence, including history
// whose authentication window has passed. It does not authorize execution.
// A failed write can leave a complete intent; query it rather than inferring
// that the operation failed. Only a cold reopen restores known accounting.
func (j *Journal) RecordUnknown(ctx context.Context, evidence controlprotocol.ExecStartEvidence) (*ExecJournalRecord, error) {
	if ctx == nil {
		return nil, ErrInvalidConfiguration
	}
	if j == nil {
		return nil, ErrJournalUnavailable
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.checkLocked(ctx, true); err != nil {
		return nil, err
	}
	r, err := recordFromEvidence(evidence)
	if err != nil {
		return nil, err
	}
	if err = j.recordBinding(r); err != nil {
		return nil, err
	}
	wire, err := encodeExecJournalRecord(r)
	if err != nil {
		return nil, err
	}
	existing, err := j.readCommandLocked(ctx, r.Context.CommandID)
	if err != nil {
		return nil, err
	}
	// Existing retries precede new-record capacity checks. Compare every
	// canonical byte: matching digests alone cannot preserve the start window.
	if existing != nil {
		previous, err := encodeExecJournalRecord(*existing)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(previous, wire) {
			return nil, ErrConflict
		}
		return existing, nil
	}
	if err = j.persistNewCommandLocked(ctx, r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Lookup is a bounded point read of owned unknown history. Absence is nil,nil;
// corruption and unsafe files are errors, including on poisoned handles.
func (j *Journal) Lookup(ctx context.Context, commandID string) (*ExecJournalRecord, error) {
	if ctx == nil {
		return nil, ErrInvalidConfiguration
	}
	if j == nil {
		return nil, ErrJournalUnavailable
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.readCommandLocked(ctx, commandID)
}

func (j *Journal) recordBinding(r ExecJournalRecord) error {
	c := r.Context
	i := j.gate.Identity
	if c.Namespace != i.Namespace || c.AuthorityID != i.AuthorityID || c.Target != i.Target || c.RestoreEpoch != i.RestoreEpoch || c.SandboxID != i.SandboxID || c.WorkspaceHash != i.WorkspaceHash || c.Generation != i.Generation || c.Runtime != i.Runtime || c.DataGateEpoch != j.gate.DataGateEpoch {
		return ErrIdentityMismatch
	}
	return nil
}

func sameAcceptance(a, b ExecJournalRecord) bool {
	return a.Context == b.Context && a.DescriptorDigest == b.DescriptorDigest && a.TicketDigest == b.TicketDigest && a.NotBefore.Equal(b.NotBefore) && a.NotAfter.Equal(b.NotAfter)
}
