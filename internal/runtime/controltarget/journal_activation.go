package controltarget

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

const maxJournalActivationBytes = 16384

// Inner wires remain bounded opaque diagnostics on cold recovery. Only a fresh
// live handle authenticates them; historical bytes can never open admission.
type journalActivationBundle struct {
	Version            uint32          `json:"version"`
	Identity           JournalIdentity `json:"identity"`
	DataGateEpoch      int64           `json:"data_gate_epoch"`
	Digest             string          `json:"digest"`
	Wire               []byte          `json:"wire"`
	RuntimeCertificate []byte          `json:"runtime_certificate"`
	IssuerCertificate  []byte          `json:"issuer_certificate"`
	// PreviousDigest is present only in v2 replacements. Together with the
	// signed activation digest it identifies one original renewal retry.
	PreviousDigest string `json:"previous_digest,omitempty"`
}

var journalActivationSchema = journalObject(map[string]*journalSchema{"version": journalNumberField, "identity": journalIdentitySchema, "data_gate_epoch": journalNumberField, "digest": journalStringField, "wire": journalStringField, "runtime_certificate": journalStringField, "issuer_certificate": journalStringField})
var journalRenewedActivationSchema = journalObject(map[string]*journalSchema{"version": journalNumberField, "identity": journalIdentitySchema, "data_gate_epoch": journalNumberField, "digest": journalStringField, "wire": journalStringField, "runtime_certificate": journalStringField, "issuer_certificate": journalStringField, "previous_digest": journalStringField})

func (b journalActivationBundle) validate() error {
	if err := b.Identity.Validate(); err != nil {
		return err
	}
	h := sha256.Sum256(b.Wire)
	versionValid := b.Version == 1 && b.PreviousDigest == "" || b.Version == 2 && journalHash(b.PreviousDigest) && b.PreviousDigest != b.Digest
	if !versionValid || b.DataGateEpoch <= 0 || !journalHash(b.Digest) || b.Digest != hex.EncodeToString(h[:]) || len(b.Wire) == 0 || len(b.Wire) > 16384 || len(b.RuntimeCertificate) == 0 || len(b.RuntimeCertificate) > 8192 || len(b.IssuerCertificate) == 0 || len(b.IssuerCertificate) > 8192 {
		return ErrInvalidRecord
	}
	return nil
}
func decodeJournalActivation(b []byte) (journalActivationBundle, error) {
	var v journalActivationBundle
	// Dispatch only selects the schema. The strict pass over the original
	// bytes still rejects duplicate, missing and unknown fields in both versions.
	var version struct {
		Version uint32 `json:"version"`
	}
	if len(b) > maxJournalActivationBytes || json.Unmarshal(b, &version) != nil {
		return v, ErrInvalidRecord
	}
	schema := journalActivationSchema
	if version.Version == 2 {
		schema = journalRenewedActivationSchema
	}
	if err := decodeJournalWireLimit(b, schema, &v, maxJournalActivationBytes); err != nil {
		return v, err
	}
	return v, v.validate()
}

// authorityNow uses local monotonic time only to bound the trusted clock call.
// It starts no worker or timer until a live operation requests an observation.
func (j *Journal) authorityNow(ctx context.Context) (time.Time, error) {
	if nilJournalDependency(ctx) || nilJournalDependency(j.clock) {
		return time.Time{}, ErrInvalidConfiguration
	}
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	start := time.Now()
	o, err := j.clock.Observe(bounded)
	elapsed := time.Since(start)
	if err != nil {
		return time.Time{}, err
	}
	if err = bounded.Err(); err != nil {
		return time.Time{}, err
	}
	if !journalUTC(o.UTC) || o.Uncertainty < 0 || o.Uncertainty > time.Second || elapsed < 0 || elapsed > time.Second-o.Uncertainty {
		return time.Time{}, fmt.Errorf("invalid or late authority clock")
	}
	return o.UTC.Add(elapsed), nil
}
func (j *Journal) verifyActivationLocked(ctx context.Context, e controlprotocol.TargetActivationEvidence) (controlprotocol.TargetActivationEvidence, time.Time, error) {
	if j.birth == nil || j.verifier == nil || !j.fresh {
		return controlprotocol.TargetActivationEvidence{}, time.Time{}, ErrJournalUnavailable
	}
	now, err := j.authorityNow(ctx)
	if err != nil {
		return controlprotocol.TargetActivationEvidence{}, time.Time{}, err
	}
	verified, err := j.verifier.VerifyTargetActivation(e.Wire(), e.RuntimeCertificate(), e.IssuerCertificate(), *j.birth, now)
	if err != nil {
		return controlprotocol.TargetActivationEvidence{}, time.Time{}, err
	}
	i, b := verified.Identity(), verified.Binding()
	want := j.gate.Identity
	if i.SandboxID != want.SandboxID || i.WorkspaceHash != want.WorkspaceHash || i.Generation != want.Generation || i.Runtime != want.Runtime || b.Namespace != want.Namespace || b.AuthorityID != want.AuthorityID || b.Target != want.Target || b.RestoreEpoch != want.RestoreEpoch || verified.DataGateEpoch() != j.gate.DataGateEpoch {
		return controlprotocol.TargetActivationEvidence{}, time.Time{}, ErrIdentityMismatch
	}
	return verified, now, nil
}
func (j *Journal) liveNowLocked(ctx context.Context) (time.Time, error) {
	if err := j.checkLocked(ctx, true); err != nil {
		return time.Time{}, err
	}
	if j.activation == nil || j.gate.GateState != "open" {
		return time.Time{}, ErrJournalUnavailable
	}
	_, now, err := j.verifyActivationLocked(ctx, *j.activation)
	return now, err
}

// InstallActivation is available only on the freshly created live handle.
// Every uncertain write revokes admission and leaves all bytes for diagnosis.
func (j *Journal) InstallActivation(ctx context.Context, e controlprotocol.TargetActivationEvidence) error {
	if j == nil || j.self != j {
		return ErrJournalUnavailable
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.checkLocked(ctx, true); err != nil {
		return err
	}
	verified, _, err := j.verifyActivationLocked(ctx, e)
	if err != nil {
		return err
	}
	if j.activation != nil {
		if j.activation.Digest() != verified.Digest() {
			return ErrConflict
		}
		if j.gate.GateState != "open" {
			return ErrJournalUnavailable
		}
		return nil
	}
	b := journalActivationBundle{Version: 1, Identity: j.gate.Identity, DataGateEpoch: j.gate.DataGateEpoch, Digest: verified.Digest(), Wire: verified.Wire(), RuntimeCertificate: verified.RuntimeCertificate(), IssuerCertificate: verified.IssuerCertificate()}
	if err = b.validate(); err != nil {
		return err
	}
	wire, err := encodeJournalWireLimit(b, maxJournalActivationBytes)
	if err != nil {
		return err
	}
	if err = j.persistActivationLocked(ctx, wire); err != nil {
		return err
	}
	if _, _, err = j.verifyActivationLocked(ctx, verified); err != nil {
		return j.poison(err)
	}
	if err = j.persistOpenGateLocked(ctx); err != nil {
		return j.poison(err)
	}
	if _, _, err = j.verifyActivationLocked(ctx, verified); err != nil {
		return j.poison(err)
	}
	j.activation = &verified
	return nil
}

// RenewActivation replaces certification on the original live journal without
// opening a gate. The caller must serialize its enclosing supervisor and retain
// the original predecessor/candidate pair for uncertain-outcome queries. The
// candidate reuses the independently installed birth and gate, and its strictly
// later signing baseline prevents replaying an older valid root certificate.
// NotAfter may decrease: shortening a business TTL is a supported operation.
// Cold history cannot call this method into a new live admission.
func (j *Journal) RenewActivation(ctx context.Context, previousDigest string, next controlprotocol.TargetActivationEvidence) error {
	if j == nil || j.self != j {
		return ErrJournalUnavailable
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.checkLocked(ctx, true); err != nil {
		return err
	}
	if j.activation == nil || j.gate.GateState != "open" || j.usersClosed {
		return ErrJournalUnavailable
	}
	verified, _, err := j.verifyActivationLocked(ctx, next)
	if err != nil {
		return err
	}
	current := j.activation
	if verified.Digest() == current.Digest() {
		if previousDigest != j.activationPreviousDigest || previousDigest == "" {
			return ErrConflict
		}
		// A duplicate is read-only and must still match the exact durable live
		// bundle. It cannot rewrite a lost acknowledgement or revive a closed gate.
		return j.confirmActivationLocked(ctx)
	}
	if previousDigest != current.Digest() || verified.ActivationID() == current.ActivationID() || !verified.NotBefore().After(current.NotBefore()) {
		return ErrConflict
	}
	if _, _, err := j.verifyActivationLocked(ctx, *current); err != nil {
		return err
	}

	b := journalActivationBundle{Version: 2, Identity: j.gate.Identity, DataGateEpoch: j.gate.DataGateEpoch, Digest: verified.Digest(), Wire: verified.Wire(), RuntimeCertificate: verified.RuntimeCertificate(), IssuerCertificate: verified.IssuerCertificate(), PreviousDigest: previousDigest}
	if err := b.validate(); err != nil {
		return err
	}
	wire, err := encodeJournalWireLimit(b, maxJournalActivationBytes)
	if err != nil {
		return err
	}
	if err := j.replaceActivationLocked(ctx, wire); err != nil {
		return err
	}

	// Both authorities must remain live across actual persistence. Losing a
	// clock acknowledgement or the old interval after rename poisons the owner;
	// only fresh observations may diagnose those retained bytes afterwards.
	if _, _, err := j.verifyActivationLocked(ctx, *current); err != nil {
		return j.poison(err)
	}
	if _, _, err := j.verifyActivationLocked(ctx, verified); err != nil {
		return j.poison(err)
	}
	j.activationPreviousDigest = previousDigest
	j.activation = &verified
	return nil
}

// QueryActivationRenewal confirms only the last durable replacement on this
// original live handle. It is independent of business expiry and gate closure,
// but never blesses poisoned, cold or externally replaced journal bytes. An
// unknown pair is an error, not evidence that an installation was never sent.
func (j *Journal) QueryActivationRenewal(ctx context.Context, previousDigest, candidateDigest string) (bool, error) {
	if j == nil || j.self != j {
		return false, ErrJournalUnavailable
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.checkLocked(ctx, true); err != nil {
		return false, err
	}
	if !j.fresh || j.birth == nil || j.activation == nil || !journalHash(previousDigest) || !journalHash(candidateDigest) || previousDigest == candidateDigest {
		return false, ErrJournalUnavailable
	}
	if j.activationPreviousDigest != previousDigest || j.activation.Digest() != candidateDigest {
		return false, ErrConflict
	}
	if err := j.confirmActivationLocked(ctx); err != nil {
		return false, err
	}
	return true, nil
}
