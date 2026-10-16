package sandboxsnapshot

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/goairix/sandbox/pkg/types"
	"github.com/stretchr/testify/require"
)

func snapshotDefaults() Defaults {
	return Defaults{TimeoutSeconds: 60, Resources: types.ResourceLimits{CPU: "2", Memory: "512Mi", Disk: "1Mi", TmpDisk: "50Mi"}}
}

func TestBusinessSnapshotFullRequestAndOwnership(t *testing.T) {
	request := types.CreateSandboxRequest{
		Mode: "persistent", Timeout: 90,
		Resources:     &types.ResourceLimits{CPU: "2000m", Memory: "1Gi", Disk: "2Gi", TmpDisk: "100Mi"},
		Network:       &types.NetworkConfig{Enabled: true, Whitelist: []string{"b.example", "a.example"}, BlockPrivate: true},
		Dependencies:  []types.DependencySpec{{Name: "numpy", Version: "2.1", Manager: "pip"}, {Name: "left-pad", Version: "1", Manager: "npm"}},
		WorkspacePath: "team/project/", WorkspaceMountMode: types.WorkspaceMountFUSE,
		WorkspaceSyncExclude: []string{"z/**", "a/**"},
	}
	snapshot, err := Build(request, snapshotDefaults())
	require.NoError(t, err)
	require.Equal(t, request, snapshot.Requested)
	require.Equal(t, TTL{Kind: "finite", Seconds: 90}, snapshot.Resolved.TTL)
	require.Equal(t, *request.Resources, snapshot.Resolved.Resources)
	require.Equal(t, types.WorkspaceMountFUSE, snapshot.Resolved.WorkspaceMountMode)
	wire, err := Encode(snapshot)
	require.NoError(t, err)
	decoded, err := Decode(wire)
	require.NoError(t, err)
	require.Equal(t, snapshot, decoded)
	request.Resources.CPU, request.Network.Whitelist[0], request.Dependencies[0].Name, request.WorkspaceSyncExclude[0] = "1", "changed", "changed", "changed"
	require.Equal(t, "2000m", snapshot.Requested.Resources.CPU)
	require.Equal(t, "b.example", snapshot.Requested.Network.Whitelist[0])
	require.Equal(t, "numpy", snapshot.Requested.Dependencies[0].Name)
	require.Equal(t, "z/**", snapshot.Requested.WorkspaceSyncExclude[0])
	decoded.Requested.Network.Whitelist[0] = "decoded"
	clear(wire)
	require.Equal(t, "b.example", snapshot.Requested.Network.Whitelist[0])
	require.Equal(t, "2000m", decoded.Requested.Resources.CPU)
}

func TestBusinessSnapshotDefaultsFreezeAndNormalize(t *testing.T) {
	request := types.CreateSandboxRequest{Mode: "ephemeral", WorkspacePath: "workspace/"}
	first, err := Build(request, snapshotDefaults())
	require.NoError(t, err)
	otherDefaults := snapshotDefaults()
	otherDefaults.TimeoutSeconds, otherDefaults.Resources.CPU, otherDefaults.WorkspaceMountMode = 120, "4", types.WorkspaceMountFUSE
	second, err := Build(request, otherDefaults)
	require.NoError(t, err)
	require.Equal(t, first.RequestedDigest, second.RequestedDigest)
	require.NotEqual(t, first.Resolved, second.Resolved)
	wire, err := Encode(first)
	require.NoError(t, err)
	decoded, err := Decode(wire)
	require.NoError(t, err)
	require.Equal(t, first, decoded)
	require.Equal(t, types.WorkspaceMountSync, decoded.Resolved.WorkspaceMountMode)
	bare, err := Build(types.CreateSandboxRequest{Mode: "ephemeral"}, snapshotDefaults())
	require.NoError(t, err)
	empty, err := Build(types.CreateSandboxRequest{Mode: "ephemeral", Resources: &types.ResourceLimits{}, Network: &types.NetworkConfig{Whitelist: []string{}}, Dependencies: []types.DependencySpec{}, WorkspaceSyncExclude: []string{}}, snapshotDefaults())
	require.NoError(t, err)
	require.Equal(t, bare, empty)
	withPolicy, err := Build(types.CreateSandboxRequest{Mode: "ephemeral", Network: &types.NetworkConfig{BlockPrivate: true}}, snapshotDefaults())
	require.NoError(t, err)
	require.NotNil(t, withPolicy.Requested.Network, "disabled network policy must still be preserved")
	defaults := snapshotDefaults()
	defaults.Resources.TmpDisk = ""
	fallback, err := Build(types.CreateSandboxRequest{Mode: "ephemeral"}, defaults)
	require.NoError(t, err)
	require.Equal(t, "50Mi", fallback.Resolved.Resources.TmpDisk)
}

func TestBusinessSnapshotValidation(t *testing.T) {
	for name, mutate := range map[string]func(*types.CreateSandboxRequest, *Defaults){
		"mode":             func(r *types.CreateSandboxRequest, _ *Defaults) { r.Mode = "unknown" },
		"negative timeout": func(r *types.CreateSandboxRequest, _ *Defaults) { r.Timeout = -2 },
		"overflow timeout": func(r *types.CreateSandboxRequest, _ *Defaults) {
			r.Timeout = int(math.MaxInt64/int64(time.Second)) + 1
		},
		"zero default":            func(_ *types.CreateSandboxRequest, d *Defaults) { d.TimeoutSeconds = 0 },
		"negative default":        func(_ *types.CreateSandboxRequest, d *Defaults) { d.TimeoutSeconds = -2 },
		"bad quantity":            func(r *types.CreateSandboxRequest, _ *Defaults) { r.Resources = &types.ResourceLimits{CPU: "bogus"} },
		"zero quantity":           func(r *types.CreateSandboxRequest, _ *Defaults) { r.Resources = &types.ResourceLimits{Memory: "0"} },
		"negative quantity":       func(r *types.CreateSandboxRequest, _ *Defaults) { r.Resources = &types.ResourceLimits{Disk: "-1Gi"} },
		"missing default":         func(_ *types.CreateSandboxRequest, d *Defaults) { d.Resources.CPU = "" },
		"mount without workspace": func(r *types.CreateSandboxRequest, _ *Defaults) { r.WorkspaceMountMode = types.WorkspaceMountSync },
		"unknown mount": func(r *types.CreateSandboxRequest, _ *Defaults) {
			r.WorkspacePath, r.WorkspaceMountMode = "w", "unknown"
		},
		"unknown default mount": func(r *types.CreateSandboxRequest, d *Defaults) {
			r.WorkspacePath, d.WorkspaceMountMode = "w", "unknown"
		},
		"dependency name": func(r *types.CreateSandboxRequest, _ *Defaults) {
			r.Dependencies = []types.DependencySpec{{Manager: "pip"}}
		},
		"dependency manager": func(r *types.CreateSandboxRequest, _ *Defaults) {
			r.Dependencies = []types.DependencySpec{{Name: "x", Manager: "apt"}}
		},
		"UTF8": func(r *types.CreateSandboxRequest, _ *Defaults) { r.WorkspacePath = "\xff" },
	} {
		t.Run(name, func(t *testing.T) {
			request, defaults := types.CreateSandboxRequest{Mode: "ephemeral"}, snapshotDefaults()
			mutate(&request, &defaults)
			_, err := Build(request, defaults)
			require.Error(t, err)
		})
	}
	for _, requested := range []int{-1, 0} {
		defaults := snapshotDefaults()
		defaults.TimeoutSeconds = -1
		permanent, err := Build(types.CreateSandboxRequest{Mode: "persistent", Timeout: requested}, defaults)
		require.NoError(t, err)
		require.Equal(t, TTL{Kind: "permanent"}, permanent.Resolved.TTL)
	}
	maximum, err := Build(types.CreateSandboxRequest{Mode: "ephemeral", Timeout: int(math.MaxInt64 / int64(time.Second))}, snapshotDefaults())
	require.NoError(t, err)
	require.Equal(t, int64(math.MaxInt64/int64(time.Second)), maximum.Resolved.TTL.Seconds)
}

func TestBusinessSnapshotBoundedInputsAndWire(t *testing.T) {
	base, err := Build(types.CreateSandboxRequest{Mode: "ephemeral", WorkspacePath: "x"}, snapshotDefaults())
	require.NoError(t, err)
	baseWire, err := Encode(base)
	require.NoError(t, err)
	pathSize := MaxBytes - len(baseWire) + 1
	boundary, err := Build(types.CreateSandboxRequest{Mode: "ephemeral", WorkspacePath: strings.Repeat("x", pathSize)}, snapshotDefaults())
	require.NoError(t, err)
	wire, err := Encode(boundary)
	require.NoError(t, err)
	require.Len(t, wire, MaxBytes)
	_, err = Decode(wire)
	require.NoError(t, err)
	_, err = Build(types.CreateSandboxRequest{Mode: "ephemeral", WorkspacePath: strings.Repeat("x", pathSize+1)}, snapshotDefaults())
	require.Error(t, err)
	for name, request := range map[string]types.CreateSandboxRequest{
		"huge string":        {Mode: "ephemeral", WorkspacePath: strings.Repeat("x", 4*MaxBytes)},
		"many empty strings": {Mode: "ephemeral", WorkspaceSyncExclude: make([]string, MaxBytes)},
		"many dependencies":  {Mode: "ephemeral", Dependencies: make([]types.DependencySpec, MaxBytes)},
		"escaped expansion":  {Mode: "ephemeral", WorkspacePath: strings.Repeat("<", MaxBytes/3)},
	} {
		t.Run(name, func(t *testing.T) { _, err := Build(request, snapshotDefaults()); require.Error(t, err) })
	}
	defaults := snapshotDefaults()
	defaults.Resources.Memory = strings.Repeat("9", 4*MaxBytes)
	_, err = Build(types.CreateSandboxRequest{Mode: "ephemeral"}, defaults)
	require.Error(t, err)
}
