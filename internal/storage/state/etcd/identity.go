package etcd

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const (
	maxIdentityFieldBytes = 128
	maxIdentityJSONBytes  = 4096
)

// Identity is the operator-provisioned authority binding. RestoreEpoch is stored
// separately so an operator can fence state after restoring a snapshot.
// StorageID, RuntimeID, and RestoreEpoch are limited to 128 bytes each. The
// stored identity JSON is limited to 4096 bytes, including whitespace.
// RestoreEpoch is a non-reusable identifier containing only ASCII letters,
// digits, '.', '_', and '-'; empty strings and the identifiers '.' and '..'
// are invalid. Uniqueness across restores is the operator's responsibility.
type Identity struct {
	SchemaVersion uint32 `json:"schema_version"`
	Prefix        string `json:"prefix"`
	AuthorityID   string `json:"authority_id"`
	Cell          string `json:"cell"`
	ClusterID     uint64 `json:"cluster_id"`
	StorageID     string `json:"storage_id"`
	RuntimeID     string `json:"runtime_id"`
	RestoreEpoch  string `json:"-"`
}

func (i Identity) valid(n Namespace) bool {
	return i.SchemaVersion == 1 && i.Prefix == n.prefix && i.AuthorityID == n.scope && i.Cell == n.cell && i.ClusterID != 0 && strings.TrimSpace(i.StorageID) != "" && len(i.StorageID) <= maxIdentityFieldBytes && strings.TrimSpace(i.RuntimeID) != "" && len(i.RuntimeID) <= maxIdentityFieldBytes
}

func decodeIdentity(value string, n Namespace) (Identity, error) {
	if len(value) > maxIdentityJSONBytes {
		return Identity{}, fmt.Errorf("%w: identity JSON exceeds 4096 bytes", ErrIdentityMismatch)
	}
	var identity Identity
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&identity); err != nil {
		return Identity{}, fmt.Errorf("%w: invalid identity JSON: %v", ErrIdentityMismatch, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Identity{}, fmt.Errorf("%w: trailing identity JSON", ErrIdentityMismatch)
	}
	if !identity.valid(n) {
		return Identity{}, fmt.Errorf("%w: invalid identity fields", ErrIdentityMismatch)
	}
	return identity, nil
}
