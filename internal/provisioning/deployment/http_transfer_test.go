package deployment

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	"github.com/stretchr/testify/require"
)

func transferManifest(t *testing.T, transfer string) *httpManifest {
	t.Helper()
	wire := fmt.Sprintf(`{"version":1,"http":{"host":"127.0.0.1","port":8080,"api_key_file":"key","env":{},"file_transfer":%s}}`, transfer)
	m, err := parseManifest([]byte(wire))
	require.NoError(t, err)
	return m.HTTP
}

// Detects filesystem effects during startup check and shared optional settings.
func TestHTTPTransferSettingsDefaultsReadOnlyAndClone(t *testing.T) {
	base := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(base, "key"), []byte("mounted-key"), 0600))
	directory := filepath.Join(base, "not-created-by-check")
	input := transferManifest(t, fmt.Sprintf(`{"directory":%q}`, directory))
	settings, err := loadHTTPSettings(base, input, launcherconfig.Config{UID: 1000, GID: 1001})
	require.NoError(t, err)
	defer settings.Zero()
	require.Equal(t, &FileTransferSettings{Directory: directory, MaxFileBytes: 2147483648, MaxTotalBytes: 4294967296, MaxActive: 2, TimeoutSeconds: 300}, settings.FileTransfer)
	_, err = os.Lstat(directory)
	require.ErrorIs(t, err, os.ErrNotExist)
	input.FileTransfer.Directory = "/changed-by-input"
	require.Equal(t, directory, settings.FileTransfer.Directory)
	owned := settings.clone()
	defer owned.Zero()
	settings.FileTransfer.Directory = "/changed-by-settings"
	settings.FileTransfer.MaxFileBytes = 1
	require.Equal(t, directory, owned.FileTransfer.Directory)
	require.Equal(t, int64(2147483648), owned.FileTransfer.MaxFileBytes)
	legacy := &httpManifest{Host: "127.0.0.1", Port: 8080, APIKeyFile: "key", Env: map[string]string{}}
	settings, err = loadHTTPSettings(base, legacy, launcherconfig.Config{UID: 1000, GID: 1001})
	require.NoError(t, err)
	defer settings.Zero()
	require.Nil(t, settings.FileTransfer)
}

func TestHTTPTransferSettingsStrictManifestAndBudgetBounds(t *testing.T) {
	base := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(base, "key"), []byte("mounted-key"), 0600))
	for _, transfer := range []string{
		`null`, `{"directory":"/tmp/spool","max_active":null}`,
		`{"directory":"/tmp/spool","Max_active":1}`, `{"directory":"/tmp/spool","unknown":1}`,
		`{"directory":"/tmp/spool","max_active":1,"max_active":2}`,
		`{}`, `{"directory":"relative"}`, `{"directory":"/tmp/../spool"}`,
		`{"directory":"/tmp/spool","max_file_bytes":0}`, `{"directory":"/tmp/spool","max_file_bytes":-1}`,
		`{"directory":"/tmp/spool","max_file_bytes":2147483649}`,
		`{"directory":"/tmp/spool","max_total_bytes":0}`, `{"directory":"/tmp/spool","max_total_bytes":1}`,
		`{"directory":"/tmp/spool","max_total_bytes":17179869185}`,
		`{"directory":"/tmp/spool","max_active":0}`, `{"directory":"/tmp/spool","max_active":9}`,
		`{"directory":"/tmp/spool","timeout_seconds":0}`, `{"directory":"/tmp/spool","timeout_seconds":601}`,
	} {
		wire := fmt.Sprintf(`{"version":1,"http":{"host":"127.0.0.1","port":8080,"api_key_file":"key","env":{},"file_transfer":%s}}`, transfer)
		m, err := parseManifest([]byte(wire))
		if err == nil {
			settings, loadErr := loadHTTPSettings(base, m.HTTP, launcherconfig.Config{UID: 1000, GID: 1001})
			if settings != nil {
				settings.Zero()
			}
			err = loadErr
		}
		require.Error(t, err, "transfer=%s", transfer)
	}
	for _, transfer := range []string{
		`{"directory":"/tmp/spool","max_file_bytes":1,"max_total_bytes":1,"max_active":1,"timeout_seconds":1}`,
		`{"directory":"/tmp/spool","max_file_bytes":2147483648,"max_total_bytes":17179869184,"max_active":8,"timeout_seconds":600}`,
	} {
		settings, err := loadHTTPSettings(base, transferManifest(t, transfer), launcherconfig.Config{UID: 1000, GID: 1001})
		require.NoError(t, err)
		settings.Zero()
	}
}
