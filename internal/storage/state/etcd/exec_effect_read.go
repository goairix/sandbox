package etcd

import (
	"bytes"
	"context"
	"fmt"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// LoadExecEffect returns copied structural history of a committed metadata
// attempt. Expired tickets remain readable without issuer or clock calls. An
// absent initial snapshot is not evidence that a delayed attempt failed.
func (b *Backend) LoadExecEffect(ctx context.Context, ref ExecEffectReference) (*ExecEffectEntry, error) {
	if ctx == nil {
		return nil, ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	if ref.Operation.Namespace != b.namespace.Root() || ref.Operation.RestoreEpoch != b.restoreEpoch {
		return nil, ErrIdentityMismatch
	}
	effectKey, err := b.namespace.execEffectKey(ref.Operation)
	if err != nil {
		return nil, err
	}
	_, receiptKey, err := b.stageKeys(ref.Stage)
	if err != nil {
		return nil, err
	}
	bounded, cancel := b.requestContext(ctx)
	defer cancel()
	keys := []string{b.identityKey, b.restoreKey, effectKey}
	initial, err := b.readExecEffectPoints(bounded, keys)
	if err != nil {
		return nil, err
	}
	if initial[2] == nil {
		return nil, nil
	}
	var located ExecEffectRecord
	if err := decodeExecEffectRecord(initial[2], &located); err != nil {
		return nil, err
	}
	if !execEffectMatchesReference(located, ref) {
		return nil, ErrCorruptRecord
	}
	issuerKey, err := b.namespace.commandIssuerKey(located.IssuerCertificateID)
	if err != nil {
		return nil, err
	}
	// Retain owned discovery evidence across the next RPC; helper values are
	// borrowed call-local data, not stable storage for the original revision.
	originalValue := bytes.Clone(initial[2].Value)
	originalRevision := initial[2].CreateRevision
	keys = append(keys, receiptKey, issuerKey)
	points, err := b.readExecEffectPoints(bounded, keys)
	if err != nil {
		return nil, err
	}
	// Discovery cannot adopt a replacement effect and thereby replace its
	// original issuer revision, even if every later record is coherent.
	effect := points[2]
	if effect == nil || effect.CreateRevision != originalRevision || !bytes.Equal(effect.Value, originalValue) {
		return nil, ErrCorruptRecord
	}
	var record ExecEffectRecord
	if err := decodeExecEffectRecord(effect, &record); err != nil {
		return nil, err
	}
	if !execEffectMatchesReference(record, ref) {
		return nil, ErrCorruptRecord
	}
	outcome, err := decodeReceipt(points[3], ref.Stage)
	if err != nil {
		return nil, err
	}
	if outcome != OutcomeCommitted || points[3].CreateRevision != effect.CreateRevision {
		return nil, ErrCorruptReceipt
	}
	var issuer CommandIssuerRecord
	if err := decodeCommandIssuerRecord(points[4], &issuer); err != nil {
		return nil, err
	}
	if points[4].CreateRevision != record.IssuerRevision || issuer.Namespace != ref.Operation.Namespace || issuer.RestoreEpoch != ref.Operation.RestoreEpoch || issuer.CertificateID != record.IssuerCertificateID || issuer.CertificateDigest != record.IssuerCertificateDigest {
		return nil, ErrCorruptRecord
	}
	return &ExecEffectEntry{Reference: ref, Outcome: OutcomeCommitted, Record: &record, Revision: effect.CreateRevision}, nil
}

func execEffectMatchesReference(record ExecEffectRecord, ref ExecEffectReference) bool {
	return record.Operation.Reference == ref.Operation && record.CommandID == ref.CommandID && record.Attempt.reference(ref.Stage.Digest) == ref.Stage
}

// Both branches execute the same fixed nonserializable points. Validate every
// envelope and fence before interpreting Succeeded or any business evidence.
func (b *Backend) readExecEffectPoints(ctx context.Context, keys []string) ([]*mvccpb.KeyValue, error) {
	operations := make([]clientv3.Op, len(keys))
	for i, key := range keys {
		operations[i] = clientv3.OpGet(key)
	}
	response, err := b.client.Txn(ctx).If(b.baseComparisons()...).Then(operations...).Else(operations...).Commit()
	if err != nil {
		return nil, fmt.Errorf("%w: load exec effect: %w", ErrOutcomeUnknown, err)
	}
	points, err := b.stageEvidencePoints(response, keys)
	if err != nil {
		return nil, err
	}
	if !response.Succeeded {
		return nil, ErrIdentityMismatch
	}
	return points, nil
}
