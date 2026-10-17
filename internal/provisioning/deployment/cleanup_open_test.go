package deployment

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/stretchr/testify/require"
)

// Every v1 field is explicit. This is a pure mounted configuration fixture;
// its paths and node identities do not establish a Kernel or runtime proof.
const cleanupManifestTestWire = `{
"version":1,
"launcher_config_file":"launcher.json",
"issuer_private_key_file":"issuer.pem",
"issuer_certificate_file":"issuer.json",
"etcd":{"endpoints":["https://127.0.0.1:2379"],"identity":{"schema_version":1,"prefix":"/sandbox","authority_id":"observer","cell":"v1","cluster_id":7,"storage_id":"storage","runtime_id":"runtime","restore_epoch":"epoch"},"tls":{"ca_file":"ca.pem","cert_file":"client.pem","key_file":"client-key.pem","server_name":""},"dial_timeout":"5s","request_timeout":"3s","snapshot_cache_bytes":0},
"kubernetes":{"kubeconfig_file":"kubeconfig","namespace":"runtimes","bridge_max_active":16,"provider_max_active":4,"request_timeout":"3s"},
"worker":{"worker_id":"cleanup-worker","partitions":[7],"range_slots":[0],"claim_slots":[0,1],"relist_slot":0,"session_ttl":"60s","stage_ttl":"30s"},
"storage":{"identity":"storage","bucket":"bucket","prefix_base":"","directory":"/var/lib/cleanup/target"},
"nodes":[{"name":"node-a","uid":"11111111-1111-4111-8111-111111111111","boot_id":"22222222-2222-4222-8222-222222222222","observer_address":"127.0.0.1:9443"}],
"cleanup":{"max_active":32,"claim_ttl":"120s","job_timeout":"5m","archive_timeout":"90s","release_timeout":"30s"},
"archive":{"directory":"/var/lib/cleanup/spool","jobs":1,"entries":16,"job_bytes":1048576,"total_bytes":1048576,"job_index_bytes":1048576,"total_index_bytes":1048576},
"recycling":{"budget_id":"33333333-3333-4333-8333-333333333333","slots_per_partition":1,"max_intent_bytes":16384}
}`

func TestLocalCleanupManifestStrict(t *testing.T) {
	wire := []byte(cleanupManifestTestWire)
	m, err := parseCleanupManifest(wire)
	require.NoError(t, err)
	require.Equal(t, 1, m.Version)
	require.Equal(t, "cleanup-worker", m.Worker.WorkerID)
	require.Equal(t, "60s", m.Worker.SessionTTL)
	require.Equal(t, "120s", m.Cleanup.ClaimTTL)
	require.Equal(t, "90s", m.Cleanup.ArchiveTimeout)
	clear(wire)
	require.Equal(t, "issuer.pem", m.IssuerPrivateKeyFile, "parsed values own their input")

	for name, bad := range map[string][]byte{
		"duplicate-root":   []byte(strings.Replace(cleanupManifestTestWire, `"version":1`, `"version":1,"version":1`, 1)),
		"duplicate-nested": []byte(strings.Replace(cleanupManifestTestWire, `"relist_slot":0`, `"relist_slot":0,"relist_slot":1`, 1)),
		"unknown":          []byte(strings.Replace(cleanupManifestTestWire, `"version":1`, `"version":1,"root_private_key_file":"secret-sentinel"`, 1)),
		"wrong-case":       []byte(strings.Replace(cleanupManifestTestWire, `"version"`, `"Version"`, 1)),
		"unicode-alias":    []byte(strings.Replace(cleanupManifestTestWire, `"storage":{`, `"\u017ftorage":{`, 1)),
		"null-object":      []byte(strings.Replace(cleanupManifestTestWire, `"cleanup":{"max_active":32,"claim_ttl":"120s","job_timeout":"5m","archive_timeout":"90s","release_timeout":"30s"}`, `"cleanup":null`, 1)),
		"null-field":       []byte(strings.Replace(cleanupManifestTestWire, `"prefix_base":""`, `"prefix_base":null`, 1)),
		"missing-field":    []byte(strings.Replace(cleanupManifestTestWire, `"relist_slot":0,`, "", 1)),
		"missing-object":   []byte(`{"version":1}`),
		"wrong-version":    []byte(strings.Replace(cleanupManifestTestWire, `"version":1`, `"version":2`, 1)),
		"trailing":         []byte(cleanupManifestTestWire + `{}`),
		"array":            []byte(`[]`),
		"utf8":             append([]byte(cleanupManifestTestWire), 255),
		"oversize":         bytes.Repeat([]byte(" "), 65537),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseCleanupManifest(bad)
			require.ErrorIs(t, err, errConfiguration)
			require.NotContains(t, err.Error(), "secret-sentinel")
		})
	}
}

func TestLocalCleanupManifestBounds(t *testing.T) {
	for name, edit := range map[string][2]string{
		"session":             {`"session_ttl":"60s"`, `"session_ttl":"59s"`},
		"request":             {`"request_timeout":"3s"`, `"request_timeout":"3.000000001s"`},
		"numeric-duration":    {`"claim_ttl":"120s"`, `"claim_ttl":120`},
		"zero-duration":       {`"job_timeout":"5m"`, `"job_timeout":"0s"`},
		"fractional-claim":    {`"claim_ttl":"120s"`, `"claim_ttl":"120.5s"`},
		"archive-window":      {`"claim_ttl":"120s"`, `"claim_ttl":"100s"`},
		"active33":            {`"max_active":32`, `"max_active":33`},
		"no-default-active":   {`"max_active":32`, `"max_active":0`},
		"duplicate-partition": {`"partitions":[7]`, `"partitions":[7,7]`},
		"duplicate-share":     {`"claim_slots":[0,1]`, `"claim_slots":[1,1]`},
		"claim-share100":      {`"claim_slots":[0,1]`, `"claim_slots":[100]`},
		"range-share20":       {`"range_slots":[0]`, `"range_slots":[20]`},
		"relist4":             {`"relist_slot":0`, `"relist_slot":4`},
		"partition256":        {`"partitions":[7]`, `"partitions":[256]`},
		"relative-target":     {`"directory":"/var/lib/cleanup/target"`, `"directory":"target"`},
		"path-traversal":      {`"prefix_base":""`, `"prefix_base":"../"`},
		"spool-zero":          {`"jobs":1`, `"jobs":0`},
		"spool-overflow":      {`"total_bytes":1048576`, `"total_bytes":18446744073709551616`},
		"spool-accounting":    {`"total_bytes":1048576`, `"total_bytes":1`},
		"node-uuid":           {`11111111-1111-4111-8111-111111111111`, `not-a-uuid`},
		"plaintext":           {`https://127.0.0.1:2379`, `http://127.0.0.1:2379`},
		"budget-uuid":         {`33333333-3333-4333-8333-333333333333`, `not-a-budget`},
	} {
		t.Run(name, func(t *testing.T) {
			bad := strings.Replace(cleanupManifestTestWire, edit[0], edit[1], 1)
			require.NotEqual(t, cleanupManifestTestWire, bad)
			_, err := parseCleanupManifest([]byte(bad))
			require.ErrorIs(t, err, errConfiguration)
		})
	}
}

func TestOpenLocalCleanupRefusal(t *testing.T) {
	// These public host refusals establish no positive mounted-worker claim.
	path := filepath.Join(t.TempDir(), "cleanup.json")
	require.NoError(t, os.WriteFile(path, []byte(cleanupManifestTestWire), 0600))
	d, err := OpenLocalCleanup(nil, path, nil)
	require.Error(t, err)
	require.Nil(t, d)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d, err = OpenLocalCleanup(ctx, path, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, d)
	d, err = OpenLocalCleanup(context.Background(), path, nil)
	require.Error(t, err)
	require.Nil(t, d)
	var absent *LocalCleanupDeployment
	require.NoError(t, absent.Close())
	result, err := absent.Execute(context.Background(), state.TaskReference{})
	require.Error(t, err)
	require.NotEqual(t, state.OutcomeCommitted, result.Outcome)
}
