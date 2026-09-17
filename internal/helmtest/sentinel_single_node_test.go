//go:build helmtests

package helmtest

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const sentinelTestNamespace = "sandbox-cce-sentinel-test-012345abcdef"
const sentinelTestRelease = "sandbox-fuse"

func singleNodeFixture(t *testing.T) []map[string]any {
	t.Helper()
	args := []string{"template", sentinelTestRelease, sentinelVersionChart(t), "--namespace", sentinelTestNamespace, "--kube-version", "1.31.0"}
	for _, override := range autoSentinelOverrides() {
		args = append(args, "--set", override)
	}
	data, err := exec.Command("helm", args...).Output()
	if err != nil {
		t.Fatal("public fixture render failed", err)
	}
	return singleNodeDecode(t, data)
}

func singleNodeDecode(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var docs []map[string]any
	for {
		var doc map[string]any
		err := decoder.Decode(&doc)
		if err == io.EOF {
			return docs
		}
		if err != nil {
			t.Fatal("fixture output is not valid YAML", err)
		}
		if len(doc) != 0 {
			docs = append(docs, doc)
		}
	}
}

func singleNodeInput(t *testing.T, docs []map[string]any) []byte {
	t.Helper()
	var input bytes.Buffer
	encoder := yaml.NewEncoder(&input)
	for _, doc := range docs {
		if err := encoder.Encode(doc); err != nil {
			t.Fatal("fixture serialization failed", err)
		}
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	return input.Bytes()
}

func singleNodeRenderer(t *testing.T, input []byte, namespace, release string) ([]byte, []byte, error) {
	t.Helper()
	renderer := filepath.Join(sentinelVersionChart(t), "..", "..", "..", "testdata", "sentinel-kubernetes-compatibility", "post-renderer.rb")
	if _, err := os.Stat(renderer); err != nil {
		t.Fatal("test renderer unavailable:", renderer, err)
	}
	cmd := exec.Command("ruby", renderer)
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, "SANDBOX_SENTINEL_TEST_NAMESPACE=") && !strings.HasPrefix(variable, "SANDBOX_SENTINEL_TEST_RELEASE=") && !strings.HasPrefix(variable, "PATH=") {
			cmd.Env = append(cmd.Env, variable)
		}
	}
	cmd.Env = append(cmd.Env, "SANDBOX_SENTINEL_TEST_NAMESPACE="+namespace, "SANDBOX_SENTINEL_TEST_RELEASE="+release)
	// Go adds its module/build directories to the test PATH. Ruby must not search
	// those potentially writable directories; this renderer uses no child tools.
	cmd.Env = append(cmd.Env, "PATH="+filepath.Dir(cmd.Path)+":/usr/bin:/bin")
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func singleNodeAffinity(t *testing.T, docs []map[string]any) map[string]any {
	t.Helper()
	sts := object(t, docs, "StatefulSet", sentinelTestRelease+"-redis-sentinel")
	pod := mapping(t, mapping(t, mapping(t, sts["spec"])["template"])["spec"])
	return mapping(t, mapping(t, pod["affinity"])["podAntiAffinity"])
}

func TestSentinelSingleNodePostRendererChangesOnlyTestAffinity(t *testing.T) {
	docs := singleNodeFixture(t)
	before, err := json.Marshal(docs)
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := singleNodeRenderer(t, singleNodeInput(t, docs), sentinelTestNamespace, sentinelTestRelease)
	if err != nil || len(stderr) != 0 {
		t.Fatal("single-node renderer must accept its exact isolated fixture", err)
	}
	result := singleNodeDecode(t, stdout)
	anti := singleNodeAffinity(t, result)
	if anti["requiredDuringSchedulingIgnoredDuringExecution"] != nil {
		t.Fatal("isolated fixture must allow same-node scheduling")
	}
	preferred := sequence(t, anti["preferredDuringSchedulingIgnoredDuringExecution"])
	if len(preferred) != 1 || mapping(t, preferred[0])["weight"] != 100 {
		t.Fatal("fixture must retain a single weighted hostname preference")
	}
	term := mapping(t, mapping(t, preferred[0])["podAffinityTerm"])
	if term["topologyKey"] != "kubernetes.io/hostname" {
		t.Fatal("hostname identity must not change")
	}
	// Restore only the expected field, then compare every resource in full.
	delete(anti, "preferredDuringSchedulingIgnoredDuringExecution")
	anti["requiredDuringSchedulingIgnoredDuringExecution"] = []any{term}
	if !reflect.DeepEqual(docs, result) {
		t.Fatal("renderer changed fields or resources outside the test affinity")
	}
	after, err := json.Marshal(docs)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("original production fixture was mutated")
	}
}

func TestSentinelSingleNodePostRendererRejectsUnsafeTargets(t *testing.T) {
	for _, name := range []string{"missing-env", "production-namespace", "wrong-release", "missing-target", "duplicate-target", "wrong-namespace", "wrong-selector", "wrong-topology", "mixed-affinity", "malformed-yaml", "yaml-alias"} {
		t.Run(name, func(t *testing.T) {
			docs := singleNodeFixture(t)
			namespace, release := sentinelTestNamespace, sentinelTestRelease
			switch name {
			case "missing-env":
				namespace, release = "", ""
			case "production-namespace":
				namespace = "aiadp-sandbox-fuse"
			case "wrong-release":
				release = "sentinel-compat"
			case "missing-target":
				mapping(t, object(t, docs, "StatefulSet", sentinelTestRelease+"-redis-sentinel")["metadata"])["name"] = "unrelated-redis"
			case "duplicate-target":
				docs = append(docs, object(t, docs, "StatefulSet", sentinelTestRelease+"-redis-sentinel"))
			case "wrong-namespace":
				mapping(t, object(t, docs, "StatefulSet", sentinelTestRelease+"-redis-sentinel")["metadata"])["namespace"] = "default"
			case "wrong-selector":
				term := mapping(t, sequence(t, singleNodeAffinity(t, docs)["requiredDuringSchedulingIgnoredDuringExecution"])[0])
				mapping(t, mapping(t, term["labelSelector"])["matchLabels"])["release"] = "unrelated-release"
			case "wrong-topology":
				mapping(t, sequence(t, singleNodeAffinity(t, docs)["requiredDuringSchedulingIgnoredDuringExecution"])[0])["topologyKey"] = "topology.kubernetes.io/zone"
			case "mixed-affinity":
				singleNodeAffinity(t, docs)["preferredDuringSchedulingIgnoredDuringExecution"] = []any{}
			}
			input := singleNodeInput(t, docs)
			if name == "malformed-yaml" {
				input = append(input, []byte("\n---\nprivate: [DO_NOT_LEAK_PRIVATE_VALUE\n")...)
			}
			if name == "yaml-alias" {
				input = append(input, []byte("\n---\nprivate: &a DO_NOT_LEAK_PRIVATE_VALUE\ncopy: *a\n")...)
			}
			stdout, stderr, err := singleNodeRenderer(t, input, namespace, release)
			if err == nil || len(stdout) != 0 || string(stderr) != "single-node Sentinel test renderer rejected input\n" {
				t.Fatalf("unsafe renderer input must fail closed without output or sensitive diagnostics (stdout bytes=%d, stderr bytes=%d, fixed message=%t)", len(stdout), len(stderr), strings.Contains(string(stderr), "single-node Sentinel test renderer rejected input"))
			}
		})
	}
}
