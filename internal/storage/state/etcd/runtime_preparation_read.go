package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// Entries expose durable metadata only, never authority to invoke a runtime.
type RuntimeBindingEntry struct {
	Record      RuntimeBindingRecord
	Certificate json.RawMessage
	Reference   StageReference
}
type RuntimeMountIntentEntry struct {
	Record    RuntimeMountIntentRecord
	Reference StageReference
}

// runtimePreparationBundle keeps all CAS inputs from one identity-fenced snapshot.
type runtimePreparationBundle struct {
	Dispatch                                       *RuntimeDispatchEntry
	Binding                                        *RuntimeBindingEntry
	Mount                                          *RuntimeMountIntentEntry
	DispatchKVs, BindingKVs, MountKVs              []*mvccpb.KeyValue
	IndexKV                                        *mvccpb.KeyValue
	BindingKey, CertificateKey, IndexKey, MountKey string
}

func (b *Backend) LoadRuntimeBinding(ctx context.Context, w WorkspaceIdentity, intent string) (*RuntimeBindingEntry, error) {
	bundle, err := b.loadRuntimePreparation(ctx, w, intent, "")
	if err != nil {
		return nil, err
	}
	return bundle.Binding, nil
}
func (b *Backend) LoadRuntimeMountIntent(ctx context.Context, w WorkspaceIdentity, intent string) (*RuntimeMountIntentEntry, error) {
	bundle, err := b.loadRuntimePreparation(ctx, w, intent, "")
	if err != nil {
		return nil, err
	}
	return bundle.Mount, nil
}

// candidateUID is used only to locate the absent binding's prospective reservation.
// All final records, including the dispatch and optional mount, share one read.
func (b *Backend) loadRuntimePreparation(ctx context.Context, w WorkspaceIdentity, intent, candidateUID string) (*runtimePreparationBundle, error) {
	if err := b.validateWorkspaceBinding(w); err != nil {
		return nil, err
	}
	dk, ik, err := b.namespace.runtimeDispatchKeys(w.Partition(), intent)
	if err != nil {
		return nil, err
	}
	bk, ck, err := b.namespace.runtimeBindingKeys(w.Partition(), intent)
	if err != nil {
		return nil, err
	}
	mk, err := b.namespace.runtimeMountIntentKey(w.Partition(), intent)
	if err != nil {
		return nil, err
	}
	initial, err := b.readDomain(ctx, dk, ik, bk, ck, mk)
	if err != nil {
		return nil, err
	}
	d, _, err := b.decodeRuntimeDispatchPair(initial[0], initial[1], w, intent)
	if err != nil {
		return nil, err
	}
	bundle := &runtimePreparationBundle{BindingKey: bk, CertificateKey: ck, MountKey: mk}
	if d == nil {
		if initial[2] != nil || initial[3] != nil || initial[4] != nil {
			return nil, ErrCorruptRecord
		}
		return bundle, nil
	}
	drk, err := b.runtimeDispatchReceiptKey(d.Attempt)
	if err != nil {
		return nil, err
	}
	var locatedBinding RuntimeBindingRecord
	var locatedMount RuntimeMountIntentRecord
	if initial[2] != nil {
		if err := decodeDomainRecord(initial[2], &locatedBinding); err != nil {
			return nil, err
		}
		candidateUID = locatedBinding.Runtime.UID
	} else if initial[3] != nil || initial[4] != nil {
		return nil, ErrCorruptRecord
	}
	if initial[4] != nil {
		if err := decodeDomainRecord(initial[4], &locatedMount); err != nil {
			return nil, err
		}
	}
	keys := []string{dk, ik, drk, bk, ck, mk}
	receiptIndexes := map[int]bool{2: true}
	xindex, brindex, mrindex := -1, -1, -1
	if candidateUID != "" {
		bundle.IndexKey, err = b.namespace.runtimeIndexKey(candidateUID)
		if err != nil {
			return nil, err
		}
		xindex = len(keys)
		keys = append(keys, bundle.IndexKey)
	}
	if initial[2] != nil {
		rk, e := b.runtimeDispatchReceiptKey(locatedBinding.Attempt)
		if e != nil {
			return nil, e
		}
		brindex = len(keys)
		receiptIndexes[brindex] = true
		keys = append(keys, rk)
	}
	if initial[4] != nil {
		rk, e := b.runtimeDispatchReceiptKey(locatedMount.Attempt)
		if e != nil {
			return nil, e
		}
		mrindex = len(keys)
		receiptIndexes[mrindex] = true
		keys = append(keys, rk)
	}
	values, err := b.readDomain(ctx, keys...)
	if err != nil {
		var point *domainPointReadError
		if errors.As(err, &point) && receiptIndexes[point.index] && point.index < len(keys) && point.key == keys[point.index] {
			return nil, ErrCorruptReceipt
		}
		return nil, err
	}
	bundle.Dispatch, err = b.decodeRuntimeDispatchEntry(values[:3], w, intent, dk, ik, drk, d.Attempt)
	if err != nil {
		return nil, err
	}
	if bundle.Dispatch == nil {
		return nil, ErrCorruptRecord
	}
	bundle.DispatchKVs = values[:3]
	if xindex >= 0 {
		bundle.IndexKV = values[xindex]
	}
	// An appearance or disappearance since discovery requires retry, never borrowing a locator.
	if (initial[2] == nil) != (values[3] == nil) || (initial[4] == nil) != (values[5] == nil) {
		return nil, ErrCorruptRecord
	}
	if values[3] == nil {
		if values[4] != nil || values[5] != nil {
			return nil, ErrCorruptRecord
		}
		return bundle, nil
	}
	bindingValues := []*mvccpb.KeyValue{values[3], values[4], values[xindex], values[brindex]}
	if err := validatePreparationQuartet(bindingValues); err != nil {
		return nil, err
	}
	var r RuntimeBindingRecord
	var cert RuntimeBindingCertificateRecord
	var index RuntimeIndexRecord
	if err := decodeDomainRecord(values[3], &r); err != nil {
		return nil, err
	}
	if err := decodeDomainRecord(values[4], &cert); err != nil {
		return nil, err
	}
	if err := decodeDomainRecord(values[xindex], &index); err != nil {
		return nil, err
	}
	if err := b.domainEpoch(r.RestoreEpoch); err != nil {
		return nil, err
	}
	if r.Attempt != locatedBinding.Attempt || r.Runtime.UID != candidateUID || r.Attempt.Namespace != b.namespace.Root() || !bindingMatchesDispatch(r, bundle.Dispatch.Record) || cert.IntentID != r.IntentID || cert.SandboxID != r.SandboxID || cert.WorkspaceHash != r.WorkspaceHash || cert.RestoreEpoch != r.RestoreEpoch || cert.Generation != r.Generation || cert.CertificateDigest != r.CertificateDigest || index != (RuntimeIndexRecord{Version: 1, IntentID: r.IntentID, SandboxID: r.SandboxID, WorkspaceHash: r.WorkspaceHash, RestoreEpoch: r.RestoreEpoch, Generation: r.Generation, Runtime: r.Runtime}) {
		return nil, ErrCorruptRecord
	}
	ref, err := decodeRuntimeDispatchReceipt(values[brindex], r.Attempt)
	if err != nil {
		return nil, err
	}
	bundle.Binding = &RuntimeBindingEntry{Record: r, Certificate: append(json.RawMessage(nil), cert.Payload...), Reference: ref}
	bundle.BindingKVs = bindingValues
	if values[5] == nil {
		return bundle, nil
	}
	mountValues := []*mvccpb.KeyValue{values[5], values[mrindex]}
	if err := validatePreparationQuartet(mountValues); err != nil {
		return nil, err
	}
	var m RuntimeMountIntentRecord
	if err := decodeDomainRecord(values[5], &m); err != nil {
		return nil, err
	}
	if m.Attempt != locatedMount.Attempt || m.Attempt.Namespace != b.namespace.Root() || m.IntentID != r.IntentID || m.SandboxID != r.SandboxID || m.WorkspaceHash != r.WorkspaceHash || m.RestoreEpoch != r.RestoreEpoch || m.Generation != r.Generation || m.Runtime != r.Runtime || m.CertificateDigest != r.CertificateDigest || m.DispatchOperationID != r.OperationID || m.WorkspaceMode != r.WorkspaceMode || m.Attempt.RequestID != r.Attempt.RequestID {
		return nil, ErrCorruptRecord
	}
	ref, err = decodeRuntimeDispatchReceipt(values[mrindex], m.Attempt)
	if err != nil {
		return nil, err
	}
	bundle.Mount = &RuntimeMountIntentEntry{Record: m, Reference: ref}
	bundle.MountKVs = mountValues
	return bundle, nil
}
func validatePreparationQuartet(values []*mvccpb.KeyValue) error {
	for i, kv := range values {
		if !immutableDispatchKV(kv) {
			if i == len(values)-1 {
				return ErrCorruptReceipt
			}
			return ErrCorruptRecord
		}
		if kv.CreateRevision != values[0].CreateRevision {
			return ErrCorruptRecord
		}
	}
	return nil
}
func bindingMatchesDispatch(r RuntimeBindingRecord, d RuntimeDispatchRecord) bool {
	return r.IntentID == d.IntentID && r.SandboxID == d.SandboxID && r.WorkspaceHash == d.WorkspaceHash && r.RestoreEpoch == d.RestoreEpoch && r.Generation == d.Generation && r.Snapshot == d.Snapshot && r.ExpiresAt.Equal(d.ExpiresAt) && r.OperationID == d.OperationID && r.PayloadDigest == d.PayloadDigest && r.Attempt.RequestID == d.Attempt.RequestID
}
