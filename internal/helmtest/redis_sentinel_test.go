//go:build helmtests

package helmtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

func autoSentinelOverrides() []string {
	return []string{"redis.mode=sentinel", "redis.password=fixture_data_012345678901234567890123456789", "redis.sentinel.password=fixture_sentinel_012345678901234567890123456789"}
}

func TestSentinelAutomaticIdentityUsesOneFreshClusterID(t *testing.T) {
	docs := render(t, autoSentinelOverrides()...)
	state := object(t, docs, "ConfigMap", "sandbox-redis-sentinel-state")
	var cluster map[string]any
	if err := json.Unmarshal([]byte(mapping(t, state["data"])["cluster.json"].(string)), &cluster); err != nil {
		t.Fatal(err)
	}
	cid, ok := cluster["clusterID"].(string)
	if !ok || len(cid) != 32 || cluster["phase"] != "Pending" {
		t.Fatal("fresh cluster state missing bounded Pending identity")
	}
	sts := object(t, docs, "StatefulSet", "sandbox-redis-sentinel")
	claim := mapping(t, sequence(t, mapping(t, sts["spec"])["volumeClaimTemplates"])[0])
	if mapping(t, mapping(t, claim["metadata"])["annotations"])["sandbox/redis-cluster-id"] != cid {
		t.Fatal("PVC template must carry the exact state clusterID")
	}
	job := object(t, docs, "Job", "sandbox-redis-sentinel-identity-1")
	pod := mapping(t, mapping(t, mapping(t, job["spec"])["template"])["spec"])
	c := container(t, pod, "containers", "ensure-identity")
	args := sequence(t, c["args"])
	if len(args) == 0 || len(args)%2 != 1 {
		t.Fatal("identity arguments must contain complete flag/value pairs")
	}
	want := map[string]string{"-namespace": "release-ns", "-statefulset": "sandbox-redis-sentinel", "-secret": "sandbox-redis-sentinel-identity", "-state-configmap": "sandbox-redis-sentinel-state", "-fresh-cluster-id": cid, "-timeout": "2m"}
	if args[0] != "ensure-identity" || sequence(t, c["command"])[0] != "/app/redis-bootstrap" {
		t.Fatal("identity Job must use the current API bootstrap binary")
	}
	membersPresent := false
	for i := 1; i < len(args); i += 2 {
		flag, value := args[i].(string), args[i+1].(string)
		if flag == "-members-json" {
			membersPresent = true
			if value != string(mustJSON(t, cluster["members"])) {
				t.Fatal("identity Job members must exactly match state")
			}
			continue
		}
		if want[flag] != value {
			t.Fatalf("unexpected identity argument %s", flag)
		}
		delete(want, flag)
	}
	if len(want) != 0 || !membersPresent {
		t.Fatal("identity creation authorization/arguments missing")
	}
	for _, doc := range docs {
		if doc["kind"] == "Secret" && mapping(t, doc["metadata"])["name"] == "sandbox-redis-sentinel-identity" {
			t.Fatal("identity seed must not enter Helm release manifest")
		}
		if doc["kind"] != "Deployment" && doc["kind"] != "StatefulSet" && doc["kind"] != "Job" {
			continue
		}
		p := mapping(t, mapping(t, mapping(t, doc["spec"])["template"])["spec"])
		for _, key := range []string{"containers", "initContainers"} {
			containers, _ := p[key].([]any)
			for _, entry := range containers {
				c := mapping(t, entry)
				if c["env"] != nil {
					env(t, c)
				}
			}
		}
		volumes, _ := p["volumes"].([]any)
		for _, value := range volumes {
			v := mapping(t, value)
			if v["name"] == "identity-public" || v["name"] == "identity-source" || v["name"] == "redis-public" {
				if mapping(t, v["secret"])["secretName"] != "sandbox-redis-sentinel-identity" {
					t.Fatal("every identity consumer must use the effective Secret name")
				}
			}
		}
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSentinelIdentityJobAndLeastPrivilegeRBAC(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(map[bool]string{false: "automatic", true: "external-even-default-name"}[external], func(t *testing.T) {
			o := autoSentinelOverrides()
			if external {
				o = append(o, "redis.sentinel.identitySecretName=sandbox-redis-sentinel-identity")
			}
			docs := render(t, o...)
			job := object(t, docs, "Job", "sandbox-redis-sentinel-identity-1")
			metadata := mapping(t, job["metadata"])
			annotations, _ := metadata["annotations"].(map[string]any)
			if annotations["helm.sh/hook"] != nil || annotations["helm.sh/resource-policy"] != nil || metadata["ownerReferences"] != nil {
				t.Fatal("identity Job must be ordinary uninstallable resource")
			}
			spec := mapping(t, job["spec"])
			if spec["backoffLimit"] != 0 || spec["activeDeadlineSeconds"] != 120 {
				t.Fatal("identity Job must have bounded two-minute budget")
			}
			pod := mapping(t, mapping(t, spec["template"])["spec"])
			if pod["serviceAccountName"] != "sandbox-redis-sentinel-identity" || pod["automountServiceAccountToken"] != true || pod["volumes"] != nil || pod["initContainers"] != nil {
				t.Fatal("identity Job must only receive its dedicated API token, not Redis volumes")
			}
			c := container(t, pod, "containers", "ensure-identity")
			if c["env"] != nil || c["volumeMounts"] != nil {
				t.Fatal("identity Job must not mount seeds/PVCs or receive credentials")
			}
			security := mapping(t, c["securityContext"])
			if security["runAsUser"] != 999 || security["runAsGroup"] != 999 || security["runAsNonRoot"] != true || security["readOnlyRootFilesystem"] != true || security["allowPrivilegeEscalation"] != false || sequence(t, mapping(t, security["capabilities"])["drop"])[0] != "ALL" || mapping(t, security["seccompProfile"])["type"] != "RuntimeDefault" {
				t.Fatal("identity Job must retain hardened nonroot context")
			}
			for _, arg := range sequence(t, c["args"]) {
				if strings.Contains(strings.ToLower(arg.(string)), "password") || (external && arg == "-fresh-cluster-id") {
					t.Fatal("external identity must be readonly and arguments must contain no passwords")
				}
			}
			object(t, docs, "ServiceAccount", "sandbox-redis-sentinel-identity")
			binding := object(t, docs, "RoleBinding", "sandbox-redis-sentinel-identity")
			if mapping(t, binding["roleRef"])["kind"] != "Role" || mapping(t, binding["roleRef"])["name"] != "sandbox-redis-sentinel-identity" {
				t.Fatal("identity privileges must remain namespace-scoped")
			}
			rules := sequence(t, object(t, docs, "Role", "sandbox-redis-sentinel-identity")["rules"])
			getNames := map[string][]any{"secrets": {"sandbox-redis-sentinel-identity"}, "configmaps": {"sandbox-redis-sentinel-state"}, "persistentvolumeclaims": {"data-sandbox-redis-sentinel-0", "data-sandbox-redis-sentinel-1", "data-sandbox-redis-sentinel-2"}}
			creates := 0
			for _, entry := range rules {
				rule := mapping(t, entry)
				resources, verbs := sequence(t, rule["resources"]), sequence(t, rule["verbs"])
				if len(resources) != 1 || len(verbs) != 1 || len(sequence(t, rule["apiGroups"])) != 1 || sequence(t, rule["apiGroups"])[0] != "" {
					t.Fatal("identity RBAC must contain only single core resource/verb rules")
				}
				resource := resources[0].(string)
				if verbs[0] == "create" {
					creates++
					if external || resource != "secrets" || rule["resourceNames"] != nil {
						t.Fatal("only automatic mode may create namespace Secrets")
					}
					continue
				}
				if verbs[0] != "get" || string(mustJSON(t, rule["resourceNames"])) != string(mustJSON(t, getNames[resource])) {
					t.Fatal("GET must target only fixed identity/state/PVC names")
				}
				delete(getNames, resource)
			}
			if len(getNames) != 0 || (!external && creates != 1) || (external && creates != 0) {
				t.Fatal("identity RBAC privileges mismatch")
			}
		})
	}
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
  identity{{ $revision }}: {{ include "sandbox.sentinelIdentityJobName" $context | quote }}
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

func TestSentinelResourceAndMemberDNSLengthBoundaries(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	chart := filepath.Join(filepath.Dir(file), "..", "..", "deploy", "helm", "sandbox")
	for _, tc := range []struct {
		name, domain string
		valid        bool
	}{
		{name: "253-characters", domain: strings.Repeat("a", 62) + ".b", valid: true},
		{name: "254-characters", domain: strings.Repeat("a", 63) + ".b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"template", strings.Repeat("r", 39), chart, "--namespace", strings.Repeat("n", 63), "--kube-version", "1.33.0"}
			for _, o := range append(autoSentinelOverrides(), "redis.sentinel.clusterDomain="+tc.domain) {
				args = append(args, "--set", o)
			}
			data, err := exec.Command("helm", args...).Output()
			if !tc.valid {
				if err == nil {
					t.Fatal("member DNS exceeding 253 characters must fail rendering")
				}
				return
			}
			if err != nil {
				t.Fatal("valid boundary failed rendering:", err)
			}
			decoder := yaml.NewDecoder(bytes.NewReader(data))
			for {
				var doc map[string]any
				if err := decoder.Decode(&doc); err == io.EOF {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				if len(doc) == 0 {
					continue
				}
				name := mapping(t, doc["metadata"])["name"].(string)
				if strings.HasSuffix(name, "-redis-sentinel-identity") && (doc["kind"] == "ServiceAccount" || doc["kind"] == "Role" || doc["kind"] == "RoleBinding") && len(name) > 63 {
					t.Fatal("identity RBAC resource name exceeds DNS label boundary")
				}
				if doc["kind"] == "ConfigMap" && strings.HasSuffix(name, "-redis-sentinel-state") {
					var cluster map[string]any
					if err := json.Unmarshal([]byte(mapping(t, doc["data"])["cluster.json"].(string)), &cluster); err != nil {
						t.Fatal(err)
					}
					for _, member := range sequence(t, cluster["members"]) {
						if len(member.(string)) != 253 {
							t.Fatal("expected exact DNS boundary fixture")
						}
					}
				}
			}
		})
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
			if strings.HasPrefix(mapping(t, d["metadata"])["name"].(string), "sandbox-redis-sentinel") {
				t.Fatal("Sentinel must stayopt-in")
			}
		}
	}
}

// Exercise actual Helm lookup against an isolated HTTP API, never a business cluster.
func TestSentinelStateLookupRetainsIdentityAndFailsClosed(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	templateDir := filepath.Join(filepath.Dir(file), "..", "..", "deploy", "helm", "sandbox", "templates")
	helpers, err := os.ReadFile(filepath.Join(templateDir, "_helpers.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	sentinelTemplate, err := os.ReadFile(filepath.Join(templateDir, "redis-sentinel.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// Render the actual chart's PVC template fragment, not a duplicated test implementation.
	claimFragment := "  volumeClaimTemplates:\n" + strings.Split(strings.Split(string(sentinelTemplate), "  volumeClaimTemplates:\n")[1], "\n---")[0]
	members := []string{"sandbox-redis-sentinel-0.sandbox-redis-sentinel-headless.release-ns.svc.cluster.local", "sandbox-redis-sentinel-1.sandbox-redis-sentinel-headless.release-ns.svc.cluster.local", "sandbox-redis-sentinel-2.sandbox-redis-sentinel-headless.release-ns.svc.cluster.local"}
	retainedCluster := map[string]any{"clusterID": "retained-cid", "members": members, "phase": "Pending"}
	retainedRegistration := mustJSON(t, map[string]any{"cluster": retainedCluster, "keyDigest": strings.Repeat("a", 64), "markerIDs": []string{strings.Repeat("1", 32), strings.Repeat("2", 32), strings.Repeat("3", 32)}})
	// This fixture verifies retention of structurally valid registration bytes.
	// Binding to actual Secret keys is the runtime's responsibility, covered by
	// TestEnsureIdentityRegisteredReuse and the real three-member fixture.
	retainedData := map[string]any{"cluster.json": string(mustJSON(t, retainedCluster)), "registration.json": string(retainedRegistration)}
	for _, tc := range []struct {
		name                                    string
		cm, pvc, secret, external, wrongMembers bool
		sts                                     bool
		claimAnnotations                        map[string]any
		wantErr                                 string
	}{
		{name: "new-automatic"},
		{name: "existing-secret-only", secret: true},
		{name: "new-external", external: true},
		{name: "retained-pending-is-not-fresh", cm: true, secret: true},
		{name: "retained-full-group", cm: true, pvc: true, secret: true},
		{name: "upgrade-legacy-sts-without-marker", cm: true, pvc: true, secret: true, sts: true},
		{name: "existing-sts-with-secret-is-not-fresh", secret: true, sts: true},
		{name: "upgrade-legacy-sts-preserves-other-annotations", cm: true, pvc: true, secret: true, sts: true, claimAnnotations: map[string]any{"example.com/retained": "unchanged"}},
		{name: "upgrade-marked-sts-keeps-metadata", cm: true, pvc: true, secret: true, sts: true, claimAnnotations: map[string]any{"sandbox/redis-cluster-id": "retained-cid", "example.com/retained": "unchanged"}},
		{name: "upgrade-mismatching-marker-fails", cm: true, pvc: true, secret: true, sts: true, claimAnnotations: map[string]any{"sandbox/redis-cluster-id": "foreign-cid"}, wantErr: "PVC template clusterID differs"},
		{name: "missing-secret-with-cm", cm: true, wantErr: "restore the original identity Secret"},
		{name: "missing-secret-with-pvc", cm: true, pvc: true, wantErr: "restore the original identity Secret"},
		{name: "missing-cm-with-secret-and-pvc", pvc: true, secret: true, wantErr: "restore the original state"},
		{name: "changed-membership", cm: true, secret: true, wrongMembers: true, wantErr: "membership differs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			requests := map[string]int{}
			objects := map[string]any{}
			prefix := "/api/v1/namespaces/release-ns/"
			stsPath := "/apis/apps/v1/namespaces/release-ns/statefulsets/sandbox-redis-sentinel"
			if tc.sts {
				metadata := map[string]any{"name": "data"}
				if tc.claimAnnotations != nil {
					metadata["annotations"] = tc.claimAnnotations
				}
				objects[stsPath] = map[string]any{"apiVersion": "apps/v1", "kind": "StatefulSet", "metadata": map[string]any{"name": "sandbox-redis-sentinel", "namespace": "release-ns"}, "spec": map[string]any{"volumeClaimTemplates": []any{map[string]any{"metadata": metadata, "spec": map[string]any{"accessModes": []string{"ReadWriteOnce"}, "resources": map[string]any{"requests": map[string]any{"storage": "1Gi"}}}}}}}
			}
			if tc.cm {
				data := retainedData
				if tc.wrongMembers {
					data = map[string]any{"cluster.json": string(mustJSON(t, map[string]any{"clusterID": "retained-cid", "members": []string{"changed", "member", "names"}, "phase": "Pending"}))}
				}
				objects[prefix+"configmaps/sandbox-redis-sentinel-state"] = map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "sandbox-redis-sentinel-state", "namespace": "release-ns"}, "data": data}
			}
			if tc.pvc {
				objects[prefix+"persistentvolumeclaims/data-sandbox-redis-sentinel-0"] = map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": "data-sandbox-redis-sentinel-0", "namespace": "release-ns"}}
			}
			if tc.secret {
				objects[prefix+"secrets/sandbox-redis-sentinel-identity"] = map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "sandbox-redis-sentinel-identity", "namespace": "release-ns"}}
			}
			var renderedProbe map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				mu.Lock()
				requests[r.URL.Path]++
				mu.Unlock()
				var value any
				switch r.URL.Path {
				case "/version":
					value = map[string]any{"major": "1", "minor": "33", "gitVersion": "v1.33.0"}
				case "/api":
					value = map[string]any{"apiVersion": "v1", "kind": "APIVersions", "versions": []string{"v1"}}
				case "/apis":
					value = map[string]any{"apiVersion": "v1", "kind": "APIGroupList", "groups": []any{map[string]any{"name": "apps", "versions": []any{map[string]any{"groupVersion": "apps/v1", "version": "v1"}}, "preferredVersion": map[string]any{"groupVersion": "apps/v1", "version": "v1"}}}}
				case "/apis/apps/v1":
					value = map[string]any{"apiVersion": "v1", "kind": "APIResourceList", "groupVersion": "apps/v1", "resources": []any{map[string]any{"name": "statefulsets", "kind": "StatefulSet", "namespaced": true, "verbs": []string{"get"}}}}
				case "/api/v1":
					value = map[string]any{"apiVersion": "v1", "kind": "APIResourceList", "groupVersion": "v1", "resources": []any{map[string]any{"name": "configmaps", "kind": "ConfigMap", "namespaced": true, "verbs": []string{"get"}}, map[string]any{"name": "secrets", "kind": "Secret", "namespaced": true, "verbs": []string{"get"}}, map[string]any{"name": "persistentvolumeclaims", "kind": "PersistentVolumeClaim", "namespaced": true, "verbs": []string{"get"}}}}
				default:
					if r.URL.Path == prefix+"secrets" && r.Method == http.MethodGet {
						// Helm's own release storage queries use label selectors; lookup must not list.
						if r.URL.Query().Get("labelSelector") == "" {
							t.Error("identity helper attempted an unrestricted namespace Secret list")
						}
						value = map[string]any{"apiVersion": "v1", "kind": "SecretList", "items": []any{}}
						break
					}
					if r.Method == http.MethodPost || r.Method == http.MethodPut {
						var written map[string]any
						if err := json.NewDecoder(r.Body).Decode(&written); err != nil {
							t.Error(err)
						}
						if r.URL.Path == prefix+"configmaps" {
							mu.Lock()
							defer mu.Unlock()
							renderedProbe = written
						} else if !strings.HasPrefix(r.URL.Path, prefix+"secrets") {
							t.Errorf("unexpected mock API write: %s %s", r.Method, r.URL.Path)
						}
						value = written
						break
					}
					value = objects[r.URL.Path]
					if r.Method != http.MethodGet || value == nil {
						w.WriteHeader(http.StatusNotFound)
						value = map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Failure", "reason": "NotFound", "code": 404}
					}
				}
				if err := json.NewEncoder(w).Encode(value); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			private := t.TempDir()
			if err := os.Mkdir(filepath.Join(private, "templates"), 0700); err != nil {
				t.Fatal(err)
			}
			identityName := ""
			if tc.external {
				identityName = "sandbox-redis-sentinel-identity"
			}
			inputs := map[string][]byte{
				"Chart.yaml":             []byte("apiVersion: v2\nname: state-test\nversion: 0.1.0\n"),
				"templates/_helpers.tpl": helpers,
				"templates/state.yaml":   []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: state-probe\ndata:\n  first: {{ include \"sandbox.sentinelState\" . | quote }}\n  second: {{ include \"sandbox.sentinelState\" . | quote }}\n  claim-template: |\n{{ tpl (.Files.Get \"vct.tpl\") . | indent 4 }}\n"),
				"vct.tpl":                []byte("{{- $state := include \"sandbox.sentinelState\" . | fromJson -}}\n" + claimFragment),
				"values.yaml":            mustJSON(t, map[string]any{"redis": map[string]any{"sentinel": map[string]any{"clusterDomain": "cluster.local", "identitySecretName": identityName}, "persistence": map[string]any{"size": "1Gi", "storageClass": ""}}, "_sandboxSentinelState": map[string]any{"clusterID": "user-forged-cid", "freshClusterID": "user-forged-cid"}}),
				"kubeconfig":             []byte(fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: isolated\n  cluster:\n    server: %s\ncontexts:\n- name: isolated\n  context:\n    cluster: isolated\n    user: isolated\ncurrent-context: isolated\nusers:\n- name: isolated\n  user: {}\n", server.URL)),
			}
			for name, data := range inputs {
				if err := os.WriteFile(filepath.Join(private, name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			data, err := exec.Command("helm", "install", "sandbox", private, "--namespace", "release-ns", "--kubeconfig", filepath.Join(private, "kubeconfig"), "--disable-openapi-validation").CombinedOutput()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(string(data), tc.wantErr) {
					t.Fatalf("expected fail-closed error %q, got %v: %s", tc.wantErr, err, data)
				}
				return
			}
			if err != nil {
				t.Fatalf("isolated lookup render failed: %v: %s", err, data)
			}
			mu.Lock()
			probe := renderedProbe
			requestCounts := map[string]int{}
			for path, count := range requests {
				requestCounts[path] = count
			}
			mu.Unlock()
			values := mapping(t, probe["data"])
			if values["first"] != values["second"] {
				t.Fatal("root helper cache must reuse one exact state")
			}
			var state map[string]any
			if err := json.Unmarshal([]byte(values["first"].(string)), &state); err != nil {
				t.Fatal(err)
			}
			if tc.cm && (state["clusterID"] != "retained-cid" || string(mustJSON(t, state["data"])) != string(mustJSON(t, retainedData))) {
				t.Fatal("retained Pending CM must preserve all original data")
			}
			fresh := !tc.cm && !tc.pvc && !tc.secret && !tc.external && !tc.sts
			if (state["freshClusterID"] != "") != fresh || state["clusterID"] == "user-forged-cid" {
				t.Fatal("only genuinely new automatic install may authorize identity creation")
			}
			var renderedClaim map[string]any
			if err := yaml.Unmarshal([]byte(values["claim-template"].(string)), &renderedClaim); err != nil {
				t.Fatal(err)
			}
			claim := mapping(t, sequence(t, renderedClaim["volumeClaimTemplates"])[0])
			wantSpec := map[string]any{"accessModes": []any{"ReadWriteOnce"}, "resources": map[string]any{"requests": map[string]any{"storage": "1Gi"}}}
			if string(mustJSON(t, claim["spec"])) != string(mustJSON(t, wantSpec)) {
				t.Fatal("identity metadata compatibility must not change other PVC template fields")
			}
			annotations, _ := mapping(t, claim["metadata"])["annotations"].(map[string]any)
			if tc.sts {
				if string(mustJSON(t, annotations)) != string(mustJSON(t, tc.claimAnnotations)) {
					t.Fatal("upgrade must preserve old PVC template annotation metadata without adding or changing a marker")
				}
			} else if annotations["sandbox/redis-cluster-id"] != state["clusterID"] {
				t.Fatal("new StatefulSet PVC template must carry the exact state clusterID")
			}
			if requestCounts[stsPath] != 1 {
				t.Fatalf("lookup must perform exactly one additional fixed StatefulSet GET: %v", requestCounts)
			}
			for _, path := range []string{"configmaps/sandbox-redis-sentinel-state", "secrets/sandbox-redis-sentinel-identity", "persistentvolumeclaims/data-sandbox-redis-sentinel-0", "persistentvolumeclaims/data-sandbox-redis-sentinel-1", "persistentvolumeclaims/data-sandbox-redis-sentinel-2"} {
				if requestCounts[prefix+path] != 1 {
					t.Fatalf("lookup must perform one fixed GET for %s, requests: %v", path, requestCounts)
				}
			}
		})
	}
}
