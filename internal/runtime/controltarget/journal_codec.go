package controltarget

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

const maxJournalWireBytes = 8192
const maxJournalJSONDepth = 8

// The schema pass retains duplicate/missing key information before typed
// decoding. It never normalizes malformed Unicode, names or time strings.
type journalSchema struct {
	kind   byte
	fields map[string]*journalSchema
}

var (
	journalStringField    = &journalSchema{kind: 's'}
	journalNumberField    = &journalSchema{kind: 'n'}
	journalTimeField      = &journalSchema{kind: 't'}
	journalRuntimeSchema  = journalObject(map[string]*journalSchema{"id": journalStringField, "uid": journalStringField, "boot_id": journalStringField})
	journalIdentitySchema = journalObject(map[string]*journalSchema{
		"namespace": journalStringField, "authority_id": journalStringField, "target": journalStringField, "restore_epoch": journalStringField, "sandbox_id": journalStringField, "workspace_hash": journalStringField, "generation": journalNumberField, "runtime": journalRuntimeSchema,
	})
	journalGateSchema    = journalObject(map[string]*journalSchema{"version": journalNumberField, "identity": journalIdentitySchema, "data_gate_epoch": journalNumberField, "gate_state": journalStringField})
	journalContextSchema = journalObject(map[string]*journalSchema{
		"namespace": journalStringField, "authority_id": journalStringField, "target": journalStringField, "restore_epoch": journalStringField, "issuer_certificate_id": journalStringField, "issuer_certificate_digest": journalStringField, "command_id": journalStringField, "operation_id": journalStringField, "request_id": journalStringField, "operation_digest": journalStringField, "sandbox_id": journalStringField, "workspace_hash": journalStringField, "generation": journalNumberField, "data_gate_epoch": journalNumberField, "control_revision": journalNumberField, "admission_revision": journalNumberField, "lease_id": journalNumberField, "runtime": journalRuntimeSchema, "expires_at": journalTimeField,
	})
	journalRecordSchema = journalObject(map[string]*journalSchema{"version": journalNumberField, "state": journalStringField, "context": journalContextSchema, "descriptor_digest": journalStringField, "ticket_digest": journalStringField, "not_before": journalTimeField, "not_after": journalTimeField})
)

func journalObject(fields map[string]*journalSchema) *journalSchema {
	return &journalSchema{kind: 'o', fields: fields}
}

// This producer only writes closed gates. Decoding historical open gates is
// deliberately separate, so history cannot become a write/open capability.
func encodeGateManifest(m GateManifest) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if m.GateState != "closed" {
		return nil, fmt.Errorf("%w: only closed gates may be produced", ErrInvalidRecord)
	}
	return encodeJournalWire(m)
}
func decodeGateManifest(w []byte, m *GateManifest) error {
	if m == nil {
		return fmt.Errorf("%w: nil manifest destination", ErrInvalidRecord)
	}
	var candidate GateManifest
	if err := decodeJournalWire(w, journalGateSchema, &candidate); err != nil {
		return err
	}
	if err := candidate.Validate(); err != nil {
		return err
	}
	*m = candidate
	return nil
}
func encodeExecJournalRecord(r ExecJournalRecord) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if r.Version == 1 {
		return encodeJournalWire(journalRecordV1{r.Version, r.State, r.Context, r.DescriptorDigest, r.TicketDigest, r.NotBefore, r.NotAfter})
	}
	return encodeJournalWire(r)
}
func decodeExecJournalRecord(w []byte, r *ExecJournalRecord) error {
	if r == nil {
		return fmt.Errorf("%w: nil record destination", ErrInvalidRecord)
	}
	var candidate ExecJournalRecord
	// This preliminary dispatch never replaces the strict pass over ORIGINAL
	// bytes below: duplicates, trailing fields, and version ambiguity still fail.
	var version struct {
		Version uint32 `json:"version"`
	}
	if len(w) > maxJournalWireBytes || json.Unmarshal(w, &version) != nil {
		return ErrInvalidRecord
	}
	schema := journalRecordSchema
	if version.Version == 2 {
		schema = journalRecordV2Schema
	}
	if err := decodeJournalWire(w, schema, &candidate); err != nil {
		return err
	}
	if err := candidate.Validate(); err != nil {
		return err
	}
	*r = candidate
	return nil
}
func encodeJournalWire(v any) ([]byte, error) { return encodeJournalWireLimit(v, maxJournalWireBytes) }
func encodeJournalWireLimit(v any, limit int) ([]byte, error) {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return nil, fmt.Errorf("%w: encode: %w", ErrInvalidRecord, err)
	}
	wire := bytes.TrimSuffix(b.Bytes(), []byte("\n"))
	if len(wire) > limit {
		return nil, fmt.Errorf("%w: wire exceeds limit", ErrInvalidRecord)
	}
	return wire, nil
}
func decodeJournalWire(w []byte, schema *journalSchema, dst any) error {
	return decodeJournalWireLimit(w, schema, dst, maxJournalWireBytes)
}
func decodeJournalWireLimit(w []byte, schema *journalSchema, dst any, limit int) error {
	if len(w) == 0 || len(w) > limit || !utf8.Valid(w) {
		return fmt.Errorf("%w: wire size or UTF-8", ErrInvalidRecord)
	}
	if err := journalEscapedUnicode(w); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(w))
	d.UseNumber()
	if err := parseJournalSchema(d, schema, 0); err != nil {
		return fmt.Errorf("%w: schema: %w", ErrInvalidRecord, err)
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("%w: trailing JSON", ErrInvalidRecord)
	}
	if err := json.Unmarshal(w, dst); err != nil {
		return fmt.Errorf("%w: typed JSON: %w", ErrInvalidRecord, err)
	}
	return nil
}
func parseJournalSchema(d *json.Decoder, s *journalSchema, depth int) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	switch s.kind {
	case 's', 't':
		value, ok := token.(string)
		if !ok {
			return fmt.Errorf("field must be a non-null string")
		}
		if s.kind == 't' {
			parsed, err := time.Parse(time.RFC3339Nano, value)
			if err != nil || !journalUTC(parsed) || parsed.Format(time.RFC3339Nano) != value {
				return fmt.Errorf("time must be canonical nonzero UTC Z")
			}
		}
	case 'b':
		if _, ok := token.(bool); !ok {
			return fmt.Errorf("field must be boolean")
		}
	case 'n':
		value, ok := token.(json.Number)
		if !ok {
			return fmt.Errorf("field must be an integer")
		}
		if _, err := strconv.ParseInt(string(value), 10, 64); err != nil {
			return fmt.Errorf("field must be an int64 integer: %w", err)
		}
	case 'o':
		if token != json.Delim('{') {
			return fmt.Errorf("field must be a non-null object")
		}
		depth++
		if depth > maxJournalJSONDepth {
			return fmt.Errorf("JSON nesting exceeds limit")
		}
		seen := make(map[string]bool, len(s.fields))
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return fmt.Errorf("invalid object key")
			}
			child, ok := s.fields[name]
			if !ok || seen[name] {
				return fmt.Errorf("unknown or duplicate field %q", name)
			}
			seen[name] = true
			if err := parseJournalSchema(d, child, depth); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("invalid object terminator")
		}
		if len(seen) != len(s.fields) {
			return fmt.Errorf("missing field")
		}
	default:
		return fmt.Errorf("invalid journal schema")
	}
	return nil
}

// encoding/json silently replaces isolated escaped UTF-16 surrogates. Reject
// them so opaque identity and canonical comparisons preserve exact characters.
func journalEscapedUnicode(w []byte) error {
	inString := false
	for i := 0; i < len(w); i++ {
		if w[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || w[i] != '\\' {
			continue
		}
		i++
		if i >= len(w) {
			return fmt.Errorf("%w: unfinished escape", ErrInvalidRecord)
		}
		if w[i] != 'u' {
			continue
		}
		if i+4 >= len(w) {
			return fmt.Errorf("%w: unfinished Unicode escape", ErrInvalidRecord)
		}
		n, err := strconv.ParseUint(string(w[i+1:i+5]), 16, 16)
		if err != nil {
			return fmt.Errorf("%w: Unicode escape: %w", ErrInvalidRecord, err)
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return fmt.Errorf("%w: unpaired Unicode surrogate", ErrInvalidRecord)
		}
		if n < 0xd800 || n > 0xdbff {
			continue
		}
		if i+6 >= len(w) || w[i+1] != '\\' || w[i+2] != 'u' {
			return fmt.Errorf("%w: unpaired Unicode surrogate", ErrInvalidRecord)
		}
		low, err := strconv.ParseUint(string(w[i+3:i+7]), 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return fmt.Errorf("%w: unpaired Unicode surrogate", ErrInvalidRecord)
		}
		i += 6
	}
	return nil
}

// Separate version1 DTO preserves the old canonical bytes exactly.
type journalRecordV1 struct {
	Version          uint32                           `json:"version"`
	State            string                           `json:"state"`
	Context          controlprotocol.ExecStartContext `json:"context"`
	DescriptorDigest string                           `json:"descriptor_digest"`
	TicketDigest     string                           `json:"ticket_digest"`
	NotBefore        time.Time                        `json:"not_before"`
	NotAfter         time.Time                        `json:"not_after"`
}

var journalRecordV2Schema = func() *journalSchema {
	fields := make(map[string]*journalSchema)
	for k, v := range journalRecordSchema.fields {
		fields[k] = v
	}
	fields["authority_deadline"] = journalTimeField
	fields["root_pid"] = journalNumberField
	fields["root_wait_status"] = journalNumberField
	fields["drain_confirmed"] = &journalSchema{kind: 'b'}
	fields["reason"] = journalStringField
	return journalObject(fields)
}()
