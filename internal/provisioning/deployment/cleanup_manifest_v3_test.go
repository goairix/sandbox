package deployment

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Counts are written individually on the wire: decoding a short JSON array
// straight into state.PreparationInventoryOptions would silently pad zeroes.
func coldQuotaCleanupManifestTestWire(counts []string) string {
	wire := strings.Replace(automaticCleanupManifestTestWire(), `"version":2`, `"version":3`, 1)
	return strings.Replace(wire, `"version":3`, `"version":3,"preparation":{"quota_slots":[`+strings.Join(counts, ",")+`]}`, 1)
}

func coldQuotaCleanupTestCounts() []string {
	counts := strings.Split(strings.Repeat("0,", 255)+"0", ",")
	counts[0] = "1"
	counts[7] = "40"
	counts[255] = "1000000"
	return counts
}

func TestColdQuotaCleanupManifestProfile(t *testing.T) {
	wire := []byte(coldQuotaCleanupManifestTestWire(coldQuotaCleanupTestCounts()))
	m, err := parseColdQuotaCleanupManifest(wire)
	require.NoError(t, err)
	require.Equal(t, 3, m.Cleanup.Version)
	require.Equal(t, "cleanup-worker", m.Cleanup.Worker.WorkerID)
	require.NotNil(t, m.Cleanup.Worker.AutomaticCleanup)
	require.True(t, *m.Cleanup.Worker.AutomaticCleanup)
	require.Equal(t, uint32(1), m.Inventory.QuotaSlots[0])
	require.Equal(t, uint32(40), m.Inventory.QuotaSlots[7])
	require.Equal(t, uint32(0), m.Inventory.QuotaSlots[254])
	require.Equal(t, uint32(1000000), m.Inventory.QuotaSlots[255])
	clear(wire)
	require.Equal(t, "issuer.pem", m.Cleanup.IssuerPrivateKeyFile)

	// The storage profile permits every partition to reach its own limit;
	// a deployment must not invent a smaller aggregate inventory limit.
	counts := strings.Split(strings.Repeat("1000000,", 255)+"1000000", ",")
	_, err = parseColdQuotaCleanupManifest([]byte(coldQuotaCleanupManifestTestWire(counts)))
	require.NoError(t, err)

	counts = strings.Split(strings.Repeat("0,", 255)+"1", ",")
	_, err = parseColdQuotaCleanupManifest([]byte(coldQuotaCleanupManifestTestWire(counts)))
	require.NoError(t, err, "one slot in the final partition is a nonempty profile")
}

func TestColdQuotaCleanupManifestStrictShape(t *testing.T) {
	wire := coldQuotaCleanupManifestTestWire(coldQuotaCleanupTestCounts())
	profile := `"preparation":{"quota_slots":[` + strings.Join(coldQuotaCleanupTestCounts(), ",") + `]}`
	for name, edit := range map[string][2]string{
		"missing-profile":       {profile + ",", ""},
		"null-profile":          {profile, `"preparation":null`},
		"missing-counts":        {profile, `"preparation":{}`},
		"null-counts":           {profile, `"preparation":{"quota_slots":null}`},
		"null-zero-count":       {`"quota_slots":[1,0,`, `"quota_slots":[1,null,`},
		"null-final-count":      {`0,1000000]`, `0,null]`},
		"profile-case":          {`"preparation":`, `"Preparation":`},
		"counts-case":           {`"quota_slots":`, `"Quota_slots":`},
		"counts-unicode-alias":  {`"quota_slots":`, `"quota_\u017flots":`},
		"duplicate-profile":     {profile, profile + "," + profile},
		"duplicate-counts":      {`"quota_slots":[`, `"quota_slots":[],"quota_slots":[`},
		"extra-partition":       {`"quota_slots":[`, `"quota_partition":7,"quota_slots":[`},
		"caller-digest":         {`"quota_slots":[`, `"profile_digest":"secret-sentinel","quota_slots":[`},
		"caller-ready":          {`"quota_slots":[`, `"ready":true,"quota_slots":[`},
		"caller-epoch":          {`"quota_slots":[`, `"restore_epoch":"epoch","quota_slots":[`},
		"unknown-root":          {`"version":3`, `"version":3,"warm_inventory":{}`},
		"missing-original-zero": {`"relist_slot":0,`, ""},
		"null-original-zero":    {`"snapshot_cache_bytes":0`, `"snapshot_cache_bytes":null`},
		"original-case":         {`"worker_id":`, `"Worker_id":`},
		"original-duplicate":    {`"relist_slot":0`, `"relist_slot":0,"relist_slot":0`},
		"missing-version":       {`"version":3,`, ""},
		"null-version":          {`"version":3`, `"version":null`},
		"duplicate-version":     {`"version":3`, `"version":3,"version":3`},
	} {
		t.Run(name, func(t *testing.T) {
			bad := strings.Replace(wire, edit[0], edit[1], 1)
			require.NotEqual(t, wire, bad)
			_, err := parseColdQuotaCleanupManifest([]byte(bad))
			require.ErrorIs(t, err, errConfiguration)
			require.NotContains(t, err.Error(), "secret-sentinel")
		})
	}
	for name, bad := range map[string][]byte{
		"trailing": []byte(wire + `{}`),
		"utf8":     append([]byte(wire), 255),
		"oversize": bytes.Repeat([]byte(" "), 65537),
		"null":     []byte(`null`),
		"array":    []byte(`[]`),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseColdQuotaCleanupManifest(bad)
			require.ErrorIs(t, err, errConfiguration)
		})
	}
}

func TestColdQuotaCleanupManifestInventoryBounds(t *testing.T) {
	valid := coldQuotaCleanupTestCounts()
	for name, counts := range map[string][]string{
		"short":      valid[:255],
		"long":       append(append([]string(nil), valid...), "0"),
		"empty":      {},
		"all-zero":   strings.Split(strings.Repeat("0,", 255)+"0", ","),
		"one-count":  {"1"},
		"null-count": append([]string{"null"}, valid[1:]...),
		"over-limit": append([]string{"1000001"}, valid[1:]...),
		"overflow":   append([]string{"4294967296"}, valid[1:]...),
		"negative":   append([]string{"-1"}, valid[1:]...),
		"fraction":   append([]string{"1.5"}, valid[1:]...),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseColdQuotaCleanupManifest([]byte(coldQuotaCleanupManifestTestWire(counts)))
			require.ErrorIs(t, err, errConfiguration)
		})
	}
}

func TestColdQuotaCleanupManifestModeIsolation(t *testing.T) {
	wire := coldQuotaCleanupManifestTestWire(coldQuotaCleanupTestCounts())
	for name, edit := range map[string][2]string{
		"v1":           {`"version":3`, `"version":1`},
		"v2":           {`"version":3`, `"version":2`},
		"future":       {`"version":3`, `"version":4`},
		"missing-auto": {`,"automatic_cleanup":true`, ""},
		"false-auto":   {`"automatic_cleanup":true`, `"automatic_cleanup":false`},
		"null-auto":    {`"automatic_cleanup":true`, `"automatic_cleanup":null`},
		"numeric-auto": {`"automatic_cleanup":true`, `"automatic_cleanup":1`},
	} {
		t.Run(name, func(t *testing.T) {
			bad := strings.Replace(wire, edit[0], edit[1], 1)
			require.NotEqual(t, wire, bad)
			_, err := parseColdQuotaCleanupManifest([]byte(bad))
			require.ErrorIs(t, err, errConfiguration)
		})
	}

	// Both mounted constructors still pass through the original parser. Even
	// selecting version 3 there must refuse it until its own worker is wired.
	for _, version := range []int{1, 2, 3} {
		_, err := parseCleanupManifestVersion([]byte(wire), version)
		require.ErrorIs(t, err, errConfiguration)
	}
	profileless := strings.Replace(automaticCleanupManifestTestWire(), `"version":2`, `"version":3`, 1)
	_, err := parseCleanupManifest([]byte(profileless))
	require.ErrorIs(t, err, errConfiguration)

	for _, legacy := range []string{cleanupManifestTestWire, automaticCleanupManifestTestWire()} {
		_, err := parseCleanupManifest([]byte(legacy))
		require.NoError(t, err)
		_, err = parseColdQuotaCleanupManifest([]byte(legacy))
		require.ErrorIs(t, err, errConfiguration)
		bad := strings.Replace(legacy, `"launcher_config_file":`, `"preparation":{"quota_slots":[1]},"launcher_config_file":`, 1)
		_, err = parseCleanupManifest([]byte(bad))
		require.ErrorIs(t, err, errConfiguration)
	}
}
