package deployment

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCleanupManifestEntrypointVersion(t *testing.T) {
	for _, version := range []int{1, 2} {
		wire := cleanupManifestTestWire
		if version == 2 {
			wire = automaticCleanupManifestTestWire()
		}
		m, err := parseCleanupManifestVersion([]byte(wire), version)
		require.NoError(t, err)
		require.Equal(t, version, m.Version)
		_, err = parseCleanupManifestVersion([]byte(wire), 3-version)
		require.ErrorIs(t, err, errConfiguration)
	}
}

func TestOpenAutomaticLocalCleanupRefusal(t *testing.T) {
	// Neither malformed configuration nor a caller-created Task reference can
	// substitute for the genuine process boundary required by this entry.
	d, err := OpenAutomaticLocalCleanup(nil, "", nil)
	require.ErrorIs(t, err, errConfiguration)
	require.Nil(t, d)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d, err = OpenAutomaticLocalCleanup(ctx, "", nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, d)
	d, err = OpenAutomaticLocalCleanup(context.Background(), "", nil)
	require.ErrorIs(t, err, errConfiguration)
	require.Nil(t, d)
}

func automaticCleanupManifestTestWire() string {
	wire := strings.Replace(cleanupManifestTestWire, `"version":1`, `"version":2`, 1)
	return strings.Replace(wire, `"worker_id":"cleanup-worker"`, `"worker_id":"cleanup-worker","automatic_cleanup":true`, 1)
}

func TestAutomaticLocalCleanupManifestStrict(t *testing.T) {
	wire := automaticCleanupManifestTestWire()
	m, err := parseCleanupManifest([]byte(wire))
	require.NoError(t, err)
	require.Equal(t, 2, m.Version)
	// A valid v2 document is the behavior under test. Every refusal below
	// must retain original strict v1 field presence, shape and duplicate checks.
	for name, bad := range map[string]string{
		"missing-auto":    strings.Replace(wire, `,"automatic_cleanup":true`, "", 1),
		"disabled-auto":   strings.Replace(wire, `"automatic_cleanup":true`, `"automatic_cleanup":false`, 1),
		"null-auto":       strings.Replace(wire, `"automatic_cleanup":true`, `"automatic_cleanup":null`, 1),
		"numeric-auto":    strings.Replace(wire, `"automatic_cleanup":true`, `"automatic_cleanup":1`, 1),
		"duplicate-auto":  strings.Replace(wire, `"automatic_cleanup":true`, `"automatic_cleanup":true,"automatic_cleanup":true`, 1),
		"wrong-case":      strings.Replace(wire, `"automatic_cleanup":true`, `"Automatic_cleanup":true`, 1),
		"unknown":         strings.Replace(wire, `"automatic_cleanup":true`, `"automatic_cleanup":true,"credential":"secret-sentinel"`, 1),
		"missing-share":   strings.Replace(wire, `"relist_slot":0,`, "", 1),
		"missing-path":    strings.Replace(wire, `"prefix_base":"",`, "", 1),
		"null-path":       strings.Replace(wire, `"prefix_base":""`, `"prefix_base":null`, 1),
		"null-worker":     strings.Replace(wire, `"worker":{"worker_id":"cleanup-worker","automatic_cleanup":true,"partitions":[7],"range_slots":[0],"claim_slots":[0,1],"relist_slot":0,"session_ttl":"60s","stage_ttl":"30s"}`, `"worker":null`, 1),
		"v1-auto-true":    strings.Replace(wire, `"version":2`, `"version":1`, 1),
		"v1-auto-false":   strings.Replace(strings.Replace(wire, `"version":2`, `"version":1`, 1), `"automatic_cleanup":true`, `"automatic_cleanup":false`, 1),
		"unknown-version": strings.Replace(wire, `"version":2`, `"version":3`, 1),
		"trailing":        wire + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			require.NotEqual(t, wire, bad)
			_, err := parseCleanupManifest([]byte(bad))
			require.ErrorIs(t, err, errConfiguration)
			require.NotContains(t, err.Error(), "secret-sentinel")
		})
	}
}
