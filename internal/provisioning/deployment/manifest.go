// Package deployment loads mounted credentials into the existing cold-create
// constructors. It never provisions identities, inventory or runtime state.
package deployment

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

var errConfiguration = errors.New("deployment: invalid mounted configuration")

type resources struct {
	CPU     string `json:"cpu"`
	Memory  string `json:"memory"`
	Disk    string `json:"disk"`
	TmpDisk string `json:"tmp_disk"`
}

type manifest struct {
	Version int `json:"version"`
	Etcd    struct {
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
	Preparation struct {
		QuotaSlots     []uint32 `json:"quota_slots"`
		QuotaPartition uint8    `json:"quota_partition"`
	} `json:"preparation"`
	LauncherConfigFile    string `json:"launcher_config_file"`
	RootPrivateKeyFile    string `json:"root_private_key_file"`
	IssuerPrivateKeyFile  string `json:"issuer_private_key_file"`
	IssuerCertificateFile string `json:"issuer_certificate_file"`
	Kubernetes            struct {
		KubeconfigFile string `json:"kubeconfig_file"`
		Namespace      string `json:"namespace"`
		MaxActive      int    `json:"max_active"`
		RequestTimeout string `json:"request_timeout"`
	} `json:"kubernetes"`
	Image     string    `json:"image"`
	NodeName  string    `json:"node_name"`
	WorkerID  string    `json:"worker_id"`
	Resources resources `json:"resources"`
	Business  struct {
		Principal string `json:"principal"`
		Defaults  struct {
			TimeoutSeconds int       `json:"timeout_seconds"`
			Resources      resources `json:"resources"`
		} `json:"defaults"`
	} `json:"business"`
}

func parseManifest(wire []byte) (manifest, error) {
	var m manifest
	if len(wire) == 0 || len(wire) > 65536 || !utf8.Valid(wire) {
		return m, errConfiguration
	}
	d := json.NewDecoder(bytes.NewReader(wire))
	if err := strictValue(d, 0); err != nil {
		return m, errConfiguration
	}
	if _, err := d.Token(); err != io.EOF {
		return m, errConfiguration
	}
	d = json.NewDecoder(bytes.NewReader(wire))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil || m.Version != 1 {
		return manifest{}, errConfiguration
	}
	return m, nil
}

// Reject duplicate keys before decoding into structs, where duplicates and case
// aliases would otherwise silently replace credential and identity settings.
func strictValue(d *json.Decoder, depth int) error {
	if depth > 32 {
		return errConfiguration
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, container := token.(json.Delim)
	if !container {
		return nil
	}
	if delim != '{' && delim != '[' {
		return errConfiguration
	}
	seen := make(map[string]bool)
	for d.More() {
		if delim == '{' {
			key, err := d.Token()
			name, ok := key.(string)
			if err != nil || !ok || seen[name] || strings.ToLower(name) != name || strings.IndexFunc(name, func(r rune) bool { return r > 127 }) >= 0 {
				return errConfiguration
			}
			seen[name] = true
		}
		if err := strictValue(d, depth+1); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}
