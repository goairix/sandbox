package redisbootstrap

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"
)

const maximumStateBytes = 64 * 1024

// NewVolumeIdentity reserves an independently random 128-bit marker for a fixed member.
// It does not persist state or authorize creating a Redis role/configuration.
func NewVolumeIdentity(c ClusterState, m Member) (VolumeIdentity, error) {
	if err := m.Validate(c); err != nil {
		return VolumeIdentity{}, err
	}
	marker := make([]byte, 16)
	if _, err := rand.Read(marker); err != nil {
		return VolumeIdentity{}, fmt.Errorf("generate PVC marker: %w", err)
	}
	return VolumeIdentity{ClusterID: c.ClusterID, MarkerID: hex.EncodeToString(marker), Member: m, InitialConfig: Reserved}, nil
}

func checkJSON(data []byte) error {
	if len(data) == 0 || len(data) > maximumStateBytes || !utf8.Valid(data) {
		return errors.New("state JSON size outside allowed bounds")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 8 {
			return errors.New("state JSON nesting exceeds limit")
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("duplicate JSON field")
				}
				seen[name] = true
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("unexpected JSON delimiter")
		}
		_, err = d.Token()
		return err
	}
	if err := walk(0); err != nil {
		return fmt.Errorf("invalid state JSON: %w", err)
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing JSON state data")
	}
	return nil
}

func exactKeys(data []byte, keys ...string) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	if len(obj) != len(keys) {
		return nil, errors.New("state object has missing or unknown fields")
	}
	for _, key := range keys {
		value, ok := obj[key]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, errors.New("state object has noncanonical or missing fields")
		}
	}
	return obj, nil
}

// ParseClusterState rejects unknown/duplicate fields, noncanonical keys, trailing
// values and any member array whose length is not exactly three.
func ParseClusterState(data []byte) (ClusterState, error) {
	var c ClusterState
	if err := checkJSON(data); err != nil {
		return c, err
	}
	fields, err := exactKeys(data, "clusterID", "members", "phase")
	if err != nil {
		return c, err
	}
	var members []string
	if err := json.Unmarshal(fields["members"], &members); err != nil {
		return c, err
	}
	if len(members) != 3 {
		return c, errors.New("state must contain exactly three members")
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, err
	}
	return c, c.Validate()
}

// ParseVolumeIdentity strictly parses and validates the retained PVC identity.
func ParseVolumeIdentity(data []byte, c ClusterState) (VolumeIdentity, error) {
	var v VolumeIdentity
	if err := checkJSON(data); err != nil {
		return v, err
	}
	fields, err := exactKeys(data, "clusterID", "markerID", "member", "initialConfig")
	if err != nil {
		return v, err
	}
	if _, err := exactKeys(fields["member"], "dns", "ordinal"); err != nil {
		return v, err
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return v, err
	}
	return v, v.Validate(c)
}

// WriteVolumeIdentity atomically CREATES a private immutable identity file.
// Existing destinations are never replaced, including a concurrent initializer's
// marker. A same-directory fsynced file is installed using exclusive hard-link
// creation, then the directory is synced. The caller must supply a trusted local
// PVC directory on a filesystem supporting hard links and directory fsync.
// This is not a role/config transaction or a Configured phase update protocol.
func WriteVolumeIdentity(ctx context.Context, path string, v VolumeIdentity, c ClusterState) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := v.Validate(c); err != nil {
		return err
	}
	if path == "" || filepath.Base(path) == "." || filepath.Base(path) == string(filepath.Separator) {
		return errors.New("explicit identity file path required")
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode PVC identity: %w", err)
	}
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, ".redis-identity-*")
	if err != nil {
		return fmt.Errorf("create private identity temporary file: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			if err := file.Close(); err != nil {
				resultErr = errors.Join(resultErr, err)
			}
		}
		if err := os.Remove(file.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write PVC identity: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync PVC identity: %w", err)
	}
	if err := file.Close(); err != nil {
		closed = true
		return fmt.Errorf("close PVC identity: %w", err)
	}
	closed = true
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Link(file.Name(), path); err != nil {
		return fmt.Errorf("exclusively install PVC identity: %w", err)
	}
	dir, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("identity installed but directory open failed: %w", err)
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return fmt.Errorf("identity installed but directory durability uncertain: %w", err)
	}
	return nil
}
