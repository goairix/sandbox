package sandboxsnapshot

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"unicode/utf8"

	"github.com/goairix/sandbox/pkg/types"
)

func requestedDigest(request types.CreateSandboxRequest) string {
	// The caller has already bounded all strings and slice elements. This DTO
	// has no custom encoders or unsupported JSON field types.
	wire, _ := json.Marshal(request)
	digest := sha256.Sum256(wire)
	return hex.EncodeToString(digest[:])
}

// Encode validates caller-owned objects and emits the canonical snapshot JSON.
func Encode(snapshot *Snapshot) ([]byte, error) {
	if snapshot == nil || snapshot.Version != Version || len(snapshot.RequestedDigest) != sha256.Size*2 || snapshot.Resolved.TTL.Kind != "finite" && snapshot.Resolved.TTL.Kind != "permanent" {
		return nil, errInvalid
	}
	if err := bounded(snapshot.Requested, snapshot.Resolved); err != nil {
		return nil, err
	}
	if !normalized(snapshot.Requested) {
		return nil, errInvalid
	}
	if err := validateFields(snapshot.Requested, snapshot.Resolved); err != nil {
		return nil, err
	}
	if snapshot.RequestedDigest != requestedDigest(snapshot.Requested) {
		return nil, errInvalid
	}
	wire, err := json.Marshal(snapshot)
	if err != nil || len(wire) > MaxBytes {
		return nil, errInvalid
	}
	return wire, nil
}

// Decode accepts only canonical UTF-8 JSON in the fixed schema and returns
// independent configuration. Decoded data provides no runtime capability.
func Decode(wire []byte) (*Snapshot, error) {
	if len(wire) > MaxBytes || !utf8.Valid(wire) {
		return nil, errInvalid
	}
	var snapshot Snapshot
	decoder := json.NewDecoder(bytes.NewReader(wire))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, errInvalid
	}
	canonical, err := Encode(&snapshot)
	if err != nil || !bytes.Equal(canonical, wire) {
		return nil, errInvalid
	}
	return &snapshot, nil
}
