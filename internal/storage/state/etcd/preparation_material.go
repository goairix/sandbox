package etcd

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// ColdBootstrapMaterial contains only public, creation-bound bootstrap bytes.
// It is immutable recovery data, never a replacement for a create permit.
type ColdBootstrapMaterial struct {
	Birth                  ct.Birth        `json:"birth"`
	RuntimeCertificate     json.RawMessage `json:"runtime_certificate"`
	IssuerCertificate      json.RawMessage `json:"issuer_certificate"`
	PublicationCertificate json.RawMessage `json:"publication_certificate"`
	Activation             json.RawMessage `json:"activation"`
}

func (m ColdBootstrapMaterial) BootstrapRequest() ct.BootstrapRequest {
	return ct.BootstrapRequest{Version: 1, Purpose: "activate", Activation: append([]byte(nil), m.Activation...), RuntimeCertificate: append([]byte(nil), m.RuntimeCertificate...), IssuerCertificate: append([]byte(nil), m.IssuerCertificate...)}
}

func (b *Backend) validateColdBootstrapMaterial(m ColdBootstrapMaterial, d RuntimeDispatchRecord, uid string, now time.Time) error {
	if b.enrolledVerifier == nil || b.publicationVerifier == nil || b.enrolledPublication == nil {
		return ErrInvalidConfiguration
	}
	if err := coldMaterialBounds(m); err != nil {
		return err
	}
	if !validPreparationUUID(uid) || d.Kind != RuntimeDispatchCreate || m.Birth.ContractDigest != b.enrolledPublication.ContractDigest {
		return ErrInvalidRecord
	}
	birth := p.BirthContext{BootID: m.Birth.BootID, RuntimePublicKey: m.Birth.RuntimePublicKey, UID: m.Birth.UID, GID: m.Birth.GID, NetworkAllowed: m.Birth.NetworkAllowed, ContractDigest: m.Birth.ContractDigest}
	activation, err := b.enrolledVerifier.VerifyTargetActivation(m.Activation, m.RuntimeCertificate, m.IssuerCertificate, birth, now)
	if err != nil {
		return err
	}
	identity := activation.Identity()
	if identity.SandboxID != d.SandboxID || identity.WorkspaceHash != d.WorkspaceHash || identity.Generation != d.Generation || identity.Runtime.UID != uid || activation.DataGateEpoch() != d.DataGateEpoch || activation.NotAfter().After(d.ExpiresAt) {
		return ErrInvalidRecord
	}
	certificate, err := b.publicationVerifier.VerifyCertificate(m.PublicationCertificate, b.preparationCertificateContext(d), now)
	if err != nil {
		return err
	}
	if certificate.Runtime() != identity.Runtime || certificate.WorkspaceMode() != "plain" || !bytes.Equal(certificate.PublicKey(), birth.RuntimePublicKey) {
		return ErrInvalidRecord
	}
	return nil
}

func coldMaterialBounds(m ColdBootstrapMaterial) error {
	for _, wire := range []json.RawMessage{m.Activation, m.RuntimeCertificate, m.IssuerCertificate, m.PublicationCertificate} {
		if len(wire) == 0 || len(wire) > 8192 {
			return ErrInvalidRecord
		}
	}
	if len(m.Birth.RuntimePublicKey) != 32 || len(m.Birth.BootID) > 36 || len(m.Birth.ContractDigest) > 64 {
		return ErrInvalidRecord
	}
	if _, err := coldEncode(m); err != nil {
		return err
	}
	// The native bootstrap frame base64-encodes []byte fields. The durable
	// RawMessage record's bound alone cannot prove this request is sendable.
	frame, err := json.Marshal(m.BootstrapRequest())
	if err != nil || len(frame)+4 > ct.MaxBootstrap {
		return ErrInvalidRecord
	}
	return nil
}

type ColdBootstrapMaterialRecord struct {
	Version           int                   `json:"version"`
	RestoreEpoch      string                `json:"restore_epoch"`
	ProfileDigest     string                `json:"profile_digest"`
	Owner             ColdSlotOwner         `json:"owner"`
	AttemptDigest     string                `json:"attempt_digest"`
	ObservationDigest string                `json:"observation_digest"`
	Material          ColdBootstrapMaterial `json:"material"`
}

// FixColdBootstrapMaterial persists the exact public bytes before activation.
// Unknown commit outcomes must be resolved by LoadColdBootstrapMaterial; they
// never permit replacement Birth, certificates, or activation identifiers.
func (b *Backend) FixColdBootstrapMaterial(ctx context.Context, c *CreationClaim, material ColdBootstrapMaterial) (*ColdBootstrapMaterialRecord, error) {
	if err := coldMaterialBounds(material); err != nil {
		return nil, err
	}
	wire, err := coldEncode(material)
	if err != nil {
		return nil, err
	}
	var owned ColdBootstrapMaterial
	if json.Unmarshal([]byte(wire), &owned) != nil {
		return nil, ErrInvalidRecord
	}
	claim, err := b.creationClaimComparisons(c)
	if err != nil {
		return nil, err
	}
	s, attempt, observation, observed, dispatch, err := b.coldMaterialState(ctx, c.workspace, c.reference.IntentID)
	if err != nil {
		return nil, err
	}
	if s.record.Owner != coldClaimOwner(c) || !c.matchesRuntimeDispatch(dispatch) {
		return nil, ErrCorruptRecord
	}
	now, err := b.observePublicationClock(ctx)
	if err != nil {
		return nil, err
	}
	if err := b.validateColdBootstrapMaterial(owned, dispatch, observed.PodUID, now); err != nil {
		return nil, err
	}
	r := ColdBootstrapMaterialRecord{Version: 1, RestoreEpoch: b.restoreEpoch, ProfileDigest: b.preparationDigest, Owner: s.record.Owner, AttemptDigest: workspaceSyncIntentDigest(string(attempt.Value)), ObservationDigest: workspaceSyncIntentDigest(string(observation.Value)), Material: owned}
	recordWire, err := coldEncode(r)
	if err != nil {
		return nil, err
	}
	key := s.attemptKey + "-material"
	existing, err := b.coldReadMaterial(ctx, key, s.record.Owner, attempt, observation)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		oldWire, _ := coldEncode(existing)
		if oldWire != recordWire {
			return nil, ErrIdempotencyConflict
		}
		return existing, nil
	}
	cmps := append(b.baseComparisons(), claim...)
	cmps = append(cmps, s.comparisons()...)
	cmps = append(cmps, coldComparisons(attempt)...)
	cmps = append(cmps, coldComparisons(observation)...)
	cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	writes := []Write{{Key: key, Value: []byte(recordWire)}, {Key: string(s.work.Key), Value: s.work.Value}}
	if len(cmps) != 47 || len(writes) != 2 {
		return nil, ErrInvalidMutation
	}
	if _, err := b.coldCommit(ctx, cmps, writes, key); err != nil {
		return nil, err
	}
	return &r, nil
}

// LoadColdBootstrapMaterial authenticates current recovery material. Historical
// preparation settlement uses its independent monotonic publication witness.
func (b *Backend) LoadColdBootstrapMaterial(ctx context.Context, w WorkspaceIdentity, intent string) (*ColdBootstrapMaterialRecord, error) {
	s, attempt, observation, observed, dispatch, err := b.coldMaterialState(ctx, w, intent)
	if err != nil {
		return nil, err
	}
	r, err := b.coldReadMaterial(ctx, s.attemptKey+"-material", s.record.Owner, attempt, observation)
	if err != nil || r == nil {
		return r, err
	}
	now, err := b.observePublicationClock(ctx)
	if err != nil {
		return nil, err
	}
	if err := b.validateColdBootstrapMaterial(r.Material, dispatch, observed.PodUID, now); err != nil {
		return nil, err
	}
	return r, nil
}

func (b *Backend) coldMaterialState(ctx context.Context, w WorkspaceIdentity, intent string) (*coldPendingState, *mvccpb.KeyValue, *mvccpb.KeyValue, *ColdCreateObservation, RuntimeDispatchRecord, error) {
	fail := func(err error) (*coldPendingState, *mvccpb.KeyValue, *mvccpb.KeyValue, *ColdCreateObservation, RuntimeDispatchRecord, error) {
		return nil, nil, nil, nil, RuntimeDispatchRecord{}, err
	}
	s, err := b.coldPendingSnapshot(ctx, w, intent)
	if err != nil {
		return fail(err)
	}
	a, attempt, err := b.coldReadAttempt(ctx, s)
	if err != nil {
		return fail(err)
	}
	if a == nil {
		return fail(ErrConflict)
	}
	o, observation, err := b.coldReadObservation(ctx, s, a, attempt)
	if err != nil {
		return fail(err)
	}
	if o == nil {
		return fail(ErrConflict)
	}
	d, err := b.LoadRuntimeDispatch(ctx, w, intent)
	if err != nil {
		return fail(err)
	}
	if d == nil || d.Record.Kind != RuntimeDispatchCreate || d.Record.OperationID != a.OperationID || d.Record.PayloadDigest != a.PayloadDigest {
		return fail(ErrCorruptRecord)
	}
	return s, attempt, observation, o, d.Record, nil
}

func (b *Backend) coldReadMaterial(ctx context.Context, key string, owner ColdSlotOwner, attempt, observation *mvccpb.KeyValue) (*ColdBootstrapMaterialRecord, error) {
	values, err := b.readDomain(ctx, key)
	if err != nil {
		return nil, err
	}
	kv := values[0]
	if kv == nil {
		return nil, nil
	}
	var r ColdBootstrapMaterialRecord
	if !enrolledImmutableKV(kv) || coldDecode(kv, &r) != nil || r.Version != 1 || r.RestoreEpoch != b.restoreEpoch || r.ProfileDigest != b.preparationDigest || r.Owner != owner || r.AttemptDigest != workspaceSyncIntentDigest(string(attempt.Value)) || r.ObservationDigest != workspaceSyncIntentDigest(string(observation.Value)) || kv.CreateRevision <= observation.CreateRevision || coldMaterialBounds(r.Material) != nil {
		return nil, ErrCorruptRecord
	}
	return &r, nil
}
