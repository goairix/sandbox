package provisioning

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	"github.com/goairix/sandbox/internal/sandboxsnapshot"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/goairix/sandbox/pkg/types"
	"github.com/stretchr/testify/require"
)

func coldBusinessFixture(t *testing.T) (*PlainColdService, state.WorkspaceIdentity, PlainColdRequest) {
	t.Helper()
	w, err := state.NewWorkspaceIdentity("s3", "storage", "bucket", "workspace/")
	require.NoError(t, err)
	s := &PlainColdService{profile: strings.Repeat("a", 64), spec: runtime.SandboxSpec{CPU: "2", Memory: "512Mi", Disk: "1Mi", TmpDisk: "50Mi"}, config: launcherconfig.Config{}, limit: 1, active: make(map[*plainColdCall]struct{})}
	s.self = s
	r := PlainColdRequest{Principal: "principal", IdempotencyKey: "key", RequestID: "00000000-0000-4000-8000-000000000001", IntentID: "00000000-0000-4000-8000-000000000002", SandboxID: "00000000-0000-4000-8000-000000000003", SnapshotVersion: "00000000-0000-4000-8000-000000000004", TTL: time.Minute}
	return s, w, r
}

func coldBusinessSnapshot(t *testing.T, request types.CreateSandboxRequest) *sandboxsnapshot.Snapshot {
	t.Helper()
	snapshot, err := sandboxsnapshot.Build(request, sandboxsnapshot.Defaults{TimeoutSeconds: 60, Resources: types.ResourceLimits{CPU: "2", Memory: "512Mi", Disk: "1Mi", TmpDisk: "50Mi"}})
	require.NoError(t, err)
	return snapshot
}

func TestPlainColdBusinessSnapshotVersions(t *testing.T) {
	s, w, request := coldBusinessFixture(t)
	v1, err := coldRequestCapsule(s.profile, w, request)
	require.NoError(t, err)
	expected := `{"version":1,"deployment":"` + s.profile + `","workspace_hash":"` + w.Hash() + `","partition":` + jsonNumber(t, w.Partition()) + `,"request_id":"00000000-0000-4000-8000-000000000001","intent_id":"00000000-0000-4000-8000-000000000002","sandbox_id":"00000000-0000-4000-8000-000000000003","snapshot_version":"00000000-0000-4000-8000-000000000004","ttl":60000000000}`
	require.Equal(t, expected, string(v1), "v1 exact original bytes and field order")
	request.Business = coldBusinessSnapshot(t, types.CreateSandboxRequest{Mode: "ephemeral", Resources: &types.ResourceLimits{CPU: "2000m"}})
	v2, err := coldRequestCapsule(s.profile, w, request)
	require.NoError(t, err)
	var capsule plainColdCapsule
	require.NoError(t, decodeColdCapsule(v2, &capsule))
	require.Equal(t, 2, capsule.Version)
	require.NoError(t, s.validateColdBusiness(capsule))
	business, err := sandboxsnapshot.Decode(capsule.Business)
	require.NoError(t, err)
	require.Equal(t, request.Business, business)
	request.Business.Requested.Resources.CPU = "1"
	require.Equal(t, "2000m", business.Requested.Resources.CPU)
	for name, bad := range map[string][]byte{
		"v1 with business":    bytes.Replace(v2, []byte(`"version":2`), []byte(`"version":1`), 1),
		"v2 missing business": bytes.Replace(v1, []byte(`"version":1`), []byte(`"version":2`), 1),
		"v2 null business":    append(bytes.Clone(v2[:bytes.Index(v2, []byte(`,"business":`))]), []byte(`,"business":null}`)...),
		"v3":                  bytes.Replace(v2, []byte(`"version":2`), []byte(`"version":3`), 1),
		"duplicate":           bytes.Replace(v2, []byte(`"version":2`), []byte(`"version":2,"version":2`), 1),
		"bad business":        bytes.Replace(v2, []byte(`"requested_digest":"`), []byte(`"requested_digest":"f`), 1),
		"bad ID":              bytes.Replace(v2, []byte(request.IntentID), []byte("bad"), 1),
		"bad TTL":             bytes.Replace(v2, []byte(`"ttl":60000000000`), []byte(`"ttl":0`), 1),
	} {
		t.Run(name, func(t *testing.T) { var value plainColdCapsule; require.Error(t, decodeColdCapsule(bad, &value)) })
	}
}

func jsonNumber(t *testing.T, value uint8) string {
	t.Helper()
	wire, err := json.Marshal(value)
	require.NoError(t, err)
	return string(wire)
}

func TestPlainColdBusinessSnapshotRejectBeforeEffects(t *testing.T) {
	for name, mutate := range map[string]func(*types.CreateSandboxRequest){
		"permanent": func(r *types.CreateSandboxRequest) { r.Timeout = -1 },
		"TTL":       func(r *types.CreateSandboxRequest) { r.Timeout = 61 },
		"CPU":       func(r *types.CreateSandboxRequest) { r.Resources = &types.ResourceLimits{CPU: "3"} },
		"memory":    func(r *types.CreateSandboxRequest) { r.Resources = &types.ResourceLimits{Memory: "1Gi"} },
		"disk":      func(r *types.CreateSandboxRequest) { r.Resources = &types.ResourceLimits{Disk: "2Mi"} },
		"tmp":       func(r *types.CreateSandboxRequest) { r.Resources = &types.ResourceLimits{TmpDisk: "1Mi"} },
		"network":   func(r *types.CreateSandboxRequest) { r.Network = &types.NetworkConfig{Enabled: true} },
		"whitelist disabled": func(r *types.CreateSandboxRequest) {
			r.Network = &types.NetworkConfig{Whitelist: []string{"example.org"}}
		},
		"private disabled":          func(r *types.CreateSandboxRequest) { r.Network = &types.NetworkConfig{BlockPrivate: true} },
		"workspace":                 func(r *types.CreateSandboxRequest) { r.WorkspacePath = "workspace/" },
		"exclude without workspace": func(r *types.CreateSandboxRequest) { r.WorkspaceSyncExclude = []string{"*.tmp"} },
		"dependencies": func(r *types.CreateSandboxRequest) {
			r.Dependencies = []types.DependencySpec{{Name: "numpy", Manager: "pip"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, w, request := coldBusinessFixture(t)
			business := types.CreateSandboxRequest{Mode: "ephemeral"}
			mutate(&business)
			request.Business = coldBusinessSnapshot(t, business)
			// Backend, clock and drivers are deliberately absent. Any deployment
			// check, Grant or POST reached before rejection makes this test panic.
			_, err := s.Create(context.Background(), w, request)
			require.ErrorIs(t, err, ErrPlainColdProfile)
			require.Empty(t, s.active)
		})
	}
	s, w, request := coldBusinessFixture(t)
	request.Business = coldBusinessSnapshot(t, types.CreateSandboxRequest{Mode: "ephemeral"})
	request.Business.Resolved.Resources.CPU = "3"
	_, err := s.Create(context.Background(), w, request)
	require.ErrorIs(t, err, ErrPlainColdProfile, "structurally valid forged defaults must match actual provider")
	request.Business.RequestedDigest = "forged"
	_, err = s.Create(context.Background(), w, request)
	require.Error(t, err, "typed snapshot is revalidated before effects")
}
