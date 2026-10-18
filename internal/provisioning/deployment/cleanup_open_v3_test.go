package deployment

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/stretchr/testify/require"
)

// These tests inspect mounted configuration and startup refusals only. They do
// not establish inventory readiness, a running worker or physical quota return.
func TestCleanupOpenManifestExplicitVersion(t *testing.T) {
	wires := map[int]string{
		1: cleanupManifestTestWire,
		2: automaticCleanupManifestTestWire(),
		3: coldQuotaCleanupManifestTestWire(coldQuotaCleanupTestCounts()),
	}
	for requested, wire := range wires {
		for selected := 1; selected <= 3; selected++ {
			m, err := parseCleanupOpenManifest([]byte(wire), selected)
			if requested != selected {
				require.ErrorIs(t, err, errConfiguration, "entry version %d accepted manifest %d", selected, requested)
				continue
			}

			require.NoError(t, err)
			require.Equal(t, requested, m.Version)
			require.Equal(t, requested != 1, m.workerOptions().AutomaticCleanup)
		}
	}

	for _, selected := range []int{0, 4} {
		_, err := parseCleanupOpenManifest([]byte(wires[3]), selected)
		require.ErrorIs(t, err, errConfiguration, "unsupported entry version must not fall back")
	}

	disabled := strings.Replace(wires[3], `"automatic_cleanup":true`, `"automatic_cleanup":false`, 1)
	_, err := parseCleanupOpenManifest([]byte(disabled), 3)
	require.ErrorIs(t, err, errConfiguration, "cold mode must not override disabled automatic cleanup")
}

func TestColdQuotaCleanupOpenOptionsOwnProfile(t *testing.T) {
	m, err := parseCleanupOpenManifest([]byte(coldQuotaCleanupManifestTestWire(coldQuotaCleanupTestCounts())), 3)
	require.NoError(t, err)
	namespace, err := state.NewNamespace("/sandbox", "observer", "v1")
	require.NoError(t, err)

	options := m.backendOptions(namespace, launcherconfig.Config{}, nil, nil, &LocalCleanupDeployment{})
	require.NotNil(t, options.PreparationInventory)
	require.NotNil(t, options.LocalCleanup)
	require.True(t, options.LocalCleanup.ColdRuntimeQuotaReturn)
	require.Equal(t, uint32(1), options.PreparationInventory.QuotaSlots[0])
	require.Equal(t, uint32(40), options.PreparationInventory.QuotaSlots[7])
	require.Equal(t, uint32(0), options.PreparationInventory.QuotaSlots[254])
	require.Equal(t, uint32(1000000), options.PreparationInventory.QuotaSlots[255])

	// A backend receives its own profile value. Later configuration reuse and
	// option mutation must not change each other's expected inventory digest.
	m.Inventory.QuotaSlots[7] = 41
	require.Equal(t, uint32(40), options.PreparationInventory.QuotaSlots[7])
	options.PreparationInventory.QuotaSlots[255] = 999999
	require.Equal(t, uint32(1000000), m.Inventory.QuotaSlots[255])

	// The v3 adapter must retain the original common configuration projection.
	require.Equal(t, []string{"https://127.0.0.1:2379"}, options.Endpoints)
	require.Equal(t, "epoch", options.Identity.RestoreEpoch)
	require.Equal(t, 60*time.Second, options.WorkerControl.SessionTTL)
	require.Equal(t, "/var/lib/cleanup/target", options.LocalCleanup.Storage.Directory)
	require.Equal(t, "node-a", options.LocalCleanup.Nodes[0].Name)
	require.Equal(t, "/var/lib/cleanup/spool", options.TaskWorkspaceArchiveRecovery.Directory)

	worker := m.workerOptions()
	require.True(t, worker.AutomaticCleanup)
	require.Equal(t, "cleanup-worker", worker.WorkerID)
	require.Equal(t, []uint8{7}, worker.Partitions)
	require.Equal(t, []uint16{0}, worker.RangeSlots)
	require.Equal(t, []uint16{0, 1}, worker.ClaimSlots)
	require.Equal(t, uint16(0), worker.RelistSlot)
	require.Equal(t, 30*time.Second, worker.StageTTL)
}

// Configuration is projected before Backend.New; it cannot be injected into
// an already running worker or inferred merely from enabling cold quotas.
func TestColdQuotaCleanupOpenOptionsOwnDetachedProfile(t *testing.T) {
	base := coldQuotaCleanupManifestTestWire(coldQuotaCleanupTestCounts())
	image := "registry.example/sandbox:v1@sha256:" + strings.Repeat("a", 64)
	wire := []byte(strings.Replace(base, `"version":3`, `"version":3,"detached":{"pinned_plain_image":"`+image+`"}`, 1))
	m, err := parseCleanupOpenManifest(wire, 3)
	require.NoError(t, err)
	clear(wire)
	first := m.backendOptions(state.Namespace{}, launcherconfig.Config{}, nil, nil, &LocalCleanupDeployment{})
	second := m.backendOptions(state.Namespace{}, launcherconfig.Config{}, nil, nil, &LocalCleanupDeployment{})
	require.NotNil(t, first.DetachedCleanup)
	require.NotNil(t, second.DetachedCleanup)
	require.Equal(t, image, first.DetachedCleanup.PinnedPlainImage)
	first.DetachedCleanup.PinnedPlainImage = "changed"
	require.Equal(t, image, second.DetachedCleanup.PinnedPlainImage, "each open owns its option value")
	require.True(t, second.LocalCleanup.ColdRuntimeQuotaReturn)
	require.True(t, m.workerOptions().AutomaticCleanup)

	local, err := parseCleanupOpenManifest([]byte(base), 3)
	require.NoError(t, err)
	require.Nil(t, local.backendOptions(state.Namespace{}, launcherconfig.Config{}, nil, nil, &LocalCleanupDeployment{}).DetachedCleanup)
}

func TestLegacyCleanupOpenOptionsDisableColdQuota(t *testing.T) {
	for version, wire := range map[int]string{1: cleanupManifestTestWire, 2: automaticCleanupManifestTestWire()} {
		m, err := parseCleanupOpenManifest([]byte(wire), version)
		require.NoError(t, err)

		options := m.backendOptions(state.Namespace{}, launcherconfig.Config{}, nil, nil, &LocalCleanupDeployment{})
		require.Nil(t, options.PreparationInventory)
		require.NotNil(t, options.LocalCleanup)
		require.False(t, options.LocalCleanup.ColdRuntimeQuotaReturn)
		require.Equal(t, version == 2, m.workerOptions().AutomaticCleanup)
	}
}

func TestOpenColdQuotaAutomaticLocalCleanupRefusal(t *testing.T) {
	// The path deliberately does not exist. Admission errors must win before
	// mounted-file access and must not disclose the supplied path.
	const path = "/missing/secret-cold-quota-manifest.json"
	d, err := OpenColdQuotaAutomaticLocalCleanup(nil, path, nil)
	require.ErrorIs(t, err, errConfiguration)
	require.Nil(t, d)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d, err = OpenColdQuotaAutomaticLocalCleanup(ctx, path, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, d)

	d, err = OpenColdQuotaAutomaticLocalCleanup(context.Background(), path, nil)
	require.ErrorIs(t, err, errConfiguration)
	require.NotContains(t, err.Error(), "secret")
	require.Nil(t, d)
}
