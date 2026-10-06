package etcd

import clientv3 "go.etcd.io/etcd/client/v3"

// validateFenceCheckResponse validates the fixed identity point used as the
// read barrier in a no-write fence Txn. Failed comparisons may legitimately
// observe a changed or missing identity, but never a malformed point envelope.
func (b *Backend) validateFenceCheckResponse(response *clientv3.TxnResponse) error {
	if err := b.operationResponseHeader(response); err != nil {
		return err
	}
	if len(response.Responses) != 1 || response.Responses[0] == nil {
		return ErrOutcomeUnknown
	}
	point := response.Responses[0].GetResponseRange()
	if point == nil || point.More || point.Count != int64(len(point.Kvs)) || len(point.Kvs) > 1 || !operationNestedHeader(point.Header, response.Header) {
		return ErrCorruptRecord
	}
	if len(point.Kvs) == 1 {
		kv := point.Kvs[0]
		if kv == nil || string(kv.Key) != b.identityKey || kv.CreateRevision <= 0 || kv.ModRevision < kv.CreateRevision || kv.ModRevision > response.Header.Revision {
			return ErrCorruptRecord
		}
		if response.Succeeded && (kv.Lease != 0 || string(kv.Value) != b.identityValue) {
			return ErrIdentityMismatch
		}
	} else if response.Succeeded {
		return ErrIdentityMismatch
	}
	return nil
}
