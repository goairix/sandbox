package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/fuseprotocol"
	"github.com/goairix/sandbox/internal/mounter"
)

func TestRunRejectsUndocumentedGrammarBeforeIO(t *testing.T) {
	require.Error(t, run([]string{"workspace-mounter", "broker"}, strings.NewReader("secret"), &bytes.Buffer{}))
	require.Error(t, run(nil, strings.NewReader("secret"), &bytes.Buffer{}))
}

func TestCommonMounterBinaryContainsEveryProductionProfile(t *testing.T) {
	registry := mounter.BundledProfiles()
	for _, id := range []string{
		"minio-sigv4-path-style-v1",
		"huawei-obs-public-v1",
		"huawei-obs-private-2023-v1",
	} {
		_, ok := registry.Lookup(id)
		assert.True(t, ok, id)
	}
	_, ok := registry.Lookup("unknown-v1")
	assert.False(t, ok)
}

func TestReleaseImageCLIUsesGoReleaseGate(t *testing.T) {
	originalPackage, originalRelease := checkPackagedImage, checkReleaseImage
	t.Cleanup(func() { checkPackagedImage, checkReleaseImage = originalPackage, originalRelease })
	packageCalls, releaseCalls := 0, 0
	checkPackagedImage = func(string, string) error { packageCalls++; return nil }
	checkReleaseImage = func(run, cache string) error {
		releaseCalls++
		assert.Equal(t, runDir, run)
		assert.Equal(t, cacheRoot, cache)
		return assert.AnError
	}

	err := run([]string{"workspace-mounter", "health", "prepared", "--release-check-image"}, strings.NewReader("must-not-be-read"), &bytes.Buffer{})
	require.ErrorIs(t, err, assert.AnError)
	assert.Zero(t, packageCalls)
	assert.Equal(t, 1, releaseCalls)
}

func TestControlClientTimeoutAllowsLongDurabilityOperations(t *testing.T) {
	for _, command := range []string{"flush", "shutdown", "shutdown-best-effort"} {
		assert.Greater(t, controlClientTimeout(command), 2*time.Hour, command)
	}
	assert.Equal(t, 10*time.Second, controlClientTimeout("health-ready"))
	assert.Equal(t, 10*time.Second, controlClientTimeout("authorize"))
}

type cliTestRunner struct{}

func (cliTestRunner) Start(context.Context, []string, []string) (mounter.Process, error) {
	return nil, assert.AnError
}
func (cliTestRunner) Run(context.Context, []string) error { return nil }

func TestCLIUsesSharedSocketContractEndToEnd(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "wm-cli-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	for _, directory := range []string{"secrets", "run", "cache/tmp", "workspace"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, directory), 0o700))
	}
	require.NoError(t, os.Chmod(filepath.Join(root, "run"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "secrets", "accessKey"), []byte("AK\n"), 0o400))
	require.NoError(t, os.WriteFile(filepath.Join(root, "secrets", "secretKey"), []byte("SK\n"), 0o400))
	bootstrap := fuseprotocol.BootstrapConfig{
		Version: fuseprotocol.Version, RuntimeUID: "uid-a", Provider: "minio", Bucket: "bucket-a", Endpoint: "https://minio.example.com", Region: "us-east-1", Profile: "minio-sigv4-path-style-v1",
		AccessKeyFile: filepath.Join(root, "secrets", "accessKey"), SecretKeyFile: filepath.Join(root, "secrets", "secretKey"), PasswdFile: filepath.Join(root, "run", "passwd-s3fs"), CacheDir: filepath.Join(root, "cache"), CacheLimitBytes: 1024, MountPath: filepath.Join(root, "workspace"), PoolKey: strings.Repeat("a", 64), MountTimeoutSeconds: 1, FlushTimeoutSeconds: 1, UnmountTimeoutSeconds: 1,
	}
	supervisor := mounter.NewSupervisor(mounter.Config{RunDir: filepath.Join(root, "run"), CredentialRoot: filepath.Join(root, "secrets"), CacheRoot: filepath.Join(root, "cache"), MountPath: bootstrap.MountPath, CheckFuse: func() error { return nil }, CheckAnchor: func(string) error { return nil }, MountInfo: func() (mounter.Mount, error) {
		return mounter.Mount{MountPoint: bootstrap.MountPath, FilesystemType: "tmpfs"}, nil
	}}, cliTestRunner{})
	require.NoError(t, supervisor.Bootstrap(context.Background(), bootstrap, "uid-a"))
	socket := filepath.Join(root, "run", "control.sock")
	server := mounter.Server{Supervisor: supervisor, SocketPath: socket, ExpectedRuntimeUID: "uid-a", VerifyPeer: func(*net.UnixConn) error { return nil }, IOTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	require.Eventually(t, func() bool { _, err := os.Stat(socket); return err == nil }, time.Second, time.Millisecond)

	var output bytes.Buffer
	require.NoError(t, runWithSocket([]string{"workspace-mounter", "health", "prepared"}, strings.NewReader(""), &output, socket))
	var status fuseprotocol.MounterStatus
	require.NoError(t, fuseprotocol.DecodeExact(output.Bytes(), &status))
	assert.Equal(t, "uid-a", status.RuntimeUID)
	assert.Equal(t, "prepared", status.State)

	cancel()
	require.NoError(t, <-done)
}

func TestCLIHealthRejectsRestartDetectedTerminalStatus(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "wm-cli-restart-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	runPath := filepath.Join(root, "run")
	require.NoError(t, os.MkdirAll(runPath, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(runPath, "mount-generation"), []byte(`{}`), 0o600))
	supervisor := mounter.NewSupervisor(mounter.Config{RunDir: runPath}, cliTestRunner{})
	require.Equal(t, mounter.StateRestartDetected, supervisor.State())

	socket := filepath.Join(runPath, "control.sock")
	server := mounter.Server{Supervisor: supervisor, SocketPath: socket, VerifyPeer: func(*net.UnixConn) error { return nil }, IOTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	require.Eventually(t, func() bool { _, err := os.Stat(socket); return err == nil }, time.Second, time.Millisecond)

	for _, health := range []string{"prepared", "ready"} {
		var output bytes.Buffer
		err := runWithSocket([]string{"workspace-mounter", "health", health}, strings.NewReader(""), &output, socket)
		require.Error(t, err)
		assert.Empty(t, output.Bytes())
	}

	cancel()
	require.NoError(t, <-done)
}

func TestValidateHealthStatusRequiresExactHealthyState(t *testing.T) {
	prepared := fuseprotocol.MounterStatus{Version: fuseprotocol.Version, State: "prepared", CacheLimitBytes: 1024}
	ready := fuseprotocol.MounterStatus{Version: fuseprotocol.Version, State: "ready", MountType: "fuse", Generation: 1, CacheLimitBytes: 1024}
	require.NoError(t, validateHealthStatus("health-prepared", prepared))
	require.NoError(t, validateHealthStatus("health-ready", ready))

	for _, test := range []struct {
		command string
		status  fuseprotocol.MounterStatus
	}{
		{"health-prepared", fuseprotocol.MounterStatus{Version: fuseprotocol.Version, State: "restart-detected", RestartDetected: true}},
		{"health-prepared", fuseprotocol.MounterStatus{Version: fuseprotocol.Version, State: "prepared", MountType: "fuse"}},
		{"health-ready", fuseprotocol.MounterStatus{Version: fuseprotocol.Version, State: "restart-detected", RestartDetected: true}},
		{"health-ready", fuseprotocol.MounterStatus{Version: fuseprotocol.Version, State: "ready", MountType: "", Generation: 1}},
		{"health-ready", fuseprotocol.MounterStatus{Version: fuseprotocol.Version, State: "ready", MountType: "fuse", Generation: 0}},
		{"health-prepared", fuseprotocol.MounterStatus{Version: fuseprotocol.Version, State: "prepared", CacheBytes: 1, CacheLimitBytes: 1024}},
		{"health-ready", fuseprotocol.MounterStatus{Version: fuseprotocol.Version, State: "ready", MountType: "fuse", Generation: 1, CacheBytes: 1024, CacheLimitBytes: 1024}},
	} {
		require.Error(t, validateHealthStatus(test.command, test.status))
	}
}

func TestValidateHealthOutputRejectsNonCanonicalStatus(t *testing.T) {
	for _, raw := range []string{
		`{"version":1,"state":"prepared","runtime_uid":"uid-a","pool_key":"key","mount_type":"","generation":0,"restart_detected":false,"unknown":true}`,
		`{"version":1,"state":"prepared","runtime_uid":"uid-a","pool_key":"key","mount_type":"","generation":0,"restart_detected":null}`,
	} {
		require.Error(t, validateHealthOutput("health-prepared", []byte(raw)))
	}
}

func TestReadBoundedRejectsOversizedInput(t *testing.T) {
	_, err := readBounded(strings.NewReader(strings.Repeat("x", fuseprotocol.MaxJSONBytes+1)))
	require.Error(t, err)
}
