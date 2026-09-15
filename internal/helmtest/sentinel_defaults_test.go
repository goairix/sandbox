//go:build helmtests

package helmtest

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Reproduce deployments that retain their own chart values.yaml, including
// installations that did not copy values.schema.json alongside templates.
func partialSentinelChart(t *testing.T, schema bool, configure func(map[string]any)) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	source := filepath.Join(filepath.Dir(file), "..", "..", "deploy", "helm", "sandbox")
	dest := t.TempDir()
	if err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(filepath.Join(dest, rel), 0700)
		}
		if !schema && rel == "values.schema.json" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dest, rel), data, 0600)
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(source, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// JSON output is also valid YAML.
	var values map[string]any
	if err := yaml.Unmarshal(data, &values); err != nil {
		t.Fatal(err)
	}
	redis := mapping(t, values["redis"])
	redis["mode"] = "sentinel"
	redis["password"] = "fixture_data_012345678901234567890123456789"
	redis["sentinel"] = map[string]any{
		"password":           "fixture_sentinel_012345678901234567890123456789",
		"identitySecretName": "", "existingSecret": "",
		"dataPasswordKey": "password", "sentinelPasswordKey": "sentinel-password",
		"bootstrapImage": map[string]any{
			"repository": "registry.example.com/sandbox-redis-bootstrap",
			"tag":        "v0.1.0", "pullPolicy": "IfNotPresent",
		},
	}
	if configure != nil {
		configure(values)
	}
	if err := os.WriteFile(filepath.Join(dest, "values.yaml"), mustJSON(t, values), 0600); err != nil {
		t.Fatal(err)
	}
	return dest
}

func TestSentinelTemplatesRejectMissingBootstrapImageWithoutSchema(t *testing.T) {
	chart := partialSentinelChart(t, false, func(v map[string]any) {
		delete(mapping(t, mapping(t, v["redis"])["sentinel"]), "bootstrapImage")
	})
	output, err := exec.Command("helm", "template", "sandbox", chart, "--namespace", "release-ns", "--kube-version", "1.33.0").CombinedOutput()
	if err == nil || !strings.Contains(string(output), "redis.sentinel.bootstrapImage") {
		t.Fatalf("missing bootstrap image must fail without API fallback; got %v: %s", err, output)
	}
}

func TestSentinelTemplatesSupplyMissingDefaults(t *testing.T) {
	for _, schema := range []bool{false, true} {
		for _, missingProbe := range []bool{false, true} {
			t.Run(map[bool]string{false: "without-schema", true: "with-schema"}[schema]+map[bool]string{false: "/with-probe", true: "/missing-probe"}[missingProbe], func(t *testing.T) {
				chart := partialSentinelChart(t, schema, func(v map[string]any) {
					if missingProbe {
						delete(v, "startupProbe")
					}
				})
				docs := renderChart(t, chart)
				assertSentinelDefaults(t, docs)
			})
		}
	}
}

func assertSentinelDefaults(t *testing.T, docs []map[string]any) {
	t.Helper()
	sts := object(t, docs, "StatefulSet", "sandbox-redis-sentinel")
	pod := mapping(t, mapping(t, mapping(t, sts["spec"])["template"])["spec"])
	for _, process := range []string{"redis", "sentinel"} {
		c := container(t, pod, "containers", process)
		args := sequence(t, c["args"])
		want := []any{process, "-master-name=sandbox", "-timeout=600s", "-down-after-milliseconds=10000", "-failover-timeout-milliseconds=60000", "-parallel-syncs=1"}
		if string(mustJSON(t, args)) != string(mustJSON(t, want)) {
			t.Fatalf("missing defaults rendered invalid %s arguments: %v", process, args)
		}
		if mapping(t, c["startupProbe"])["failureThreshold"] != 330 {
			t.Fatal("Redis process probe must retain its startup budget")
		}
	}
	if container(t, pod, "initContainers", "identity")["resources"] == nil || container(t, pod, "containers", "sentinel")["resources"] == nil {
		t.Fatal("missing resource maps must receive bounded defaults")
	}
	job := object(t, docs, "Job", "sandbox-redis-sentinel-initialize-1")
	if mapping(t, job["spec"])["activeDeadlineSeconds"] != 750 {
		t.Fatal("missing initialize timeout must not produce a 30-second deadline")
	}
	jp := mapping(t, mapping(t, mapping(t, job["spec"])["template"])["spec"])
	if got := sequence(t, container(t, jp, "containers", "initialize")["args"]); string(mustJSON(t, got)) != string(mustJSON(t, []any{"initialize", "-state-configmap=sandbox-redis-sentinel-state", "-master-name=sandbox", "-timeout=720s", "-ack-timeout=1000ms"})) {
		t.Fatalf("invalid default initialize arguments: %v", got)
	}
	d := object(t, docs, "Deployment", "sandbox-api")
	ap := mapping(t, mapping(t, mapping(t, d["spec"])["template"])["spec"])
	c := container(t, ap, "containers", "sandbox")
	probe := mapping(t, c["startupProbe"])
	if probe["periodSeconds"] != 5 || probe["timeoutSeconds"] != 1 || probe["failureThreshold"] != 190 {
		t.Fatalf("API startup defaults must cover initialization: %v", probe)
	}
	e := env(t, c)
	if e["SANDBOX_STORAGE_STATE_REDIS_MASTER_NAME"]["value"] != "sandbox" || e["SANDBOX_STORAGE_STATE_REDIS_ACK_TIMEOUT_MS"]["value"] != "1000" {
		t.Fatal("API must consume the same effective defaults")
	}
	if !strings.Contains(e["SANDBOX_STORAGE_STATE_REDIS_ADDRS"]["value"].(string), ".svc.cluster.local:26379") {
		t.Fatal("missing domain must produce valid cluster.local member addresses")
	}
	secret := object(t, docs, "Secret", "sandbox-redis")
	if mapping(t, secret["data"])["password"] == nil || mapping(t, secret["data"])["sentinel-password"] == nil {
		t.Fatal("all auth consumers must use default Secret keys")
	}
	var cluster map[string]any
	state := object(t, docs, "ConfigMap", "sandbox-redis-sentinel-state")
	if err := json.Unmarshal([]byte(mapping(t, state["data"])["cluster.json"].(string)), &cluster); err != nil {
		t.Fatal(err)
	}
	for _, member := range sequence(t, cluster["members"]) {
		if !strings.HasSuffix(member.(string), ".svc.cluster.local") {
			t.Fatalf("invalid state member DNS: %v", member)
		}
	}
	serialized := string(mustJSON(t, docs))
	if strings.Contains(serialized, "%!") || strings.Contains(serialized, "-timeout=0s") || strings.Contains(serialized, "-ack-timeout=0ms") {
		t.Fatal("missing defaults must never render malformed names or zero timeouts")
	}
}

func TestSentinelTemplatesRejectInvalidConfigWithoutSchema(t *testing.T) {
	cases := []struct {
		key   string
		value any
	}{
		{"clusterDomain", ""}, {"clusterDomain", "bad/domain"}, {"clusterDomain", false},
		{"masterName", ""}, {"masterName", "bad name"},
		{"startupTimeoutSeconds", 0}, {"startupTimeoutSeconds", 601}, {"startupTimeoutSeconds", "600"},
		{"initializeTimeoutSeconds", 0}, {"initializeTimeoutSeconds", -1}, {"initializeTimeoutSeconds", 721},
		{"ackTimeoutMs", 0}, {"ackTimeoutMs", 999}, {"ackTimeoutMs", 10001},
		{"downAfterMilliseconds", 0}, {"downAfterMilliseconds", 1.5}, {"downAfterMilliseconds", 60001},
		{"failoverTimeoutMilliseconds", 0}, {"failoverTimeoutMilliseconds", 600001},
		{"parallelSyncs", 0}, {"parallelSyncs", 3}, {"parallelSyncs", false},
		{"dataPasswordKey", ""}, {"sentinelPasswordKey", "bad/key"},
		{"identitySecretName", "bad/name"}, {"existingSecret", false},
		{"resources", "invalid"}, {"identityResources", false},
	}
	for _, tc := range cases {
		t.Run(tc.key+"/"+string(mustJSON(t, tc.value)), func(t *testing.T) {
			chart := partialSentinelChart(t, false, func(v map[string]any) {
				mapping(t, mapping(t, v["redis"])["sentinel"])[tc.key] = tc.value
			})
			output, err := exec.Command("helm", "template", "sandbox", chart, "--namespace", "release-ns", "--kube-version", "1.33.0").CombinedOutput()
			if err == nil || !strings.Contains(string(output), "redis.sentinel."+tc.key) {
				t.Fatalf("invalid %s must fail before creating resources with a field-specific error; got %v: %s", tc.key, err, output)
			}
		})
	}
}

func TestSentinelTemplatesPreserveExplicitSettings(t *testing.T) {
	chart := partialSentinelChart(t, false, func(v map[string]any) {
		cfg := mapping(t, mapping(t, v["redis"])["sentinel"])
		for key, value := range map[string]any{
			"masterName": "custom-master", "clusterDomain": "custom.internal",
			"startupTimeoutSeconds": 123, "initializeTimeoutSeconds": 321, "ackTimeoutMs": 2000,
			"downAfterMilliseconds": 5000, "failoverTimeoutMilliseconds": 20000, "parallelSyncs": 2,
			"dataPasswordKey": "data-auth", "sentinelPasswordKey": "sentinel-auth",
			"resources":         map[string]any{"requests": map[string]any{"cpu": "75m", "memory": "96Mi"}},
			"identityResources": map[string]any{"requests": map[string]any{"cpu": "20m", "memory": "96Mi"}},
		} {
			cfg[key] = value
		}
		v["startupProbe"] = map[string]any{"enabled": true, "periodSeconds": 7, "timeoutSeconds": 3, "failureThreshold": 1000}
	})
	docs := renderChart(t, chart)
	sts := object(t, docs, "StatefulSet", "sandbox-redis-sentinel")
	pod := mapping(t, mapping(t, mapping(t, sts["spec"])["template"])["spec"])
	for _, process := range []string{"redis", "sentinel"} {
		c := container(t, pod, "containers", process)
		want := []any{process, "-master-name=custom-master", "-timeout=123s", "-down-after-milliseconds=5000", "-failover-timeout-milliseconds=20000", "-parallel-syncs=2"}
		if string(mustJSON(t, c["args"])) != string(mustJSON(t, want)) {
			t.Fatalf("explicit process settings were replaced: %v", c["args"])
		}
	}
	for _, entry := range []struct{ key, name, cpu string }{{"containers", "sentinel", "75m"}, {"initContainers", "identity", "20m"}} {
		requests := mapping(t, mapping(t, container(t, pod, entry.key, entry.name)["resources"])["requests"])
		if requests["cpu"] != entry.cpu || requests["memory"] != "96Mi" {
			t.Fatal("explicit resource settings were replaced")
		}
	}
	job := object(t, docs, "Job", "sandbox-redis-sentinel-initialize-1")
	if mapping(t, job["spec"])["activeDeadlineSeconds"] != 351 {
		t.Fatal("explicit initialize deadline was replaced")
	}
	jp := mapping(t, mapping(t, mapping(t, job["spec"])["template"])["spec"])
	want := []any{"initialize", "-state-configmap=sandbox-redis-sentinel-state", "-master-name=custom-master", "-timeout=321s", "-ack-timeout=2000ms"}
	if string(mustJSON(t, container(t, jp, "containers", "initialize")["args"])) != string(mustJSON(t, want)) {
		t.Fatal("initializer must consume the same explicit settings")
	}
	d := object(t, docs, "Deployment", "sandbox-api")
	ap := mapping(t, mapping(t, mapping(t, d["spec"])["template"])["spec"])
	c := container(t, ap, "containers", "sandbox")
	probe := mapping(t, c["startupProbe"])
	if probe["periodSeconds"] != 7 || probe["timeoutSeconds"] != 3 || probe["failureThreshold"] != 1000 {
		t.Fatal("explicit longer API startup budget was replaced")
	}
	e := env(t, c)
	if e["SANDBOX_STORAGE_STATE_REDIS_MASTER_NAME"]["value"] != "custom-master" || e["SANDBOX_STORAGE_STATE_REDIS_ACK_TIMEOUT_MS"]["value"] != "2000" || !strings.Contains(e["SANDBOX_STORAGE_STATE_REDIS_ADDRS"]["value"].(string), ".svc.custom.internal:26379") {
		t.Fatal("API settings differ from Redis/initializer settings")
	}
	secret := mapping(t, object(t, docs, "Secret", "sandbox-redis")["data"])
	if secret["data-auth"] == nil || secret["sentinel-auth"] == nil || secret["password"] != nil {
		t.Fatal("explicit Secret keys were replaced")
	}
}

func TestSentinelTemplatesDefaultNullAndMinimalConfig(t *testing.T) {
	for _, minimal := range []bool{false, true} {
		t.Run(map[bool]string{false: "null-defaults", true: "minimal-config"}[minimal], func(t *testing.T) {
			chart := partialSentinelChart(t, false, func(v map[string]any) {
				cfg := mapping(t, mapping(t, v["redis"])["sentinel"])
				if minimal {
					for key := range cfg {
						if key != "password" && key != "bootstrapImage" {
							delete(cfg, key)
						}
					}
					v["startupProbe"] = map[string]any{"enabled": true}
				} else {
					for _, key := range []string{"clusterDomain", "masterName", "startupTimeoutSeconds", "initializeTimeoutSeconds", "ackTimeoutMs", "resources", "identityResources"} {
						cfg[key] = nil
					}
					v["startupProbe"] = nil
				}
			})
			assertSentinelDefaults(t, renderChart(t, chart))
		})
	}
}

func TestSentinelTemplatesRejectUnsafeProbeWithoutSchema(t *testing.T) {
	for _, tc := range []struct {
		key   string
		value any
	}{
		{"enabled", false}, {"enabled", "true"}, {"periodSeconds", 0},
		{"periodSeconds", "5"}, {"timeoutSeconds", 61}, {"failureThreshold", -1},
	} {
		t.Run(tc.key+"/"+string(mustJSON(t, tc.value)), func(t *testing.T) {
			chart := partialSentinelChart(t, false, func(v map[string]any) {
				v["startupProbe"] = map[string]any{tc.key: tc.value}
			})
			output, err := exec.Command("helm", "template", "sandbox", chart, "--namespace", "release-ns", "--kube-version", "1.33.0").CombinedOutput()
			if err == nil || !strings.Contains(string(output), "startupProbe."+tc.key) {
				t.Fatalf("explicit unsafe probe value must not be masked by defaults: %v: %s", err, output)
			}
		})
	}
}
