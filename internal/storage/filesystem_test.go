package storage

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/goairix/sandbox/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewFileSystem_Local(t *testing.T) {
	dir := t.TempDir()
	cfg := config.FileSystemConfig{
		Provider:  "local",
		LocalPath: dir,
	}

	fsys, meta, err := NewFileSystem(cfg)
	require.NoError(t, err)
	assert.NotNil(t, fsys)
	assert.Equal(t, ProviderLocal, meta.Provider)
	assert.Equal(t, dir, meta.LocalPath)
	assert.Empty(t, meta.Bucket)
}

func TestNewFileSystemMetaIncludesRemoteConfiguration(t *testing.T) {
	cfg := config.FileSystemConfig{
		Provider: "obs",
		Bucket:   "sandbox",
		Region:   "local-1",
		Endpoint: "https://obs.example.test",
		SubPath:  "workspaces",
		UseSSL:   true,
	}

	_, meta, err := NewFileSystemWithStorageIdentity(cfg, "obs-primary")
	require.NoError(t, err)
	assert.Equal(t, &FileSystemMeta{
		Provider:        ProviderOBS,
		Bucket:          "sandbox",
		Region:          "local-1",
		Endpoint:        "https://obs.example.test",
		SubPath:         "workspaces",
		UseSSL:          true,
		StorageIdentity: "obs-primary",
	}, meta)
}

func TestNewFileSystem_LocalEmptyPath(t *testing.T) {
	cfg := config.FileSystemConfig{
		Provider:  "local",
		LocalPath: "",
	}

	_, _, err := NewFileSystem(cfg)
	assert.Error(t, err)
}

func TestNewFileSystem_UnknownProvider(t *testing.T) {
	cfg := config.FileSystemConfig{
		Provider: "unknown",
	}

	_, _, err := NewFileSystem(cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported filesystem provider")
}

func TestNewFileSystemFromConfiguredCredentialsSupportsInlineFilesAndLocal(t *testing.T) {
	t.Run("local bypasses credentials", func(t *testing.T) {
		root := t.TempDir()
		fsys, meta, err := NewFileSystemFromConfiguredCredentials(config.FileSystemConfig{Provider: "local", LocalPath: root}, "")
		require.NoError(t, err)
		require.NotNil(t, fsys)
		assert.Equal(t, ProviderLocal, meta.Provider)
	})

	t.Run("inline remote", func(t *testing.T) {
		cfg := config.FileSystemConfig{Provider: "obs", Endpoint: "https://obs.example.com", Bucket: "workspace", AccessKey: "inline-access", SecretKey: "inline-secret"}
		fsys, meta, err := NewFileSystemFromConfiguredCredentials(cfg, "physical-a")
		require.NoError(t, err)
		require.NotNil(t, fsys)
		assert.Equal(t, "physical-a", meta.StorageIdentity)
		assert.Equal(t, "inline-access", cfg.AccessKey, "caller-owned config must not be mutated")
	})

	t.Run("credential files", func(t *testing.T) {
		root := t.TempDir()
		accessFile, secretFile := filepath.Join(root, "access"), filepath.Join(root, "secret")
		require.NoError(t, os.WriteFile(accessFile, []byte("file-access"), 0o600))
		require.NoError(t, os.WriteFile(secretFile, []byte("file-secret"), 0o600))
		cfg := config.FileSystemConfig{
			Provider: "obs", Endpoint: "https://obs.example.com", Bucket: "workspace",
			CredentialFiles: config.FileSystemCredentialFileConfig{AccessKeyFile: accessFile, SecretKeyFile: secretFile},
		}
		fsys, _, err := NewFileSystemFromConfiguredCredentials(cfg, "physical-b")
		require.NoError(t, err)
		require.NotNil(t, fsys)
		assert.Empty(t, cfg.AccessKey)
		assert.Empty(t, cfg.SecretKey)
	})

	t.Run("mixed sources fail closed", func(t *testing.T) {
		_, _, err := NewFileSystemFromConfiguredCredentials(config.FileSystemConfig{
			Provider: "minio", AccessKey: "inline-access", SecretKey: "inline-secret",
			CredentialFiles: config.FileSystemCredentialFileConfig{AccessKeyFile: "/secret/access", SecretKeyFile: "/secret/key"},
		}, "physical-c")
		require.ErrorContains(t, err, "mutually exclusive")
		assert.NotContains(t, err.Error(), "inline-access")
		assert.NotContains(t, err.Error(), "inline-secret")
	})
}

func TestLoadFileSystemCredentialsInlineReturnsOwnedBuffers(t *testing.T) {
	cfg := config.FileSystemConfig{AccessKey: "access", SecretKey: "secret"}

	credentials, err := LoadFileSystemCredentials(cfg)
	require.NoError(t, err)
	assert.Equal(t, []byte("access"), credentials.AccessKey)
	assert.Equal(t, []byte("secret"), credentials.SecretKey)

	accessAlias := credentials.AccessKey
	secretAlias := credentials.SecretKey
	credentials.Zero()
	assert.Equal(t, make([]byte, len(accessAlias)), accessAlias)
	assert.Equal(t, make([]byte, len(secretAlias)), secretAlias)
	assert.Nil(t, credentials.AccessKey)
	assert.Nil(t, credentials.SecretKey)
	assert.Equal(t, "access", cfg.AccessKey)
	assert.Equal(t, "secret", cfg.SecretKey)
}

func TestLoadFileSystemCredentialsFromFilesTrimsOnlyOneNewline(t *testing.T) {
	dir := t.TempDir()
	accessPath := filepath.Join(dir, "access")
	secretPath := filepath.Join(dir, "secret")
	require.NoError(t, os.WriteFile(accessPath, []byte(" access \n\n"), 0o600))
	require.NoError(t, os.WriteFile(secretPath, []byte("secret\r\n"), 0o400))

	credentials, err := LoadFileSystemCredentials(config.FileSystemConfig{
		CredentialFiles: config.FileSystemCredentialFileConfig{
			AccessKeyFile: accessPath,
			SecretKeyFile: secretPath,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, []byte(" access \n"), credentials.AccessKey)
	assert.Equal(t, []byte("secret\r"), credentials.SecretKey)
}

func TestLoadFileSystemCredentialsRejectsMixedSources(t *testing.T) {
	_, err := LoadFileSystemCredentials(config.FileSystemConfig{
		AccessKey: "do-not-leak-access",
		CredentialFiles: config.FileSystemCredentialFileConfig{
			AccessKeyFile: "/run/secrets/access",
		},
	})
	require.ErrorContains(t, err, "mutually exclusive")
	assert.NotContains(t, err.Error(), "do-not-leak-access")
}

func TestLoadFileSystemCredentialsRejectsMissingOrEmptyValues(t *testing.T) {
	dir := t.TempDir()
	emptyPath := filepath.Join(dir, "empty")
	newlinePath := filepath.Join(dir, "newline")
	require.NoError(t, os.WriteFile(emptyPath, nil, 0o600))
	require.NoError(t, os.WriteFile(newlinePath, []byte("\n"), 0o600))

	tests := []struct {
		name string
		cfg  config.FileSystemConfig
	}{
		{name: "no source", cfg: config.FileSystemConfig{}},
		{name: "inline access only", cfg: config.FileSystemConfig{AccessKey: "access"}},
		{name: "inline secret only", cfg: config.FileSystemConfig{SecretKey: "secret"}},
		{name: "file access only", cfg: config.FileSystemConfig{CredentialFiles: config.FileSystemCredentialFileConfig{AccessKeyFile: emptyPath}}},
		{name: "file secret only", cfg: config.FileSystemConfig{CredentialFiles: config.FileSystemCredentialFileConfig{SecretKeyFile: emptyPath}}},
		{name: "empty access file", cfg: config.FileSystemConfig{CredentialFiles: config.FileSystemCredentialFileConfig{AccessKeyFile: emptyPath, SecretKeyFile: filepath.Join(dir, "missing")}}},
		{name: "newline only access file", cfg: config.FileSystemConfig{CredentialFiles: config.FileSystemCredentialFileConfig{AccessKeyFile: newlinePath, SecretKeyFile: newlinePath}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadFileSystemCredentials(tt.cfg)
			require.Error(t, err)
		})
	}
}

func TestLoadFileSystemCredentialsRejectsGroupOrWorldReadableFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix credential file permissions")
	}
	dir := t.TempDir()
	accessPath := filepath.Join(dir, "access")
	secretPath := filepath.Join(dir, "secret")
	require.NoError(t, os.WriteFile(accessPath, []byte("do-not-leak-access"), 0o640))
	require.NoError(t, os.WriteFile(secretPath, []byte("do-not-leak-secret"), 0o600))
	require.NoError(t, os.Chmod(accessPath, 0o640))
	require.NoError(t, os.Chmod(secretPath, 0o600))

	_, err := LoadFileSystemCredentials(config.FileSystemConfig{
		CredentialFiles: config.FileSystemCredentialFileConfig{
			AccessKeyFile: accessPath,
			SecretKeyFile: secretPath,
		},
	})
	require.ErrorContains(t, err, "permissions")
	assert.NotContains(t, err.Error(), "do-not-leak-access")
	assert.NotContains(t, err.Error(), "do-not-leak-secret")
}

func TestLoadCredentialFilesRetriesProjectedSecretRotationAfterOpen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows symlink replacement semantics differ")
	}
	root := t.TempDir()
	writeCredentialGeneration(t, root, "..2026_09_03", "old-access", "old-secret")
	writeCredentialGeneration(t, root, "..2026_09_04", "new-access", "new-secret")
	require.NoError(t, os.Symlink("..2026_09_03", filepath.Join(root, "..data")))
	require.NoError(t, os.Symlink("..data/access", filepath.Join(root, "access")))
	require.NoError(t, os.Symlink("..data/secret", filepath.Join(root, "secret")))

	credentials, err := loadCredentialFiles(config.FileSystemCredentialFileConfig{
		AccessKeyFile: filepath.Join(root, "access"),
		SecretKeyFile: filepath.Join(root, "secret"),
	}, credentialLoadHooks{
		afterOpen: func(attempt int) {
			if attempt != 0 {
				return
			}
			next := filepath.Join(root, "..data-next")
			require.NoError(t, os.Symlink("..2026_09_04", next))
			require.NoError(t, os.Rename(next, filepath.Join(root, "..data")))
		},
	})
	require.NoError(t, err)
	assert.Equal(t, []byte("new-access"), credentials.AccessKey)
	assert.Equal(t, []byte("new-secret"), credentials.SecretKey)
}

func TestLoadCredentialFilesReadsAlreadyValidatedFileDescriptors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows replacement semantics differ")
	}
	root := t.TempDir()
	accessPath := filepath.Join(root, "access")
	secretPath := filepath.Join(root, "secret")
	require.NoError(t, os.WriteFile(accessPath, []byte("opened-access"), 0o600))
	require.NoError(t, os.WriteFile(secretPath, []byte("opened-secret"), 0o600))

	credentials, err := loadCredentialFiles(config.FileSystemCredentialFileConfig{
		AccessKeyFile: accessPath,
		SecretKeyFile: secretPath,
	}, credentialLoadHooks{
		afterValidate: func(attempt int) {
			require.Equal(t, 0, attempt)
			replaceFile(t, accessPath, []byte("replacement-access"))
			replaceFile(t, secretPath, []byte("replacement-secret"))
		},
	})
	require.NoError(t, err)
	assert.Equal(t, []byte("opened-access"), credentials.AccessKey)
	assert.Equal(t, []byte("opened-secret"), credentials.SecretKey)
}

func writeCredentialGeneration(t *testing.T, root, generation, access, secret string) {
	t.Helper()
	dir := filepath.Join(root, generation)
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "access"), []byte(access), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secret"), []byte(secret), 0o600))
}

func replaceFile(t *testing.T, path string, value []byte) {
	t.Helper()
	replacement := path + ".replacement"
	require.NoError(t, os.WriteFile(replacement, value, 0o600))
	require.NoError(t, os.Rename(replacement, path))
}

func TestReadCredentialValueZerosPartialBufferOnError(t *testing.T) {
	readErr := errors.New("injected read failure")
	reader := &partialCredentialErrorReader{value: []byte("partial-secret"), err: readErr}

	value, err := readCredentialValue(reader, "access key")
	require.ErrorIs(t, err, readErr)
	assert.Nil(t, value)
	assert.Equal(t, make([]byte, len(reader.observed)), reader.observed)
}

type partialCredentialErrorReader struct {
	value    []byte
	err      error
	observed []byte
}

func (r *partialCredentialErrorReader) Read(buffer []byte) (int, error) {
	count := copy(buffer, r.value)
	r.observed = buffer[:count]
	return count, r.err
}
