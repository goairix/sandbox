//go:build helmtests

package helmtest

import (
	"os/exec"
	"strings"
	"testing"
)

func TestAppArmorLoaderDisablesServiceLinks(t *testing.T) {
	for _, priorityClass := range []string{"", "trusted-loader"} {
		t.Run("priority-class="+priorityClass, func(t *testing.T) {
			ds := object(t, render(t, "apparmorLoader.enabled=true", "apparmorLoader.priorityClassName="+priorityClass), "DaemonSet", "sandbox-apparmor-loader")
			pod := mapping(t, mapping(t, mapping(t, ds["spec"])["template"])["spec"])
			if pod["enableServiceLinks"] != false {
				t.Fatal("loader must explicitly disable service links to match the startup gate contract")
			}
		})
	}
}

func TestAppArmorTemplatesSupplyMissingDefaults(t *testing.T) {
	reference := object(t, render(t, "apparmorLoader.enabled=true"), "DaemonSet", "sandbox-apparmor-loader")
	cases := []struct {
		name    string
		config  any
		omit    bool
		enabled bool
	}{
		{name: "disabled", config: map[string]any{"enabled": false}},
		{name: "omitted", omit: true},
		{name: "null"},
		{name: "empty", config: map[string]any{}},
		{name: "enabled", config: map[string]any{"enabled": true}, enabled: true},
		{name: "null-image-fields", config: map[string]any{"enabled": true, "image": map[string]any{"repository": nil, "tag": nil, "pullPolicy": nil}}, enabled: true},
		{name: "null-fields", config: map[string]any{"enabled": true, "image": nil, "resources": nil, "checkIntervalSeconds": nil, "parserTimeoutSeconds": nil, "startupTimeoutSeconds": nil}, enabled: true},
		{name: "partial-image", config: map[string]any{"enabled": true, "image": map[string]any{"repository": "example.test/loader"}}, enabled: true},
	}
	for _, schema := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(map[bool]string{false: "without-schema", true: "with-schema"}[schema]+"/"+tc.name, func(t *testing.T) {
				chart := partialSentinelChart(t, schema, func(v map[string]any) {
					v["apparmorLoader"] = tc.config
					if tc.omit {
						delete(v, "apparmorLoader")
					}
				})
				docs := renderChart(t, chart)
				assertSentinelDefaults(t, docs)
				api := mapping(t, mapping(t, mapping(t, object(t, docs, "Deployment", "sandbox-api")["spec"])["template"])["spec"])
				apiEnv := env(t, container(t, api, "containers", "sandbox"))
				if !tc.enabled {
					for _, d := range docs {
						if d["kind"] == "DaemonSet" || strings.HasPrefix(mapping(t, d["metadata"])["name"].(string), "sandbox-apparmor") {
							t.Fatal("disabled/omitted loader must not create privileged resources or RBAC")
						}
					}
					if apiEnv["SANDBOX_RUNTIME_KUBERNETES_APPARMOR_LOADER_NAME"] != nil || apiEnv["SANDBOX_WORKSPACE_BACKEND_LSM_PROFILE"]["value"] != "sandbox-fuse" || apiEnv["SANDBOX_RUNTIME_KUBERNETES_NODE_SELECTOR"] != nil {
						t.Fatal("disabled loader must preserve manual profile and default scheduling")
					}
					return
				}
				ds := object(t, docs, "DaemonSet", "sandbox-apparmor-loader")
				if tc.name != "partial-image" && string(mustJSON(t, ds)) != string(mustJSON(t, reference)) {
					t.Fatal("template defaults must produce the same complete DaemonSet as chart values.yaml defaults")
				}
				pod := mapping(t, mapping(t, mapping(t, ds["spec"])["template"])["spec"])
				if pod["enableServiceLinks"] != false {
					t.Fatal("loader must explicitly disable service links to match the startup gate contract")
				}
				c := container(t, pod, "containers", "apparmor-loader")
				wantImage := "registry.i.huaxisy.com/library/ai-infra/sandbox-apparmor-loader:v0.1.0"
				if tc.name == "partial-image" {
					wantImage = "example.test/loader:v0.1.0"
				}
				if c["image"] != wantImage || c["imagePullPolicy"] != "IfNotPresent" {
					t.Fatalf("missing image fields must resolve to documented defaults: %v", c["image"])
				}
				args := string(mustJSON(t, c["args"]))
				if !strings.Contains(args, "--check-interval=10s") || !strings.Contains(args, "--parser-timeout=10s") {
					t.Fatalf("missing timeouts rendered invalid arguments: %s", args)
				}
				resources := mapping(t, c["resources"])
				if mapping(t, resources["requests"])["cpu"] != "25m" || mapping(t, resources["limits"])["memory"] != "128Mi" {
					t.Fatal("missing resources must receive bounded defaults")
				}
				probe := mapping(t, c["readinessProbe"])
				if probe["periodSeconds"] != 10 || !strings.Contains(string(mustJSON(t, mapping(t, probe["exec"])["command"])), "--max-age=30s") {
					t.Fatal("readiness must consume the same effective check interval")
				}
				if apiEnv["SANDBOX_RUNTIME_KUBERNETES_APPARMOR_LOADER_TIMEOUT_SECONDS"]["value"] != "180" || apiEnv["SANDBOX_RUNTIME_KUBERNETES_NODE_SELECTOR"]["value"] != `{"kubernetes.io/os":"linux"}` {
					t.Fatal("API must consume effective defaults and Linux scheduling")
				}
				if mapping(t, c["securityContext"])["privileged"] != true || pod["automountServiceAccountToken"] != false {
					t.Fatal("loader security contract must remain unchanged")
				}
			})
		}
	}
}

func TestAppArmorTemplatesPreserveExplicitSettings(t *testing.T) {
	chart := partialSentinelChart(t, true, func(v map[string]any) {
		v["apparmorLoader"] = map[string]any{
			"enabled": true, "priorityClassName": "trusted-loader",
			"image":                map[string]any{"repository": "example.test/trusted-loader", "tag": "custom", "pullPolicy": "Always"},
			"resources":            map[string]any{"requests": map[string]any{"cpu": "75m", "memory": "96Mi"}},
			"checkIntervalSeconds": 7, "parserTimeoutSeconds": 23, "startupTimeoutSeconds": 321,
		}
	})
	docs := renderChart(t, chart)
	ds := object(t, docs, "DaemonSet", "sandbox-apparmor-loader")
	pod := mapping(t, mapping(t, mapping(t, ds["spec"])["template"])["spec"])
	c := container(t, pod, "containers", "apparmor-loader")
	args := string(mustJSON(t, c["args"]))
	if c["image"] != "example.test/trusted-loader:custom" || c["imagePullPolicy"] != "Always" || pod["priorityClassName"] != "trusted-loader" || !strings.Contains(args, "--check-interval=7s") || !strings.Contains(args, "--parser-timeout=23s") {
		t.Fatal("explicit image, priority, and timeouts must not be overwritten")
	}
	resources := mapping(t, c["resources"])
	if mapping(t, resources["requests"])["cpu"] != "75m" || resources["limits"] != nil {
		t.Fatal("explicit resource map must be preserved, not merged with default limits")
	}
	probe := mapping(t, c["readinessProbe"])
	if probe["periodSeconds"] != 7 || !strings.Contains(string(mustJSON(t, mapping(t, probe["exec"])["command"])), "--max-age=21s") {
		t.Fatal("readiness must use explicit interval")
	}
	api := mapping(t, mapping(t, mapping(t, object(t, docs, "Deployment", "sandbox-api")["spec"])["template"])["spec"])
	if env(t, container(t, api, "containers", "sandbox"))["SANDBOX_RUNTIME_KUBERNETES_APPARMOR_LOADER_TIMEOUT_SECONDS"]["value"] != "321" {
		t.Fatal("API must retain explicit startup timeout")
	}
}

func TestAppArmorTemplatesRejectInvalidConfigWithoutSchema(t *testing.T) {
	cases := []struct {
		key   string
		value any
	}{
		{"enabled", "false"}, {"enabled", 1},
		{"priorityClassName", false}, {"priorityClassName", "bad..class"},
		{"image", false}, {"image", "invalid"},
		{"resources", false}, {"resources", "invalid"},
		{"checkIntervalSeconds", 0}, {"checkIntervalSeconds", 601}, {"checkIntervalSeconds", "10"}, {"checkIntervalSeconds", 1.5}, {"checkIntervalSeconds", true},
		{"parserTimeoutSeconds", 0}, {"parserTimeoutSeconds", -1},
		{"startupTimeoutSeconds", 0}, {"startupTimeoutSeconds", "180"},
		{"image.repository", ""}, {"image.repository", false},
		{"image.tag", ""}, {"image.tag", false},
		{"image.pullPolicy", "Sometimes"}, {"image.pullPolicy", false},
	}
	for _, tc := range cases {
		t.Run(tc.key+"/"+string(mustJSON(t, tc.value)), func(t *testing.T) {
			chart := partialSentinelChart(t, false, func(v map[string]any) {
				cfg := map[string]any{"enabled": true}
				if strings.HasPrefix(tc.key, "image.") {
					cfg["image"] = map[string]any{strings.TrimPrefix(tc.key, "image."): tc.value}
				} else {
					cfg[tc.key] = tc.value
				}
				v["apparmorLoader"] = cfg
			})
			output, err := exec.Command("helm", "template", "sandbox", chart, "--namespace", "release-ns", "--kube-version", "1.33.0").CombinedOutput()
			if err == nil || !strings.Contains(string(output), "apparmorLoader."+tc.key) {
				t.Fatalf("invalid explicit %s must fail with a field-specific error: %v: %s", tc.key, err, output)
			}
		})
	}
}
