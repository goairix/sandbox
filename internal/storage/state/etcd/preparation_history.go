package etcd

import (
	"bytes"
	"context"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// This private thirteen-record view authenticates monotonic creation completion.
// It excludes only the mutable runtime index: it is never current health,
// activation authority, a death certificate or runtime-quota release evidence.
type coldPublicationHistory struct {
	entry            *RuntimeEnrolledPublicationEntry
	journal, receipt *mvccpb.KeyValue
}

func (b *Backend) coldHistoricalKeys(partition uint8, intent string, r RuntimeEnrolledPublicationRecord) ([]string, error) {
	dk, payload, err := b.namespace.runtimeDispatchKeys(partition, intent)
	if err != nil {
		return nil, err
	}
	bk, cert, err := b.namespace.runtimeBindingKeys(partition, intent)
	if err != nil {
		return nil, err
	}
	mk, err := b.namespace.runtimeMountIntentKey(partition, intent)
	if err != nil {
		return nil, err
	}
	dr, err := b.runtimeDispatchReceiptKey(r.DispatchAttempt)
	if err != nil {
		return nil, err
	}
	br, err := b.runtimeDispatchReceiptKey(r.BindingAttempt)
	if err != nil {
		return nil, err
	}
	mr, err := b.runtimeDispatchReceiptKey(r.MountAttemptLocator)
	if err != nil {
		return nil, err
	}
	return []string{dk, payload, dr, bk, cert, br, mk, mr}, nil
}

func (b *Backend) decodeColdHistoricalPreparation(values []*mvccpb.KeyValue, keys []string, w WorkspaceIdentity, intent string, located RuntimeEnrolledPublicationRecord) (*runtimePreparationBundle, error) {
	if len(values) != 8 || len(keys) != 8 {
		return nil, ErrCorruptRecord
	}
	for i, kv := range values {
		if !enrolledImmutableKV(kv) || string(kv.Key) != keys[i] {
			return nil, ErrCorruptRecord
		}
	}
	dispatch, err := b.decodeRuntimeDispatchEntry(values[:3], w, intent, keys[0], keys[1], keys[2], located.DispatchAttempt)
	if err != nil || dispatch == nil {
		return nil, ErrCorruptRecord
	}
	var d RuntimeDispatchRecord
	var input RuntimeDispatchInputRecord
	var r RuntimeBindingRecord
	var cert RuntimeBindingCertificateRecord
	var mount RuntimeMountIntentRecord
	for _, item := range []struct {
		index  int
		record interface{ Validate() error }
	}{{0, &d}, {1, &input}, {3, &r}, {4, &cert}, {6, &mount}} {
		if err := enrolledDecodeHistory(values[item.index], item.record); err != nil {
			return nil, err
		}
	}
	if err := validatePreparationQuartet(values[3:6]); err != nil {
		return nil, err
	}
	if err := validatePreparationQuartet(values[6:]); err != nil {
		return nil, err
	}
	if r.Attempt != located.BindingAttempt || r.Runtime != located.Runtime || r.Attempt.Namespace != b.namespace.Root() || !bindingMatchesDispatch(r, d) || cert.IntentID != r.IntentID || cert.SandboxID != r.SandboxID || cert.WorkspaceHash != r.WorkspaceHash || cert.RestoreEpoch != r.RestoreEpoch || cert.Generation != r.Generation || cert.CertificateDigest != r.CertificateDigest {
		return nil, ErrCorruptRecord
	}
	if mount.Attempt != located.MountAttemptLocator || mount.Attempt.Namespace != b.namespace.Root() || mount.IntentID != r.IntentID || mount.SandboxID != r.SandboxID || mount.WorkspaceHash != r.WorkspaceHash || mount.RestoreEpoch != r.RestoreEpoch || mount.Generation != r.Generation || mount.Runtime != r.Runtime || mount.CertificateDigest != r.CertificateDigest || mount.DispatchOperationID != r.OperationID || mount.WorkspaceMode != "plain" || r.WorkspaceMode != "plain" || mount.MountAttempt != 0 || mount.Attempt.RequestID != r.Attempt.RequestID {
		return nil, ErrCorruptRecord
	}
	refs := make([]StageReference, 3)
	for i, item := range []struct {
		index   int
		locator StageAttemptLocator
	}{{2, d.Attempt}, {5, r.Attempt}, {7, mount.Attempt}} {
		ref, err := decodeRuntimeDispatchReceipt(values[item.index], item.locator)
		if err != nil {
			return nil, err
		}
		canonical, _ := encodeReceipt(ref, OutcomeCommitted)
		if string(values[item.index].Value) != canonical {
			return nil, ErrCorruptReceipt
		}
		refs[i] = ref
	}
	return &runtimePreparationBundle{Dispatch: dispatch, Binding: &RuntimeBindingEntry{Record: r, Certificate: bytes.Clone(cert.Payload), Reference: refs[1]}, Mount: &RuntimeMountIntentEntry{Record: mount, Reference: refs[2]}, DispatchKVs: values[:3], BindingKVs: values[3:6], MountKVs: values[6:], BindingKey: keys[3], CertificateKey: keys[4], MountKey: keys[6]}, nil
}

func (b *Backend) loadColdPublicationHistory(ctx context.Context, w WorkspaceIdentity, intent string) (*coldPublicationHistory, error) {
	if b == nil || b.publicationVerifier == nil || b.enrolledPublication == nil {
		return nil, ErrInvalidConfiguration
	}
	if err := b.validateWorkspaceBinding(w); err != nil {
		return nil, err
	}
	jk, pk, err := b.namespace.runtimePublicationKeys(w.Partition(), intent)
	if err != nil {
		return nil, err
	}
	ek, ak, err := b.namespace.runtimeEnrollmentKeys(w.Partition(), intent)
	if err != nil {
		return nil, err
	}
	initial, err := b.readEnrolledPoints(ctx, []string{jk, pk, ek, ak}, []int{8192, 16384, 8192, 8192})
	if err != nil {
		return nil, err
	}
	if initial[0] == nil {
		for _, kv := range initial {
			if kv != nil {
				return nil, ErrCorruptRecord
			}
		}
		return nil, nil
	}
	var record RuntimeEnrolledPublicationRecord
	if !enrolledImmutableKV(initial[0]) || decodeEnrolledRecord(initial[0].Value, &record) != nil {
		return nil, ErrCorruptRecord
	}
	if record.IntentID != intent || record.WorkspaceHash != w.Hash() || record.Attempt.Namespace != b.namespace.Root() || record.Attempt.Partition != w.Partition() || record.RestoreEpoch != b.restoreEpoch {
		return nil, ErrCorruptRecord
	}
	receipt, err := b.runtimeDispatchReceiptKey(record.Attempt)
	if err != nil {
		return nil, err
	}
	history, err := b.coldHistoricalKeys(w.Partition(), intent, record)
	if err != nil {
		return nil, err
	}
	keys := append([]string{jk, pk, ek, ak, receipt}, history...)
	caps := append([]int{8192, 16384, 8192, 8192, 4096}, []int{4096, 65536, 4096, 4096, 8192, 4096, 4096, 4096}...)
	values, err := b.readEnrolledPoints(ctx, keys, caps)
	if err != nil {
		return nil, err
	}
	entry, err := b.decodeColdHistoricalPublication(values, keys, w, intent, record)
	if err != nil {
		return nil, err
	}
	return &coldPublicationHistory{entry: entry, journal: values[0], receipt: values[4]}, nil
}

func (b *Backend) decodeColdHistoricalPublication(values []*mvccpb.KeyValue, keys []string, w WorkspaceIdentity, intent string, located RuntimeEnrolledPublicationRecord) (*RuntimeEnrolledPublicationEntry, error) {
	if len(values) != 13 || len(keys) != 13 {
		return nil, ErrCorruptRecord
	}
	for i, kv := range values {
		if !enrolledImmutableKV(kv) || string(kv.Key) != keys[i] {
			return nil, ErrCorruptRecord
		}
	}
	for _, kv := range values[:5] {
		if kv.CreateRevision != values[0].CreateRevision {
			return nil, ErrCorruptRecord
		}
	}
	var r RuntimeEnrolledPublicationRecord
	var proof RuntimeEnrolledPublicationProofRecord
	var enrollment RuntimeEnrollmentRecord
	var activation RuntimeEnrollmentActivationRecord
	for i, record := range []interface{ Validate() error }{&r, &proof, &enrollment, &activation} {
		if err := decodeEnrolledRecord(values[i].Value, record); err != nil {
			return nil, err
		}
	}
	if r != located || r.IntentID != intent || r.WorkspaceHash != w.Hash() || r.RestoreEpoch != b.restoreEpoch || r.Attempt.Namespace != b.namespace.Root() || r.Attempt.Partition != w.Partition() {
		return nil, ErrCorruptRecord
	}
	if proof.IntentID != r.IntentID || proof.SandboxID != r.SandboxID || proof.WorkspaceHash != r.WorkspaceHash || proof.RestoreEpoch != r.RestoreEpoch || proof.Generation != r.Generation || proof.ProofDigest != r.ProofDigest || enrollment.IntentID != r.IntentID || enrollment.SandboxID != r.SandboxID || enrollment.WorkspaceHash != r.WorkspaceHash || enrollment.RestoreEpoch != r.RestoreEpoch || enrollment.Generation != r.Generation || enrollment.Runtime != r.Runtime || activation.IntentID != r.IntentID || activation.SandboxID != r.SandboxID || activation.WorkspaceHash != r.WorkspaceHash || activation.RestoreEpoch != r.RestoreEpoch || activation.Generation != r.Generation || activation.Runtime != r.Runtime {
		return nil, ErrCorruptRecord
	}
	ed, err := snapshotDigest(values[2].Value)
	if err != nil || ed != r.EnrollmentRecordDigest {
		return nil, ErrCorruptRecord
	}
	ad, err := snapshotDigest(values[3].Value)
	if err != nil || ad != r.ActivationRecordDigest {
		return nil, ErrCorruptRecord
	}
	ref, err := decodeRuntimeDispatchReceipt(values[4], r.Attempt)
	if err != nil {
		return nil, err
	}
	canonicalReceipt, _ := encodeReceipt(ref, OutcomeCommitted)
	if string(values[4].Value) != canonicalReceipt {
		return nil, ErrCorruptReceipt
	}
	bundle, err := b.decodeColdHistoricalPreparation(values[5:], keys[5:], w, intent, r)
	if err != nil {
		return nil, err
	}
	d := bundle.Dispatch.Record
	if !bindingMatchesDispatch(bundle.Binding.Record, d) || r.SandboxID != d.SandboxID || r.Generation != d.Generation || r.DataGateEpoch != d.DataGateEpoch || r.Snapshot != d.Snapshot || !r.ExpiresAt.Equal(d.ExpiresAt) || r.OperationID != d.OperationID || r.PayloadDigest != d.PayloadDigest || r.CertificateDigest != bundle.Binding.Record.CertificateDigest || r.Runtime != bundle.Binding.Record.Runtime {
		return nil, ErrCorruptRecord
	}
	e := p.EnrolledRuntimeEvidence{Request: p.NodeEnrollmentRequest{Version: 1, Purpose: "runtime_node_enrollment", Activation: bytes.Clone(activation.Activation), RuntimeCertificate: bytes.Clone(activation.RuntimeCertificate), IssuerCertificate: bytes.Clone(activation.IssuerCertificate)}, ObserverCertificate: bytes.Clone(enrollment.ObserverCertificate), Receipt: bytes.Clone(enrollment.Receipt)}
	context := p.PublicationContext{Certificate: b.preparationCertificateContext(d), Runtime: p.RuntimeReference(r.Runtime), CertificateDigest: r.CertificateDigest, Claim: p.ClaimReference{ClaimID: r.Claim.ClaimID, CreateRevision: r.Claim.CreateRevision, LeaseID: r.Claim.LeaseID}, DataGateEpoch: r.DataGateEpoch}
	verified, err := b.publicationVerifier.VerifyEnrolledPublicationHistorical(proof.Payload, context, e, b.enrolledPublication.ContractDigest)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(verified.Certificate().Wire(), bundle.Binding.Certificate) || verified.Digest() != r.ProofDigest || verified.Ready().EnrollmentDigest != r.EnrollmentDigest || verified.Ready().ObserverCertificateDigest != r.ObserverCertificateDigest || verified.Ready().ActivationDigest != r.ActivationDigest || verified.EnrollmentReceipt().NodeUID != r.NodeUID || verified.EnrollmentReceipt().NodeBootID != r.NodeBootID {
		return nil, ErrCorruptRecord
	}
	return &RuntimeEnrolledPublicationEntry{Record: r, Proof: bytes.Clone(proof.Payload), Enrollment: e.Clone(), Reference: ref}, nil
}
