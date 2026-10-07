package etcd

import (
	"context"
	"encoding/json"
	"fmt"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// registerTaskQuiescenceIssuer is a fixed-purpose writing composition. Its
// caller holds the original claim mutex and has pinned a copied certificate.
// Generic issuer registration callers retain their existing contract.
func (b *Backend) registerTaskQuiescenceIssuer(ctx context.Context, c *TaskClaim, d *taskQuiescenceDraft) (*CommandIssuerEntry, error) {
	if err := taskQuiescenceLive(ctx, c, d); err != nil {
		return nil, err
	}
	now, err := b.observePublicationClock(ctx)
	if live := taskQuiescenceLive(ctx, c, d); live != nil {
		return nil, live
	}
	if err != nil {
		return nil, err
	}
	identity, err := b.taskQuiescenceVerifier.VerifyCommandIssuerCertificate(d.certificate, now)
	if err != nil {
		return nil, fmt.Errorf("%w: task quiescence issuer certificate: %v", ErrInvalidRecord, err)
	}
	record := CommandIssuerRecord{Version: 1, Namespace: b.namespace.Root(), RestoreEpoch: b.restoreEpoch, CertificateID: identity.CertificateID(), CertificateDigest: identity.Digest(), Certificate: identity.Wire()}
	value, err := encodeCommandIssuerRecord(record)
	if err != nil {
		return nil, err
	}
	key, err := b.namespace.commandIssuerKey(record.CertificateID)
	if err != nil {
		return nil, err
	}
	cmps := b.reservationComparisons(c, d)
	cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	// Fixed reserved issuer metadata is not a Stage business write. Account
	// its full request directly without widening prepareMutation's reserved keys.
	reads := []string{b.identityKey, b.restoreKey, key}
	if len(cmps)+1+len(reads) > maxStageOperations || len(cmps)-1+2*len(reads) > maxStageOperations || len(value) > maxRecordBytes {
		return nil, ErrInvalidMutation
	}
	for _, k := range reads {
		if !validNamespaceKey(b.namespace, k) {
			return nil, ErrInvalidMutation
		}
	}
	for _, cmp := range cmps {
		if !validNamespaceKey(b.namespace, string(cmp.Key)) || len(cmp.RangeEnd) != 0 || !validComparison(cmp) {
			return nil, ErrInvalidMutation
		}
	}
	accounted, err := json.Marshal(struct {
		Comparisons []clientv3.Cmp
		Writes      []Write
		Reads       []string
	}{cmps, []Write{{Key: key, Value: []byte(value)}}, append(append([]string{}, reads...), reads...)})
	if err != nil || len(accounted)+16*maxKeyBytes > maxMutationBytes {
		return nil, ErrInvalidMutation
	}
	if err = taskQuiescenceLive(ctx, c, d); err != nil {
		return nil, err
	}
	response, err := b.client.Txn(ctx).If(cmps...).Then(clientv3.OpPut(key, value)).Else(b.commandIssuerPoints(key)...).Commit()
	if live := taskQuiescenceLive(ctx, c, d); live != nil {
		return nil, live
	}
	if err != nil {
		return nil, fmt.Errorf("%w: register task quiescence issuer: %w", ErrOutcomeUnknown, err)
	}
	if err = b.operationResponseHeader(response); err != nil {
		return nil, err
	}
	if response.Succeeded {
		if !operationPutResponses(response, 1) {
			return nil, ErrOutcomeUnknown
		}
	} else {
		kv, e := b.commandIssuerResponse(response, key)
		if e != nil {
			return nil, e
		}
		if _, e = b.decodeCommandIssuer(kv); e != nil {
			return nil, e
		}
	}
	// Even a successful write cannot lend its former claim to this later read.
	// An existing identical issuer also requires current original claim fences.
	if err = taskQuiescenceLive(ctx, c, d); err != nil {
		return nil, err
	}
	points := b.commandIssuerPoints(key)
	response, err = b.client.Txn(ctx).If(b.reservationComparisons(c, d)...).Then(points...).Else(points...).Commit()
	if live := taskQuiescenceLive(ctx, c, d); live != nil {
		return nil, live
	}
	if err != nil {
		return nil, fmt.Errorf("%w: read task quiescence issuer: %w", ErrOutcomeUnknown, err)
	}
	kv, err := b.commandIssuerResponse(response, key)
	if err != nil {
		return nil, err
	}
	if !response.Succeeded {
		c.lost = true
		return nil, ErrConflict
	}
	entry, err := b.decodeCommandIssuer(kv)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, ErrOutcomeUnknown
	}
	if string(kv.Value) != value {
		return nil, ErrConflict
	}
	return entry, nil
}
