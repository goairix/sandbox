//go:build helmtests

package helmtest

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestDNSAdmissionDefaultHelmBaseline(t *testing.T) {
	docs := render(t, "apparmorLoader.enabled=true")
	for _, tc := range []struct{ kind, name, want string }{
		{"Deployment", "sandbox-api", "0741a9dbeef99a955540d7cbca279d13be71c3bb024214458030902e61f649c2"},
		{"DaemonSet", "sandbox-apparmor-loader", "d12941e36702f546e509e3fe066deb979b998d5cae582932f97eac9fafdba19d"},
		{"StatefulSet", "sandbox-redis", "c287ff99b6ff26d718585fcafa9ba5d61754452e08c6065b30a4b0e6a4948fac"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			template := mapping(t, object(t, docs, tc.kind, tc.name)["spec"])["template"]
			encoded, err := json.Marshal(template)
			if err != nil {
				t.Fatal(err)
			}
			got := fmt.Sprintf("%x", sha256.Sum256(encoded))
			if got != tc.want {
				t.Fatalf("template baseline got %s", got)
			}
		})
	}
}

func dnsChart(t *testing.T, schema bool, configure func(map[string]any)) string {
	t.Helper()
	chart := partialSentinelChart(t, schema, func(v map[string]any) {
		mapping(t, v["apparmorLoader"])["enabled"] = true
		configure(mapping(t, mapping(t, v["config"])["runtime"]))
	})
	// Use a public deterministic identity instead of comparing two unrelated
	// fresh installations' randomly generated cluster IDs. No fields are removed.
	replaceHelmLookup(t, chart, `randAlphaNum 32`, `"dns_admission_fixture_01234567890"`)
	return chart
}

func dnsHelmOptions() []any {
	return []any{map[string]any{"name": "single-request-reopen", "value": ""}, map[string]any{"name": "timeout", "value": "2"}}
}

func helmDNSFingerprint(t *testing.T, docs []map[string]any) any {
	t.Helper()
	metadata := mapping(t, mapping(t, object(t, docs, "Deployment", "sandbox-api")["spec"])["template"])
	return mapping(t, mapping(t, metadata["metadata"])["annotations"])["goairix.github.io/sandbox-backend-fingerprint"]
}

func TestDNSAdmissionHelmExplicitConfig(t *testing.T) {
	chart := dnsChart(t, true, func(runtime map[string]any) {
		k := mapping(t, runtime["kubernetes"])
		k["disableNodeLocalDNSInjection"] = true
		k["dnsOptions"] = dnsHelmOptions()
	})
	for _, version := range []string{"1.29.0", "1.31.0", "1.31.14-r20-31.0.62.9-arm64", "1.33.0"} {
		t.Run(version, func(t *testing.T) {
			docs := renderChartForKubeVersion(t, chart, version)
			ds := mapping(t, mapping(t, object(t, docs, "DaemonSet", "sandbox-apparmor-loader")["spec"])["template"])
			if mapping(t, mapping(t, ds["metadata"])["labels"])["node-local-dns-injection"] != "disabled" {
				t.Fatal("loader missing opt-out")
			}
			pod := mapping(t, ds["spec"])
			if !reflect.DeepEqual(mapping(t, pod["dnsConfig"])["options"], dnsHelmOptions()) {
				t.Fatal("loader DNS options differ")
			}
			for _, item := range []struct{ kind, name, container string }{
				{"Deployment", "sandbox-api", "sandbox"}, {"Job", "sandbox-backend-change-drain", "drain"}, {"Job", "sandbox-api-drain", "drain"},
			} {
				template := mapping(t, mapping(t, object(t, docs, item.kind, item.name)["spec"])["template"])
				pod := mapping(t, template["spec"])
				environment := env(t, container(t, pod, "containers", item.container))
				if environment["SANDBOX_RUNTIME_KUBERNETES_DISABLE_NODE_LOCAL_DNS_INJECTION"]["value"] != "true" {
					t.Fatal("missing opt-out env")
				}
				var options []any
				if err := json.Unmarshal([]byte(environment["SANDBOX_RUNTIME_KUBERNETES_DNS_OPTIONS"]["value"].(string)), &options); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(options, dnsHelmOptions()) {
					t.Fatal("different API/drain options")
				}
				if pod["dnsConfig"] != nil || mapping(t, template["metadata"])["labels"] != nil && mapping(t, mapping(t, template["metadata"])["labels"])["node-local-dns-injection"] != nil {
					t.Fatal("API/drain DNS itself must not change")
				}
			}
		})
	}
}

func TestDNSAdmissionHelmContractAndRedisStable(t *testing.T) {
	base := renderChart(t, dnsChart(t, true, func(runtime map[string]any) {
		k := mapping(t, runtime["kubernetes"])
		delete(k, "dnsOptions")
		delete(k, "disableNodeLocalDNSInjection")
	}))
	defaults := renderChart(t, dnsChart(t, true, func(runtime map[string]any) {
		k := mapping(t, runtime["kubernetes"])
		k["dnsOptions"] = []any{}
		k["disableNodeLocalDNSInjection"] = false
	}))
	if !reflect.DeepEqual(base, defaults) {
		t.Fatal("omitted and explicit default DNS must render identical resources")
	}
	configured := renderChart(t, dnsChart(t, true, func(runtime map[string]any) {
		k := mapping(t, runtime["kubernetes"])
		k["dnsOptions"] = dnsHelmOptions()
		k["disableNodeLocalDNSInjection"] = true
	}))
	reordered := renderChart(t, dnsChart(t, true, func(runtime map[string]any) {
		k := mapping(t, runtime["kubernetes"])
		k["dnsOptions"] = []any{dnsHelmOptions()[1], dnsHelmOptions()[0]}
		k["disableNodeLocalDNSInjection"] = true
	}))
	if helmDNSFingerprint(t, base) == helmDNSFingerprint(t, configured) {
		t.Fatal("nondefault DNS must change fingerprint")
	}
	if helmDNSFingerprint(t, configured) != helmDNSFingerprint(t, reordered) {
		t.Fatal("ordering must not change fingerprint")
	}
	for _, docs := range [][]map[string]any{configured, reordered} {
		want := mapping(t, object(t, base, "StatefulSet", "sandbox-redis-sentinel")["spec"])["template"]
		got := mapping(t, object(t, docs, "StatefulSet", "sandbox-redis-sentinel")["spec"])["template"]
		if !reflect.DeepEqual(want, got) {
			t.Fatal("DNS settings must not restart/change Redis")
		}
	}
}

func TestDNSAdmissionPreUpgradeDrainRetainsInstalledContract(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(fmt.Sprintf("installedConfigured=%t", configured), func(t *testing.T) {
			chart := dnsChart(t, true, func(runtime map[string]any) {
				k := mapping(t, runtime["kubernetes"])
				k["disableNodeLocalDNSInjection"] = true
				k["dnsOptions"] = dnsHelmOptions()
			})
			oldEnv := []any{map[string]any{"name": "SANDBOX_STATE_SCOPE", "value": "old-runtime/sandbox"}}
			if configured {
				oldEnv = append(oldEnv,
					map[string]any{"name": "SANDBOX_RUNTIME_KUBERNETES_DISABLE_NODE_LOCAL_DNS_INJECTION", "value": "true"},
					map[string]any{"name": "SANDBOX_RUNTIME_KUBERNETES_DNS_OPTIONS", "value": `[{"name":"timeout","value":"3"}]`})
			}
			installed := map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
				"containers": []any{map[string]any{"name": "sandbox", "env": oldEnv}},
			}}}}
			encoded, err := json.Marshal(installed)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(chart, "templates", "pre-backend-change-drain.yaml")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			expression := `lookup "apps/v1" "Deployment" .Release.Namespace (printf "%s-api" .Release.Name)`
			if strings.Count(string(data), expression) != 1 {
				t.Fatal("installed Deployment lookup expression changed")
			}
			data = []byte(strings.Replace(string(data), expression, `(fromJson `+strconv.Quote(string(encoded))+`)`, 1))
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			docs := renderChart(t, chart)
			pod := mapping(t, mapping(t, mapping(t, object(t, docs, "Job", "sandbox-backend-change-drain")["spec"])["template"])["spec"])
			drain := container(t, pod, "containers", "drain")
			_ = env(t, drain) // Also rejects duplicate variable names.
			if !reflect.DeepEqual(oldEnv, drain["env"]) {
				t.Fatal("pre-upgrade drain must use installed DNS configuration without mixing desired settings")
			}
			want := "--kubernetes-backend-fingerprint=" + helmDNSFingerprint(t, docs).(string)
			found := false
			for _, arg := range drain["args"].([]any) {
				found = found || arg == want
			}
			if !found {
				t.Fatal("drain must compare against the desired new backend fingerprint")
			}
		})
	}
}

func TestDNSAdmissionHelmRejectsInvalidConfig(t *testing.T) {
	for _, schema := range []bool{false, true} {
		for _, tc := range []struct {
			name, field string
			value       any
		}{
			{"bool string", "disableNodeLocalDNSInjection", "true"}, {"bool null", "disableNodeLocalDNSInjection", nil},
			{"null", "dnsOptions", nil}, {"object", "dnsOptions", map[string]any{}},
			{"unknown", "dnsOptions", []any{map[string]any{"name": "ndots", "value": "5"}}},
			{"missing", "dnsOptions", []any{map[string]any{"name": "single-request-reopen"}}},
			{"wrong type", "dnsOptions", []any{map[string]any{"name": "timeout", "value": 2}}},
			{"unknown field", "dnsOptions", []any{map[string]any{"name": "timeout", "value": "2", "private-marker": "hidden"}}},
			{"duplicate", "dnsOptions", []any{dnsHelmOptions()[0], dnsHelmOptions()[0]}},
			{"flag value", "dnsOptions", []any{map[string]any{"name": "single-request-reopen", "value": "1"}}},
			{"large", "dnsOptions", []any{map[string]any{"name": "timeout", "value": "31"}}},
			{"padding", "dnsOptions", []any{map[string]any{"name": "timeout", "value": "02"}}},
		} {
			t.Run(fmt.Sprintf("schema=%t/%s", schema, tc.name), func(t *testing.T) {
				chart := dnsChart(t, schema, func(runtime map[string]any) { mapping(t, runtime["kubernetes"])[tc.field] = tc.value })
				output, err := exec.Command("helm", "template", "sandbox", chart, "--namespace", "release-ns", "--kube-version", "1.31.0").CombinedOutput()
				if err == nil || !strings.Contains(string(output), tc.field) {
					t.Fatalf("invalid %s must fail with field category", tc.field)
				}
				if !schema && strings.Contains(string(output), "private-marker") {
					t.Fatal("template error leaked original input")
				}
			})
		}
	}
}
