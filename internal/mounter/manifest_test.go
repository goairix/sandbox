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

func validProfileBundle(t *testing.T) ([]byte, string) {
	t.Helper()
	hash := sha256.Sum256([]byte("trusted-s3fs"))
	digest := hex.EncodeToString(hash[:])
	descriptors := make([]ProfileDescriptor, 0, 4)
	for _, id := range []string{
		"minio-sigv4-path-style-v1",
		"minio-sigv4-path-style-private-http-v1",
		"huawei-obs-public-v1",
		"huawei-obs-private-2023-v1",
	} {
		profile, ok := InspectCompiledProfile(id)
		require.True(t, ok)
		descriptors = append(descriptors, profile.Descriptor)
	}
	raw, err := json.Marshal(ProfileBundle{
		Version: ProfileBundleVersion, Profiles: descriptors, S3FSSHA256: digest,
	})
	require.NoError(t, err)
	return raw, digest
}

func TestProfileBundleRequiresExactCompiledCatalogAndBinaryHash(t *testing.T) {
	raw, digest := validProfileBundle(t)
	require.NoError(t, validateProfileBundle(raw, digest))

	var bundle ProfileBundle
	require.NoError(t, json.Unmarshal(raw, &bundle))
	bundle.Profiles[0], bundle.Profiles[2] = bundle.Profiles[2], bundle.Profiles[0]
	reordered, err := json.Marshal(bundle)
	require.NoError(t, err)
	require.NoError(t, validateProfileBundle(reordered, digest), "descriptor order must not affect the exact-set check")

	require.NoError(t, json.Unmarshal(raw, &bundle))
	bundle.Profiles = bundle.Profiles[:2]
	missing, err := json.Marshal(bundle)
	require.NoError(t, err)
	require.ErrorContains(t, validateProfileBundle(missing, digest), "exact compiled catalog")

	require.NoError(t, json.Unmarshal(raw, &bundle))
	bundle.Profiles[0].ID = "unknown-v1"
	unknown, err := json.Marshal(bundle)
	require.NoError(t, err)
	require.ErrorContains(t, validateProfileBundle(unknown, digest), "not compiled")

	require.NoError(t, json.Unmarshal(raw, &bundle))
	bundle.Profiles[0].SignatureVersion = "sigv2"
	tampered, err := json.Marshal(bundle)
	require.NoError(t, err)
	require.ErrorContains(t, validateProfileBundle(tampered, digest), "descriptor")
	require.ErrorContains(t, validateProfileBundle(raw, "0"+digest[1:]), "SHA-256")
}

func TestBundledProfilesContainsEveryProductionProfile(t *testing.T) {
	profiles := BundledProfiles()
	for _, tc := range []struct {
		provider string
		id       string
	}{
		{provider: "minio", id: "minio-sigv4-path-style-v1"},
		{provider: "minio", id: "minio-sigv4-path-style-private-http-v1"},
		{provider: "obs", id: "huawei-obs-public-v1"},
		{provider: "obs", id: "huawei-obs-private-2023-v1"},
	} {
		profile, ok := profiles.Lookup(tc.id)
		require.True(t, ok, tc.id)
		require.Equal(t, tc.provider, profile.Provider)
		require.NoError(t, CheckProductionProfile(tc.provider, tc.id))
	}
}

func TestProfileBundleRejectsNonCanonicalSchema(t *testing.T) {
	raw, digest := validProfileBundle(t)
	var disabled ProfileBundle
	require.NoError(t, json.Unmarshal(raw, &disabled))
	disabled.Profiles[0].TLSRequired = false
	tlsDisabled, err := json.Marshal(disabled)
	require.NoError(t, err)
	duplicateID := disabled
	duplicateID.Profiles = append([]ProfileDescriptor(nil), disabled.Profiles...)
	duplicateID.Profiles[0] = duplicateID.Profiles[1]
	duplicateProfile, err := json.Marshal(duplicateID)
	require.NoError(t, err)
	cases := map[string][]byte{
		"unknown":           append(raw[:len(raw)-1], []byte(`,"options":["-o","allow_other"]}`)...),
		"extra args":        append(raw[:len(raw)-1], []byte(`,"extra_args":["-o","allow_other"]}`)...),
		"duplicate":         append(raw[:len(raw)-1], []byte(`,"version":1}`)...),
		"nested duplicate":  []byte(strings.Replace(string(raw), `"provider":"minio"`, `"provider":"minio","provider":"obs"`, 1)),
		"null":              []byte(`{"version":1,"profiles":null,"s3fs_sha256":"` + digest + `"}`),
		"missing":           []byte(`{"version":1,"s3fs_sha256":"` + digest + `"}`),
		"empty":             []byte(`{"version":1,"profiles":[],"s3fs_sha256":"` + digest + `"}`),
		"duplicate profile": duplicateProfile,
		"tls disabled":      tlsDisabled,
	}
	for name, candidate := range cases {
		t.Run(name, func(t *testing.T) {
			require.Error(t, validateProfileBundle(candidate, digest))
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
	manifest := filepath.Join(root, "profile-bundle.json")

	config := ImageCheckConfig{
		RunDir: runDir, CacheRoot: cacheRoot, ManifestPath: manifest, S3FSPath: s3fs,
		WorkspacePath: workspace, FuseConfigPath: fuseConfig, RequiredBinaries: []string{s3fs},
		ExpectedOwnerUID: os.Geteuid(), VersionTimeout: maxS3FSVersionTimeout,
	}
	rewriteBundleForS3FS(t, config)
	require.NoError(t, CheckImageWithConfig(config), "packaging checks must validate the release profile image")
	require.NoError(t, CheckImageReleaseWithConfig(config), "every bundled production profile must be releaseable")

	link := filepath.Join(root, "profile-link.json")
	require.NoError(t, os.Symlink(manifest, link))
	config.ManifestPath = link
	err := CheckImageWithConfig(config)
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
	rewriteBundleForS3FS(t, config)
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
	s3fs, manifest := filepath.Join(root, "s3fs"), filepath.Join(root, "profile-bundle.json")
	require.NoError(t, os.MkdirAll(runDir, 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(cacheRoot, "tmp"), 0o700))
	require.NoError(t, os.Mkdir(workspace, 0o555))
	require.NoError(t, os.WriteFile(fuseConfig, []byte("user_allow_other\n"), 0o644))
	require.NoError(t, os.WriteFile(s3fs, executable, 0o755))
	config := ImageCheckConfig{
		RunDir: runDir, CacheRoot: cacheRoot, ManifestPath: manifest, S3FSPath: s3fs,
		WorkspacePath: workspace, FuseConfigPath: fuseConfig, RequiredBinaries: []string{s3fs},
		ExpectedOwnerUID: os.Geteuid(), VersionTimeout: maxS3FSVersionTimeout,
	}
	rewriteBundleForS3FS(t, config)
	return config
}

func rewriteBundleForS3FS(t *testing.T, config ImageCheckConfig) {
	t.Helper()
	digest, err := sha256File(config.S3FSPath)
	require.NoError(t, err)
	descriptors := make([]ProfileDescriptor, 0, len(bundledProfileIDs))
	for _, id := range bundledProfileIDs {
		profile, ok := InspectCompiledProfile(id)
		require.True(t, ok)
		descriptors = append(descriptors, profile.Descriptor)
	}
	raw, err := json.Marshal(ProfileBundle{Version: ProfileBundleVersion, Profiles: descriptors, S3FSSHA256: digest})
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
	s3fs, manifest := filepath.Join(root, "s3fs"), filepath.Join(root, "profile-bundle.json")
	require.NoError(t, os.MkdirAll(runDir, 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(cacheRoot, "tmp"), 0o700))
	require.NoError(t, os.Mkdir(workspace, 0o755))
	require.NoError(t, os.WriteFile(fuseConfig, []byte("user_allow_other\n"), 0o644))
	require.NoError(t, os.WriteFile(s3fs, []byte("trusted-s3fs"), 0o755))
	raw, _ := validProfileBundle(t)
	require.NoError(t, os.WriteFile(manifest, raw, 0o644))
	config := ImageCheckConfig{
		RunDir: runDir, CacheRoot: cacheRoot, ManifestPath: manifest, S3FSPath: s3fs,
		WorkspacePath: workspace, FuseConfigPath: fuseConfig, RequiredBinaries: []string{s3fs},
		ExpectedOwnerUID: os.Geteuid(),
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

func TestProfileBundleDoesNotSupplyRuntimeOptions(t *testing.T) {
	raw, digest := validProfileBundle(t)
	assert.NotContains(t, string(raw), "options")
	assert.NotContains(t, string(raw), "extra_args")
	require.NoError(t, validateProfileBundle(raw, digest))
}

func TestRepositoryProfileBundleTemplateMatchesCompiledDescriptors(t *testing.T) {
	path := filepath.Join("..", "..", "docker", "images", "workspace-mounter", "profile-bundle.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	placeholder := strings.Repeat("0", sha256.Size*2)
	actualDigest := strings.Repeat("a", sha256.Size*2)
	require.Equal(t, 1, strings.Count(string(raw), placeholder), path)
	raw = []byte(strings.Replace(string(raw), placeholder, actualDigest, 1))
	var bundle ProfileBundle
	require.NoError(t, decodeStrictBundle(raw, &bundle), path)
	require.NoError(t, validateProfileBundle(raw, actualDigest), path)
	seen := make(map[string]bool, len(bundle.Profiles))
	for _, descriptor := range bundle.Profiles {
		seen[descriptor.ID] = true
	}
	assert.Equal(t, map[string]bool{
		"minio-sigv4-path-style-v1":              true,
		"minio-sigv4-path-style-private-http-v1": true,
		"huawei-obs-public-v1":                   true,
		"huawei-obs-private-2023-v1":             true,
	}, seen)
}
