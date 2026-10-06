package etcd

import (
	"context"
	"fmt"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// ResolveOperation arbitrates only the original admission. Committed, aborted,
// and expired describe metadata; none proves that an external target has drained.
// It never reconstructs an OperationCapability or allocates a replacement Lease.
func (b *Backend) ResolveOperation(ctx context.Context, ref OperationReference) (OperationOutcome, error) {
	if ctx == nil {
		return OperationUnknown, ErrInvalidRecord
	}
	token, guard, receipt, mutation, err := b.namespace.operationKeys(ref)
	if err != nil {
		return OperationUnknown, err
	}
	if err = b.domainEpoch(ref.RestoreEpoch); err != nil {
		return OperationUnknown, err
	}
	keys := []string{b.identityKey, b.restoreKey, guard, token, receipt, mutation}
	reads := make([]clientv3.Op, len(keys))
	for i, k := range keys {
		reads[i] = clientv3.OpGet(k)
	}
	request, cancel := b.requestContext(ctx)
	response, err := b.client.Txn(request).If(b.baseComparisons()...).Then(reads...).Else(reads...).Commit()
	cancel()
	if err != nil {
		return OperationUnknown, fmt.Errorf("%w: read operation: %w", ErrOutcomeUnknown, err)
	}
	values, err := b.operationResolvePoints(response, keys)
	if err != nil {
		return OperationUnknown, err
	}
	outcome, err := b.operationResolvedEvidence(ref, values)
	if err != nil || outcome != OperationUnknown {
		return outcome, err
	}
	// A validated guard-only attempt is the sole state we may abort. Race the
	// exact original envelope and both absence predicates used by admission.
	g := values[0]
	comparisons := b.baseComparisons()
	comparisons = append(comparisons, operationEnvelopeComparisons(guard, string(g.Value), g.Lease, g.CreateRevision)...)
	comparisons = append(comparisons, clientv3.Compare(clientv3.CreateRevision(token), "=", 0), clientv3.Compare(clientv3.CreateRevision(receipt), "=", 0))
	marker, err := encodeOperationReceipt(operationReceipt{Version: 1, Reference: ref, Outcome: OperationAborted})
	if err != nil {
		return OperationUnknown, err
	}
	request, cancel = b.requestContext(ctx)
	response, err = b.client.Txn(request).If(comparisons...).Then(clientv3.OpPut(receipt, marker, clientv3.WithLease(clientv3.LeaseID(ref.LeaseID)))).Else(reads...).Commit()
	cancel()
	if err != nil {
		return OperationUnknown, fmt.Errorf("%w: abort operation: %w", ErrOutcomeUnknown, err)
	}
	if err = b.operationResponseHeader(response); err != nil {
		return OperationUnknown, err
	}
	if response.Succeeded {
		if !operationPutResponses(response, 1) {
			return OperationUnknown, ErrOutcomeUnknown
		}
		return OperationAborted, nil
	}
	values, err = b.operationResolvePoints(response, keys)
	if err != nil {
		return OperationUnknown, err
	}
	outcome, err = b.operationResolvedEvidence(ref, values)
	if outcome == OperationUnknown && err == nil {
		err = ErrOutcomeUnknown
	}
	return outcome, err
}

// Includes ModRevision to reject an in-place same-value rewrite as well as a
// delete/recreate. These four clauses retain the original immutable envelope.
func operationEnvelopeComparisons(key, value string, lease, revision int64) []clientv3.Cmp {
	return []clientv3.Cmp{clientv3.Compare(clientv3.Value(key), "=", value), clientv3.Compare(clientv3.LeaseValue(key), "=", lease), clientv3.Compare(clientv3.CreateRevision(key), "=", revision), clientv3.Compare(clientv3.ModRevision(key), "=", revision)}
}

// Resolver evidence order is identity/restore/guard/token/receipt/mutation;
// admission's receipt-first helper must not be used here.
func (b *Backend) operationResolvePoints(r *clientv3.TxnResponse, keys []string) ([]*mvccpb.KeyValue, error) {
	if err := b.operationResponseHeader(r); err != nil {
		return nil, err
	}
	if len(r.Responses) != len(keys) {
		return nil, ErrCorruptRecord
	}
	values := make([]*mvccpb.KeyValue, len(keys))
	for i, k := range keys {
		op := r.Responses[i]
		if op == nil {
			return nil, ErrCorruptRecord
		}
		point := op.GetResponseRange()
		if point == nil || point.More || point.Count != int64(len(point.Kvs)) || len(point.Kvs) > 1 || !operationNestedHeader(point.Header, r.Header) {
			return nil, ErrCorruptRecord
		}
		if len(point.Kvs) == 0 {
			continue
		}
		kv := point.Kvs[0]
		if kv == nil || string(kv.Key) != k || kv.CreateRevision <= 0 || kv.ModRevision < kv.CreateRevision || kv.ModRevision > r.Header.Revision {
			return nil, ErrCorruptRecord
		}
		values[i] = kv
	}
	if !operationPermanentKV(values[0]) || !operationPermanentKV(values[1]) || string(values[0].Value) != b.identityValue || string(values[1].Value) != b.restoreEpoch {
		return nil, ErrIdentityMismatch
	}
	return values[2:], nil
}

func (b *Backend) operationResolvedEvidence(ref OperationReference, v []*mvccpb.KeyValue) (OperationOutcome, error) {
	guard, token, receipt, lock := v[0], v[1], v[2], v[3]
	var mutation OperationRecord
	if lock != nil {
		if err := decodeOperationRecord(lock, &mutation); err != nil {
			return OperationUnknown, err
		}
		if err := b.domainEpoch(mutation.Reference.RestoreEpoch); err != nil {
			return OperationUnknown, err
		}
		m := mutation.Reference
		if m.Namespace != ref.Namespace || m.SandboxID != ref.SandboxID || m.Partition != ref.Partition || m.Kind != OperationMutation {
			return OperationUnknown, ErrCorruptRecord
		}
	}
	ownLock := lock != nil && mutation.Reference.OperationID == ref.OperationID
	if guard == nil && token == nil && receipt == nil {
		if ownLock {
			return OperationUnknown, ErrCorruptRecord
		}
		return OperationExpired, nil
	}
	if guard == nil {
		return OperationUnknown, ErrCorruptRecord
	}
	var original OperationRecord
	if err := decodeOperationRecord(guard, &original); err != nil {
		return OperationUnknown, err
	}
	if original.Reference != ref {
		return OperationUnknown, ErrCorruptRecord
	}
	if token != nil {
		var record OperationRecord
		if err := decodeOperationRecord(token, &record); err != nil {
			return OperationUnknown, err
		}
		if record != original || string(token.Value) != string(guard.Value) || token.CreateRevision <= guard.CreateRevision {
			return OperationUnknown, ErrCorruptRecord
		}
	}
	if receipt == nil {
		if token != nil || ownLock {
			return OperationUnknown, ErrCorruptRecord
		}
		return OperationUnknown, nil // validated guard-only, eligible for arbitration
	}
	var r operationReceipt
	if err := decodeOperationReceipt(receipt, &r); err != nil {
		return OperationUnknown, err
	}
	if r.Reference != ref || receipt.CreateRevision <= guard.CreateRevision {
		return OperationUnknown, ErrCorruptReceipt
	}
	if r.Outcome == OperationAborted {
		if token != nil || ownLock {
			return OperationUnknown, ErrCorruptRecord
		}
		return OperationAborted, nil
	}
	if err := validateOperationCompletion(token, receipt); err != nil {
		return OperationUnknown, err
	}
	if ref.Kind == OperationMutation {
		if !ownLock || mutation != original || string(lock.Value) != string(guard.Value) || lock.CreateRevision != token.CreateRevision {
			return OperationUnknown, ErrCorruptRecord
		}
	} else if ownLock {
		return OperationUnknown, ErrCorruptRecord
	}
	return OperationCommitted, nil
}
