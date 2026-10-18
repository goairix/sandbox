package deployment

import (
	"context"
	"errors"
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

// These host refusals check loader reuse and cancellation only. Initializing
// actual progress/slots/ready requires the separate original mounted native
// fixture; a malformed manifest never constitutes a positive operator proof.
func TestInitializeInventoryMountedRefusals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operator-private-path-sentinel.json")
	if err := InitializeInventory(nil, path); !errors.Is(err, errConfiguration) {
		t.Fatalf("nil operator context was accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := InitializeInventory(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled operator context attempted mounted loading: %v", err)
	}
	for _, tc := range []struct {
		name string
		wire string
		want string
	}{
		{"unknown-field", `{"version":1,"private_key":"private-material-sentinel"}`, "configuration"},
		{"duplicate", `{"version":1,"version":1}`, "configuration"},
		{"wrong-version", `{"version":3}`, "configuration"},
		{"launcher-missing", `{"version":1}`, "launcher configuration"},
		{"invalid-optional-http", `{"version":1,"http":{"port":"private-material-sentinel"}}`, "configuration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.wire), 0600); err != nil {
				t.Fatal(err)
			}
			err := InitializeInventory(context.Background(), path)
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "private-material-sentinel") || strings.Contains(err.Error(), "operator-private-path-sentinel") {
				t.Fatalf("operator did not retain strict, redacted mounted validation: %v", err)
			}
		})
	}
}
