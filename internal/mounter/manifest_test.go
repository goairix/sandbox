package mounter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validS3FSVersionStub = `#!/bin/sh
test "$#" -eq 1
test "$1" = "--version"
test "$PATH" = "/usr/bin:/bin"
test "$LANG" = "C"
test "$LC_ALL" = "C"
test "$HOME" = "/nonexistent"
test -z "${S3FS_PACKAGE_URL:-}"
printf '%s\n' 'Amazon Simple Storage Service File System V1.95 (commit:test) with OpenSSL'
`

func validManifest(t *testing.T) ([]byte, string) {
	t.Helper()
	profile, ok := InspectCompiledProfile("minio-sigv4-path-style-v1")
	require.True(t, ok)
	hash := sha256.Sum256([]byte("trusted-s3fs"))
	digest := hex.EncodeToString(hash[:])
	raw, err := json.Marshal(ProfileManifest{Version: ProfileManifestVersion, Profile: profile.Descriptor, S3FSSHA256: digest})
	require.NoError(t, err)
	return raw, digest
}

func TestProfileManifestRequiresExactCompiledDescriptorAndBinaryHash(t *testing.T) {
	raw, digest := validManifest(t)
	require.NoError(t, validateProfileManifest(raw, "minio-sigv4-path-style-v1", digest))

	var manifest ProfileManifest
	require.NoError(t, json.Unmarshal(raw, &manifest))
	manifest.Profile.SignatureVersion = "sigv2"
	tampered, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.ErrorContains(t, validateProfileManifest(tampered, "minio-sigv4-path-style-v1", digest), "descriptor")
	require.ErrorContains(t, validateProfileManifest(raw, "huawei-obs-public-v1", digest), "binding")
	require.ErrorContains(t, validateProfileManifest(raw, "minio-sigv4-path-style-v1", "0"+digest[1:]), "SHA-256")
}

func TestProfileManifestRejectsNonCanonicalSchema(t *testing.T) {
	raw, digest := validManifest(t)
	var disabled ProfileManifest
	require.NoError(t, json.Unmarshal(raw, &disabled))
	disabled.Profile.TLSRequired = false
	tlsDisabled, err := json.Marshal(disabled)
	require.NoError(t, err)
	cases := map[string][]byte{
		"unknown":          append(raw[:len(raw)-1], []byte(`,"options":["-o","allow_other"]}`)...),
		"extra args":       append(raw[:len(raw)-1], []byte(`,"extra_args":["-o","allow_other"]}`)...),
		"duplicate":        append(raw[:len(raw)-1], []byte(`,"version":1}`)...),
		"nested duplicate": []byte(strings.Replace(string(raw), `"provider":"minio"`, `"provider":"minio","provider":"obs"`, 1)),
		"null":             []byte(`{"version":1,"profile":null,"s3fs_sha256":"` + digest + `"}`),
		"missing":          []byte(`{"version":1,"s3fs_sha256":"` + digest + `"}`),
		"multiple":         []byte(`{"version":1,"profiles":[],"s3fs_sha256":"` + digest + `"}`),
		"tls disabled":     tlsDisabled,
	}
	for name, candidate := range cases {
		t.Run(name, func(t *testing.T) {
			require.Error(t, validateProfileManifest(candidate, "minio-sigv4-path-style-v1", digest))
		})
	}
}

func TestCheckImageContractValidatesTrustedRegularManifestAndS3FS(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix ownership and executable mode contract")
	}
	root := t.TempDir()
	runDir := filepath.Join(root, "run")
	cacheRoot := filepath.Join(root, "cache")
	workspace := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(runDir, 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(cacheRoot, "tmp"), 0o700))
	require.NoError(t, os.Mkdir(workspace, 0o555))
	fuseConfig := filepath.Join(root, "fuse.conf")
	require.NoError(t, os.WriteFile(fuseConfig, []byte("user_allow_other\n"), 0o644))

	s3fs := filepath.Join(root, "s3fs")
	require.NoError(t, os.WriteFile(s3fs, []byte(validS3FSVersionStub), 0o755))
	manifest := filepath.Join(root, "profile.json")

	config := ImageCheckConfig{
		RunDir: runDir, CacheRoot: cacheRoot, ManifestPath: manifest, S3FSPath: s3fs,
		WorkspacePath: workspace, FuseConfigPath: fuseConfig, RequiredBinaries: []string{s3fs},
		BoundProfileID: "minio-sigv4-path-style-v1", ExpectedOwnerUID: os.Geteuid(),
	}
	rewriteManifestForS3FS(t, config)
	require.NoError(t, CheckImageWithConfig(config), "packaging checks must permit a blocked profile to be built and tested")
	err := CheckImageReleaseWithConfig(config)
	require.ErrorContains(t, err, "pending durable-flush provider spike", "release readiness remains fail closed without flush evidence")

	public, ok := InspectCompiledProfile("huawei-obs-public-v1")
	require.True(t, ok)
	actualDigest, err := sha256File(s3fs)
	require.NoError(t, err)
	publicManifest, err := json.Marshal(ProfileManifest{Version: ProfileManifestVersion, Profile: public.Descriptor, S3FSSHA256: actualDigest})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifest, publicManifest, 0o644))
	config.BoundProfileID = public.Descriptor.ID
	require.NoError(t, CheckImageWithConfig(config), "candidate packaging must be testable")
	require.ErrorContains(t, CheckImageReleaseWithConfig(config), "pending provider spike")

	link := filepath.Join(root, "profile-link.json")
	require.NoError(t, os.Symlink(manifest, link))
	config.ManifestPath = link
	err = CheckImageWithConfig(config)
	require.ErrorContains(t, err, "regular non-symlink")

	config.ManifestPath = manifest
	require.NoError(t, os.WriteFile(s3fs, []byte("changed"), 0o755))
	err = CheckImageWithConfig(config)
	require.ErrorContains(t, err, "SHA-256")
}

func TestCheckImageContractExecutesFixedS3FSVersionProbe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix executable image contract")
	}
	t.Setenv("S3FS_PACKAGE_URL", "must-not-reach-child")
	config := newExecutableImageCheckConfig(t, []byte(validS3FSVersionStub))
	require.NoError(t, CheckImageWithConfig(config))

	require.NoError(t, os.WriteFile(config.S3FSPath, []byte("not an executable image"), 0o755))
	rewriteManifestForS3FS(t, config)
	require.ErrorContains(t, CheckImageWithConfig(config), "version probe")
}

func TestCheckImageContractRejectsInvalidOrHungS3FSVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix executable image contract")
	}
	for name, executable := range map[string][]byte{
		"wrong identity": []byte("#!/bin/sh\nprintf 's3fs compatible wrapper 1.0\\n'\n"),
		"timeout":        []byte("#!/bin/sh\nsleep 10\n"),
	} {
		t.Run(name, func(t *testing.T) {
			config := newExecutableImageCheckConfig(t, executable)
			config.VersionTimeout = 20 * time.Millisecond
			require.ErrorContains(t, CheckImageWithConfig(config), "version probe")
		})
	}
}

func newExecutableImageCheckConfig(t *testing.T, executable []byte) ImageCheckConfig {
	t.Helper()
	root := t.TempDir()
	runDir, cacheRoot := filepath.Join(root, "run"), filepath.Join(root, "cache")
	workspace, fuseConfig := filepath.Join(root, "workspace"), filepath.Join(root, "fuse.conf")
	s3fs, manifest := filepath.Join(root, "s3fs"), filepath.Join(root, "profile.json")
	require.NoError(t, os.MkdirAll(runDir, 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(cacheRoot, "tmp"), 0o700))
	require.NoError(t, os.Mkdir(workspace, 0o555))
	require.NoError(t, os.WriteFile(fuseConfig, []byte("user_allow_other\n"), 0o644))
	require.NoError(t, os.WriteFile(s3fs, executable, 0o755))
	config := ImageCheckConfig{
		RunDir: runDir, CacheRoot: cacheRoot, ManifestPath: manifest, S3FSPath: s3fs,
		WorkspacePath: workspace, FuseConfigPath: fuseConfig, RequiredBinaries: []string{s3fs},
		BoundProfileID: "minio-sigv4-path-style-v1", ExpectedOwnerUID: os.Geteuid(),
	}
	rewriteManifestForS3FS(t, config)
	return config
}

func rewriteManifestForS3FS(t *testing.T, config ImageCheckConfig) {
	t.Helper()
	digest, err := sha256File(config.S3FSPath)
	require.NoError(t, err)
	profile, ok := InspectCompiledProfile(config.BoundProfileID)
	require.True(t, ok)
	raw, err := json.Marshal(ProfileManifest{Version: ProfileManifestVersion, Profile: profile.Descriptor, S3FSSHA256: digest})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(config.ManifestPath, raw, 0o644))
}

func TestCheckImageContractRejectsUnsafeWorkspaceAndFuseConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix ownership and executable mode contract")
	}
	root := t.TempDir()
	runDir, cacheRoot := filepath.Join(root, "run"), filepath.Join(root, "cache")
	workspace, fuseConfig := filepath.Join(root, "workspace"), filepath.Join(root, "fuse.conf")
	s3fs, manifest := filepath.Join(root, "s3fs"), filepath.Join(root, "profile.json")
	require.NoError(t, os.MkdirAll(runDir, 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(cacheRoot, "tmp"), 0o700))
	require.NoError(t, os.Mkdir(workspace, 0o755))
	require.NoError(t, os.WriteFile(fuseConfig, []byte("user_allow_other\n"), 0o644))
	require.NoError(t, os.WriteFile(s3fs, []byte("trusted-s3fs"), 0o755))
	raw, _ := validManifest(t)
	require.NoError(t, os.WriteFile(manifest, raw, 0o644))
	config := ImageCheckConfig{
		RunDir: runDir, CacheRoot: cacheRoot, ManifestPath: manifest, S3FSPath: s3fs,
		WorkspacePath: workspace, FuseConfigPath: fuseConfig, RequiredBinaries: []string{s3fs},
		BoundProfileID: "minio-sigv4-path-style-v1", ExpectedOwnerUID: os.Geteuid(),
	}
	require.ErrorContains(t, CheckImageWithConfig(config), "workspace anchor")

	require.NoError(t, os.Chmod(workspace, 0o555))
	for name, content := range map[string]string{
		"duplicate": "user_allow_other\nuser_allow_other\n",
		"extra":     "user_allow_other\nmount_max = 100000\n",
		"missing":   "# user_allow_other\n",
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, os.Chmod(fuseConfig, 0o644))
			require.NoError(t, os.WriteFile(fuseConfig, []byte(content), 0o444))
			require.NoError(t, os.Chmod(fuseConfig, 0o444))
			require.ErrorContains(t, CheckImageWithConfig(config), "fuse.conf")
		})
	}

	require.NoError(t, os.Chmod(fuseConfig, 0o644))
	require.NoError(t, os.WriteFile(fuseConfig, []byte("user_allow_other\n"), 0o644))
	require.NoError(t, os.Chmod(fuseConfig, 0o666))
	require.ErrorContains(t, CheckImageWithConfig(config), "fuse.conf")
}

func TestProfileManifestDoesNotSupplyRuntimeOptions(t *testing.T) {
	raw, digest := validManifest(t)
	assert.NotContains(t, string(raw), "options")
	assert.NotContains(t, string(raw), "extra_args")
	require.NoError(t, validateProfileManifest(raw, "minio-sigv4-path-style-v1", digest))
}

func TestRepositoryProfileManifestTemplatesMatchCompiledDescriptors(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "docker", "images", "workspace-mounter", "profiles", "*.json"))
	require.NoError(t, err)
	require.Len(t, paths, 3)
	placeholder := strings.Repeat("0", sha256.Size*2)
	actualDigest := strings.Repeat("a", sha256.Size*2)
	seen := make(map[string]bool)
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, 1, strings.Count(string(raw), placeholder), path)
		raw = []byte(strings.Replace(string(raw), placeholder, actualDigest, 1))

		var manifest ProfileManifest
		require.NoError(t, decodeStrictManifest(raw, &manifest), path)
		require.NoError(t, validateProfileManifest(raw, manifest.Profile.ID, actualDigest), path)
		seen[manifest.Profile.ID] = true
	}
	assert.Equal(t, map[string]bool{
		"minio-sigv4-path-style-v1":  true,
		"huawei-obs-public-v1":       true,
		"huawei-obs-private-2023-v1": true,
	}, seen)
}
