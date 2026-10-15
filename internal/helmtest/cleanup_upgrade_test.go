//go:build helmtests

package helmtest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Override only Helm's cluster lookup input; the real chart still chooses the
// drain image, environment, runtime contract comparison, and hook ordering.
func installedCleanupChart(t *testing.T, pod map[string]any) string {
	t.Helper()
	chart := copyHelmChart(t)
	installed := map[string]any{"spec": map[string]any{"template": map[string]any{"spec": pod}}}
	encoded, err := json.Marshal(installed)
	require.NoError(t, err)
	path := filepath.Join(chart, "templates", "pre-backend-change-drain.yaml")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	expression := `lookup "apps/v1" "Deployment" .Release.Namespace (printf "%s-api" .Release.Name)`
	require.Equal(t, 1, strings.Count(string(data), expression))
	data = []byte(strings.Replace(string(data), expression, `(fromJson `+strconv.Quote(string(encoded))+`)`, 1))
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return chart
}

func TestCleanupUpgradeDrainsWithInstalledAPIContract(t *testing.T) {
	oldEnv := []any{
		map[string]any{"name": "SANDBOX_STATE_SCOPE", "value": "original-runtime/sandbox"},
		map[string]any{"name": "SANDBOX_IMAGES_SANDBOX", "value": "registry/runtime:old"},
		map[string]any{"name": "SANDBOX_STORAGE_FILESYSTEM_BUCKET", "value": "original-bucket"},
		map[string]any{"name": "SANDBOX_REDIS_PASSWORD", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "original-redis", "key": "password"}}},
	}
	oldEnvFrom := []any{map[string]any{"secretRef": map[string]any{"name": "original-extra-config"}}}
	oldMounts := []any{map[string]any{"name": "original-config", "mountPath": "/config", "readOnly": true}}
	oldVolumes := []any{map[string]any{"name": "original-config", "secret": map[string]any{"secretName": "original-config"}}}
	for _, sidecarFirst := range []bool{false, true} {
		t.Run(strconv.FormatBool(sidecarFirst), func(t *testing.T) {
			api := map[string]any{"name": "sandbox", "image": "registry/api:installed-v2", "imagePullPolicy": "Never", "env": oldEnv, "envFrom": oldEnvFrom, "volumeMounts": oldMounts}
			containers := []any{api}
			if sidecarFirst {
				containers = []any{map[string]any{"name": "injected-sidecar", "image": "registry/sidecar:1"}, api}
			}
			chart := installedCleanupChart(t, map[string]any{"containers": containers, "volumes": oldVolumes})
			docs := renderChart(t, chart, "image.repository=registry/api", "image.tag=desired-v3", "image.pullPolicy=Always", "config.images.sandbox=registry/runtime:new", "config.storage.filesystem.bucket=new-bucket")
			job := object(t, docs, "Job", "sandbox-backend-change-drain")
			annotations := mapping(t, mapping(t, job["metadata"])["annotations"])
			require.Equal(t, "pre-upgrade", annotations["helm.sh/hook"], "old runtime must disappear before the new Deployment is applied")
			pod := mapping(t, mapping(t, mapping(t, job["spec"])["template"])["spec"])
			drain := container(t, pod, "containers", "drain")
			require.Equal(t, "registry/api:installed-v2", drain["image"], "cleanup must use the control client that created the installed runtimes")
			require.Equal(t, "Never", drain["imagePullPolicy"])
			require.Equal(t, oldEnv, drain["env"])
			require.Equal(t, oldEnvFrom, drain["envFrom"])
			require.Equal(t, oldMounts, drain["volumeMounts"])
			require.Equal(t, oldVolumes, pod["volumes"])
			args := sequence(t, drain["args"])
			require.Contains(t, args, "--kubernetes-cleanup-protocol=v3", "even unchanged storage must trigger a v2-to-v3 drain")
			require.Contains(t, args, "--kubernetes-backend-fingerprint="+helmDNSFingerprint(t, docs).(string))
			require.Contains(t, args, "--required-kubernetes-drain-protocol=v1")
			require.Contains(t, args, "--drain-release")
			targetPod := mapping(t, mapping(t, mapping(t, object(t, docs, "Deployment", "sandbox-api")["spec"])["template"])["spec"])
			require.Equal(t, "registry/api:desired-v3", container(t, targetPod, "containers", "sandbox")["image"])
		})
	}
}

func TestCleanupUpgradeRejectsUnknownInstalledAPIImage(t *testing.T) {
	for _, containers := range [][]any{
		{},
		{map[string]any{"name": "sandbox", "env": []any{}}},
		{map[string]any{"name": "sidecar", "image": "registry/sidecar:1"}},
		{map[string]any{"name": "sandbox", "image": "registry/api:installed-v2"}},
		{map[string]any{"name": "sandbox", "image": "registry/api:installed-v2", "env": []any{}}},
		{map[string]any{"name": "sandbox", "image": "registry/api:installed-v2", "env": nil}},
	} {
		chart := installedCleanupChart(t, map[string]any{"containers": containers})
		docs, output := renderChartResult(t, chart)
		require.True(t, len(docs) == 0, "an installed release must never fall back to the target control client")
		require.Contains(t, string(output), "installed sandbox API image")
	}
}

func TestCleanupContractGuardsRollbackAndPreservesAPIOnlyUpgrade(t *testing.T) {
	base := render(t)
	deployment := object(t, base, "Deployment", "sandbox-api")
	annotations := mapping(t, mapping(t, mapping(t, mapping(t, deployment["spec"])["template"])["metadata"])["annotations"])
	t.Run("cleanup_migration", func(t *testing.T) { require.Equal(t, "v3", annotations["goairix.github.io/sandbox-cleanup-protocol"]) })
	fingerprint := annotations["goairix.github.io/sandbox-backend-fingerprint"]
	// Independently frozen default v2 fingerprint: old target revisions compare
	// this value during rollback. It must differ even with unchanged storage.
	t.Run("rollback_contract_fence", func(t *testing.T) {
		require.NotEqual(t, "c7ceb8ca2acfe06ef7c0bd9ef99c2e6e82ee5dad9dd8541e669f5355c3b40e18", fingerprint, "legacy rollback guard must reject a control contract downgrade")
	})
	sameContract := render(t, "image.tag=another-api-release", "config.images.sandbox=registry/runtime:compatible-update")
	require.Equal(t, fingerprint, helmDNSFingerprint(t, sameContract), "compatible image updates preserve the pool; contract bumps force drain")
	rollback := object(t, base, "Job", "sandbox-rollback-backend-guard")
	require.Equal(t, "pre-rollback", mapping(t, mapping(t, rollback["metadata"])["annotations"])["helm.sh/hook"])
	pod := mapping(t, mapping(t, mapping(t, rollback["spec"])["template"])["spec"])
	require.Contains(t, sequence(t, container(t, pod, "containers", "verify-backend")["args"]), "--kubernetes-backend-fingerprint="+fingerprint.(string))
}
