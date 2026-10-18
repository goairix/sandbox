package etcd

import "go.etcd.io/etcd/api/v3/mvccpb"

// coldQuotaReturnProofStamp is copied intermediate metadata, not worker or
// mutation authority. It retains neither the raw return KV nor source history;
// the entry digest is fixed while the authenticated return bytes are available.
type coldQuotaReturnProofStamp struct {
	attempt StageAttemptLocator
	birth   int64
	entry   ColdRuntimeQuotaReturnEntry
}

// decodeColdQuotaReturnProof authenticates the immutable return record against
// its complete original history before any receipt read is attempted.
func (b *Backend) decodeColdQuotaReturnProof(h *coldQuotaReturnHistory, kv *mvccpb.KeyValue) (coldQuotaReturnProofStamp, error) {
	binding, err := coldQuotaReturnRelease(h)
	if err != nil {
		return coldQuotaReturnProofStamp{}, ErrCorruptRecord
	}
	if binding.detached != nil {
		r, err := decodeDetachedColdRuntimeQuotaReturn(kv)
		if err != nil || r != b.detachedColdQuotaReturnRecord(h, r.Attempt, binding.detached) {
			return coldQuotaReturnProofStamp{}, ErrCorruptRecord
		}
		return coldQuotaReturnProofStamp{
			attempt: r.Attempt,
			birth:   kv.CreateRevision,
			entry: ColdRuntimeQuotaReturnEntry{
				Task: r.Release.Task, TaskBirth: r.Release.TaskBirth, Owner: r.Owner, Quota: r.Quota,
				Runtime: r.Runtime, NodeUID: r.NodeUID, NodeBootID: r.NodeBootID,
				CreateRevision: kv.CreateRevision, SHA256: workspaceSyncIntentDigest(string(kv.Value)),
			},
		}, nil
	}
	r, err := decodeColdRuntimeQuotaReturn(kv)
	if err != nil || r != b.coldQuotaReturnRecord(h, r.Attempt) {
		return coldQuotaReturnProofStamp{}, ErrCorruptRecord
	}

	return coldQuotaReturnProofStamp{
		attempt: r.Attempt,
		birth:   kv.CreateRevision,
		entry:   *coldQuotaReturnEntry(r, kv),
	}, nil
}

// decodeColdQuotaReturnReceipt binds the canonical committed receipt to the
// exact attempt and birth authenticated by the return proof. Each successful
// result owns its entry copy; no caller can mutate a later result through it.
func (b *Backend) decodeColdQuotaReturnReceipt(stamp coldQuotaReturnProofStamp, kv *mvccpb.KeyValue) (StageReference, *ColdRuntimeQuotaReturnEntry, error) {
	key, err := b.runtimeDispatchReceiptKey(stamp.attempt)
	if err != nil {
		return StageReference{}, nil, err
	}
	if !enrolledImmutableKV(kv) || string(kv.Key) != key || kv.CreateRevision != stamp.birth {
		return StageReference{}, nil, ErrCorruptReceipt
	}

	ref, err := decodeRuntimeDispatchReceipt(kv, stamp.attempt)
	if err != nil {
		return StageReference{}, nil, err
	}
	wire, err := encodeReceipt(ref, OutcomeCommitted)
	if err != nil || wire != string(kv.Value) {
		return StageReference{}, nil, ErrCorruptReceipt
	}

	entry := stamp.entry
	return ref, &entry, nil
}
