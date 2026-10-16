package etcd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDomainDetachedIdentityIsolation(t *testing.T) {
	w, err := NewDetachedIdentity("storage", "operator", "key")
	require.NoError(t, err)
	require.NoError(t, w.validate())
	require.Equal(t, WorkspaceIdentityDetached, w.Kind())
	again, err := NewDetachedIdentity("storage", "operator", "key")
	require.NoError(t, err)
	require.Equal(t, w, again)
	for _, values := range [][3]string{{"other", "operator", "key"}, {"storage", "other", "key"}, {"storage", "operator", "other"}, {"storage", "operatork", "ey"}} {
		other, err := NewDetachedIdentity(values[0], values[1], values[2])
		require.NoError(t, err)
		require.NotEqual(t, w.Hash(), other.Hash())
	}
	workspace, err := NewWorkspaceIdentity("minio", "store", "bucket", "work/")
	require.NoError(t, err)
	require.Equal(t, WorkspaceIdentityWorkspace, workspace.Kind())
	require.Equal(t, "1bab2aa6b5393553247aaca84415eed0c40a14e1436b8b152ea171249cd920dc", workspace.Hash())
	require.Equal(t, uint8(0x1b), workspace.Partition())
	require.NotEqual(t, workspace.Hash(), w.Hash())
	require.NoError(t, validateWorkspaceSyncIdentity(workspace))
	require.ErrorIs(t, validateWorkspaceSyncIdentity(w), ErrInvalidRecord)
	_, err = NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspace})
	require.NoError(t, err, "the corresponding original workspace plan is valid")
	_, err = NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: w})
	require.ErrorIs(t, err, ErrInvalidRecord, "detached identity cannot acquire workspace sync semantics")
}

func TestDomainDetachedIdentityBoundsAndForgery(t *testing.T) {
	for _, values := range [][3]string{{"", "operator", "key"}, {"storage", "", "key"}, {"storage", "operator", ""}, {strings.Repeat("s", 129), "operator", "key"}, {"storage", strings.Repeat("p", 129), "key"}, {"storage", "operator", strings.Repeat("k", 257)}, {"storage", "bad\n", "key"}, {"storage", "operator", "\xff"}} {
		_, err := NewDetachedIdentity(values[0], values[1], values[2])
		require.ErrorIs(t, err, ErrInvalidRecord)
	}
	_, err := NewDetachedIdentity(strings.Repeat("s", 128), strings.Repeat("p", 128), strings.Repeat("k", 256))
	require.NoError(t, err)
	w, err := NewDetachedIdentity("storage", "operator", "key")
	require.NoError(t, err)
	for name, mutate := range map[string]func(*WorkspaceIdentity){
		"kind":           func(v *WorkspaceIdentity) { v.kind = WorkspaceIdentityWorkspace },
		"unknown kind":   func(v *WorkspaceIdentity) { v.kind = WorkspaceIdentityKind(255) },
		"provider graft": func(v *WorkspaceIdentity) { v.provider = "s3" },
		"bucket graft":   func(v *WorkspaceIdentity) { v.bucket = "bucket" },
		"prefix graft":   func(v *WorkspaceIdentity) { v.prefix = "work/" },
		"request hash":   func(v *WorkspaceIdentity) { v.requestHash = strings.Repeat("f", 64) },
		"storage hash":   func(v *WorkspaceIdentity) { v.storageIdentityHash = strings.Repeat("f", 64) },
		"hash":           func(v *WorkspaceIdentity) { v.hash = strings.Repeat("f", 64) },
		"partition":      func(v *WorkspaceIdentity) { v.partition++ },
	} {
		t.Run(name, func(t *testing.T) {
			bad := w
			mutate(&bad)
			require.ErrorIs(t, bad.validate(), ErrInvalidRecord)
		})
	}
	workspace, err := NewWorkspaceIdentity("s3", "storage", "bucket", "work/")
	require.NoError(t, err)
	workspace.requestHash = w.requestHash
	require.ErrorIs(t, workspace.validate(), ErrInvalidRecord, "workspace cannot retain detached attribution")
}

func TestDomainDetachedIdentityBackendBinding(t *testing.T) {
	ns, err := NewNamespace("/sandbox", "operator", "cell")
	require.NoError(t, err)
	wire, err := json.Marshal(Identity{SchemaVersion: 1, Prefix: "/sandbox", AuthorityID: "operator", Cell: "cell", ClusterID: 1, StorageID: "bound-storage", RuntimeID: "launcher"})
	require.NoError(t, err)
	b := &Backend{namespace: ns, identityValue: string(wire)}
	w, err := b.DetachedIdentity("principal", "key")
	require.NoError(t, err)
	require.NoError(t, b.validateWorkspaceBinding(w), "binding uses the Backend's original operator identity")
	foreign, err := NewDetachedIdentity("foreign-storage", "principal", "key")
	require.NoError(t, err)
	require.ErrorIs(t, b.validateWorkspaceBinding(foreign), ErrIdentityMismatch)
	b.identityValue = "{}"
	_, err = b.DetachedIdentity("principal", "key")
	require.Error(t, err)
}
