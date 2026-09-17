//go:build helmtests

package helmtest

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func sentinelVersionChart(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("test location unavailable")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "deploy", "helm", "sandbox")
}

func TestSentinelKubernetesVersionCompatibility(t *testing.T) {
	chart := sentinelVersionChart(t)
	for _, version := range []string{"1.29.0", "1.30.0", "1.31.0", "1.31.14-r20-31.0.62.9-arm64", "1.32.0", "1.33.0"} {
		for _, mode := range []struct {
			name      string
			overrides []string
		}{
			{"external-identity", sentinelOverrides()},
			{"automatic-identity", autoSentinelOverrides()},
			{"production-external-identity", append(sentinelOverrides(), "productionSafetyChecks=true", "config.workspace.lsmProfile=sandbox-production", "config.workspace.allowMissingLSMForKind=false")},
			{"production-automatic-identity", append(autoSentinelOverrides(), "productionSafetyChecks=true", "config.workspace.lsmProfile=sandbox-production", "config.workspace.allowMissingLSMForKind=false")},
		} {
			t.Run(version+"/"+mode.name, func(t *testing.T) {
				docs := renderChartForKubeVersion(t, chart, version, mode.overrides...)
				assertSentinelNativeTopologyAndPermissions(t, docs)
				assertSentinelPrivateKeyMountAndNetworkIsolation(t, docs)
				sts := object(t, docs, "StatefulSet", "sandbox-redis-sentinel")
				pod := mapping(t, mapping(t, mapping(t, sts["spec"])["template"])["spec"])
				for _, c := range []map[string]any{
					container(t, pod, "initContainers", "prepare"),
					container(t, pod, "initContainers", "identity"),
					container(t, pod, "containers", "redis"),
					container(t, pod, "containers", "sentinel"),
				} {
					ordinal := env(t, c)["POD_ORDINAL"]
					ref := mapping(t, mapping(t, ordinal["valueFrom"])["fieldRef"])
					if ref["fieldPath"] != "metadata.labels['apps.kubernetes.io/pod-index']" {
						t.Fatal("fixed member ordinal must come from the StatefulSet pod-index label")
					}
				}
				identity := container(t, pod, "initContainers", "identity")
				if mapping(t, mapping(t, identity["startupProbe"])["httpGet"])["path"] != "/healthz" {
					t.Fatal("native identity sidecar startup must retain its local health gate")
				}
				for _, key := range []string{"startupProbe", "livenessProbe"} {
					probe := mapping(t, identity[key])
					if mapping(t, probe["httpGet"])["port"] != "identity" {
						t.Fatal("identity probe must not depend on Redis or remote quorum")
					}
				}
			})
		}
	}
}

func TestSentinelRejectsUnsupportedKubernetes(t *testing.T) {
	chart := sentinelVersionChart(t)
	for _, version := range []string{"1.27.0", "1.28.0", "1.28.15-vendor"} {
		t.Run(version, func(t *testing.T) {
			args := []string{"template", "sandbox", chart, "--namespace", "release-ns", "--kube-version", version}
			for _, override := range sentinelOverrides() {
				args = append(args, "--set", override)
			}
			output, err := exec.Command("helm", args...).CombinedOutput()
			if err == nil || !strings.Contains(string(output), "Kubernetes >=1.29") || !strings.Contains(string(output), "SidecarContainers and PodIndexLabel enabled") {
				t.Fatal("unsupported version must fail with the actual minimum and required features")
			}
		})
	}
}

func TestSentinelLowerVersionSafety(t *testing.T) {
	chart := sentinelVersionChart(t)
	cases := []struct {
		name      string
		overrides []string
		want      string
	}{
		{"persistence", append(sentinelOverrides(), "redis.persistence.enabled=false"), "built-in Sentinel requires persistence"},
		{"separate-passwords", append(autoSentinelOverrides(), "redis.sentinel.password=fixture_data_012345678901234567890123456789"), "requires separate data and Sentinel passwords"},
		{"separate-secret-keys", append(sentinelOverrides(), "redis.sentinel.sentinelPasswordKey=password"), "authentication Secret keys must differ"},
		{"missing-lsm-bypass", append(sentinelOverrides(), "productionSafetyChecks=true", "config.workspace.lsmProfile=sandbox-production", "config.workspace.allowMissingLSMForKind=true"), "productionSafetyChecks forbids allowMissingLSMForKind"},
		{"confined-lsm", append(sentinelOverrides(), "productionSafetyChecks=true", "config.workspace.lsmProfile=unconfined"), "productionSafetyChecks requires a confined"},
		{"standalone-production", []string{"productionSafetyChecks=true", "redis.mode=standalone"}, "productionSafetyChecks requires external HA Redis or built-in Sentinel"},
	}
	for _, version := range []string{"1.29.0", "1.31.0"} {
		for _, tc := range cases {
			t.Run(version+"/"+tc.name, func(t *testing.T) {
				args := []string{"template", "sandbox", chart, "--namespace", "release-ns", "--kube-version", version}
				for _, override := range tc.overrides {
					args = append(args, "--set", override)
				}
				output, err := exec.Command("helm", args...).CombinedOutput()
				if err == nil || !strings.Contains(string(output), tc.want) {
					t.Fatalf("invalid configuration must still fail at %s", tc.want)
				}
			})
		}
	}
}
