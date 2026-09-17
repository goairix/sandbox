package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goairix/sandbox/internal/config"
	"github.com/stretchr/testify/require"
)

func dnsConfigEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SANDBOX_SECURITY_API_KEY", "test")
	t.Setenv("SANDBOX_RUNTIME_TYPE", "kubernetes")
	t.Setenv("SANDBOX_RUNTIME_KUBERNETES_NAMESPACE", "test")
}

func TestDNSAdmissionConfigEnv(t *testing.T) {
	dnsConfigEnv(t)
	for _, tc := range []struct {
		name, input string
		valid       bool
	}{
		{"empty", `[]`, true},
		{"valid", `[{"name":"timeout","value":"2"},{"name":"single-request-reopen","value":""}]`, true},
		{"null", `null`, false},
		{"object", `{}`, false},
		{"unknown", `[{"name":"ndots","value":"5"}]`, false},
		{"missing value", `[{"name":"single-request-reopen"}]`, false},
		{"number", `[{"name":"timeout","value":2}]`, false},
		{"null value", `[{"name":"timeout","value":null}]`, false},
		{"unknown property", `[{"name":"timeout","value":"2","extra":"private-marker"}]`, false},
		{"duplicate property", `[{"name":"timeout","name":"timeout","value":"2"}]`, false},
		{"duplicate option", `[{"name":"timeout","value":"2"},{"name":"timeout","value":"2"}]`, false},
		{"trailing", `[] []`, false},
		{"yaml not json", `- name: timeout`, false},
		{"size", strings.Repeat(" ", 1024) + `[]`, false},
		{"maximum size", strings.Repeat(" ", 1022) + `[]`, true},
		{"one over maximum size", strings.Repeat(" ", 1023) + `[]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SANDBOX_RUNTIME_KUBERNETES_DNS_OPTIONS", tc.input)
			cfg, err := config.Load("")
			if !tc.valid {
				require.Error(t, err)
				require.Contains(t, err.Error(), "dns_options")
				require.NotContains(t, err.Error(), "private-marker")
				return
			}
			require.NoError(t, err)
			if tc.name == "valid" {
				require.Equal(t, "single-request-reopen", cfg.Runtime.Kubernetes.DNSOptions[0].Name)
			}
		})
	}
	for _, input := range []string{"true", "false", "1", "TRUE", "", "private-marker"} {
		t.Run("bool "+input, func(t *testing.T) {
			t.Setenv("SANDBOX_RUNTIME_KUBERNETES_DISABLE_NODE_LOCAL_DNS_INJECTION", input)
			cfg, err := config.Load("")
			if input == "true" || input == "false" {
				require.NoError(t, err)
				require.Equal(t, input == "true", cfg.Runtime.Kubernetes.DisableNodeLocalDNSInjection)
			} else {
				require.ErrorContains(t, err, "disable_node_local_dns_injection")
			}
		})
	}
}

func TestDNSAdmissionConfigPerFieldPrecedence(t *testing.T) {
	dnsConfigEnv(t)
	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"runtime":{"kubernetes":{"namespace":"test","disable_node_local_dns_injection":false,"dns_options":[{"name":"timeout","value":"2"}]}}}`), 0o600))
	t.Setenv("SANDBOX_RUNTIME_KUBERNETES_DISABLE_NODE_LOCAL_DNS_INJECTION", "true")
	cfg, err := config.Load(path)
	require.NoError(t, err)
	require.True(t, cfg.Runtime.Kubernetes.DisableNodeLocalDNSInjection)
	require.Len(t, cfg.Runtime.Kubernetes.DNSOptions, 1)
	require.Equal(t, "2", cfg.Runtime.Kubernetes.DNSOptions[0].Value)
	t.Setenv("SANDBOX_RUNTIME_KUBERNETES_DNS_OPTIONS", `[]`)
	cfg, err = config.Load(path)
	require.NoError(t, err)
	require.Empty(t, cfg.Runtime.Kubernetes.DNSOptions)
}

func TestDNSAdmissionConfigFile(t *testing.T) {
	dnsConfigEnv(t)
	for _, tc := range []struct {
		name, input string
		valid       bool
	}{
		{"omitted", "", true},
		{"defaults", "    disable_node_local_dns_injection: false\n    dns_options: []\n", true},
		{"valid", "    disable_node_local_dns_injection: true\n    dns_options:\n      - name: timeout\n        value: \"2\"\n", true},
		{"bool string", "    disable_node_local_dns_injection: \"true\"\n", false},
		{"bool null", "    disable_node_local_dns_injection: null\n", false},
		{"options null", "    dns_options: null\n", false},
		{"missing value", "    dns_options:\n      - name: single-request-reopen\n", false},
		{"number", "    dns_options:\n      - name: timeout\n        value: 2\n", false},
		{"case property", "    dns_options:\n      - Name: timeout\n        value: \"2\"\n", false},
		{"option alias", "    anchor: &opts []\n    dns_options: *opts\n", false},
		{"item alias", "    anchor: &opt {name: timeout, value: \"2\"}\n    dns_options: [*opt]\n", false},
		{"value alias", "    anchor: &val \"2\"\n    dns_options: [{name: timeout, value: *val}]\n", false},
		{"bool alias", "    anchor: &val true\n    disable_node_local_dns_injection: *val\n", false},
		{"alias", "    dns_options: &opts []\n    disable_node_local_dns_injection: *opts\n", false},
		{"merge", "    <<: {dns_options: []}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(path, []byte("runtime:\n  kubernetes:\n    namespace: test\n"+tc.input), 0600))
			_, err := config.Load(path)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestDNSAdmissionConfigPrecedenceAndValidation(t *testing.T) {
	dnsConfigEnv(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("runtime:\n  kubernetes:\n    disable_node_local_dns_injection: private-marker\n    dns_options: private-marker\n"), 0600))
	t.Setenv("SANDBOX_RUNTIME_KUBERNETES_DISABLE_NODE_LOCAL_DNS_INJECTION", "true")
	t.Setenv("SANDBOX_RUNTIME_KUBERNETES_DNS_OPTIONS", `[{"name":"timeout","value":"2"}]`)
	cfg, err := config.Load(path)
	require.NoError(t, err)
	require.True(t, cfg.Runtime.Kubernetes.DisableNodeLocalDNSInjection)
	require.Len(t, cfg.Runtime.Kubernetes.DNSOptions, 1)
	t.Setenv("SANDBOX_RUNTIME_TYPE", "docker")
	_, err = config.Load(path)
	require.ErrorContains(t, err, "DNS admission")
	for _, input := range []string{
		"runtime: &runtime\n  kubernetes:\n    dns_options: []\nRUNTIME: *runtime\n",
		"runtime:\n  kubernetes: &k\n    dns_options: []\n  KUBERNETES: *k\n",
		"defaults: &runtime\n  kubernetes:\n    dns_options: []\nruntime: *runtime\n",
		"runtime:\n  defaults: &k\n    dns_options: []\n  kubernetes: *k\n",
		"runtime.kubernetes.dns_options: []\n",
	} {
		t.Run("structural "+fmt.Sprint(len(input)), func(t *testing.T) {
			// The adopted file setting is tested without any env override.
			t.Setenv("SANDBOX_RUNTIME_TYPE", "kubernetes")
			t.Setenv("SANDBOX_RUNTIME_KUBERNETES_DNS_OPTIONS", "")
			require.NoError(t, os.Unsetenv("SANDBOX_RUNTIME_KUBERNETES_DNS_OPTIONS"))
			p := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(p, []byte(input), 0600))
			_, err := config.Load(p)
			require.Error(t, err)
		})
	}
}
