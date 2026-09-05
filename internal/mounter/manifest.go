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

const ProfileManifestVersion = 1

// ProfileManifest contains exactly one compiled descriptor and the digest of
// the s3fs binary it accompanies. It is audit evidence, never configuration.
type ProfileManifest struct {
	Version    int               `json:"version"`
	Profile    ProfileDescriptor `json:"profile"`
	S3FSSHA256 string            `json:"s3fs_sha256"`
}

func validateProfileManifest(raw []byte, boundProfileID, actualS3FSSHA256 string) error {
	var manifest ProfileManifest
	if err := decodeStrictManifest(raw, &manifest); err != nil {
		return fmt.Errorf("profile manifest schema is invalid: %w", err)
	}
	if manifest.Version != ProfileManifestVersion {
		return fmt.Errorf("profile manifest version is unsupported")
	}
	if manifest.Profile.ID == "" || manifest.Profile.Provider == "" || manifest.Profile.MountParameters == "" || manifest.Profile.DurableFlush == "" || manifest.Profile.EndpointOption == "" || manifest.Profile.RegionOption == "" || manifest.Profile.AddressingStyle == "" || manifest.Profile.SignatureVersion == "" {
		return fmt.Errorf("profile manifest is missing a descriptor field")
	}
	if !manifest.Profile.TLSRequired {
		return fmt.Errorf("profile manifest cannot disable TLS")
	}
	if manifest.Profile.ID != boundProfileID {
		return fmt.Errorf("profile manifest does not match the binary profile binding")
	}
	compiled, ok := InspectCompiledProfile(boundProfileID)
	if !ok {
		return fmt.Errorf("profile manifest binding is not compiled")
	}
	if manifest.Profile != compiled.Descriptor {
		return fmt.Errorf("profile manifest descriptor does not exactly match compiled code")
	}
	if !validSHA256(manifest.S3FSSHA256) || !validSHA256(actualS3FSSHA256) || manifest.S3FSSHA256 != actualS3FSSHA256 {
		return fmt.Errorf("profile manifest s3fs SHA-256 does not match the packaged binary")
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

func decodeStrictManifest(raw []byte, out *ProfileManifest) error {
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
	return requireManifestFields(raw)
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

func requireManifestFields(raw []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return err
	}
	for _, field := range []string{"version", "profile", "s3fs_sha256"} {
		value, ok := top[field]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("missing or null field %q", field)
		}
	}
	var profile map[string]json.RawMessage
	if err := json.Unmarshal(top["profile"], &profile); err != nil || profile == nil {
		return fmt.Errorf("profile must be an object")
	}
	profileType := reflect.TypeOf(ProfileDescriptor{})
	for i := 0; i < profileType.NumField(); i++ {
		name := profileType.Field(i).Tag.Get("json")
		value, ok := profile[name]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("missing or null profile field %q", name)
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
