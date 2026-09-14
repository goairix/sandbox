//go:build helmtests

package helmtest

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func render(t *testing.T, overrides ...string) []map[string]any {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("test location unavailable")
	}
	chart := filepath.Join(filepath.Dir(filename), "..", "..", "deploy", "helm", "sandbox")
	args := []string{"template", "sandbox", chart, "--namespace", "release-ns", "--kube-version", "1.33.0"}
	for _, override := range overrides {
		option := "--set"
		if strings.HasPrefix(override, "string:") {
			option = "--set-string"
			override = strings.TrimPrefix(override, "string:")
		}
		args = append(args, option, override)
	}
	data, err := exec.Command("helm", args...).Output()
	if err != nil {
		t.Fatalf("chart failed to render: %v", err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var docs []map[string]any
	for {
		var doc map[string]any
		err := decoder.Decode(&doc)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal("invalid rendered YAML:", err)
		}
		if len(doc) > 0 {
			docs = append(docs, doc)
		}
	}
	return docs
}

func object(t *testing.T, docs []map[string]any, kind, name string) map[string]any {
	t.Helper()
	for _, d := range docs {
		metadata := mapping(t, d["metadata"])
		if d["kind"] == kind && metadata["name"] == name {
			return d
		}
	}
	t.Fatalf("missing %s %s", kind, name)
	return nil
}
func mapping(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected mapping, got %T", v)
	}
	return m
}
func sequence(t *testing.T, v any) []any {
	t.Helper()
	s, ok := v.([]any)
	if !ok {
		t.Fatalf("expected sequence, got %T", v)
	}
	return s
}
func container(t *testing.T, pod map[string]any, key, name string) map[string]any {
	t.Helper()
	for _, v := range sequence(t, pod[key]) {
		c := mapping(t, v)
		if c["name"] == name {
			return c
		}
	}
	t.Fatalf("missing %s %s", key, name)
	return nil
}
func env(t *testing.T, c map[string]any) map[string]map[string]any {
	t.Helper()
	entries := map[string]map[string]any{}
	for _, v := range sequence(t, c["env"]) {
		e := mapping(t, v)
		name, ok := e["name"].(string)
		if !ok || entries[name] != nil {
			t.Fatal("invalid/duplicate env")
		}
		entries[name] = e
	}
	return entries
}

func sentinelOverrides() []string {
	return []string{"redis.mode=sentinel", "redis.sentinel.identitySecretName=sandbox-redis-sentinel-identity", "redis.sentinel.existingSecret=sandbox-auth"}
}

func TestSentinelNativeTopologyAndPermissions(t *testing.T) {
	docs := render(t, sentinelOverrides()...)
	sts := object(t, docs, "StatefulSet", "sandbox-redis-sentinel")
	spec := mapping(t, sts["spec"])
	if spec["replicas"] != 3 || spec["podManagementPolicy"] != "Parallel" {
		t.Fatal("Sentinel requires 3 Parallel Pods")
	}
	pod := mapping(t, mapping(t, spec["template"])["spec"])
	if pod["automountServiceAccountToken"] != false {
		t.Fatal("Redis Pods must not receive Kubernetes token")
	}
	security := mapping(t, pod["securityContext"])
	if _, ok := security["fsGroup"]; ok {
		t.Fatal("fsGroup may broaden retained600permissions")
	}
	if mapping(t, pod["nodeSelector"])["kubernetes.io/os"] != "linux" {
		t.Fatal("native Redis Pods need Linux")
	}
	affinity := mapping(t, mapping(t, pod["affinity"])["podAntiAffinity"])
	if len(sequence(t, affinity["requiredDuringSchedulingIgnoredDuringExecution"])) != 1 {
		t.Fatal("must require separate hostnames")
	}
	prepare := container(t, pod, "initContainers", "prepare")
	if mapping(t, prepare["securityContext"])["runAsUser"] != 0 {
		t.Fatal("only prepare must use root")
	}
	identity := container(t, pod, "initContainers", "identity")
	if identity["restartPolicy"] != "Always" {
		t.Fatal("identity must be native persistent sidecar")
	}
	for _, c := range []map[string]any{identity, container(t, pod, "containers", "redis"), container(t, pod, "containers", "sentinel")} {
		if mapping(t, c["securityContext"])["runAsUser"] != 999 {
			t.Fatal("all long-lived processes need same nonroot UID")
		}
		if env(t, c)["POD_ORDINAL"] == nil {
			t.Fatal("ordinal must use downward StatefulSetindex, not shell")
		}
	}
	if len(sequence(t, spec["volumeClaimTemplates"])) != 1 {
		t.Fatal("each member must get independent PVC")
	}
	service := mapping(t, object(t, docs, "Service", "sandbox-redis-sentinel-headless")["spec"])
	if service["clusterIP"] != "None" || service["publishNotReadyAddresses"] != true || len(sequence(t, service["ports"])) != 3 {
		t.Fatal("bootstrap DNS/identity endpoints missing")
	}
	if mapping(t, object(t, docs, "PodDisruptionBudget", "sandbox-redis-sentinel")["spec"])["minAvailable"] != 2 {
		t.Fatal("PDB must retain2members")
	}
	cm := object(t, docs, "ConfigMap", "sandbox-redis-sentinel-state")
	if mapping(t, mapping(t, cm["metadata"])["annotations"])["helm.sh/resource-policy"] != "keep" {
		t.Fatal("cluster identity must survive uninstall with PVC")
	}
	job := object(t, docs, "Job", "sandbox-redis-sentinel-initialize-1")
	annotations, _ := mapping(t, job["metadata"])["annotations"].(map[string]any)
	if annotations["helm.sh/hook"] != nil {
		t.Fatal("initializer must be ordinaryJob to avoid Helm/APIgate hook deadlock")
	}
	rules := sequence(t, object(t, docs, "Role", "sandbox-redis-sentinel-initialize")["rules"])
	if len(rules) != 1 {
		t.Fatal("initializer RBAC must only name stateCM")
	}
	rule := mapping(t, rules[0])
	if len(sequence(t, rule["verbs"])) != 2 || len(sequence(t, rule["resourceNames"])) != 1 || sequence(t, rule["resourceNames"])[0] != "sandbox-redis-sentinel-state" {
		t.Fatal("initializer cannot mutate arbitrary resources")
	}
	object(t, docs, "NetworkPolicy", "sandbox-redis-sentinel")
	api := object(t, docs, "Deployment", "sandbox-api")
	apiPod := mapping(t, mapping(t, mapping(t, api["spec"])["template"])["spec"])
	apiEnv := env(t, container(t, apiPod, "containers", "sandbox"))
	if apiEnv["SANDBOX_STORAGE_STATE_REDIS_MODE"]["value"] != "sentinel" || apiEnv["SANDBOX_STORAGE_STATE_REDIS_DURABILITY"]["value"] != "replica_ack" || apiEnv["SANDBOX_STORAGE_STATE_REDIS_REQUIRE_HA"]["value"] != "true" || apiEnv["SANDBOX_STORAGE_STATE_REDIS_BOOTSTRAP_STATE_DIRECTORY"]["value"] != "/bootstrap" {
		t.Fatal("APIeffectiveSentinelcontract/gate not wired")
	}
	for _, doc := range docs {
		if doc["kind"] != "Job" {
			continue
		}
		p := mapping(t, mapping(t, mapping(t, doc["spec"])["template"])["spec"])
		for _, c := range sequence(t, p["containers"]) {
			entry := mapping(t, c)
			if entry["env"] != nil {
				env(t, entry)
			}
		}
	}
}

func TestSentinelProductionSafetyAcceptsBuiltinHA(t *testing.T) {
	o := append(sentinelOverrides(), "productionSafetyChecks=true", "config.workspace.lsmProfile=sandbox-production", "config.workspace.allowMissingLSMForKind=false")
	object(t, render(t, o...), "StatefulSet", "sandbox-redis-sentinel")
}

func TestSentinelAPIStartupProbeCoversGateAndRuntimeMargin(t *testing.T) {
	o := append(sentinelOverrides(), "startupProbe.periodSeconds=7", "startupProbe.failureThreshold=1", "redis.sentinel.initializeTimeoutSeconds=1")
	d := object(t, render(t, o...), "Deployment", "sandbox-api")
	pod := mapping(t, mapping(t, mapping(t, d["spec"])["template"])["spec"])
	p := mapping(t, container(t, pod, "containers", "sandbox")["startupProbe"])
	period, failures := p["periodSeconds"].(int), p["failureThreshold"].(int)
	if (failures-1)*period < 780 {
		t.Fatal("API startup probe must cover gate budget plus runtime startup margin")
	}
	o = append(sentinelOverrides(), "startupProbe.failureThreshold=1000")
	d = object(t, render(t, o...), "Deployment", "sandbox-api")
	pod = mapping(t, mapping(t, mapping(t, d["spec"])["template"])["spec"])
	if mapping(t, container(t, pod, "containers", "sandbox")["startupProbe"])["failureThreshold"] != 1000 {
		t.Fatal("longer caller probe budgets must be preserved")
	}
}

func TestSentinelAPIStartupProbeRejectsUnsafeSettings(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	chart := filepath.Join(filepath.Dir(file), "..", "..", "deploy", "helm", "sandbox")
	for _, invalid := range []string{"enabled=false", "periodSeconds=0", "periodSeconds=1.5", "periodSeconds=61", "timeoutSeconds=0", "timeoutSeconds=61", "failureThreshold=0", "failureThreshold=-1"} {
		t.Run(invalid, func(t *testing.T) {
			args := []string{"template", "sandbox", chart, "--namespace", "release-ns", "--kube-version", "1.33.0"}
			for _, o := range append(sentinelOverrides(), "startupProbe."+invalid) {
				args = append(args, "--set", o)
			}
			if exec.Command("helm", args...).Run() == nil {
				t.Fatal("unsafe Sentinel API startup probe was accepted")
			}
		})
	}
}

func TestSentinelRollbackCannotReplayPendingBootstrapManifest(t *testing.T) {
	docs := render(t, sentinelOverrides()...)
	job := object(t, docs, "Job", "sandbox-rollback-backend-guard")
	pod := mapping(t, mapping(t, mapping(t, job["spec"])["template"])["spec"])
	c := container(t, pod, "containers", "verify-backend")
	command, args := sequence(t, c["command"]), sequence(t, c["args"])
	if len(command) != 1 || command[0] != "/app/redis-bootstrap" || len(args) != 1 || args[0] != "deny-rollback" {
		t.Fatal("Sentinel pre-rollback must deny replay before any state patch")
	}
	annotations := mapping(t, mapping(t, job["metadata"])["annotations"])
	if annotations["helm.sh/hook"] != "pre-rollback" {
		t.Fatal("Sentinel rollback guard must run before manifest mutation")
	}
	standalone := object(t, render(t), "Job", "sandbox-rollback-backend-guard")
	pod = mapping(t, mapping(t, mapping(t, standalone["spec"])["template"])["spec"])
	if sequence(t, container(t, pod, "containers", "verify-backend")["args"])[0] != "--verify-kubernetes-backend-fingerprint" {
		t.Fatal("standalone rollback behavior must remain unchanged")
	}
}

func TestSentinelInlineCustomAuthKeys(t *testing.T) {
	o := []string{"redis.mode=sentinel", "redis.sentinel.identitySecretName=sandbox-redis-sentinel-identity", "redis.password=fixture_data_012345678901234567890123456789", "redis.sentinel.password=fixture_sentinel_012345678901234567890123456789", "redis.sentinel.dataPasswordKey=data-token", "redis.sentinel.sentinelPasswordKey=sentinel-token"}
	d := mapping(t, object(t, render(t, o...), "Secret", "sandbox-redis")["data"])
	if d["data-token"] == nil || d["sentinel-token"] == nil || len(d) != 2 {
		t.Fatal("inline Secret must honor the exact keys used by every consumer")
	}
}

func TestSentinelYAMLReservedSecretNamesAndKeysRemainStrings(t *testing.T) {
	o := append(sentinelOverrides(), "string:redis.sentinel.existingSecret=true", "string:redis.sentinel.identitySecretName=123", "string:redis.sentinel.dataPasswordKey=null", "string:redis.sentinel.sentinelPasswordKey=false")
	docs := render(t, o...)
	sts := object(t, docs, "StatefulSet", "sandbox-redis-sentinel")
	pod := mapping(t, mapping(t, mapping(t, sts["spec"])["template"])["spec"])
	for _, c := range []map[string]any{container(t, pod, "initContainers", "identity"), container(t, pod, "containers", "redis"), container(t, pod, "containers", "sentinel")} {
		values := env(t, c)
		data := mapping(t, mapping(t, values["REDIS_PASSWORD"]["valueFrom"])["secretKeyRef"])
		if data["name"] != "true" || data["key"] != "null" {
			t.Fatal("literal Secret names/keys must not change YAML type")
		}
		if values["REDIS_SENTINEL_PASSWORD"] != nil {
			s := mapping(t, mapping(t, values["REDIS_SENTINEL_PASSWORD"]["valueFrom"])["secretKeyRef"])
			if s["key"] != "false" {
				t.Fatal("Sentinel key must remain string")
			}
		}
	}
	for _, v := range sequence(t, pod["volumes"]) {
		volume := mapping(t, v)
		if volume["name"] == "identity-public" || volume["name"] == "identity-source" {
			if mapping(t, volume["secret"])["secretName"] != "123" {
				t.Fatal("identity Secret name must remain literal string")
			}
		}
	}
}

func TestSentinelJobNamesFitAllRevisionsWithoutChangingMemberDNS(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	chart := filepath.Join(filepath.Dir(file), "..", "..", "deploy", "helm", "sandbox")
	helpers, err := os.ReadFile(filepath.Join(chart, "templates", "_helpers.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	private := t.TempDir()
	if err := os.Mkdir(filepath.Join(private, "templates"), 0700); err != nil {
		t.Fatal(err)
	}
	inputs := map[string][]byte{"Chart.yaml": []byte("apiVersion: v2\nname: names-test\nversion: 0.1.0\n"), "templates/_helpers.tpl": helpers, "templates/names.yaml": []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: names
data:
{{- range $revision := list 1 2 100 9223372036854775807 }}
{{- $context := dict "Release" (dict "Name" "abcdefghijklmnopqrstuvwxyzabcdefghijklm" "Revision" $revision) }}
  r{{ $revision }}: {{ include "sandbox.sentinelJobName" $context | quote }}
{{- end }}
`)}
	for name, data := range inputs {
		if err := os.WriteFile(filepath.Join(private, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	data, err := exec.Command("helm", "template", "names-test", private).Output()
	if err != nil {
		t.Fatal("private actual helper render failed:", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	names := mapping(t, doc["data"])
	seen := map[string]bool{}
	for _, value := range names {
		name, ok := value.(string)
		if !ok || len(name) > 63 || seen[name] {
			t.Fatal("Job name too long or repeats across revisions")
		}
		seen[name] = true
	}
}

func mounts(t *testing.T, c map[string]any) map[string]map[string]any {
	t.Helper()
	result := map[string]map[string]any{}
	for _, value := range sequence(t, c["volumeMounts"]) {
		m := mapping(t, value)
		name := m["name"].(string)
		if result[name] != nil {
			t.Fatal("duplicate volume mount")
		}
		result[name] = m
	}
	return result
}

func TestSentinelPrivateKeyMountAndNetworkIsolation(t *testing.T) {
	docs := render(t, sentinelOverrides()...)
	sts := object(t, docs, "StatefulSet", "sandbox-redis-sentinel")
	pod := mapping(t, mapping(t, mapping(t, sts["spec"])["template"])["spec"])
	prepare := mounts(t, container(t, pod, "initContainers", "prepare"))
	if prepare["identity-source"]["mountPath"] != "/identity-source/seed" || prepare["identity-source"]["subPathExpr"] != "$(POD_NAME)" || prepare["identity-source"]["readOnly"] != true {
		t.Fatal("prepare must mount only its own root400 seed file")
	}
	identity := container(t, pod, "initContainers", "identity")
	i := mounts(t, identity)
	if i["identity-private"] == nil || i["identity-private"]["readOnly"] != true || i["identity-source"] != nil || env(t, identity)["REDIS_SENTINEL_PASSWORD"] != nil {
		t.Fatal("identity must use own prepared key and only required data auth")
	}
	for _, name := range []string{"redis", "sentinel"} {
		m := mounts(t, container(t, pod, "containers", name))
		if m["identity-private"] != nil || m["identity-source"] != nil || m["redis-tools"]["readOnly"] != true {
			t.Fatal("Redis/Sentinel must not see private keys or writable tool")
		}
	}
	for _, v := range sequence(t, pod["volumes"]) {
		volume := mapping(t, v)
		if volume["name"] == "identity-public" {
			items := sequence(t, mapping(t, volume["secret"])["items"])
			if len(items) != 1 || mapping(t, items[0])["key"] != "public-keys.json" {
				t.Fatal("public volume must project only public JSON")
			}
		}
		if volume["name"] == "identity-source" && mapping(t, volume["secret"])["defaultMode"] != 256 {
			t.Fatal("root source seed must be400")
		}
	}
	api := object(t, docs, "Deployment", "sandbox-api")
	apiPod := mapping(t, mapping(t, mapping(t, api["spec"])["template"])["spec"])
	if apiPod["initContainers"] != nil {
		t.Fatal("Sentinel must not reuse standalone wait-for-service init")
	}
	apiMounts := mounts(t, container(t, apiPod, "containers", "sandbox"))
	if len(apiMounts) != 2 || apiMounts["redis-public"]["readOnly"] != true || apiMounts["redis-bootstrap"]["readOnly"] != true {
		t.Fatal("API must only see public JSON and state gate")
	}
	np := mapping(t, object(t, docs, "NetworkPolicy", "sandbox-redis-sentinel")["spec"])
	ingress := sequence(t, np["ingress"])
	if len(ingress) != 2 {
		t.Fatal("identity ingress must be distinct from business data/Sentinel access")
	}
	identityRule := mapping(t, ingress[1])
	ports := sequence(t, identityRule["ports"])
	if len(ports) != 1 || mapping(t, ports[0])["port"] != 18080 {
		t.Fatal("identity port rule invalid")
	}
	from := mapping(t, sequence(t, identityRule["from"])[0])
	selector := mapping(t, from["podSelector"])
	if mapping(t, selector["matchLabels"])["release"] != "sandbox" {
		t.Fatal("identity access must bind release")
	}
	values := sequence(t, mapping(t, sequence(t, selector["matchExpressions"])[0])["values"])
	if len(values) != 2 || values[0] != "sandbox-redis-sentinel" || values[1] != "sandbox-redis-bootstrap" {
		t.Fatal("API/drain must not call identity service")
	}
	dns := mapping(t, sequence(t, np["egress"])[1])
	if dns["to"] != nil || len(sequence(t, dns["ports"])) != 2 {
		t.Fatal("DNS must support NodeLocal/Calico/VPC without fixed namespace or CIDR")
	}
}

func TestSentinelRequiresKubernetes133AndIdentityMemoryBudget(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	chart := filepath.Join(filepath.Dir(file), "..", "..", "deploy", "helm", "sandbox")
	args := []string{"template", "sandbox", chart, "--namespace", "release-ns", "--kube-version", "1.29.0"}
	for _, o := range sentinelOverrides() {
		args = append(args, "--set", o)
	}
	if exec.Command("helm", args...).Run() == nil {
		t.Fatal("native identity design requires stable Kubernetes1.33 contract")
	}
	sts := object(t, render(t, sentinelOverrides()...), "StatefulSet", "sandbox-redis-sentinel")
	pod := mapping(t, mapping(t, mapping(t, sts["spec"])["template"])["spec"])
	resources := mapping(t, container(t, pod, "initContainers", "identity")["resources"])
	if mapping(t, resources["requests"])["memory"] != "64Mi" || mapping(t, resources["limits"])["memory"] != "256Mi" {
		t.Fatal("identity resource defaults differ from reviewed budget")
	}
}

func TestDefaultStandaloneAndExternalResourcesUnchanged(t *testing.T) {
	for _, o := range [][]string{nil, {"redis.enabled=false", "redis.external.addr=external-redis:6379"}} {
		docs := render(t, o...)
		for _, d := range docs {
			if mapping(t, d["metadata"])["name"] == "sandbox-redis-sentinel" {
				t.Fatal("Sentinel must stayopt-in")
			}
		}
	}
}
