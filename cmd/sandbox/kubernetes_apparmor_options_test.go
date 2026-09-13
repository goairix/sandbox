package main

import (
	"strings"
	"testing"

	"github.com/goairix/sandbox/internal/config"
	k8sruntime "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/stretchr/testify/require"
)

func TestKubernetesAppArmorOptionsExcludeInspectionAndDrain(t *testing.T) {
	profile := "sandbox-fuse-" + strings.Repeat("a", 64)
	cfg := config.KubernetesConfig{NodeSelector: map[string]string{"zone": "one"}, AppArmorLoaderName: "loader", AppArmorLoaderNamespace: "release", AppArmorLoaderTimeoutSeconds: 180}
	for _, name := range []string{"normal", "inspection", "drain", "disabled"} {
		t.Run(name, func(t *testing.T) {
			c := cfg
			inspection := name == "inspection"
			drain := name == "drain"
			if name == "disabled" {
				c.AppArmorLoaderName = ""
			}
			r := &k8sruntime.Runtime{}
			for _, option := range kubernetesAppArmorOptions(c, profile, inspection, drain) {
				option(r)
			}
			require.Contains(t, r.WarmPoolContract(), "node-selector=")
			if name == "normal" {
				require.Contains(t, r.WarmPoolContract(), ":apparmor="+profile)
			} else {
				require.NotContains(t, r.WarmPoolContract(), ":apparmor=")
			}
		})
	}
}
