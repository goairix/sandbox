package etcd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

// The v1 row has an 8KiB canonical JSON limit. Its separate 128KiB RPC
// envelope reserves room for maximal fences and either bounded reply branch.
const dueCheckpointBytes = 8192
const dueCheckpointWireBytes = 128 * 1024

// This permanent row is discovery evidence, never an adopted worker cursor.
// ReplayFrom deliberately stays at fixed R+1 for the entire view, including
// when future entries exist only in the original process's memory.
type dueCheckpointRecord struct {
	Version                 uint32    `json:"version"`
	Namespace               string    `json:"namespace"`
	RestoreEpoch            string    `json:"restore_epoch"`
	IdentityDigest          string    `json:"identity_digest"`
	ProfileID               string    `json:"profile_id"`
	ProfileDigest           string    `json:"profile_digest"`
	Partition               uint8     `json:"partition"`
	WriterSessionID         string    `json:"writer_session_id"`
	WriterPartitionRevision int64     `json:"writer_partition_revision"`
	Revision                int64     `json:"revision"`
	ReplayFrom              int64     `json:"replay_from"`
	WindowEnd               time.Time `json:"window_end"`
	Phase                   string    `json:"phase"`
	Frontier                string    `json:"frontier"`
	// Pending retains one exact pre-Grant request through every unknown
	// outcome. It is nullable but mandatory in the canonical v1 wire schema.
	Pending *duePendingRecord `json:"pending"`
}

type duePendingRecord struct {
	Candidate DueCandidateRecord `json:"candidate"`
	Reference StageReference     `json:"reference"`
	Task      TaskReference      `json:"task"`
}

func (n Namespace) dueCheckpointKey(partition uint8) (string, error) {
	return n.Key("p", fmt.Sprintf("%02x", partition), "due-worker-checkpoint")
}

func dueCheckpointNamespace(value string) (Namespace, error) {
	if len(value) > maxNamespaceRootBytes || !validWorkerRoot(value) {
		return Namespace{}, ErrInvalidRecord
	}
	parts := strings.Split(strings.TrimSuffix(value, "/"), "/")
	return NewNamespace(strings.Join(parts[:len(parts)-2], "/"), parts[len(parts)-2], parts[len(parts)-1])
}

// DELETE watch events and durable frontiers have no candidate value. Validate
// exactly the canonical derived key rather than inventing an authority record.
func dueKeyExpiry(n Namespace, partition uint8, key string) (time.Time, error) {
	prefix := fmt.Sprintf("%sp/%02x/due/", n.Root(), partition)
	if len(key) > maxKeyBytes || !strings.HasPrefix(key, prefix) {
		return time.Time{}, ErrInvalidRecord
	}
	parts := strings.Split(strings.TrimPrefix(key, prefix), "/")
	if len(parts) != 4 || !validDomainSegment(parts[1]) || !validDomainSegment(parts[3]) {
		return time.Time{}, ErrInvalidRecord
	}
	expiry, err := time.Parse(dueTimeLayout, parts[0])
	if err != nil || !validDomainExpiry(expiry) || expiry.Format(dueTimeLayout) != parts[0] {
		return time.Time{}, ErrInvalidRecord
	}
	generation, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || generation <= 0 || fmt.Sprintf("%019d", generation) != parts[2] {
		return time.Time{}, ErrInvalidRecord
	}
	return expiry.UTC(), nil
}

func (r dueCheckpointRecord) Validate() error {
	n, err := dueCheckpointNamespace(r.Namespace)
	if err != nil || r.Version != 1 || !validDomainSegment(r.RestoreEpoch) || !validWorkerDigest(r.IdentityDigest) || r.ProfileID != workerProfileID || !validWorkerDigest(r.ProfileDigest) || !validPreparationUUID(r.WriterSessionID) || r.WriterPartitionRevision <= 0 || r.Revision <= 0 || r.Revision == math.MaxInt64 || r.ReplayFrom != r.Revision+1 || !validDomainExpiry(r.WindowEnd) || r.WindowEnd.Location() != time.UTC || (r.Phase != "list" && r.Phase != "watch") {
		return ErrInvalidRecord
	}
	if r.Frontier != "" {
		expiry, err := dueKeyExpiry(n, r.Partition, r.Frontier)
		if err != nil || expiry.After(r.WindowEnd) {
			return ErrInvalidRecord
		}
	}
	if p := r.Pending; p != nil {
		c, ref, task := p.Candidate, p.Reference, p.Task
		if c.Validate() != nil || c.Partition != r.Partition || c.RestoreEpoch != r.RestoreEpoch || c.ExpiresAt.After(r.WindowEnd) || task.Validate() != nil || task.Namespace != r.Namespace || task.RestoreEpoch != r.RestoreEpoch || task.Partition != r.Partition || task.SandboxID != c.SandboxID || ref.Namespace != r.Namespace || ref.RestoreEpoch != r.RestoreEpoch || ref.Partition != r.Partition || ref.StageID != "destroy" || !validPreparationUUID(ref.RequestID) || !validPreparationUUID(ref.AttemptID) || !validWorkerDigest(ref.Digest) {
			return ErrInvalidRecord
		}
	}
	return nil
}

func encodeDueCheckpoint(r dueCheckpointRecord) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	wire, err := json.Marshal(r)
	if err != nil || len(wire) > dueCheckpointBytes {
		return "", ErrInvalidRecord
	}
	return string(wire), nil
}

// The shared strict metadata reader intentionally disallows null pointers.
// This schema has exactly one nullable field, whose explicit presence remains
// mandatory. Every non-null nested record uses the existing strict reader.
func strictDueCheckpoint(wire []byte) error {
	typ := reflect.TypeOf(dueCheckpointRecord{})
	dec := json.NewDecoder(bytes.NewReader(wire))
	start, err := dec.Token()
	if err != nil || start != json.Delim('{') {
		return ErrCorruptRecord
	}
	seen := make(map[string]bool, typ.NumField())
	for dec.More() {
		token, err := dec.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return ErrCorruptRecord
		}
		var field reflect.Type
		for i := 0; i < typ.NumField(); i++ {
			if f := typ.Field(i); f.Tag.Get("json") == name {
				field = f.Type
				break
			}
		}
		if field == nil {
			return ErrCorruptRecord
		}
		seen[name] = true
		var raw json.RawMessage
		if dec.Decode(&raw) != nil {
			return ErrCorruptRecord
		}
		if name == "pending" && bytes.Equal(raw, []byte("null")) {
			continue
		}
		if strictPreparationMetadata(raw, field) != nil {
			return ErrCorruptRecord
		}
	}
	end, err := dec.Token()
	if err != nil || end != json.Delim('}') || len(seen) != typ.NumField() || dec.Decode(new(any)) != io.EOF {
		return ErrCorruptRecord
	}
	return nil
}

func (b *Backend) validateDueCheckpoint(r dueCheckpointRecord) error {
	if r.Validate() != nil {
		return ErrInvalidRecord
	}
	if r.Namespace != b.namespace.Root() || r.RestoreEpoch != b.restoreEpoch || r.IdentityDigest != workerIdentityDigest(b.identityValue) || b.workerControl == nil || r.ProfileDigest != workerIdentityDigest(b.workerControl.profile.value) {
		return ErrIdentityMismatch
	}
	if r.Pending != nil {
		if _, _, err := b.stageKeys(r.Pending.Reference); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) decodeDueCheckpoint(kv *mvccpb.KeyValue, revision int64) (dueCheckpointRecord, error) {
	var r dueCheckpointRecord
	if !operationPermanentKV(kv) || kv.Version <= 0 || kv.ModRevision > revision || len(kv.Value) == 0 || len(kv.Value) > dueCheckpointBytes || len(kv.XXX_unrecognized) != 0 || strictDueCheckpoint(kv.Value) != nil || json.Unmarshal(kv.Value, &r) != nil {
		return r, ErrCorruptRecord
	}
	wire, err := encodeDueCheckpoint(r)
	if err != nil || !bytes.Equal(kv.Value, []byte(wire)) {
		return dueCheckpointRecord{}, ErrCorruptRecord
	}
	if err = b.validateDueCheckpoint(r); err != nil {
		return dueCheckpointRecord{}, err
	}
	key, err := b.namespace.dueCheckpointKey(r.Partition)
	if err != nil || string(kv.Key) != key {
		return dueCheckpointRecord{}, ErrCorruptRecord
	}
	return r, nil
}
