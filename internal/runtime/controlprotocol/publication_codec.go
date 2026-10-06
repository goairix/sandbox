package controlprotocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxCertificateWireBytes = 4096
	maxReadyWireBytes       = 8192
)

// Static schema validation preserves duplicate and missing-field information
// that unmarshalling into a struct would otherwise discard. Numbers remain
// json.Number throughout and are never rounded through a float.
type wireSchema struct {
	kind     byte
	fields   map[string]*wireSchema
	maxBytes int
}

var (
	stringField             = &wireSchema{kind: 's'}
	numberField             = &wireSchema{kind: 'n'}
	utcField                = &wireSchema{kind: 't'}
	runtimeSchema           = objectSchema(map[string]*wireSchema{"id": stringField, "uid": stringField, "boot_id": stringField})
	snapshotSchema          = objectSchema(map[string]*wireSchema{"version": stringField, "digest": stringField})
	claimSchema             = objectSchema(map[string]*wireSchema{"claim_id": stringField, "create_revision": numberField, "lease_id": numberField})
	certificateClaimsSchema = objectSchema(map[string]*wireSchema{
		"version": numberField, "root_key_id": stringField, "namespace": stringField, "authority_id": stringField, "target": stringField, "restore_epoch": stringField, "intent_id": stringField, "sandbox_id": stringField, "workspace_hash": stringField, "generation": numberField, "operation_id": stringField, "payload_digest": stringField, "snapshot": snapshotSchema, "expires_at": utcField, "runtime": runtimeSchema, "workspace_mode": stringField, "public_key": stringField, "not_before": utcField, "not_after": utcField,
	})
	certificateSchema = &wireSchema{kind: 'o', maxBytes: maxCertificateWireBytes, fields: map[string]*wireSchema{"claims": certificateClaimsSchema, "signature": stringField}}
	readyClaimsSchema = objectSchema(map[string]*wireSchema{"version": numberField, "certificate_digest": stringField, "claim": claimSchema, "data_gate_epoch": numberField, "gate_state": stringField, "mount_attempt": numberField, "mount_operation_id": stringField, "observed_at": utcField, "valid_until": utcField})
	readySchema       = &wireSchema{kind: 'o', maxBytes: maxReadyWireBytes, fields: map[string]*wireSchema{"certificate": certificateSchema, "claims": readyClaimsSchema, "signature": stringField}}
)

func objectSchema(fields map[string]*wireSchema) *wireSchema {
	return &wireSchema{kind: 'o', fields: fields}
}

func decodeWire(wire []byte, schema *wireSchema, out any) error {
	if len(wire) == 0 || len(wire) > schema.maxBytes || !utf8.Valid(wire) {
		return fmt.Errorf("invalid publication wire size or UTF-8")
	}
	if err := validateEscapedUnicode(wire); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(wire))
	d.UseNumber()
	if err := parseSchema(d, schema); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing publication JSON")
	}
	if err := json.Unmarshal(wire, out); err != nil {
		return fmt.Errorf("invalid typed publication JSON: %w", err)
	}
	return nil
}

func parseSchema(d *json.Decoder, schema *wireSchema) error {
	start := d.InputOffset()
	token, err := d.Token()
	if err != nil {
		return fmt.Errorf("invalid publication JSON: %w", err)
	}
	switch schema.kind {
	case 's', 't':
		s, ok := token.(string)
		if !ok {
			return fmt.Errorf("publication field must be a non-null string")
		}
		if schema.kind == 't' && !strings.HasSuffix(s, "Z") {
			return fmt.Errorf("publication time must use UTC Z")
		}
	case 'n':
		n, ok := token.(json.Number)
		if !ok {
			return fmt.Errorf("publication field must be an integer")
		}
		if _, err := strconv.ParseInt(string(n), 10, 64); err != nil {
			return fmt.Errorf("publication field must be an int64 integer")
		}
	case 'o':
		if token != json.Delim('{') {
			return fmt.Errorf("publication field must be a non-null object")
		}
		// Start at the opening brace, excluding the enclosing colon and any
		// surrounding whitespace. Internal whitespace counts toward the cap.
		start = d.InputOffset() - 1
		seen := make(map[string]bool, len(schema.fields))
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return fmt.Errorf("invalid publication object key")
			}
			child, ok := schema.fields[name]
			if !ok || seen[name] {
				return fmt.Errorf("unknown or duplicate publication field %q", name)
			}
			seen[name] = true
			if err := parseSchema(d, child); err != nil {
				return err
			}
		}
		if _, err := d.Token(); err != nil {
			return err
		}
		if len(seen) != len(schema.fields) {
			return fmt.Errorf("missing publication field")
		}
	default:
		return fmt.Errorf("invalid publication schema")
	}
	if schema.maxBytes > 0 && d.InputOffset()-start > int64(schema.maxBytes) {
		return fmt.Errorf("publication object exceeds wire limit")
	}
	return nil
}

// encoding/json replaces an unpaired escaped UTF-16 surrogate with U+FFFD.
// Reject it before decoding so invalid Unicode cannot silently normalize.
func validateEscapedUnicode(wire []byte) error {
	inString := false
	for i := 0; i < len(wire); i++ {
		if wire[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || wire[i] != '\\' {
			continue
		}
		i++
		if i >= len(wire) {
			return fmt.Errorf("unfinished JSON escape")
		}
		if wire[i] != 'u' {
			continue
		}
		if i+4 >= len(wire) {
			return fmt.Errorf("unfinished Unicode escape")
		}
		n, err := strconv.ParseUint(string(wire[i+1:i+5]), 16, 16)
		if err != nil {
			return fmt.Errorf("invalid Unicode escape")
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return fmt.Errorf("unpaired Unicode surrogate")
		}
		if n < 0xd800 || n > 0xdbff {
			continue
		}
		if i+6 >= len(wire) || wire[i+1] != '\\' || wire[i+2] != 'u' {
			return fmt.Errorf("unpaired Unicode surrogate")
		}
		low, err := strconv.ParseUint(string(wire[i+3:i+7]), 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return fmt.Errorf("unpaired Unicode surrogate")
		}
		i += 6
	}
	return nil
}

func encodeWire(value any, limit int) ([]byte, error) {
	wire, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode publication: %w", err)
	}
	if len(wire) > limit {
		return nil, fmt.Errorf("publication exceeds wire limit")
	}
	return wire, nil
}
