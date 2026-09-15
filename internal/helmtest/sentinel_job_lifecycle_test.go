//go:build helmtests

package helmtest

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func copyHelmChart(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	source := filepath.Join(filepath.Dir(file), "..", "..", "deploy", "helm", "sandbox")
	destination := t.TempDir()
	require.NoError(t, filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	}))
	return destination
}

func replaceHelmLookup(t *testing.T, chart, expression, replacement string) {
	t.Helper()
	path := filepath.Join(chart, "templates", "_helpers.tpl")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(data), expression), "lookup expression changed")
	updated := strings.Replace(string(data), expression, replacement, 1)
	require.NoError(t, os.WriteFile(path, []byte(updated), 0o600))
}

func renderChartResult(t *testing.T, chart string, overrides ...string) ([]map[string]any, []byte) {
	t.Helper()
	args := []string{"template", "sandbox", chart, "--namespace", "release-ns", "--kube-version", "1.33.0"}
	for _, override := range overrides {
		option := "--set"
		if strings.HasPrefix(override, "string:") {
			option = "--set-string"
			override = strings.TrimPrefix(override, "string:")
		}
		args = append(args, option, override)
	}
	output, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		return nil, output
	}
	decoder := yaml.NewDecoder(bytes.NewReader(output))
	var documents []map[string]any
	for {
		var document map[string]any
		if err := decoder.Decode(&document); err == io.EOF {
			break
		} else {
			require.NoError(t, err)
		}
		if len(document) > 0 {
			documents = append(documents, document)
		}
	}
	return documents, nil
}

func renderSentinelState(t *testing.T, phase string, identityExists bool) ([]map[string]any, []byte) {
	t.Helper()
	chart := copyHelmChart(t)
	members := []string{
		"sandbox-redis-sentinel-0.sandbox-redis-sentinel-headless.release-ns.svc.cluster.local",
		"sandbox-redis-sentinel-1.sandbox-redis-sentinel-headless.release-ns.svc.cluster.local",
		"sandbox-redis-sentinel-2.sandbox-redis-sentinel-headless.release-ns.svc.cluster.local",
	}
	cluster, err := json.Marshal(map[string]any{
		"clusterID": "fixture-cluster",
		"members":   members,
		"phase":     phase,
	})
	require.NoError(t, err)
	configMap, err := json.Marshal(map[string]any{
		"data": map[string]string{"cluster.json": string(cluster)},
	})
	require.NoError(t, err)
	replaceHelmLookup(t, chart,
		`lookup "v1" "ConfigMap" .Release.Namespace (printf "%s-state" $name)`,
		`(fromJson `+strconv.Quote(string(configMap))+`)`)
	if identityExists {
		replaceHelmLookup(t, chart,
			`lookup "v1" "Secret" .Release.Namespace (include "sandbox.sentinelIdentitySecretName" .)`,
			`(fromJson "{\"metadata\":{\"name\":\"sandbox-redis-sentinel-identity\"}}")`)
	}
	return renderChartResult(t, chart, autoSentinelOverrides()...)
}

func hasObject(documents []map[string]any, kind, name string) bool {
	for _, document := range documents {
		metadata, ok := document["metadata"].(map[string]any)
		if ok && document["kind"] == kind && metadata["name"] == name {
			return true
		}
	}
	return false
}

func TestRetainedSentinelRejectsInvalidPhase(t *testing.T) {
	for _, phase := range []string{"", "Ready", "initialized"} {
		t.Run(strconv.Quote(phase), func(t *testing.T) {
			_, output := renderSentinelState(t, phase, true)
			require.Contains(t, string(output), "invalid phase")
		})
	}
}

func TestFreshSentinelRendersOneShotJobs(t *testing.T) {
	documents := render(t, autoSentinelOverrides()...)
	for _, resource := range [][2]string{
		{"Job", "sandbox-redis-sentinel-identity-1"},
		{"ServiceAccount", "sandbox-redis-sentinel-identity"},
		{"Role", "sandbox-redis-sentinel-identity"},
		{"RoleBinding", "sandbox-redis-sentinel-identity"},
		{"Job", "sandbox-redis-sentinel-initialize-1"},
		{"ServiceAccount", "sandbox-redis-sentinel-initialize"},
		{"Role", "sandbox-redis-sentinel-initialize"},
		{"RoleBinding", "sandbox-redis-sentinel-initialize"},
	} {
		require.True(t, hasObject(documents, resource[0], resource[1]), "%s %s", resource[0], resource[1])
	}
}

func TestPendingSentinelWithIdentityOnlyRendersInitializer(t *testing.T) {
	documents, output := renderSentinelState(t, "Pending", true)
	require.Empty(t, output)
	for _, resource := range [][2]string{
		{"Job", "sandbox-redis-sentinel-identity-1"},
		{"ServiceAccount", "sandbox-redis-sentinel-identity"},
		{"Role", "sandbox-redis-sentinel-identity"},
		{"RoleBinding", "sandbox-redis-sentinel-identity"},
	} {
		require.False(t, hasObject(documents, resource[0], resource[1]), "%s %s", resource[0], resource[1])
	}
	for _, resource := range [][2]string{
		{"Job", "sandbox-redis-sentinel-initialize-1"},
		{"ServiceAccount", "sandbox-redis-sentinel-initialize"},
		{"Role", "sandbox-redis-sentinel-initialize"},
		{"RoleBinding", "sandbox-redis-sentinel-initialize"},
	} {
		require.True(t, hasObject(documents, resource[0], resource[1]), "%s %s", resource[0], resource[1])
	}
}

func TestInitializedSentinelOmitsOneShotJobs(t *testing.T) {
	documents, output := renderSentinelState(t, "Initialized", true)
	require.Empty(t, output)
	for _, resource := range [][2]string{
		{"Job", "sandbox-redis-sentinel-identity-1"},
		{"ServiceAccount", "sandbox-redis-sentinel-identity"},
		{"Role", "sandbox-redis-sentinel-identity"},
		{"RoleBinding", "sandbox-redis-sentinel-identity"},
		{"Job", "sandbox-redis-sentinel-initialize-1"},
		{"ServiceAccount", "sandbox-redis-sentinel-initialize"},
		{"Role", "sandbox-redis-sentinel-initialize"},
		{"RoleBinding", "sandbox-redis-sentinel-initialize"},
	} {
		require.False(t, hasObject(documents, resource[0], resource[1]), "%s %s", resource[0], resource[1])
	}
	for _, resource := range [][2]string{
		{"ConfigMap", "sandbox-redis-sentinel-state"},
		{"Service", "sandbox-redis-sentinel-headless"},
		{"StatefulSet", "sandbox-redis-sentinel"},
		{"NetworkPolicy", "sandbox-redis-sentinel"},
	} {
		require.True(t, hasObject(documents, resource[0], resource[1]), "%s %s", resource[0], resource[1])
	}
}

func TestRetainedSentinelWithoutIdentityFailsClosed(t *testing.T) {
	_, output := renderSentinelState(t, "Initialized", false)
	require.Contains(t, string(output), "missing the identity Secret")
}
