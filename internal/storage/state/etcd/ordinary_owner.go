package etcd

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

var _ state.ScopedOrdinaryPoolOwnerReader = (*Store)(nil)

func canonicalHex(raw string, length int) bool {
	if len(raw) != length {
		return false
	}
	decoded, err := hex.DecodeString(raw)
	return err == nil && hex.EncodeToString(decoded) == raw
}

// HasScopedOrdinaryPoolOwner checks the exact scope and pool prefix with a
// linearizable, one-key read. Fresh ordinary-pool fingerprints include their
// scope, so a different scope cannot own the same pool fingerprint.
func (s *Store) HasScopedOrdinaryPoolOwner(ctx context.Context, scopeDigest, poolKey string) (bool, error) {
	if !canonicalHex(scopeDigest, 16) || !canonicalHex(poolKey, 32) {
		return false, fmt.Errorf("etcd state: invalid ordinary pool owner identity")
	}
	prefix := s.prefix + "ordinarypool:v1:" + scopeDigest + ":" + poolKey + ":owner:"
	requestCtx, cancel := s.requestContext(ctx)
	defer cancel()
	response, err := s.client.Get(requestCtx, prefix, clientv3.WithPrefix(), clientv3.WithKeysOnly(), clientv3.WithLimit(1))
	if err != nil {
		return false, err
	}
	return len(response.Kvs) != 0, nil
}
