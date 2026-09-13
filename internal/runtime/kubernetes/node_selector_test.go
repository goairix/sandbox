package kubernetes

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	sandboxruntime "github.com/goairix/sandbox/internal/runtime"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNodeSelectorOptionOwnedAndWarmPoolContractStable(t *testing.T) {
	input := map[string]string{"zone": "one", "NodeType": "GPU"}
	option := WithNodeSelector(input)
	input["zone"] = "two"
	a := &Runtime{namespace: "sandbox"}
	option(a)
	require.Equal(t, "one", a.nodeSelector["zone"])
	b := &Runtime{namespace: "sandbox"}
	WithNodeSelector(map[string]string{"NodeType": "GPU", "zone": "one"})(b)
	require.Equal(t, a.WarmPoolContract(), b.WarmPoolContract())
	require.Contains(t, a.WarmPoolContract(), ":node-selector=")
	b.nodeSelector["zone"] = "three"
	require.NotEqual(t, a.WarmPoolContract(), b.WarmPoolContract())
	require.Equal(t, "one", a.nodeSelector["zone"])
	empty := &Runtime{namespace: "sandbox"}
	WithNodeSelector(map[string]string{})(empty)
	require.Equal(t, (&Runtime{namespace: "sandbox"}).WarmPoolContract(), empty.WarmPoolContract())
	require.False(t, strings.Contains(empty.WarmPoolContract(), "node-selector"))
}

func TestNodeSelectorAndLoaderTimeoutCaps(t *testing.T) {
	_, _, profile := loaderGateFixture()
	for _, name := range []string{"64 disabled", "65 disabled", "63 enabled", "64 enabled missing os", "600 timeout", "601 timeout"} {
		t.Run(name, func(t *testing.T) {
			r := &Runtime{}
			selector := map[string]string{}
			count := 64
			timeout := 180 * time.Second
			switch name {
			case "65 disabled":
				count = 65
			case "63 enabled":
				count = 63
			case "600 timeout":
				count = 0
				timeout = 600 * time.Second
			case "601 timeout":
				count = 0
				timeout = 601 * time.Second
			}
			for i := 0; i < count; i++ {
				selector[fmt.Sprintf("key%d", i)] = "one"
			}
			WithNodeSelector(selector)(r)
			if name != "64 disabled" && name != "65 disabled" {
				WithAppArmorLoader("loader", "release", profile, timeout)(r)
			}
			err := r.validateAppArmorLoaderOptions()
			if name == "65 disabled" || name == "64 enabled missing os" || name == "601 timeout" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestNodeSelectorPropagatesToOrdinaryAndPreparedPods(t *testing.T) {
	for _, name := range []string{"ordinary", "prepared"} {
		t.Run(name, func(t *testing.T) {
			r, _ := newFakeKubernetesRuntime(t, preparedScript())
			r.readyTimeout = time.Second
			WithNodeSelector(map[string]string{"zone": "one"})(r)
			if name == "prepared" {
				_, err := r.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
				require.NoError(t, err)
			} else {
				_, err := r.CreateSandbox(context.Background(), sandboxruntime.SandboxSpec{ID: "selector-ordinary", Image: "sandbox:v1.0.0"})
				require.NoError(t, err)
			}
			pods, err := r.client.CoreV1().Pods(r.namespace).List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			require.NotEmpty(t, pods.Items)
			require.Equal(t, map[string]string{"zone": "one"}, pods.Items[0].Spec.NodeSelector)
		})
	}
}
