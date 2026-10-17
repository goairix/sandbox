package deployment

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/util/validation"
)

// v1 is independent of the create/HTTP manifest: no root private key or
// provisioning inventory is accepted. Every field is explicit, including
// durations (Go duration strings) and byte budgets (unsigned integer bytes).
type cleanupManifest struct {
	Version               int    `json:"version"`
	LauncherConfigFile    string `json:"launcher_config_file"`
	IssuerPrivateKeyFile  string `json:"issuer_private_key_file"`
	IssuerCertificateFile string `json:"issuer_certificate_file"`

	Etcd struct {
		Endpoints []string `json:"endpoints"`
		Identity  struct {
			SchemaVersion uint32 `json:"schema_version"`
			Prefix        string `json:"prefix"`
			AuthorityID   string `json:"authority_id"`
			Cell          string `json:"cell"`
			ClusterID     uint64 `json:"cluster_id"`
			StorageID     string `json:"storage_id"`
			RuntimeID     string `json:"runtime_id"`
			RestoreEpoch  string `json:"restore_epoch"`
		} `json:"identity"`
		TLS struct {
			CAFile     string `json:"ca_file"`
			CertFile   string `json:"cert_file"`
			KeyFile    string `json:"key_file"`
			ServerName string `json:"server_name"`
		} `json:"tls"`
		DialTimeout        string `json:"dial_timeout"`
		RequestTimeout     string `json:"request_timeout"`
		SnapshotCacheBytes uint64 `json:"snapshot_cache_bytes"`
	} `json:"etcd"`
	Kubernetes struct {
		KubeconfigFile    string `json:"kubeconfig_file"`
		Namespace         string `json:"namespace"`
		BridgeMaxActive   int    `json:"bridge_max_active"`
		ProviderMaxActive uint32 `json:"provider_max_active"`
		RequestTimeout    string `json:"request_timeout"`
	} `json:"kubernetes"`
	Worker struct {
		WorkerID   string   `json:"worker_id"`
		Partitions []uint8  `json:"partitions"`
		RangeSlots []uint16 `json:"range_slots"`
		ClaimSlots []uint16 `json:"claim_slots"`
		RelistSlot uint16   `json:"relist_slot"`
		SessionTTL string   `json:"session_ttl"`
		StageTTL   string   `json:"stage_ttl"`
	} `json:"worker"`
	Storage struct {
		Identity   string `json:"identity"`
		Bucket     string `json:"bucket"`
		PrefixBase string `json:"prefix_base"`
		Directory  string `json:"directory"`
	} `json:"storage"`
	Nodes []struct {
		Name            string `json:"name"`
		UID             string `json:"uid"`
		BootID          string `json:"boot_id"`
		ObserverAddress string `json:"observer_address"`
	} `json:"nodes"`
	Cleanup struct {
		MaxActive      uint32 `json:"max_active"`
		ClaimTTL       string `json:"claim_ttl"`
		JobTimeout     string `json:"job_timeout"`
		ArchiveTimeout string `json:"archive_timeout"`
		ReleaseTimeout string `json:"release_timeout"`
	} `json:"cleanup"`
	Archive struct {
		Directory       string `json:"directory"`
		Jobs            uint64 `json:"jobs"`
		Entries         uint64 `json:"entries"`
		JobBytes        uint64 `json:"job_bytes"`
		TotalBytes      uint64 `json:"total_bytes"`
		JobIndexBytes   uint64 `json:"job_index_bytes"`
		TotalIndexBytes uint64 `json:"total_index_bytes"`
	} `json:"archive"`
	Recycling struct {
		BudgetID          string `json:"budget_id"`
		SlotsPerPartition uint32 `json:"slots_per_partition"`
		MaxIntentBytes    uint32 `json:"max_intent_bytes"`
	} `json:"recycling"`
}

func parseCleanupManifest(wire []byte) (cleanupManifest, error) {
	var m cleanupManifest
	if len(wire) == 0 || len(wire) > 65536 || !utf8.Valid(wire) {
		return m, errConfiguration
	}
	d := json.NewDecoder(bytes.NewReader(wire))
	if strictValue(d, 0) != nil {
		return m, errConfiguration
	}
	if _, err := d.Token(); err != io.EOF {
		return m, errConfiguration
	}
	d = json.NewDecoder(bytes.NewReader(wire))
	d.DisallowUnknownFields()
	if d.Decode(&m) != nil || !cleanupRequiredFields(wire, reflect.TypeOf(m)) || m.validate() != nil {
		return cleanupManifest{}, errConfiguration
	}
	return m, nil
}

// The separate token pass rejects duplicates/case aliases. This walk binds
// presence and non-null values to the one fixed struct schema, including zero
// integer fields whose omission ordinary json.Unmarshal cannot distinguish.
func cleanupRequiredFields(wire []byte, shape reflect.Type) bool {
	if bytes.Equal(bytes.TrimSpace(wire), []byte("null")) {
		return false
	}
	switch shape.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if json.Unmarshal(wire, &fields) != nil || len(fields) != shape.NumField() {
			return false
		}
		for n := 0; n < shape.NumField(); n++ {
			field := shape.Field(n)
			value, present := fields[field.Tag.Get("json")]
			if !present || !cleanupRequiredFields(value, field.Type) {
				return false
			}
		}
	case reflect.Slice:
		var values []json.RawMessage
		if json.Unmarshal(wire, &values) != nil {
			return false
		}
		for _, value := range values {
			if !cleanupRequiredFields(value, shape.Elem()) {
				return false
			}
		}
	}
	return true
}

func cleanupSegment(value string) bool {
	if len(value) == 0 || len(value) > 128 || value == "." || value == ".." {
		return false
	}
	return strings.IndexFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-')
	}) < 0
}

func cleanupUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

func cleanupPath(value string, absolute bool) bool {
	if value == "" || len(value) > 4096 || !utf8.ValidString(value) || strings.Contains(value, "\\") || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return false
	}
	return !absolute || filepath.IsAbs(value) && filepath.Clean(value) == value && value != "/"
}

// Called only for already-validated duration fields when constructing concrete
// original options. There are no loader defaults or unit conversions here.
func cleanupDuration(value string) time.Duration {
	d, _ := time.ParseDuration(value)
	return d
}

func cleanupDurationWithin(value string, minimum, maximum time.Duration, integral bool) bool {
	d, err := time.ParseDuration(value)
	return err == nil && d > 0 && d >= minimum && (maximum == 0 || d <= maximum) && (!integral || d%time.Second == 0)
}

func (m cleanupManifest) validate() error {
	i := m.Etcd.Identity
	if m.Version != 1 || i.SchemaVersion != 1 || i.ClusterID == 0 || !cleanupSegment(i.RestoreEpoch) || strings.TrimSpace(i.StorageID) == "" || len(i.StorageID) > 128 || strings.TrimSpace(i.RuntimeID) == "" || len(i.RuntimeID) > 128 {
		return errConfiguration
	}
	if _, err := state.NewNamespace(i.Prefix, i.AuthorityID, i.Cell); err != nil {
		return errConfiguration
	}
	for _, path := range []string{m.LauncherConfigFile, m.IssuerPrivateKeyFile, m.IssuerCertificateFile, m.Etcd.TLS.CAFile, m.Etcd.TLS.CertFile, m.Etcd.TLS.KeyFile, m.Kubernetes.KubeconfigFile} {
		if !cleanupPath(path, false) {
			return errConfiguration
		}
	}
	if len(m.Etcd.Endpoints) == 0 || len(m.Etcd.TLS.ServerName) > 253 {
		return errConfiguration
	}
	for _, endpoint := range m.Etcd.Endpoints {
		u, err := url.Parse(endpoint)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
			return errConfiguration
		}
	}
	if !cleanupDurationWithin(m.Etcd.DialTimeout, 0, 0, false) || !cleanupDurationWithin(m.Etcd.RequestTimeout, 0, 3*time.Second, false) || m.Etcd.SnapshotCacheBytes != 0 && (m.Etcd.SnapshotCacheBytes < 256*1024 || m.Etcd.SnapshotCacheBytes > 64*1024*1024) {
		return errConfiguration
	}
	k := m.Kubernetes
	if len(validation.IsDNS1123Label(k.Namespace)) != 0 || k.BridgeMaxActive < 1 || k.BridgeMaxActive > 16 || k.ProviderMaxActive < 1 || k.ProviderMaxActive > 32 || !cleanupDurationWithin(k.RequestTimeout, 0, 5*time.Second, false) {
		return errConfiguration
	}
	if m.validateScheduling() != nil || m.validateStorage() != nil {
		return errConfiguration
	}
	return nil
}

func (m cleanupManifest) validateScheduling() error {
	w, c := m.Worker, m.Cleanup
	if !cleanupSegment(w.WorkerID) || len(w.Partitions) == 0 || len(w.Partitions) > 256 || len(w.RangeSlots) == 0 || len(w.RangeSlots) > 20 || len(w.ClaimSlots) == 0 || len(w.ClaimSlots) > 100 || w.RelistSlot >= 4 || cleanupDuration(w.SessionTTL) != time.Minute || !cleanupDurationWithin(w.StageTTL, time.Second, 24*time.Hour, true) {
		return errConfiguration
	}
	var partitions [256]bool
	for _, partition := range w.Partitions {
		if partitions[partition] {
			return errConfiguration
		}
		partitions[partition] = true
	}
	for _, group := range []struct {
		slots []uint16
		bound uint16
	}{{w.RangeSlots, 20}, {w.ClaimSlots, 100}} {
		var seen [100]bool
		for _, slot := range group.slots {
			if slot >= group.bound || seen[slot] {
				return errConfiguration
			}
			seen[slot] = true
		}
	}
	if c.MaxActive < 1 || c.MaxActive > 32 || !cleanupDurationWithin(c.ClaimTTL, 30*time.Second, 300*time.Second, true) || !cleanupDurationWithin(c.JobTimeout, 0, 15*time.Minute, false) || !cleanupDurationWithin(c.ArchiveTimeout, 0, 90*time.Second, false) || cleanupDuration(c.ArchiveTimeout) >= cleanupDuration(c.ClaimTTL)-10*time.Second || !cleanupDurationWithin(c.ReleaseTimeout, 0, 30*time.Second, false) {
		return errConfiguration
	}
	if len(m.Nodes) == 0 || len(m.Nodes) > 256 {
		return errConfiguration
	}
	names, uids := make(map[string]bool), make(map[string]bool)
	for _, node := range m.Nodes {
		host, port, err := net.SplitHostPort(node.ObserverAddress)
		if err != nil || host == "" || port == "" || len(node.ObserverAddress) > 512 || !cleanupSegment(node.Name) || !cleanupUUID(node.UID) || !cleanupUUID(node.BootID) || names[node.Name] || uids[node.UID] {
			return errConfiguration
		}
		names[node.Name], uids[node.UID] = true, true
	}
	return nil
}

func (m cleanupManifest) validateStorage() error {
	s, a, r := m.Storage, m.Archive, m.Recycling
	if !cleanupPath(s.Directory, true) || !cleanupPath(a.Directory, true) || !cleanupUUID(r.BudgetID) || r.SlotsPerPartition < 1 || r.SlotsPerPartition > 64 || r.MaxIntentBytes < 4096 || r.MaxIntentBytes > 16384 {
		return errConfiguration
	}
	// The workspace constructor applies the original storage identity and
	// prefix codec. Empty PrefixBase is permitted as the mounted root mapping.
	prefix := s.PrefixBase
	if prefix == "" {
		prefix = "cleanup-profile/"
	}
	if _, err := state.NewWorkspaceIdentity("local", s.Identity, s.Bucket, prefix); err != nil {
		return errConfiguration
	}
	// Same original recovery arithmetic: 64 fixed slots, two 64KiB disk
	// generations, four 64KiB memory buffers, 33-byte open-addressed index cells.
	// Backend repeats its authoritative validation before opening the store.
	if a.Jobs == 0 || a.Jobs > 64 || a.Entries == 0 || a.Entries > 1000000 || a.JobBytes < 2*65536+1536 || a.JobBytes > 1<<40 || a.TotalBytes < a.JobBytes {
		return errConfiguration
	}
	indexSlots := uint64(2)
	for indexSlots < a.Entries*2 {
		indexSlots *= 2
	}
	memory := indexSlots*33 + 4*65536
	if a.JobIndexBytes < memory || a.TotalIndexBytes < memory {
		return errConfiguration
	}
	return nil
}
