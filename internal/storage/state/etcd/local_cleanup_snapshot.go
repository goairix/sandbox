package etcd

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"strings"
	"unicode/utf8"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

const (
	localCleanupDescriptorDomain = "sandbox.local-cleanup-descriptor.v1"
	// The descriptor shares the existing 64KiB snapshot envelope. Its separate
	// 8KiB limit bounds retained workspace preimages and exclusion policy.
	maxLocalCleanupDescriptorBytes = 8192
)

// LocalCleanupDescriptor preserves a local workspace preimage and synchronization
// policy in a version2 snapshot. It is caller-owned diagnostic data, never a
// claim, current storage permission or proof that a runtime can be released.
// Version1 descriptors contain only the storage identity hash, not credentials.
type LocalCleanupDescriptor struct {
	// Version and Domain identify the immutable descriptor protocol independently
	// of the enclosing snapshot version and its business payload format.
	Version uint32 `json:"version"`
	Domain  string `json:"domain"`

	// Provider, storage identity hash and canonical path preserve the exact
	// workspace preimage. The hash is attribution data, never a secret credential.
	Provider            string `json:"provider"`
	StorageIdentityHash string `json:"storage_identity_hash"`
	Bucket              string `json:"bucket"`
	Prefix              string `json:"prefix"`

	// WorkspaceHash and Partition must be recomputed from that preimage and
	// bound to each reader's original sandbox placement before returning it.
	WorkspaceHash string `json:"workspace_hash"`
	Partition     uint8  `json:"partition"`

	// Exclude is canonical and nonnil, including an explicit empty JSON array.
	// DeleteExtraneous is always present, including an explicit false policy.
	Exclude          []string `json:"exclude"`
	DeleteExtraneous bool     `json:"delete_extraneous"`
}

func cloneLocalCleanupDescriptor(d *LocalCleanupDescriptor) *LocalCleanupDescriptor {
	if d == nil {
		return nil
	}
	owned := *d
	owned.Domain = strings.Clone(d.Domain)
	owned.Provider = strings.Clone(d.Provider)
	owned.StorageIdentityHash = strings.Clone(d.StorageIdentityHash)
	owned.Bucket = strings.Clone(d.Bucket)
	owned.Prefix = strings.Clone(d.Prefix)
	owned.WorkspaceHash = strings.Clone(d.WorkspaceHash)
	if d.Exclude != nil {
		// Exact capacity avoids retaining uncharged caller backing or redundant
		// exclusions beyond the canonical slice length.
		owned.Exclude = make([]string, len(d.Exclude))
		for i, path := range d.Exclude {
			owned.Exclude[i] = strings.Clone(path)
		}
	}
	return &owned
}

// workspace reconstructs and checks the original hash and its partition from
// the stored preimage. Backend storage identity and current effect authority
// remain separate checks; this reconstruction cannot manufacture either.
func (d *LocalCleanupDescriptor) workspace() (WorkspaceIdentity, error) {
	if d == nil || d.Version != 1 || d.Domain != localCleanupDescriptorDomain || d.Provider != "local" || d.Exclude == nil {
		return WorkspaceIdentity{}, ErrInvalidRecord
	}
	w := WorkspaceIdentity{
		provider: d.Provider, storageIdentityHash: d.StorageIdentityHash,
		bucket: d.Bucket, prefix: d.Prefix, hash: d.WorkspaceHash, partition: d.Partition,
	}
	if w.validate() != nil {
		return WorkspaceIdentity{}, ErrInvalidRecord
	}
	plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: w, Exclude: d.Exclude, DeleteExtraneous: d.DeleteExtraneous})
	if err != nil || len(plan.exclude) != len(d.Exclude) {
		return WorkspaceIdentity{}, ErrInvalidRecord
	}
	for i, path := range plan.exclude {
		if path != d.Exclude[i] {
			return WorkspaceIdentity{}, ErrInvalidRecord
		}
	}
	return w, nil
}

func localCleanupDescriptorJSON(d *LocalCleanupDescriptor) ([]byte, error) {
	if _, err := d.workspace(); err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(d); err != nil {
		return nil, ErrInvalidRecord
	}
	wire := bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))
	if len(wire) > maxLocalCleanupDescriptorBytes {
		return nil, ErrInvalidRecord
	}
	return wire, nil
}

// captureLocalCleanupPlan takes ownership before the first acquisition RPC.
// A valid plan must describe exactly the supplied workspace, not merely another
// workspace in the same partition. Remote and detached profiles remain absent.
func captureLocalCleanupPlan(w WorkspaceIdentity, input *TaskWorkspaceSyncPlan) (*LocalCleanupDescriptor, error) {
	if input == nil {
		return nil, nil
	}
	plan := *input
	if plan.validate() != nil || plan.workspace != w || w.Kind() != WorkspaceIdentityWorkspace || w.provider != "local" {
		return nil, ErrInvalidRecord
	}
	d := cloneLocalCleanupDescriptor(&LocalCleanupDescriptor{
		Version: 1, Domain: localCleanupDescriptorDomain, Provider: w.provider,
		StorageIdentityHash: w.storageIdentityHash, Bucket: w.bucket, Prefix: w.prefix,
		WorkspaceHash: w.hash, Partition: w.partition, Exclude: plan.exclude, DeleteExtraneous: plan.deleteExtraneous,
	})
	if d.Exclude == nil {
		d.Exclude = []string{}
	}
	if _, err := localCleanupDescriptorJSON(d); err != nil {
		return nil, err
	}
	return d, nil
}

// snapshotRecordDigest keeps historical payload-only hashes unchanged. Version2
// uses length framing and explicit domain/version fields to bind the policy and
// workspace preimage along with the compact, otherwise opaque business payload.
func snapshotRecordDigest(r SandboxSnapshotRecord) (string, error) {
	switch r.Version {
	case 1:
		if r.LocalCleanup != nil {
			return "", ErrInvalidRecord
		}
		return snapshotDigest(r.Payload)
	case 2:
		payload, err := compactSnapshot(r.Payload)
		if err != nil {
			return "", err
		}
		descriptor, err := localCleanupDescriptorJSON(r.LocalCleanup)
		if err != nil {
			return "", err
		}
		digest := framedDigest("sandbox.snapshot.v2", "2", string(payload), string(descriptor))
		return hex.EncodeToString(digest[:]), nil
	default:
		return "", ErrInvalidRecord
	}
}

// strictCleanupObject checks only the versioned metadata object. Payload JSON
// is deliberately opaque; this codec has no dependency on provisioning types.
func strictCleanupObject(wire []byte, fields []string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(wire))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, ErrCorruptRecord
	}
	values := make(map[string]json.RawMessage, len(fields))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, ErrCorruptRecord
		}
		name, ok := token.(string)
		if !ok || values[name] != nil {
			return nil, ErrCorruptRecord
		}
		known := false
		for _, field := range fields {
			known = known || name == field
		}
		if !known {
			return nil, ErrCorruptRecord
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, ErrCorruptRecord
		}
		// The root payload is opaque JSON, including explicit null. It still
		// occupies a required field and contributes its bytes to the digest.
		// Descriptor fields cannot use this exception: their allowed-name list
		// excludes payload, and all metadata nulls remain invalid.
		if name != "payload" && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, ErrCorruptRecord
		}
		values[name] = value
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || len(values) != len(fields) || decoder.Decode(new(any)) != io.EOF {
		return nil, ErrCorruptRecord
	}
	return values, nil
}

// decodeLocalCleanupSnapshot publishes a fresh record only after shape, digest
// and canonical-byte checks. Version1 retains its old JSON/payload semantics,
// except that local_cleanup remains forbidden as it was before this field existed.
func decodeLocalCleanupSnapshot(kv *mvccpb.KeyValue, destination *SandboxSnapshotRecord) error {
	if kv == nil || destination == nil || kv.Lease != 0 || len(kv.Value) > 65536 || !utf8.Valid(kv.Value) {
		return ErrCorruptRecord
	}
	var probe struct {
		Version uint32 `json:"version"`
	}
	if json.Unmarshal(kv.Value, &probe) != nil {
		return ErrCorruptRecord
	}
	if probe.Version == 1 {
		var fields map[string]json.RawMessage
		if json.Unmarshal(kv.Value, &fields) != nil {
			return ErrCorruptRecord
		}
		for field := range fields {
			if strings.EqualFold(field, "local_cleanup") {
				return ErrCorruptRecord
			}
		}
		return decodeTypedDomain(kv, destination, 65536)
	}
	if probe.Version != 2 {
		return ErrCorruptRecord
	}
	fields, err := strictCleanupObject(kv.Value, []string{"version", "sandbox_id", "snapshot", "payload", "local_cleanup"})
	if err != nil {
		return err
	}
	if _, err := strictCleanupObject(fields["local_cleanup"], []string{"version", "domain", "provider", "storage_identity_hash", "bucket", "prefix", "workspace_hash", "partition", "exclude", "delete_extraneous"}); err != nil {
		return err
	}
	var fresh SandboxSnapshotRecord
	if err := decodeTypedDomain(kv, &fresh, 65536); err != nil {
		return err
	}
	canonical, err := encodeDomainRecord(fresh)
	if err != nil || canonical != string(kv.Value) {
		return ErrCorruptRecord
	}
	*destination = fresh
	return nil
}

// localCleanupDescriptorWeight conservatively charges object/slice overhead and
// all retained strings. The 256-byte base and 32 bytes per exclusion exceed their
// 64-bit headers; overflow refuses caching rather than wrapping the byte budget.
func localCleanupDescriptorWeight(d *LocalCleanupDescriptor) (uint64, bool) {
	if d == nil {
		return 0, true
	}
	if uint64(len(d.Exclude)) > (math.MaxUint64-256)/32 {
		return 0, false
	}
	weight := uint64(256) + uint64(len(d.Exclude))*32
	add := func(value string) bool {
		size := uint64(len(value))
		if size > math.MaxUint64-weight {
			return false
		}
		weight += size
		return true
	}
	for _, value := range []string{d.Domain, d.Provider, d.StorageIdentityHash, d.Bucket, d.Prefix, d.WorkspaceHash} {
		if !add(value) {
			return 0, false
		}
	}
	for _, path := range d.Exclude {
		if !add(path) {
			return 0, false
		}
	}
	return weight, true
}
