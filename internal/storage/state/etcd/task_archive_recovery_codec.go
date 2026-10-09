package etcd

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

func (s taskArchiveRecoverySeed) validate() error {
	if s.Task.Validate() != nil || s.TaskBirth <= 0 || s.LeaseID <= 0 || s.ClaimRevision <= 0 || s.FinalSyncRevision <= 0 || s.ArchiveRevision <= 0 || s.IssuerRevision <= 0 || !validPreparationUUID(s.ClaimID) || !validDomainSegment(s.WorkerID) || !validPreparationUUID(s.FinalSyncAttempt) || !validPreparationUUID(s.ArchiveCommand) {
		return errTaskArchive
	}
	for _, d := range []string{s.TaskDigest, s.ClaimDigest, s.FinalSyncDigest, s.ArchiveDigest, s.IssuerDigest, s.TicketDigest} {
		if !validHexDigest(d) {
			return errTaskArchive
		}
	}
	return nil
}

func recoverySafeString(s string) bool {
	if !utf8.ValidString(s) || strings.Contains(s, "\\") {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func recoveryPlanWire(p TaskWorkspaceSyncPlan) taskArchiveRecoveryPlanWire {
	w := p.workspace
	// Non-nil empty exclusion arrays have one canonical encoding, []. Existing
	// immutable strings are shared; no second copy of their content is retained.
	exclude := p.exclude
	if exclude == nil {
		exclude = []string{}
	}
	return taskArchiveRecoveryPlanWire{w.provider, w.storageIdentityHash, w.bucket, w.prefix, w.hash, w.partition, exclude, p.deleteExtraneous, p.digest}
}

func (p taskArchiveRecoveryPlanWire) plan() (TaskWorkspaceSyncPlan, error) {
	w := WorkspaceIdentity{p.Provider, p.StorageIdentityHash, p.Bucket, p.Prefix, p.Hash, p.Partition}
	plan := TaskWorkspaceSyncPlan{w, p.Exclude, p.DeleteExtraneous, p.Digest}
	if p.Exclude == nil || plan.validate() != nil || !recoverySafeString(p.Provider) || !recoverySafeString(p.Bucket) || !recoverySafeString(p.Prefix) {
		return TaskWorkspaceSyncPlan{}, errTaskArchive
	}
	return plan, nil
}

func (m taskArchiveRecoveryManifest) validate() error {
	if m.Version != 1 || m.Slot >= taskArchiveRecoverySlots || !validHexDigest(m.StoreID) || !validHexDigest(m.LimitsDigest) || m.Seed.validate() != nil || m.Root.Dev == 0 || m.Root.Ino == 0 || m.Root.MountID == 0 || m.Root.MountNamespace == 0 || !validPreparationUUID(m.Root.BootID) {
		return errTaskArchive
	}
	if m.Artifact != nil && (m.Artifact.Dev == 0 || m.Artifact.Ino == 0) {
		return errTaskArchive
	}
	if m.Received != nil && (m.Artifact == nil || m.Received.Bytes < 1536 || m.Received.Bytes > 1<<40 || m.Received.Bytes%512 != 0 || m.Received.Entries == 0 || m.Received.Entries > 1000000 || !validHexDigest(m.Received.SHA256)) {
		return errTaskArchive
	}
	switch m.State {
	case "pending":
		if m.Received != nil || m.Binding != nil {
			return errTaskArchive
		}
	case "received":
		if m.Received == nil || m.Binding != nil {
			return errTaskArchive
		}
	case "bound":
		if m.Received == nil || m.Binding == nil || m.Binding.Local.Validate() != nil || !validHexDigest(m.Binding.TargetDigest) {
			return errTaskArchive
		}
		if _, err := m.Binding.Plan.plan(); err != nil {
			return err
		}
		// This is the exact whole-local-pin encoding used by checkpointPayload.
		local, err := encodeOperationMetadata(m.Binding.Local, 16384)
		if err != nil || workspaceSyncIntentDigest(local) != m.Binding.TargetDigest {
			return errTaskArchive
		}
	default:
		return errTaskArchive
	}
	return nil
}

type taskArchiveRecoveryWriter struct {
	buffer []byte
	n      int
}

func (w *taskArchiveRecoveryWriter) Write(p []byte) (int, error) {
	if len(p) > len(w.buffer)-w.n {
		return 0, errTaskArchive
	}
	copy(w.buffer[w.n:], p)
	w.n += len(p)
	return len(p), nil
}

// The encoder traverses only this fixed schema, writing into admitted storage.
// It never builds a second full wire allocation or delegates unbounded Marshal.
func encodeTaskArchiveRecovery(m taskArchiveRecoveryManifest, out []byte) ([]byte, error) {
	if len(out) != taskArchiveRecoveryMetadataBytes || m.validate() != nil {
		return nil, errTaskArchive
	}
	w := taskArchiveRecoveryWriter{buffer: out}
	if err := w.value(reflect.ValueOf(m)); err != nil {
		return nil, err
	}
	if _, err := w.Write([]byte{'\n'}); err != nil || w.n > taskArchiveRecoveryWireBound {
		return nil, errTaskArchive
	}
	return out[:w.n], nil
}

func (w *taskArchiveRecoveryWriter) text(s string) error {
	_, err := w.Write([]byte(s))
	return err
}

func (w *taskArchiveRecoveryWriter) quoted(s string) error {
	if err := w.text("\""); err != nil {
		return err
	}
	start := 0
	for i, r := range s {
		var escape string
		switch r {
		case '"':
			escape = "\\\""
		case '\\':
			escape = "\\\\"
		case '\u2028':
			escape = "\\u2028"
		case '\u2029':
			escape = "\\u2029"
		default:
			if unicode.IsControl(r) {
				return errTaskArchive
			}
		}
		if escape != "" {
			if err := w.text(s[start:i]); err != nil {
				return err
			}
			if err := w.text(escape); err != nil {
				return err
			}
			start = i + utf8.RuneLen(r)
		}
	}
	if err := w.text(s[start:]); err != nil {
		return err
	}
	return w.text("\"")
}

func (w *taskArchiveRecoveryWriter) value(v reflect.Value) error {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return errTaskArchive
		}
		return w.value(v.Elem())
	}
	switch v.Kind() {
	case reflect.Struct:
		if err := w.text("{"); err != nil {
			return err
		}
		first := true
		for i := 0; i < v.NumField(); i++ {
			field := v.Type().Field(i)
			name, omit, _ := strings.Cut(field.Tag.Get("json"), ",")
			if omit == "omitempty" && v.Field(i).IsNil() {
				continue
			}
			if name == "" {
				name = field.Name
			}
			if !first {
				if err := w.text(","); err != nil {
					return err
				}
			}
			first = false
			if err := w.quoted(name); err != nil {
				return err
			}
			if err := w.text(":"); err != nil {
				return err
			}
			if err := w.value(v.Field(i)); err != nil {
				return err
			}
		}
		return w.text("}")
	case reflect.Slice:
		if v.Type().Elem().Kind() != reflect.String || v.Len() > workspaceSyncMaxExclusions {
			return errTaskArchive
		}
		if err := w.text("["); err != nil {
			return err
		}
		for i := 0; i < v.Len(); i++ {
			if i > 0 {
				if err := w.text(","); err != nil {
					return err
				}
			}
			if err := w.quoted(v.Index(i).String()); err != nil {
				return err
			}
		}
		return w.text("]")
	case reflect.String:
		return w.quoted(v.String())
	case reflect.Bool:
		return w.text(strconv.FormatBool(v.Bool()))
	case reflect.Uint8, reflect.Uint32, reflect.Uint64:
		return w.text(strconv.FormatUint(v.Uint(), 10))
	case reflect.Int, reflect.Int64:
		return w.text(strconv.FormatInt(v.Int(), 10))
	}
	return errTaskArchive
}

// A nonallocating lexical preflight bounds depth and the sole variable array
// before typed decoding can allocate. Typed decoding and byte-exact re-encoding
// then reject malformed, duplicate, missing, unknown, reordered and null fields.
func preflightTaskArchiveRecovery(in []byte) error {
	if len(in) == 0 || len(in) > taskArchiveRecoveryWireBound || !utf8.Valid(in) {
		return errTaskArchive
	}
	var stack [8]byte
	depth, elements := 0, 0
	for i := 0; i < len(in); i++ {
		switch in[i] {
		case '"':
			closed := false
			for i++; i < len(in); i++ {
				if in[i] == '\\' {
					i++
					continue
				}
				if in[i] == '"' {
					closed = true
					break
				}
			}
			if !closed {
				return errTaskArchive
			}
		case '{', '[':
			if depth == len(stack) {
				return errTaskArchive
			}
			if in[i] == '[' {
				// No nested or second arrays are part of this schema.
				if elements != 0 {
					return errTaskArchive
				}
				elements = 1
			}
			stack[depth] = in[i]
			depth++
		case '}', ']':
			if depth == 0 || in[i] == '}' && stack[depth-1] != '{' || in[i] == ']' && stack[depth-1] != '[' {
				return errTaskArchive
			}
			depth--
		case ',':
			if depth > 0 && stack[depth-1] == '[' {
				if elements >= workspaceSyncMaxExclusions {
					return errTaskArchive
				}
				elements++
			}
		case 'n':
			return errTaskArchive // JSON null is never legal, including optional fields.
		}
	}
	if depth != 0 {
		return errTaskArchive
	}
	return nil
}

func decodeTaskArchiveRecovery(in, out []byte) (taskArchiveRecoveryManifest, error) {
	var m taskArchiveRecoveryManifest
	if preflightTaskArchiveRecovery(in) != nil {
		return m, errTaskArchive
	}
	// Unmarshal uses the caller's input directly. A Decoder would grow its own
	// full input buffer in addition to the admitted input/output arrays.
	if json.Unmarshal(in, &m) != nil {
		return taskArchiveRecoveryManifest{}, errTaskArchive
	}
	canonical, err := encodeTaskArchiveRecovery(m, out)
	if err != nil || !bytes.Equal(in, canonical) {
		return taskArchiveRecoveryManifest{}, errTaskArchive
	}
	return m, nil
}
