package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	t "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const configPath = t.ControlDirectory + "/config.json"

type configBinding struct {
	Namespace    string `json:"namespace"`
	AuthorityID  string `json:"authority_id"`
	Target       string `json:"target"`
	RestoreEpoch string `json:"restore_epoch"`
}
type configEnrollment struct {
	NodeUID    string `json:"node_uid"`
	NodeBootID string `json:"node_boot_id"`
}
type protectedConfig struct {
	Version        uint32              `json:"version"`
	Binding        configBinding       `json:"binding"`
	Roots          []ed25519.PublicKey `json:"roots"`
	UID            uint32              `json:"uid"`
	GID            uint32              `json:"gid"`
	NetworkAllowed bool                `json:"network_allowed"`
	ContractDigest string              `json:"contract_digest"`
	ClockSocket    string              `json:"clock_socket"`
	ClockPublicKey ed25519.PublicKey   `json:"clock_public_key"`
	ClockAudience  string              `json:"clock_audience"`
	MaxActive      uint32              `json:"max_active"`
	Enrollment     configEnrollment    `json:"enrollment"`
}

func (c protectedConfig) trustBinding() p.TrustBinding {
	return p.TrustBinding{Namespace: c.Binding.Namespace, AuthorityID: c.Binding.AuthorityID, Target: c.Binding.Target, RestoreEpoch: c.Binding.RestoreEpoch}
}
func decodeProtectedConfig(wire []byte) (protectedConfig, error) {
	var c protectedConfig
	if len(wire) == 0 || len(wire) > 16384 {
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
	if _, err = p.NewManagementVerifier(c.trustBinding(), c.Roots); err != nil {
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
func readProtectedConfig(path string) (protectedConfig, error) {
	var c protectedConfig
	if err := t.CheckProtectedPath(path, 0600); err != nil {
		return c, err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return c, err
	}
	f := os.NewFile(uintptr(fd), "protected-config")
	defer f.Close()
	wire, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil {
		return c, err
	}
	return decodeProtectedConfig(wire)
}
