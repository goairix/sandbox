package etcd

import (
	"context"
	"fmt"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// RegisterExecIssuer authenticates the configured issuer freshly and records its
// canonical certificate permanently. It grants no command or runtime authority.
// An unknown RPC outcome can be retried with the same certificate; it does not
// establish failure and must not cause the caller to replace its certificate ID.
func (b *Backend) RegisterExecIssuer(ctx context.Context) (*CommandIssuerEntry, error) {
	if ctx == nil {
		return nil, ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b.execIssuer == nil || b.execVerifier == nil || b.authorityClock == nil || b.publicationVerifier == nil {
		return nil, ErrInvalidConfiguration
	}
	bounded, cancel := b.requestContext(ctx)
	defer cancel()
	certificate, err := b.execIssuer.Certificate(bounded)
	certificate = append([]byte(nil), certificate...)
	if err != nil {
		return nil, fmt.Errorf("etcd state: issuer certificate: %w", err)
	}
	if err := bounded.Err(); err != nil {
		return nil, err
	}
	return b.registerCommandIssuerCertificate(bounded, certificate, b.execVerifier)
}

// registerCommandIssuerCertificate shares the immutable registration protocol,
// not provider selection. Callers own the certificate and bounded context.
func (b *Backend) registerCommandIssuerCertificate(bounded context.Context, certificate []byte, verifier *controlprotocol.ManagementVerifier) (*CommandIssuerEntry, error) {
	now, err := b.observePublicationClock(bounded)
	if err != nil {
		return nil, err
	}
	identity, err := verifier.VerifyCommandIssuerCertificate(certificate, now)
	if err != nil {
		return nil, fmt.Errorf("%w: command issuer certificate: %v", ErrInvalidRecord, err)
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
	if err := bounded.Err(); err != nil {
		return nil, err
	}
	comparisons := append(b.baseComparisons(), clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	response, err := b.client.Txn(bounded).If(comparisons...).Then(clientv3.OpPut(key, value)).Else(b.commandIssuerPoints(key)...).Commit()
	if err != nil {
		return nil, fmt.Errorf("%w: register command issuer: %w", ErrOutcomeUnknown, err)
	}
	if err := b.operationResponseHeader(response); err != nil {
		return nil, err
	}
	if response.Succeeded {
		if !operationPutResponses(response, 1) {
			return nil, ErrOutcomeUnknown
		}
	} else {
		kv, err := b.commandIssuerResponse(response, key)
		if err != nil {
			return nil, err
		}
		// A corrupt immutable body is never a normal UUID collision, even if a
		// subsequent read could observe a repaired or replaced record.
		if _, err := b.decodeCommandIssuer(kv); err != nil {
			return nil, err
		}
	}
	kv, err := b.readCommandIssuer(bounded, key)
	if err != nil {
		return nil, err
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

// LoadExecIssuer reads copied structural history at one linearizable point.
// Expired certificates remain readable without a provider or trusted clock.
// Absence is only a snapshot, never proof that a delayed registration failed.
// This entry is not a capability: future effects require fresh authentication
// and exact value/original ModRevision/Lease=0 fences. Without an original
// revision this loader cannot detect coherent deletion and recreation.
func (b *Backend) LoadExecIssuer(ctx context.Context, certificateID string) (*CommandIssuerEntry, error) {
	if ctx == nil {
		return nil, ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key, err := b.namespace.commandIssuerKey(certificateID)
	if err != nil {
		return nil, err
	}
	kv, err := b.readCommandIssuer(ctx, key)
	if err != nil {
		return nil, err
	}
	return b.decodeCommandIssuer(kv)
}

func (b *Backend) commandIssuerPoints(key string) []clientv3.Op {
	return []clientv3.Op{clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey), clientv3.OpGet(key)}
}

// Unlike readDomain, this private reader retains the full Txn header so every
// returned point and nested header can be checked against the same revision.
// The fixed metadata points also provide a nonempty linearizable read barrier
// on either branch. No namespace scan or per-sandbox work is involved.
func (b *Backend) readCommandIssuer(ctx context.Context, key string) (*mvccpb.KeyValue, error) {
	bounded, cancel := b.requestContext(ctx)
	defer cancel()
	points := b.commandIssuerPoints(key)
	response, err := b.client.Txn(bounded).If(b.baseComparisons()...).Then(points...).Else(points...).Commit()
	if err != nil {
		return nil, fmt.Errorf("%w: read command issuer: %w", ErrOutcomeUnknown, err)
	}
	kv, err := b.commandIssuerResponse(response, key)
	if err != nil {
		return nil, err
	}
	if !response.Succeeded {
		return nil, ErrIdentityMismatch
	}
	return kv, nil
}

func (b *Backend) commandIssuerResponse(response *clientv3.TxnResponse, key string) (*mvccpb.KeyValue, error) {
	if err := b.operationResponseHeader(response); err != nil {
		return nil, err
	}
	keys := []string{b.identityKey, b.restoreKey, key}
	if len(response.Responses) != len(keys) {
		return nil, ErrCorruptRecord
	}
	var record *mvccpb.KeyValue
	for i, key := range keys {
		op := response.Responses[i]
		if op == nil {
			return nil, ErrCorruptRecord
		}
		point := op.GetResponseRange()
		if point == nil || point.More || len(point.Kvs) > 1 || point.Count != int64(len(point.Kvs)) || !operationNestedHeader(point.Header, response.Header) {
			return nil, ErrCorruptRecord
		}
		if len(point.Kvs) == 0 {
			if i < 2 {
				return nil, ErrIdentityMismatch
			}
			continue
		}
		kv := point.Kvs[0]
		if kv == nil || string(kv.Key) != key || kv.CreateRevision <= 0 || kv.ModRevision < kv.CreateRevision || kv.ModRevision > response.Header.Revision {
			return nil, ErrCorruptRecord
		}
		if i == 2 {
			record = kv
		}
	}
	if err := b.validateResponseIdentity(response); err != nil {
		return nil, err
	}
	return record, nil
}

func (b *Backend) decodeCommandIssuer(kv *mvccpb.KeyValue) (*CommandIssuerEntry, error) {
	if kv == nil {
		return nil, nil
	}
	var record CommandIssuerRecord
	if err := decodeCommandIssuerRecord(kv, &record); err != nil {
		return nil, err
	}
	if record.Namespace != b.namespace.Root() {
		return nil, ErrCorruptRecord
	}
	if err := b.domainEpoch(record.RestoreEpoch); err != nil {
		return nil, err
	}
	return &CommandIssuerEntry{Record: record, Revision: kv.CreateRevision}, nil
}
