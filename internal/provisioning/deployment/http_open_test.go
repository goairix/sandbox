package deployment

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenHTTPRequiresSettingsBeforeInitialization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deployment.json")
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	owner, err := OpenHTTP(context.Background(), path)
	if owner != nil || err == nil || !strings.Contains(err.Error(), "HTTP") || strings.Contains(err.Error(), "launcher") {
		t.Fatal("standalone HTTP startup did not reject missing settings before launcher/backend initialization")
	}
	owner, err = Open(context.Background(), path)
	if owner != nil || err == nil || !strings.Contains(err.Error(), "launcher") {
		t.Fatal("old check loader no longer accepts manifests without HTTP settings")
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"http":{"port":"private-key-sentinel"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	owner, err = OpenHTTP(context.Background(), path)
	if owner != nil || err == nil || strings.Contains(err.Error(), "private-key-sentinel") || strings.Contains(err.Error(), "launcher") {
		t.Fatal("malformed HTTP manifest was not rejected and redacted before initialization")
	}
}
