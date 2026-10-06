package etcd

import (
	"errors"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// stageEvidencePoints validates the complete point-read envelope before any
// caller interprets its CAS result. Values retain the supplied key order,
// including identity and restore at positions zero and one, and are borrowed
// only for this call. Succeeded does not by itself describe a failed fence.
func (b *Backend) stageEvidencePoints(response *clientv3.TxnResponse, keys []string) ([]*mvccpb.KeyValue, error) {
	if err := b.operationResponseHeader(response); err != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	if len(keys) < 2 || keys[0] != b.identityKey || keys[1] != b.restoreKey || len(response.Responses) != len(keys) {
		return nil, ErrOutcomeUnknown
	}
	values := make([]*mvccpb.KeyValue, len(keys))
	for i, key := range keys {
		op := response.Responses[i]
		if op == nil {
			return nil, ErrOutcomeUnknown
		}
		point := op.GetResponseRange()
		if point == nil || point.More || point.Count != int64(len(point.Kvs)) || len(point.Kvs) > 1 || !operationNestedHeader(point.Header, response.Header) {
			return nil, ErrOutcomeUnknown
		}
		if len(point.Kvs) == 0 {
			continue
		}
		kv := point.Kvs[0]
		if kv == nil || string(kv.Key) != key || kv.CreateRevision <= 0 || kv.ModRevision < kv.CreateRevision || kv.ModRevision > response.Header.Revision {
			return nil, ErrOutcomeUnknown
		}
		values[i] = kv
	}
	if !operationPermanentKV(values[0]) || !operationPermanentKV(values[1]) || string(values[0].Value) != b.identityValue || string(values[1].Value) != b.restoreEpoch {
		return nil, errors.Join(ErrOutcomeUnknown, ErrIdentityMismatch)
	}
	return values, nil
}

func stageCommitResponses(response *clientv3.TxnResponse, writes []Write) bool {
	if len(response.Responses) != len(writes)+1 {
		return false
	}
	for i, op := range response.Responses {
		if op == nil {
			return false
		}
		if i < len(writes) && writes[i].Delete {
			deleted := op.GetResponseDeleteRange()
			if deleted == nil || deleted.Deleted < 0 || deleted.Deleted > 1 || len(deleted.PrevKvs) != 0 {
				return false
			}
			validHeader := operationNestedHeader(deleted.Header, response.Header)
			// Native etcd reports the read revision for a no-op delete before
			// the transaction's first write. The mandatory receipt Put then
			// advances the outer revision by one. This exception is only for
			// Delete0; all actual writes and point reads remain exact.
			if deleted.Deleted == 0 && deleted.Header != nil {
				h := deleted.Header
				validHeader = (h.ClusterId == 0 || h.ClusterId == response.Header.ClusterId) && h.Revision > 0 && (h.Revision == response.Header.Revision || h.Revision == response.Header.Revision-1)
			}
			if !validHeader {
				return false
			}
		} else {
			put := op.GetResponsePut()
			if put == nil || put.PrevKv != nil || !operationNestedHeader(put.Header, response.Header) {
				return false
			}
		}
	}
	return true
}
