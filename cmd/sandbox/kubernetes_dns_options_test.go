package main

import (
	"strings"
	"testing"

	"github.com/goairix/sandbox/internal/config"
	"github.com/goairix/sandbox/internal/kubecontract"
	k8sruntime "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/stretchr/testify/require"
)

func TestKubernetesDNSOptionsPreservedForDrainAndInspection(t *testing.T) {
	profile := "sandbox-fuse-" + strings.Repeat("a", 64)
	cfg := config.KubernetesConfig{DisableNodeLocalDNSInjection: true, DNSOptions: []kubecontract.DNSOption{{Name: "timeout", Value: "2"}}, AppArmorLoaderName: "loader", AppArmorLoaderNamespace: "release", AppArmorLoaderTimeoutSeconds: 180}
	for _, mode := range []string{"normal", "inspection", "drain"} {
		t.Run(mode, func(t *testing.T) {
			r := &k8sruntime.Runtime{}
			options := append(kubernetesAppArmorOptions(cfg, profile, mode == "inspection", mode == "drain"), kubernetesDNSOptions(cfg)...)
			for _, option := range options {
				option(r)
			}
			require.Contains(t, r.WarmPoolContract(), ":dns-admission=")
			require.Equal(t, mode == "normal", strings.Contains(r.WarmPoolContract(), ":apparmor="))
		})
	}
}
