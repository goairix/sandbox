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
	for _, profile := range []struct {
		name, file string
		unsafe     [][]string
	}{
		{"builtin-etcd", "values-production.yaml", [][]string{
			{"redis.enabled=true"}, {"etcd.enabled=false"}, {"etcd.persistence.enabled=false"},
			{"etcd.replicaCount=1"}, {"etcd.hardAntiAffinity=false"}, {"networkPolicy.enabled=false"},
			{"config.workspace.allowMissingLSMForKind=true"},
			{"apparmorLoader.enabled=false", "config.workspace.lsmProfile=unconfined"},
			{"apparmorLoader.enabled=false", "config.workspace.lsmProfile="},
			{"apparmorLoader.enabled=false", "config.workspace.lsmProfile=label=disable"},
		}},
		{"external-redis", "values-production-redis.yaml", [][]string{
			{"redis.enabled=true"}, {"redis.external.requireHA=false"},
			{"config.workspace.allowMissingLSMForKind=true"}, {"redis.external.mode=standalone"},
			{"redis.external.durability=best_effort"}, {"config.workspace.lsmProfile=unconfined"},
			{"config.workspace.lsmProfile="}, {"config.workspace.lsmProfile=label=disable"},
		}},
	} {
		t.Run(profile.name, func(t *testing.T) {
			args := []string{"template", "sandbox", chart, "-f", filepath.Join(chart, profile.file)}
			output, err := exec.Command("helm", args...).CombinedOutput()
			require.NoError(t, err, "%s", output)
			for _, overrides := range profile.unsafe {
				command := append([]string{}, args...)
				for _, override := range overrides {
					command = append(command, "--set", override)
				}
				_, err := exec.Command("helm", command...).CombinedOutput()
				require.Error(t, err, "unsafe overrides accepted: %v", overrides)
			}
		})
	}
}
