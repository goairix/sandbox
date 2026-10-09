package etcd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

func (r TaskWorkspaceSyncIntentReference) Validate() error {
	if r.Task.Validate() != nil || !validPreparationUUID(r.BudgetID) || !validPreparationUUID(r.IntentID) || r.Slot >= 64 || r.Revision < 0 {
		return ErrInvalidRecord
	}
	return nil
}

func (r TaskWorkspaceSyncIntentRecord) Validate() error {
	w := WorkspaceIdentity{provider: r.Provider, storageIdentityHash: r.StorageIdentityHash, bucket: r.Bucket, prefix: r.Prefix, hash: r.WorkspaceHash, partition: r.Reference.Task.Partition}
	c := r.Claim
	if r.Reference.Validate() != nil || r.Reference.Revision != 0 || r.BudgetRevision <= 0 || r.Kind != "put_object" || validateWorkspaceSyncIdentity(w) != nil || c.Task != r.Reference.Task || !validPreparationUUID(c.ClaimID) || !validDomainSegment(c.WorkerID) || c.CreateRevision <= 0 || c.LeaseID <= 0 || !validPreparationUUID(r.FinalSyncAttemptID) || !validPreparationUUID(r.CommandID) || !validHexDigest(r.TicketDigest) || !validHexDigest(r.ArchiveSHA256) || !validHexDigest(r.PlanDigest) || !validHexDigest(r.BodySHA256) || !validWorkspaceSyncPath(r.RelativePath) || r.Mode < 0 || r.Mode > 0777 || r.Size < 0 || r.Offset < 0 {
		return ErrInvalidRecord
	}
	switch r.Version {
	case 1:
		if r.Local != nil {
			return ErrInvalidRecord
		}
	case 2:
		if r.Provider != "local" || r.Local == nil || r.Local.Validate() != nil {
			return ErrInvalidRecord
		}
	default:
		return ErrInvalidRecord
	}
	return nil
}

func (r taskWorkspaceSyncSlotRecord) Validate() error {
	if r.Version != 1 || !validPreparationUUID(r.BudgetID) || !validPreparationUUID(r.IntentID) || !validHexDigest(r.IntentDigest) || r.Slot >= 64 {
		return ErrInvalidRecord
	}
	return nil
}

func workspaceSyncIntentDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (w taskWorkspaceSyncIntentWire) record() TaskWorkspaceSyncIntentRecord {
	c := w.Claim
	return TaskWorkspaceSyncIntentRecord{w.taskWorkspaceSyncIntentAttribution, TaskClaimReference{Task: c.Task, ClaimID: c.ClaimID, WorkerID: c.WorkerID, CreateRevision: c.CreateRevision, LeaseID: c.LeaseID}}
}

func (w taskWorkspaceSyncIntentWire) Validate() error { return w.record().Validate() }

func encodeWorkspaceSyncIntent(r TaskWorkspaceSyncIntentRecord, limit int) (string, error) {
	c := r.Claim
	return encodeOperationMetadata(taskWorkspaceSyncIntentWire{r.taskWorkspaceSyncIntentAttribution, taskCloseClaimWire{c.Task, c.ClaimID, c.WorkerID, c.CreateRevision, c.LeaseID}}, limit)
}

func (b *Backend) workspaceSyncIntentKeys(r TaskWorkspaceSyncIntentReference) (string, string, error) {
	if r.Validate() != nil {
		return "", "", ErrInvalidRecord
	}
	if r.Task.Namespace != b.namespace.Root() || r.Task.RestoreEpoch != b.restoreEpoch {
		return "", "", ErrIdentityMismatch
	}
	i, err := b.namespace.Key("p", fmt.Sprintf("%02x", r.Task.Partition), "workspace-sync-intents", r.IntentID)
	if err != nil {
		return "", "", err
	}
	s, err := b.namespace.Key("p", fmt.Sprintf("%02x", r.Task.Partition), "workspace-sync-slots", fmt.Sprintf("%02d", r.Slot))
	return i, s, err
}

func decodeWorkspaceSyncIntent(kv *mvccpb.KeyValue, limit int) (TaskWorkspaceSyncIntentRecord, error) {
	var r TaskWorkspaceSyncIntentRecord
	var wire taskWorkspaceSyncIntentWire
	if !immutableWorkspaceSyncKV(kv) || len(kv.Value) > limit || strictWorkspaceSyncIntentMetadata(kv.Value, reflect.TypeOf(wire)) != nil || decodeTypedDomain(kv, &wire, limit) != nil {
		return r, ErrCorruptRecord
	}
	r = wire.record()
	value, err := encodeWorkspaceSyncIntent(r, limit)
	if err != nil || value != string(kv.Value) {
		return r, ErrCorruptRecord
	}
	return r, nil
}

// Keep the original v1 bytes/required fields while permitting only its absent
// local field. Nested v2 diagnostics remain exact, complete and duplicate-free.
func strictWorkspaceSyncIntentMetadata(wire []byte, typ reflect.Type) error {
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if bytes.Equal(bytes.TrimSpace(wire), []byte("null")) {
		return ErrCorruptRecord
	}
	if typ.Kind() != reflect.Struct {
		return nil
	}
	fields := make(map[string]reflect.StructField, typ.NumField())
	var add func(reflect.Type)
	add = func(t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.Anonymous {
				add(f.Type)
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "" {
				name = f.Name
			}
			fields[name] = f
		}
	}
	add(typ)
	dec := json.NewDecoder(bytes.NewReader(wire))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return ErrCorruptRecord
	}
	seen := make(map[string]bool, len(fields))
	for dec.More() {
		tok, err = dec.Token()
		if err != nil {
			return ErrCorruptRecord
		}
		name, ok := tok.(string)
		f, exists := fields[name]
		if !ok || !exists || seen[name] {
			return ErrCorruptRecord
		}
		seen[name] = true
		var raw json.RawMessage
		if dec.Decode(&raw) != nil || strictWorkspaceSyncIntentMetadata(raw, f.Type) != nil {
			return ErrCorruptRecord
		}
	}
	if _, err := dec.Token(); err != nil {
		return ErrCorruptRecord
	}
	for name, f := range fields {
		if !seen[name] && f.Tag.Get("json") != "local,omitempty" {
			return ErrCorruptRecord
		}
	}
	if dec.Decode(new(any)) != io.EOF {
		return ErrCorruptRecord
	}
	return nil
}

func decodeWorkspaceSyncSlot(kv *mvccpb.KeyValue) (taskWorkspaceSyncSlotRecord, error) {
	var r taskWorkspaceSyncSlotRecord
	if !immutableWorkspaceSyncKV(kv) || decodePreparationDomain(kv, &r, 512) != nil {
		return r, ErrCorruptRecord
	}
	value, err := encodeOperationMetadata(r, 512)
	if err != nil || value != string(kv.Value) {
		return r, ErrCorruptRecord
	}
	return r, nil
}
