//go:build helmtests

package helmtest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Unlike Redis compatibility fixtures, these calls exercise actual Chart defaults.
func renderEtcdDefaults(t *testing.T, overrides ...string) []map[string]any {
	t.Helper()
	return renderChart(t, sentinelVersionChart(t), overrides...)
}

func TestBuiltinEtcdDefaultsPersistAndWireEveryClient(t *testing.T) {
	docs := renderEtcdDefaults(t)
	sts := object(t, docs, "StatefulSet", "sandbox-etcd")
	spec := mapping(t, sts["spec"])
	if spec["replicas"] != 3 || spec["podManagementPolicy"] != "Parallel" {
		t.Fatal("three members must bootstrap in parallel to avoid a quorum/readiness deadlock")
	}
	claim := mapping(t, sequence(t, spec["volumeClaimTemplates"])[0])
	if mapping(t, claim["metadata"])["name"] != "data" || mapping(t, mapping(t, mapping(t, claim["spec"])["resources"])["requests"])["storage"] != "10Gi" {
		t.Fatal("each member must receive its own persistent 10Gi data volume")
	}
	pod := mapping(t, mapping(t, spec["template"])["spec"])
	c := container(t, pod, "containers", "etcd")
	if c["image"] != "quay.io/coreos/etcd:v3.6.14" || !strings.Contains(string(mustJSON(t, c["command"])), "/usr/local/bin/etcd") {
		t.Fatal("etcd must use the fixed official image and a direct binary entrypoint")
	}
	if pod["automountServiceAccountToken"] != false || mapping(t, c["securityContext"])["readOnlyRootFilesystem"] != true {
		t.Fatal("etcd must not receive Kubernetes credentials or a writable image filesystem")
	}
	args := string(mustJSON(t, c["args"]))
	for ordinal := 0; ordinal < 3; ordinal++ {
		peer := fmt.Sprintf("sandbox-etcd-%d=http://sandbox-etcd-%d.sandbox-etcd-headless.release-ns.svc.cluster.local:2380", ordinal, ordinal)
		if !strings.Contains(args, peer) {
			t.Fatalf("initial membership missing %s", peer)
		}
	}
	if strings.Contains(args, "force-new-cluster") {
		t.Fatal("normal startup must never recreate persisted membership")
	}
	affinity := mapping(t, mapping(t, pod["affinity"])["podAntiAffinity"])
	if len(sequence(t, affinity["requiredDuringSchedulingIgnoredDuringExecution"])) != 1 {
		t.Fatal("members must be separated across nodes")
	}
	headless := mapping(t, object(t, docs, "Service", "sandbox-etcd-headless")["spec"])
	if headless["clusterIP"] != "None" || headless["publishNotReadyAddresses"] != true {
		t.Fatal("peer discovery must work before quorum readiness")
	}
	if mapping(t, object(t, docs, "PodDisruptionBudget", "sandbox-etcd")["spec"])["minAvailable"] != 2 {
		t.Fatal("quorum requires two available members")
	}
	var endpoints []string
	for i := 0; i < 3; i++ {
		endpoints = append(endpoints, fmt.Sprintf("sandbox-etcd-%d.sandbox-etcd-headless.release-ns.svc.cluster.local:2379", i))
	}
	for _, target := range [][3]string{{"Deployment", "sandbox-api", "sandbox"}, {"Job", "sandbox-api-drain", "drain"}, {"Job", "sandbox-backend-change-drain", "drain"}} {
		p := mapping(t, mapping(t, mapping(t, object(t, docs, target[0], target[1])["spec"])["template"])["spec"])
		environment := env(t, container(t, p, "containers", target[2]))
		if environment["SANDBOX_STORAGE_STATE_ETCD_ENDPOINTS"]["value"] != strings.Join(endpoints, ",") {
			t.Fatalf("%s does not use all three internal client endpoints", target[1])
		}
		for name := range environment {
			if strings.HasPrefix(name, "SANDBOX_STORAGE_STATE_REDIS_") {
				t.Fatalf("%s retains Redis env", target[1])
			}
		}
	}
	for _, d := range docs {
		if d["kind"] == "StatefulSet" && strings.Contains(mapping(t, d["metadata"])["name"].(string), "redis") {
			t.Fatal("defaults must not install Redis")
		}
	}
}

func TestBuiltinEtcdStorageAndAPIOnlyUpgrade(t *testing.T) {
	settings := []string{"etcd.persistence.storageClass=fast-ssd", "etcd.persistence.size=20Gi", "image.pullSecretName=registry-auth", "etcd.clusterDomain=corp.internal"}
	docs := renderEtcdDefaults(t, settings...)
	sts := object(t, docs, "StatefulSet", "sandbox-etcd")
	claim := mapping(t, sequence(t, mapping(t, sts["spec"])["volumeClaimTemplates"])[0])
	if mapping(t, claim["spec"])["storageClassName"] != "fast-ssd" || mapping(t, mapping(t, mapping(t, claim["spec"])["resources"])["requests"])["storage"] != "20Gi" {
		t.Fatal("storage class and capacity must reach each claim")
	}
	pod := mapping(t, mapping(t, mapping(t, sts["spec"])["template"])["spec"])
	if !strings.Contains(string(mustJSON(t, pod["imagePullSecrets"])), "registry-auth") {
		t.Fatal("private etcd images must use configured pull credentials")
	}
	if !strings.Contains(string(mustJSON(t, container(t, pod, "containers", "etcd")["args"])), "corp.internal") {
		t.Fatal("cluster domain must reach peer membership")
	}
	changed := object(t, renderEtcdDefaults(t, append(settings, "image.tag=v0.4.1")...), "StatefulSet", "sandbox-etcd")
	if !reflect.DeepEqual(sts, changed) {
		t.Fatal("API-only upgrade must not roll etcd or mutate its claims")
	}
}

func TestExternalEtcdAndExplicitRedisDoNotInstallEtcd(t *testing.T) {
	for _, overrides := range [][]string{
		{"etcd.enabled=false", "config.storage.state.etcd.endpoints={external-a:2379,external-b:2379,external-c:2379}", "config.storage.state.etcd.existingSecret=etcd-mtls"},
		{"etcd.enabled=false", "config.storage.state.backend=redis", "redis.enabled=true"},
	} {
		docs := renderEtcdDefaults(t, overrides...)
		for _, d := range docs {
			name := mapping(t, d["metadata"])["name"].(string)
			if (d["kind"] == "StatefulSet" || d["kind"] == "Service" || d["kind"] == "PodDisruptionBudget") && strings.HasPrefix(name, "sandbox-etcd") {
				t.Fatal("external/Redis mode must not install an etcd cluster")
			}
		}
	}
}

func TestBuiltinEtcdRejectsUnsafeConfiguration(t *testing.T) {
	for _, overrides := range [][]string{
		{"etcd.persistence.enabled=false"}, {"etcd.replicaCount=1"}, {"etcd.replicaCount=5"},
		{"config.storage.state.etcd.endpoints={unexpected:2379}"},
		{"config.storage.state.etcd.existingSecret=client-only-tls"},
		{"etcd.enabled=false"},
		{"productionSafetyChecks=true", "etcd.hardAntiAffinity=false"},
		{"productionSafetyChecks=true", "networkPolicy.enabled=false"},
		{"etcd.persistence.size=1Gi"}, {"etcd.autoCompactionRetention=0h"},
		{"etcd.quotaBackendBytes=8589934593"},
	} {
		t.Run(strings.Join(overrides, "/"), func(t *testing.T) {
			for _, schema := range []bool{true, false} {
				chart := copyHelmChart(t)
				if !schema {
					if err := os.Remove(filepath.Join(chart, "values.schema.json")); err != nil {
						t.Fatal(err)
					}
				}
				args := []string{"template", "sandbox", chart, "--namespace", "release-ns"}
				for _, override := range overrides {
					args = append(args, "--set", override)
				}
				if _, err := exec.Command("helm", args...).CombinedOutput(); err == nil {
					t.Fatalf("unsafe config rendered (schema=%v)", schema)
				}
			}
		})
	}
}

func TestBuiltinEtcdBoundsHistoryAndDiskQuota(t *testing.T) {
	docs := renderEtcdDefaults(t)
	sts := object(t, docs, "StatefulSet", "sandbox-etcd")
	pod := mapping(t, mapping(t, mapping(t, sts["spec"])["template"])["spec"])
	args := string(mustJSON(t, container(t, pod, "containers", "etcd")["args"]))
	for _, flag := range []string{"--auto-compaction-mode=periodic", "--auto-compaction-retention=1h", "--quota-backend-bytes=2147483648"} {
		if !strings.Contains(args, flag) {
			t.Fatalf("persistent state history must be bounded: missing %s", flag)
		}
	}
}
