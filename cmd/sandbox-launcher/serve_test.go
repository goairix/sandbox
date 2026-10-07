package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProtectedConfigurationRefusesUnknownDuplicateAndMissing(t *testing.T) {
	for _, wire := range []string{`{}`, `{"version":1,"version":1}`, `{"version":1,"unknown":true}`, `null`} {
		if _, err := decodeProtectedConfig([]byte(wire)); err == nil {
			t.Fatal("unsafe config accepted")
		}
	}
	file := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(file, []byte(`{}`), 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := readProtectedConfig(file); err == nil {
		t.Fatal("unprotected config read")
	}
	link := filepath.Join(t.TempDir(), "config.json")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readProtectedConfig(link); err == nil {
		t.Fatal("symlink config read")
	}
}
func TestFixedModesRejectArgumentsAndEnvironmentSelection(t *testing.T) {
	for _, args := range [][]string{{}, {"serve", "/tmp/config"}, {"bridge", "/tmp/socket"}, {"shell"}} {
		if err := run(args); err == nil {
			t.Fatal("unfixed mode accepted")
		}
	}
	t.Setenv("SANDBOX_CONTROL_SOCKET", "/tmp/untrusted.sock")
	if err := run([]string{"bridge"}); err == nil {
		t.Fatal("user environment selected bridge")
	}
}
