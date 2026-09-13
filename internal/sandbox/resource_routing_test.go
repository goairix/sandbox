package sandbox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCreateRoutesResourcesAgainstPreparedPool(t *testing.T) {
	for _, tc := range []struct {
		name      string
		resources ResourceLimits
		warm      bool
	}{
		{name: "absent", warm: true},
		{name: "equivalent", resources: ResourceLimits{Memory: "0.5Gi", CPU: "0.5", Disk: "1024Mi", TmpDisk: "51200Ki"}, warm: true},
		{name: "memory", resources: ResourceLimits{Memory: "1Gi"}},
		{name: "cpu", resources: ResourceLimits{CPU: "1"}},
		{name: "disk", resources: ResourceLimits{Disk: "2Gi"}},
		{name: "tmp disk", resources: ResourceLimits{TmpDisk: "256Mi"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := newMockRuntime()
			mgr := NewManager(rt, nil, nil, ManagerConfig{PoolConfig: PoolConfig{Memory: "512Mi", MemoryRequest: "128Mi", CPU: "500m", CPURequest: "100m", Disk: "1Gi"}})
			t.Cleanup(mgr.cancelControl)
			warm, err := mgr.pool.createWarm(context.Background())
			require.NoError(t, err)
			mgr.pool.available = append(mgr.pool.available, warm)
			// Prepared inventory follows Pool's config, not a mutable manager copy.
			mgr.config.PoolConfig.Memory = "4Gi"
			sb, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, Resources: tc.resources})
			require.NoError(t, err)
			if tc.warm {
				require.Equal(t, warm.RuntimeID, sb.RuntimeID)
			} else {
				require.NotEqual(t, warm.RuntimeID, sb.RuntimeID, "incompatible resource request must bypass warm inventory")
				spec := rt.lastCreatedSpec()
				if tc.resources.Memory != "" {
					require.Equal(t, tc.resources.Memory, spec.Memory)
				}
				if tc.resources.CPU != "" {
					require.Equal(t, tc.resources.CPU, spec.CPU)
				}
				if tc.resources.Disk != "" {
					require.Equal(t, tc.resources.Disk, spec.Disk)
				}
				if tc.resources.TmpDisk != "" {
					require.Equal(t, tc.resources.TmpDisk, spec.TmpDisk)
				}
				if tc.resources.Memory == "" {
					require.Equal(t, "512Mi", spec.Memory)
					require.Equal(t, "128Mi", spec.MemoryRequest)
				}
				if tc.resources.CPU == "" {
					require.Equal(t, "500m", spec.CPU)
					require.Equal(t, "100m", spec.CPURequest)
				}
				if tc.resources.Disk == "" {
					require.Equal(t, "1Gi", spec.Disk)
				}
			}
		})
	}
}

func TestCreateNetworkDirectFillsPreparedResourceDefaults(t *testing.T) {
	rt := newMockRuntime()
	mgr := NewManager(rt, nil, nil, ManagerConfig{PoolConfig: PoolConfig{Memory: "512Mi", MemoryRequest: "128Mi", CPU: "500m", CPURequest: "100m", Disk: "1Gi", TmpDisk: "256Mi"}})
	t.Cleanup(mgr.cancelControl)
	_, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, Network: NetworkConfig{Enabled: true}})
	require.NoError(t, err)
	spec := rt.lastCreatedSpec()
	require.Equal(t, "512Mi", spec.Memory)
	require.Equal(t, "128Mi", spec.MemoryRequest)
	require.Equal(t, "500m", spec.CPU)
	require.Equal(t, "100m", spec.CPURequest)
	require.Equal(t, "1Gi", spec.Disk)
	require.Equal(t, "256Mi", spec.TmpDisk)
}
