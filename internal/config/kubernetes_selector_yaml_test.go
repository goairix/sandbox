package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goairix/sandbox/internal/config"
	"github.com/stretchr/testify/require"
)

func TestKubernetesNodeSelectorYAMLNeverSilentlyDropsEffectiveSelection(t *testing.T) {
	t.Setenv("SANDBOX_SECURITY_API_KEY", "test")
	for _, name := range []string{"structural case", "runtime merge", "kubernetes merge", "root merge"} {
		t.Run(name, func(t *testing.T) {
			var content string
			switch name {
			case "structural case":
				content = "Runtime:\n  Kubernetes:\n    Node_Selector:\n      NodeType: GPU\n"
			case "runtime merge":
				content = "defaults: &defaults\n  kubernetes:\n    node_selector:\n      zone: one\nruntime:\n  <<: *defaults\n"
			case "kubernetes merge":
				content = "defaults: &defaults\n  node_selector:\n    zone: one\nruntime:\n  kubernetes:\n    <<: *defaults\n"
			case "root merge":
				content = "defaults: &defaults\n  runtime:\n    kubernetes:\n      node_selector:\n        zone: one\n<<: *defaults\n"
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(path, []byte(content), 0600))
			cfg, err := config.Load(path)
			if name == "structural case" {
				require.NoError(t, err)
				require.Equal(t, "GPU", cfg.Runtime.Kubernetes.NodeSelector["NodeType"])
			} else {
				require.Error(t, err, "merge selectors must be rejected rather than silently broadened")
			}
		})
	}
}
