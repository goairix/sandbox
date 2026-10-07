package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"os"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	t "github.com/goairix/sandbox/internal/runtime/controltransport"
	"golang.org/x/sys/unix"
)

const configPath = t.ControlDirectory + "/config.json"

type configBinding struct {
	Namespace    string `json:"namespace"`
	AuthorityID  string `json:"authority_id"`
	Target       string `json:"target"`
	RestoreEpoch string `json:"restore_epoch"`
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
	if c.Version != 1 || c.UID == 0 || c.GID == 0 || c.UID > 2147483647 || c.GID > 2147483647 || c.MaxActive > 64 || c.ClockSocket != t.ClockSocket || len(c.ClockPublicKey) != ed25519.PublicKeySize {
		return c, fmt.Errorf("invalid protected config")
	}
	if _, err = p.NewManagementVerifier(c.trustBinding(), c.Roots); err != nil {
		return c, err
	}
	return c, nil
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
