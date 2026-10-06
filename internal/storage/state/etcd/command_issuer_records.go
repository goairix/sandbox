package etcd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

// CommandIssuerRecord is structural historical metadata. Its opaque certificate
// must be authenticated freshly by a pinned verifier before authorizing effects.
type CommandIssuerRecord struct {
	Version           uint32          `json:"version"`
	Namespace         string          `json:"namespace"`
	RestoreEpoch      string          `json:"restore_epoch"`
	CertificateID     string          `json:"certificate_id"`
	CertificateDigest string          `json:"certificate_digest"`
	Certificate       json.RawMessage `json:"certificate"`
}

// CommandIssuerEntry carries copied historical metadata and its first revision,
// without cryptographic identity evidence or command authority.
type CommandIssuerEntry struct {
	Record   CommandIssuerRecord
	Revision int64
}

func (r CommandIssuerRecord) Validate() error {
	if r.Version != 1 || !validDomainSegment(r.RestoreEpoch) || !validPreparationUUID(r.CertificateID) || !validHexDigest(r.CertificateDigest) || len(r.Namespace) > maxNamespaceRootBytes || !strings.HasSuffix(r.Namespace, "/") || len(r.Certificate) == 0 || len(r.Certificate) > 4096 || bytes.Equal(bytes.TrimSpace(r.Certificate), []byte("null")) {
		return ErrInvalidRecord
	}
	parts := strings.Split(strings.TrimSuffix(r.Namespace, "/"), "/")
	if len(parts) < 4 {
		return ErrInvalidRecord
	}
	n, err := NewNamespace(strings.Join(parts[:len(parts)-2], "/"), parts[len(parts)-2], parts[len(parts)-1])
	if err != nil || n.Root() != r.Namespace {
		return ErrInvalidRecord
	}
	digest, err := snapshotDigest(r.Certificate)
	if err != nil || digest != r.CertificateDigest {
		return ErrInvalidRecord
	}
	return nil
}

func (n Namespace) commandIssuerKey(certificateID string) (string, error) {
	if !validPreparationUUID(certificateID) {
		return "", ErrInvalidRecord
	}
	return n.Key("command-issuers", certificateID)
}

func encodeCommandIssuerRecord(record CommandIssuerRecord) (string, error) {
	record.Certificate = append(json.RawMessage(nil), record.Certificate...)
	if err := record.Validate(); err != nil {
		return "", err
	}
	var value bytes.Buffer
	encoder := json.NewEncoder(&value)
	// Escaping certificate bytes would invalidate the compact snapshot digest.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(record); err != nil {
		return "", fmt.Errorf("%w: invalid command issuer JSON", ErrInvalidRecord)
	}
	encoded := bytes.TrimSuffix(value.Bytes(), []byte("\n"))
	if len(encoded) > 8192 {
		return "", fmt.Errorf("%w: command issuer record exceeds size limit", ErrInvalidRecord)
	}
	return string(encoded), nil
}

func decodeCommandIssuerRecord(kv *mvccpb.KeyValue, destination *CommandIssuerRecord) error {
	if destination == nil || !immutableDispatchKV(kv) {
		return ErrCorruptRecord
	}
	// Decode locally so invalid keys cannot mutate the caller's destination.
	var fresh CommandIssuerRecord
	if err := decodePreparationDomain(kv, &fresh, 8192); err != nil {
		return err
	}
	if string(kv.Key) != fresh.Namespace+"command-issuers/"+fresh.CertificateID {
		return ErrCorruptRecord
	}
	*destination = fresh
	return nil
}
