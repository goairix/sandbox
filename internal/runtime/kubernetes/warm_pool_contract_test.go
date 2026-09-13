package kubernetes

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"

	sandboxruntime "github.com/goairix/sandbox/internal/runtime"
)

func TestWarmPoolContractIncludesNamespaceAndNetworkContract(t *testing.T) {
	base := &Runtime{namespace: "sandbox", hasCilium: true}
	key := base.WarmPoolContract()
	for _, tc := range []struct {
		name    string
		runtime *Runtime
		equal   bool
	}{
		{"operational_timeout", &Runtime{namespace: "sandbox", hasCilium: true, pollInterval: time.Second, terminationTimeout: time.Minute}, true},
		{"namespace", &Runtime{namespace: "other", hasCilium: true}, false},
		{"network_policy_backend", &Runtime{namespace: "sandbox", hasCilium: false}, false},
	} {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.equal, key == tc.runtime.WarmPoolContract()) })
	}
}

func TestRemoveOrdinarySandboxDoesNotDeleteReplacementUID(t *testing.T) {
	ctx := context.Background()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "reused-name", Namespace: "sandbox", UID: types.UID("new-uid")}}
	client := kubefake.NewSimpleClientset(pod)
	rt := &Runtime{client: client, namespace: "sandbox"}
	err := rt.RemoveOrdinarySandbox(ctx, sandboxruntime.RuntimeRef{ID: pod.Name, UID: "old-uid"})
	require.ErrorIs(t, err, sandboxruntime.ErrNotFound)
	remaining, err := client.CoreV1().Pods("sandbox").Get(ctx, pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, pod.UID, remaining.UID)
}

func TestResolvePreparedSandboxValidatesExactPreparationContract(t *testing.T) {
	ctx := context.Background()
	rt, _ := newFakeKubernetesRuntime(t, preparedOrphanCleanupScript())
	spec := preparedFUSESpecForTest()
	info, err := rt.PrepareSandbox(ctx, spec)
	require.NoError(t, err)
	ref, err := rt.ResolvePreparedSandbox(ctx, spec.ID, spec.WorkspaceFUSE.PoolKey)
	require.NoError(t, err)
	assert.Equal(t, info.RuntimeUID, ref.UID)
	_, err = rt.ResolvePreparedSandbox(ctx, spec.ID, "different-contract")
	require.ErrorIs(t, err, sandboxruntime.ErrInvalidRuntimeRef)
}
