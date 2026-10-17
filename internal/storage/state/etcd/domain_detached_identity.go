package etcd

import (
	"crypto/sha256"
	"encoding/hex"
)

// NewDetachedIdentity binds a request to its operator storage authority without
// inventing a workspace location. It grants no runtime or mount capability.
func NewDetachedIdentity(storageIdentity, principal, idempotencyKey string) (WorkspaceIdentity, error) {
	if !validOpaque(storageIdentity, 128) {
		return WorkspaceIdentity{}, ErrInvalidRecord
	}
	requestHash, err := requestKeyHash(principal, idempotencyKey)
	if err != nil {
		return WorkspaceIdentity{}, err
	}
	return detachedIdentityFromRequestHash(storageIdentity, requestHash)
}

// detachedIdentityFromRequestHash preserves the original identity formula for
// historical attribution when only CreationIntent.RequestHash remains. It does
// not recreate acquisition authority or any raw principal/idempotency value.
func detachedIdentityFromRequestHash(storageIdentity, requestHash string) (WorkspaceIdentity, error) {
	if !validOpaque(storageIdentity, 128) || !validHexDigest(requestHash) {
		return WorkspaceIdentity{}, ErrInvalidRecord
	}

	storageHash := sha256.Sum256([]byte(storageIdentity))
	w := WorkspaceIdentity{kind: WorkspaceIdentityDetached, storageIdentityHash: hex.EncodeToString(storageHash[:]), requestHash: requestHash}
	digest := framedDigest("sandbox.detached-identity.v1", w.storageIdentityHash, w.requestHash)
	w.hash, w.partition = hex.EncodeToString(digest[:]), digest[0]
	return w, nil
}

// DetachedIdentity derives storage binding from this Backend's immutable
// operator identity; callers cannot supply a different storage authority.
func (b *Backend) DetachedIdentity(principal, idempotencyKey string) (WorkspaceIdentity, error) {
	if b == nil {
		return WorkspaceIdentity{}, ErrInvalidRecord
	}
	identity, err := decodeIdentity(b.identityValue, b.namespace)
	if err != nil {
		return WorkspaceIdentity{}, err
	}
	return NewDetachedIdentity(identity.StorageID, principal, idempotencyKey)
}

func (w WorkspaceIdentity) validateDetached() error {
	if w.kind != WorkspaceIdentityDetached || w.provider != "" || w.bucket != "" || w.prefix != "" || !validHexDigest(w.storageIdentityHash) || !validHexDigest(w.requestHash) {
		return ErrInvalidRecord
	}
	digest := framedDigest("sandbox.detached-identity.v1", w.storageIdentityHash, w.requestHash)
	if w.hash != hex.EncodeToString(digest[:]) || w.partition != digest[0] {
		return ErrInvalidRecord
	}
	return nil
}
