// Package redisbootstrap supplies conservative bootstrap safety primitives.
// It is not an election protocol, bootstrap coordinator or attestation transport.
package redisbootstrap

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"regexp"
)

// Phase is the namespace-level initialization gate, not election state.
type Phase string

const (
	// Pending holds business writers outside an incompletely initialized cluster.
	Pending Phase = "Pending"
	// Initialized records a successful external initialization protocol.
	Initialized Phase = "Initialized"
)

// ClusterState fixes exactly three member DNS names, indexed by ordinal.
type ClusterState struct {
	ClusterID string    `json:"clusterID"`
	Members   [3]string `json:"members"`
	Phase     Phase     `json:"phase"`
}

// Member binds an ordinal to its fixed DNS name.
type Member struct {
	DNS     string `json:"dns"`
	Ordinal int    `json:"ordinal"`
}

// ConfigState distinguishes marker reservation from complete local configuration.
type ConfigState string

const (
	// Reserved cannot authorize initial launch or recreate a role.
	Reserved ConfigState = "Reserved"
	// Configured requires the caller to have verified durable Redis/Sentinel configuration.
	Configured ConfigState = "Configured"
)

// VolumeIdentity is a local PVC identity, independent of Redis authentication.
type VolumeIdentity struct {
	ClusterID     string      `json:"clusterID"`
	MarkerID      string      `json:"markerID"`
	Member        Member      `json:"member"`
	InitialConfig ConfigState `json:"initialConfig"`
}

var clusterIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var dnsLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

func validDNS(s string) bool {
	if len(s) == 0 || len(s) > 253 || net.ParseIP(s) != nil {
		return false
	}
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '.' {
			label := s[start:i]
			if len(label) == 0 || len(label) > 63 || !dnsLabelPattern.MatchString(label) {
				return false
			}
			start = i + 1
		}
	}
	return true
}

// Validate rejects malformed phase, identity or ambiguous membership.
func (c ClusterState) Validate() error {
	if !clusterIDPattern.MatchString(c.ClusterID) {
		return errors.New("invalid clusterID")
	}
	if c.Phase != Pending && c.Phase != Initialized {
		return errors.New("invalid cluster phase")
	}
	seen := make(map[string]bool, 3)
	for _, dns := range c.Members {
		if !validDNS(dns) || seen[dns] {
			return errors.New("members must be three unique DNS names")
		}
		seen[dns] = true
	}
	return nil
}

// Validate checks both ordinal and DNS against fixed membership.
func (m Member) Validate(c ClusterState) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if m.Ordinal < 0 || m.Ordinal >= len(c.Members) || c.Members[m.Ordinal] != m.DNS {
		return errors.New("member identity does not match fixed membership")
	}
	return nil
}

// Validate checks the PVC cluster, marker and configuration-state binding.
func (v VolumeIdentity) Validate(c ClusterState) error {
	if err := v.Member.Validate(c); err != nil {
		return err
	}
	if v.ClusterID != c.ClusterID {
		return errors.New("PVC clusterID mismatch")
	}
	marker, err := hex.DecodeString(v.MarkerID)
	if err != nil || len(marker) != 16 || hex.EncodeToString(marker) != v.MarkerID {
		return errors.New("markerID must be 128-bit lowercase hex")
	}
	if v.InitialConfig != Reserved && v.InitialConfig != Configured {
		return fmt.Errorf("invalid initial configuration state %q", v.InitialConfig)
	}
	return nil
}

// BusinessWritersAllowed fails closed until a valid namespace gate is Initialized.
func BusinessWritersAllowed(c *ClusterState) bool {
	return c != nil && c.Validate() == nil && c.Phase == Initialized
}
