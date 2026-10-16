package deployment

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
)

// These tests catch missing mounted-key validation, changed execution defaults,
// shared credential backing and configuration accepted outside the fixed budget.
func TestHTTPSettingsMountedDefaultsAndOwnership(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "api-key"), []byte("  mounted-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	launcher := launcherconfig.Config{UID: 1000, GID: 1001, NetworkAllowed: true}
	input := &httpManifest{Host: "127.0.0.1", Port: 8080, APIKeyFile: "api-key", Env: map[string]string{"PATH": "/usr/bin:/bin"}}
	settings, err := loadHTTPSettings(base, input, launcher)
	if err != nil {
		t.Fatal(err)
	}
	defer settings.Zero()
	if settings.Host != "127.0.0.1" || settings.Port != 8080 || string(settings.APIKey) != "mounted-secret" || settings.ExecTimeoutSeconds != 30 || settings.MaxExecTimeoutSeconds != 600 || settings.MaxActive != 32 || settings.RequestsPerSecond != 100 {
		t.Fatal("mounted settings did not resolve the bounded HTTP defaults")
	}
	if settings.UID != 1000 || settings.GID != 1001 || !settings.NetworkAllowed {
		t.Fatal("execution profile must come from the original launcher")
	}
	input.Env["PATH"] = "changed"
	if settings.Env["PATH"] != "/usr/bin:/bin" {
		t.Fatal("settings retained the manifest environment backing")
	}
	key := settings.APIKey
	settings.Zero()
	if !bytes.Equal(key, make([]byte, len(key))) {
		t.Fatal("owned mounted key was not cleared")
	}
	absent, err := loadHTTPSettings(base, nil, launcher)
	if err != nil || absent != nil {
		t.Fatal("absent HTTP configuration must preserve old startup-check manifests")
	}
}

func TestHTTPSettingsRejectInvalidAndRedactKey(t *testing.T) {
	base := t.TempDir()
	const secret = "private-key-sentinel"
	keyPath := filepath.Join(base, "api-key")
	if err := os.WriteFile(keyPath, []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	launcher := launcherconfig.Config{UID: 1000, GID: 1000}
	for name, change := range map[string]func(*httpManifest){
		"missing host":             func(m *httpManifest) { m.Host = "" },
		"DNS host":                 func(m *httpManifest) { m.Host = "localhost" },
		"missing port":             func(m *httpManifest) { m.Port = 0 },
		"large port":               func(m *httpManifest) { m.Port = 65536 },
		"missing key":              func(m *httpManifest) { m.APIKeyFile = "" },
		"missing env":              func(m *httpManifest) { m.Env = nil },
		"invalid env":              func(m *httpManifest) { m.Env = map[string]string{"1BAD": "value"} },
		"negative default":         func(m *httpManifest) { m.ExecTimeoutSeconds = -1 },
		"default above maximum":    func(m *httpManifest) { m.ExecTimeoutSeconds = 601 },
		"maximum above descriptor": func(m *httpManifest) { m.MaxExecTimeoutSeconds = 3601 },
		"negative maximum":         func(m *httpManifest) { m.MaxExecTimeoutSeconds = -1 },
		"too many active":          func(m *httpManifest) { m.MaxActive = 33 },
		"negative active":          func(m *httpManifest) { m.MaxActive = -1 },
		"too many requests":        func(m *httpManifest) { m.RequestsPerSecond = 10001 },
		"negative requests":        func(m *httpManifest) { m.RequestsPerSecond = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			input := &httpManifest{Host: "::1", Port: 8080, APIKeyFile: "api-key", Env: map[string]string{}}
			change(input)
			settings, err := loadHTTPSettings(base, input, launcher)
			if settings != nil {
				settings.Zero()
			}
			if err == nil || strings.Contains(err.Error(), secret) {
				t.Fatal("invalid settings accepted or mounted key exposed")
			}
		})
	}
	for name, key := range map[string][]byte{
		"empty":               []byte(" \n"),
		"oversize":            bytes.Repeat([]byte("s"), 4097),
		"internal whitespace": []byte(secret + " bad"),
		"control":             []byte(secret + "\x00"),
	} {
		t.Run("key "+name, func(t *testing.T) {
			if err := os.WriteFile(keyPath, key, 0600); err != nil {
				t.Fatal(err)
			}
			settings, err := loadHTTPSettings(base, &httpManifest{Host: "127.0.0.1", Port: 8080, APIKeyFile: "api-key", Env: map[string]string{}}, launcher)
			if settings != nil {
				settings.Zero()
			}
			if err == nil || strings.Contains(err.Error(), secret) {
				t.Fatal("invalid mounted key accepted or its contents exposed")
			}
		})
	}
}

func TestManifestHTTPExactEnvironmentPath(t *testing.T) {
	m, err := parseManifest([]byte(`{"version":1,"http":{"host":"127.0.0.1","port":8080,"api_key_file":"key","env":{"PATH":"/usr/bin:/bin","Mixed_Name":"ok"}}}`))
	if err != nil || m.HTTP == nil || m.HTTP.Env["PATH"] != "/usr/bin:/bin" || m.HTTP.Env["Mixed_Name"] != "ok" {
		t.Fatal("HTTP env keys must preserve their exact legitimate case")
	}
	for _, wire := range []string{
		`{"version":1,"http":{"env":{"PATH":"a","PATH":"b"}}}`,
		`{"version":1,"http":{"Host":"127.0.0.1"}}`,
		`{"version":1,"HTTP":{"env":{"PATH":"x"}}}`,
		`{"version":1,"http":{"env":{"PATH":"x"},"unknown":1}}`,
		`{"version":1,"business":{"env":{"PATH":"x"}}}`,
	} {
		if _, err := parseManifest([]byte(wire)); err == nil {
			t.Fatal("duplicate, aliased or unsupported manifest field accepted")
		}
	}
}
