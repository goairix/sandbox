package mounter

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"unicode/utf8"

	"github.com/goairix/sandbox/internal/fuseprotocol"
)

const ProfileBundleVersion = 1

// ProfileBundle contains the exact compiled production profile catalog and
// the digest of the s3fs binary it accompanies. It is audit evidence, never
// configuration: runtime options still come exclusively from compiled code.
type ProfileBundle struct {
	Version    int                 `json:"version"`
	Profiles   []ProfileDescriptor `json:"profiles"`
	S3FSSHA256 string              `json:"s3fs_sha256"`
}

func validateProfileBundle(raw []byte, actualS3FSSHA256 string) error {
	var bundle ProfileBundle
	if err := decodeStrictBundle(raw, &bundle); err != nil {
		return fmt.Errorf("profile bundle schema is invalid: %w", err)
	}
	if bundle.Version != ProfileBundleVersion {
		return fmt.Errorf("profile bundle version is unsupported")
	}
	if len(bundle.Profiles) != len(bundledProfileIDs) {
		return fmt.Errorf("profile bundle does not contain the exact compiled catalog")
	}
	seen := make(map[string]struct{}, len(bundle.Profiles))
	for _, descriptor := range bundle.Profiles {
		if descriptor.ID == "" || descriptor.Provider == "" || descriptor.MountParameters == "" || descriptor.DurableFlush == "" || descriptor.EndpointOption == "" || descriptor.RegionOption == "" || descriptor.AddressingStyle == "" || descriptor.SignatureVersion == "" {
			return fmt.Errorf("profile bundle is missing a descriptor field")
		}
		if !descriptor.TLSRequired && descriptor.ID != "minio-sigv4-path-style-private-http-v1" {
			return fmt.Errorf("profile bundle cannot disable TLS")
		}
		if _, duplicate := seen[descriptor.ID]; duplicate {
			return fmt.Errorf("profile bundle contains duplicate profile ID")
		}
		seen[descriptor.ID] = struct{}{}
		compiled, ok := InspectCompiledProfile(descriptor.ID)
		if !ok {
			return fmt.Errorf("profile bundle contains a profile that is not compiled")
		}
		if descriptor != compiled.Descriptor {
			return fmt.Errorf("profile bundle descriptor does not exactly match compiled code")
		}
	}
	for _, id := range bundledProfileIDs {
		if _, ok := seen[id]; !ok {
			return fmt.Errorf("profile bundle does not contain the exact compiled catalog")
		}
	}
	if !validSHA256(bundle.S3FSSHA256) || !validSHA256(actualS3FSSHA256) || bundle.S3FSSHA256 != actualS3FSSHA256 {
		return fmt.Errorf("profile bundle s3fs SHA-256 does not match the packaged binary")
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

func decodeStrictBundle(raw []byte, out *ProfileBundle) error {
	if len(raw) == 0 || len(raw) > fuseprotocol.MaxJSONBytes || !utf8.Valid(raw) {
		return fmt.Errorf("invalid size or encoding")
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) || token != nil {
		return fmt.Errorf("trailing data")
	}
	return requireBundleFields(raw)
}

func rejectDuplicateJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var visit func() error
	visit = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return fmt.Errorf("object key is invalid")
				}
				if _, duplicate := seen[key]; duplicate {
					return fmt.Errorf("duplicate field %q", key)
				}
				seen[key] = struct{}{}
				if err := visit(); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return fmt.Errorf("malformed object")
			}
		case '[':
			for decoder.More() {
				if err := visit(); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return fmt.Errorf("malformed array")
			}
		default:
			return fmt.Errorf("unexpected delimiter")
		}
		return nil
	}
	if err := visit(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("trailing data")
	}
	return nil
}

func requireBundleFields(raw []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return err
	}
	for _, field := range []string{"version", "profiles", "s3fs_sha256"} {
		value, ok := top[field]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("missing or null field %q", field)
		}
	}
	var profiles []map[string]json.RawMessage
	if err := json.Unmarshal(top["profiles"], &profiles); err != nil || profiles == nil || len(profiles) == 0 {
		return fmt.Errorf("profiles must be a non-empty array")
	}
	profileType := reflect.TypeOf(ProfileDescriptor{})
	for _, profile := range profiles {
		if profile == nil {
			return fmt.Errorf("profile must be an object")
		}
		for i := 0; i < profileType.NumField(); i++ {
			name := profileType.Field(i).Tag.Get("json")
			value, ok := profile[name]
			if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return fmt.Errorf("missing or null profile field %q", name)
			}
		}
	}
	return nil
}

func sha256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
