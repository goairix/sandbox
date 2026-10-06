package etcd

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"github.com/goairix/sandbox/internal/storage"
	"strings"
	"unicode"
	"unicode/utf8"
)

// WorkspaceIdentity is immutable and does not retain the raw storage identity.
type WorkspaceIdentity struct {
	provider, storageIdentityHash, bucket, prefix, hash string
	partition                                           uint8
}

// NewWorkspaceIdentity binds a canonical workspace prefix to its storage authority.
func NewWorkspaceIdentity(provider, storageIdentity, bucket, prefix string) (WorkspaceIdentity, error) {
	if !validOpaque(provider, 128) || !validOpaque(storageIdentity, 128) || !validOpaque(bucket, 256) || !validWorkspacePrefix(prefix) {
		return WorkspaceIdentity{}, fmt.Errorf("%w: invalid workspace identity", ErrInvalidRecord)
	}
	storageHash := sha256.Sum256([]byte(storageIdentity))
	w := WorkspaceIdentity{provider: provider, storageIdentityHash: hex.EncodeToString(storageHash[:]), bucket: bucket, prefix: prefix}
	digest := framedDigest(w.provider, w.storageIdentityHash, w.bucket, w.prefix)
	w.hash = hex.EncodeToString(digest[:])
	w.partition = digest[0]
	return w, nil
}
func (w WorkspaceIdentity) Hash() string     { return w.hash }
func (w WorkspaceIdentity) Partition() uint8 { return w.partition }
func (w WorkspaceIdentity) validate() error {
	if !validOpaque(w.provider, 128) || !validHexDigest(w.storageIdentityHash) || !validOpaque(w.bucket, 256) || !validWorkspacePrefix(w.prefix) {
		return fmt.Errorf("%w: invalid workspace identity", ErrInvalidRecord)
	}
	digest := framedDigest(w.provider, w.storageIdentityHash, w.bucket, w.prefix)
	if w.hash != hex.EncodeToString(digest[:]) || w.partition != digest[0] {
		return fmt.Errorf("%w: inconsistent workspace identity", ErrInvalidRecord)
	}
	return nil
}
func validWorkspacePrefix(prefix string) bool {
	if len(prefix) > 1024 {
		return false
	}
	canonical, err := storage.BuildWorkspacePrefix("", strings.TrimSuffix(prefix, "/"))
	return err == nil && canonical == prefix
}
func validOpaque(s string, max int) bool {
	if s == "" || len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func validDomainSegment(s string) bool { return len(s) <= 128 && validSegment(s) }
func validHexDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := range s {
		if !(s[i] >= '0' && s[i] <= '9' || s[i] >= 'a' && s[i] <= 'f') {
			return false
		}
	}
	return true
}
func framedDigest(parts ...string) [sha256.Size]byte {
	h := sha256.New()
	var size [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(part))
	}
	var digest [sha256.Size]byte
	copy(digest[:], h.Sum(nil))
	return digest
}

// requestKeyHash hides a principal's raw idempotency key and separates principals.
func requestKeyHash(principal, idempotencyKey string) (string, error) {
	if !validOpaque(principal, 128) || !validOpaque(idempotencyKey, 256) {
		return "", fmt.Errorf("%w: invalid idempotency identity", ErrInvalidRecord)
	}
	digest := framedDigest(principal, idempotencyKey)
	return hex.EncodeToString(digest[:]), nil
}
