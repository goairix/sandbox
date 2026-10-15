// Package launcherconfig owns the canonical public launcher configuration.
package launcherconfig

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	t "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
)

const Environment = "SANDBOX_LAUNCHER_CONFIG_V2"
const MaxBytes = 16384

type Binding struct {
	Namespace    string `json:"namespace"`
	AuthorityID  string `json:"authority_id"`
	Target       string `json:"target"`
	RestoreEpoch string `json:"restore_epoch"`
}
type Enrollment struct {
	NodeUID    string `json:"node_uid"`
	NodeBootID string `json:"node_boot_id"`
}
type Config struct {
	Version        uint32              `json:"version"`
	Binding        Binding             `json:"binding"`
	Roots          []ed25519.PublicKey `json:"roots"`
	UID            uint32              `json:"uid"`
	GID            uint32              `json:"gid"`
	NetworkAllowed bool                `json:"network_allowed"`
	ContractDigest string              `json:"contract_digest"`
	ClockSocket    string              `json:"clock_socket"`
	ClockPublicKey ed25519.PublicKey   `json:"clock_public_key"`
	ClockAudience  string              `json:"clock_audience"`
	MaxActive      uint32              `json:"max_active"`
	Enrollment     Enrollment          `json:"enrollment"`
}

func (c Config) TrustBinding() p.TrustBinding {
	return p.TrustBinding{Namespace: c.Binding.Namespace, AuthorityID: c.Binding.AuthorityID, Target: c.Binding.Target, RestoreEpoch: c.Binding.RestoreEpoch}
}

// Decode accepts precisely the original canonical v2 protected-file format.
// Its result contains public material only and is not authority evidence.
func Decode(wire []byte) (Config, error) {
	var c Config
	if len(wire) == 0 || len(wire) > MaxBytes {
		return c, fmt.Errorf("config outside size bound")
	}
	d := json.NewDecoder(bytes.NewReader(wire))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, err
	}
	canonical, err := json.Marshal(c)
	if err != nil || !bytes.Equal(canonical, wire) {
		return c, fmt.Errorf("noncanonical protected config")
	}
	if c.Version != 2 || c.UID == 0 || c.GID == 0 || c.UID > 2147483647 || c.GID > 2147483647 || c.MaxActive > 64 || c.ClockSocket != t.ClockSocket || len(c.ClockPublicKey) != ed25519.PublicKeySize {
		return c, fmt.Errorf("invalid protected config")
	}
	for _, value := range []string{c.Enrollment.NodeUID, c.Enrollment.NodeBootID} {
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil || id.String() != value {
			return c, fmt.Errorf("invalid protected node identity")
		}
	}
	digest, err := hex.DecodeString(c.ContractDigest)
	if err != nil || len(digest) != 32 || hex.EncodeToString(digest) != c.ContractDigest || !validClockAudience(c.ClockAudience) {
		return c, fmt.Errorf("invalid protected contract or clock audience")
	}
	if _, err = p.NewManagementVerifier(c.TrustBinding(), c.Roots); err != nil {
		return c, err
	}
	return c, nil
}

func validClockAudience(s string) bool {
	if len(s) == 0 || len(s) > 128 || s == "." || s == ".." {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-')
	}) == -1
}
