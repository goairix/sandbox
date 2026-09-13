package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goairix/sandbox/internal/config"
	"github.com/stretchr/testify/require"
)

func TestKubernetesNodeSelectorDefaultsAndStrictJSON(t *testing.T) {
	t.Setenv("SANDBOX_SECURITY_API_KEY", "test")
	cfg, err := config.Load("")
	require.NoError(t, err)
	require.Empty(t, cfg.Runtime.Kubernetes.NodeSelector)
	require.Empty(t, cfg.Runtime.Kubernetes.AppArmorLoaderName)
	require.Empty(t, cfg.Runtime.Kubernetes.AppArmorLoaderNamespace)
	require.Equal(t, 180, cfg.Runtime.Kubernetes.AppArmorLoaderTimeoutSeconds)
	for _, tt := range []struct {
		value string
		valid bool
	}{{`{"example.com/NodeType":"GPU"}`, true}, {`{"zone":""}`, true}, {`null`, false}, {`[]`, false}, {`{"zone":1}`, false}, {`{"zone":true}`, false}, {`{"zone":null}`, false}, {`{"zone":"a","zone":"b"}`, false}, {`{"bad key":"a"}`, false}, {`{"zone":"bad value"}`, false}, {`{} {}`, false}} {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv("SANDBOX_RUNTIME_KUBERNETES_NODE_SELECTOR", tt.value)
			cfg, err := config.Load("")
			if tt.valid {
				require.NoError(t, err)
				if strings.Contains(tt.value, "NodeType") {
					require.Equal(t, "GPU", cfg.Runtime.Kubernetes.NodeSelector["example.com/NodeType"])
					require.NotContains(t, cfg.Runtime.Kubernetes.NodeSelector, "example.com/nodetype")
				}
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestAppArmorLoaderSelectorAndTimeoutCaps(t *testing.T) {
	for _, name := range []string{"64 disabled", "65 disabled", "63 enabled", "64 enabled missing os", "600 timeout", "601 timeout"} {
		t.Run(name, func(t *testing.T) {
			cfg := validHybridConfig()
			cfg.Runtime.Type = "kubernetes"
			cfg.Runtime.Kubernetes.Namespace = "runtime"
			k := &cfg.Runtime.Kubernetes
			k.NodeSelector = map[string]string{}
			count := 64
			if name != "64 disabled" && name != "65 disabled" {
				k.AppArmorLoaderName = "loader"
				k.AppArmorLoaderNamespace = "release"
				k.AppArmorLoaderTimeoutSeconds = 180
				cfg.Workspace.Backend.LSMProfile = "sandbox-fuse-" + strings.Repeat("a", 64)
			}
			switch name {
			case "65 disabled":
				count = 65
			case "63 enabled":
				count = 63
			case "600 timeout":
				count = 0
				k.AppArmorLoaderTimeoutSeconds = 600
			case "601 timeout":
				count = 0
				k.AppArmorLoaderTimeoutSeconds = 601
			}
			for i := 0; i < count; i++ {
				k.NodeSelector[fmt.Sprintf("key%d", i)] = "one"
			}
			err := cfg.Validate()
			if name == "65 disabled" || name == "64 enabled missing os" || name == "601 timeout" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestKubernetesNodeSelectorStrictYAMLAndCase(t *testing.T) {
	t.Setenv("SANDBOX_SECURITY_API_KEY", "test")
	for _, tt := range []struct {
		value string
		valid bool
	}{{`NodeType: "GPU"`, true}, {`zone: 42`, false}, {`zone: true`, false}, {`zone: null`, false}, {`42: "one"`, false}} {
		t.Run(tt.value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(path, []byte("runtime:\n  kubernetes:\n    node_selector:\n      "+tt.value+"\n"), 0600))
			cfg, err := config.Load(path)
			if !tt.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "GPU", cfg.Runtime.Kubernetes.NodeSelector["NodeType"])
			require.NotContains(t, cfg.Runtime.Kubernetes.NodeSelector, "nodetype")
		})
	}
}

func TestAppArmorLoaderConfigurationValidation(t *testing.T) {
	for _, name := range []string{"valid", "no fuse", "docker", "kind bypass", "profile", "namespace", "timeout", "windows selector", "invalid label"} {
		t.Run(name, func(t *testing.T) {
			cfg := validHybridConfig()
			cfg.Runtime.Type = "kubernetes"
			cfg.Runtime.Kubernetes.Namespace = "runtime-ns"
			cfg.Runtime.Kubernetes.AppArmorLoaderName = "release-apparmor-loader"
			cfg.Runtime.Kubernetes.AppArmorLoaderNamespace = "release-ns"
			cfg.Runtime.Kubernetes.AppArmorLoaderTimeoutSeconds = 180
			cfg.Workspace.Backend.LSMProfile = "sandbox-fuse-" + strings.Repeat("a", 64)
			switch name {
			case "no fuse":
				cfg.Workspace.EnabledMountModes = []string{"sync"}
			case "docker":
				cfg.Runtime.Type = "docker"
			case "kind bypass":
				cfg.Workspace.AllowMissingLSMForKind = true
			case "profile":
				cfg.Workspace.Backend.LSMProfile = "manual"
			case "namespace":
				cfg.Runtime.Kubernetes.AppArmorLoaderNamespace = ""
			case "timeout":
				cfg.Runtime.Kubernetes.AppArmorLoaderTimeoutSeconds = 0
			case "windows selector":
				cfg.Runtime.Kubernetes.NodeSelector = map[string]string{"kubernetes.io/os": "windows"}
			case "invalid label":
				cfg.Runtime.Kubernetes.NodeSelector = map[string]string{"bad label": "yes"}
			}
			err := cfg.Validate()
			if name != "valid" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "linux", cfg.Runtime.Kubernetes.NodeSelector["kubernetes.io/os"])
		})
	}
	cfg := minimalValidConfig()
	cfg.Runtime.Kubernetes.NodeSelector = map[string]string{"kubernetes.io/os": "windows"}
	require.NoError(t, cfg.Validate())
	require.Equal(t, "windows", cfg.Runtime.Kubernetes.NodeSelector["kubernetes.io/os"])
}
