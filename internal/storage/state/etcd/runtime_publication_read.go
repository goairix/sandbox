package etcd

import (
	"context"
	"encoding/json"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

// RuntimePublicationEntry is structural, historical metadata evidence only.
// A recovered receipt reference grants no runtime execution or publish authority.
type RuntimePublicationEntry struct {
	Record    RuntimePublicationRecord
	Proof     json.RawMessage
	Reference StageReference
}

// LoadRuntimePublication locates the receipt with two exact point reads, then
// revalidates journal, proof and receipt together in an identity-fenced snapshot.
// Business expiry and current runtime indexes do not prevent historical reads.
// Absence carries no conclusion about the external runtime's outcome.
func (b *Backend) LoadRuntimePublication(ctx context.Context, w WorkspaceIdentity, intent string) (*RuntimePublicationEntry, error) {
	if err := b.validateWorkspaceBinding(w); err != nil {
		return nil, err
	}
	jk, pk, err := b.namespace.runtimePublicationKeys(w.Partition(), intent)
	if err != nil {
		return nil, err
	}
	initial, err := b.readDomain(ctx, jk, pk)
	if err != nil {
		return nil, err
	}
	r, _, err := b.decodeRuntimePublicationPair(initial[0], initial[1], w, intent)
	if err != nil || r == nil {
		return nil, err
	}
	rk, err := b.runtimeDispatchReceiptKey(r.Attempt)
	if err != nil {
		return nil, err
	}
	keys := []string{jk, pk, rk}
	values, err := b.readRuntimeDomain(ctx, keys, map[int]bool{2: true})
	if err != nil {
		return nil, err
	}
	return b.decodeRuntimePublicationEntry(values, w, intent, keys, r.Attempt)
}
func (b *Backend) decodeRuntimePublicationPair(journal, proof *mvccpb.KeyValue, w WorkspaceIdentity, intent string) (*RuntimePublicationRecord, *RuntimePublicationProofRecord, error) {
	if journal == nil && proof == nil {
		return nil, nil, nil
	}
	if !immutableDispatchKV(journal) || !immutableDispatchKV(proof) || journal.CreateRevision != proof.CreateRevision {
		return nil, nil, ErrCorruptRecord
	}
	var r RuntimePublicationRecord
	if err := decodeDomainRecord(journal, &r); err != nil {
		return nil, nil, err
	}
	if err := b.domainEpoch(r.RestoreEpoch); err != nil {
		return nil, nil, err
	}
	var p RuntimePublicationProofRecord
	if err := decodeDomainRecord(proof, &p); err != nil {
		return nil, nil, err
	}
	if err := b.domainEpoch(p.RestoreEpoch); err != nil {
		return nil, nil, err
	}
	if r.IntentID != intent || r.WorkspaceHash != w.Hash() || r.Attempt.Namespace != b.namespace.Root() || r.Attempt.Partition != w.Partition() || p.IntentID != r.IntentID || p.SandboxID != r.SandboxID || p.WorkspaceHash != r.WorkspaceHash || p.RestoreEpoch != r.RestoreEpoch || p.Generation != r.Generation || p.ProofDigest != r.ProofDigest {
		return nil, nil, ErrCorruptRecord
	}
	return &r, &p, nil
}
func (b *Backend) decodeRuntimePublicationEntry(values []*mvccpb.KeyValue, w WorkspaceIdentity, intent string, keys []string, located StageAttemptLocator) (*RuntimePublicationEntry, error) {
	if len(values) != 3 || len(keys) != 3 {
		return nil, ErrCorruptRecord
	}
	if values[0] == nil && values[1] == nil && values[2] == nil {
		return nil, nil
	}
	for i, key := range keys {
		if values[i] != nil && string(values[i].Key) != key {
			if i == 2 {
				return nil, ErrCorruptReceipt
			}
			return nil, ErrCorruptRecord
		}
	}
	r, p, err := b.decodeRuntimePublicationPair(values[0], values[1], w, intent)
	if err != nil {
		return nil, err
	}
	if r == nil || values[2] == nil || r.Attempt != located {
		return nil, ErrCorruptRecord
	}
	if err := validatePreparationQuartet(values); err != nil {
		return nil, err
	}
	ref, err := decodeRuntimeDispatchReceipt(values[2], r.Attempt)
	if err != nil {
		return nil, err
	}
	return &RuntimePublicationEntry{Record: *r, Proof: append(json.RawMessage(nil), p.Payload...), Reference: ref}, nil
}
