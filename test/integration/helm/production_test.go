package helm_test

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProductionProfileRejectsUnsafeOverrides(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is required")
	}
	chart, err := filepath.Abs("../../../deploy/helm/sandbox")
	require.NoError(t, err)
	profile := filepath.Join(chart, "values-production.yaml")
	for _, override := range []string{"", "redis.enabled=true", "redis.external.requireHA=false", "config.workspace.allowMissingLSMForKind=true", "redis.external.mode=standalone", "redis.external.durability=best_effort", "config.workspace.lsmProfile=unconfined", "config.workspace.lsmProfile=", "config.workspace.lsmProfile=label=disable"} {
		args := []string{"template", "sandbox", chart, "-f", profile}
		if override != "" {
			args = append(args, "--set", override)
		}
		output, err := exec.Command("helm", args...).CombinedOutput()
		if override == "" {
			require.NoError(t, err, "%s", output)
		} else {
			require.Error(t, err, "unsafe override accepted: %s", override)
		}
	}
}
