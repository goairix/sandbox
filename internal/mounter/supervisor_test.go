package mounter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/fuseprotocol"
)

type fakeProcess struct {
	exit      chan error
	signal    func(os.Signal) error
	signalsMu sync.Mutex
	signals   []os.Signal
}

func (p *fakeProcess) Wait() error { return <-p.exit }
func (p *fakeProcess) Signal(signal os.Signal) error {
	p.signalsMu.Lock()
	p.signals = append(p.signals, signal)
	p.signalsMu.Unlock()
	if p.signal != nil {
		return p.signal(signal)
	}
	return nil
}

type fakeRunner struct {
	mu         sync.Mutex
	starts     int
	argv       [][]string
	env        [][]string
	startErr   error
	process    *fakeProcess
	runs       [][]string
	runErr     error
	runFunc    func([]string) error
	runHook    func([]string)
	runDelay   time.Duration
	runStarted chan<- struct{}
	runRelease <-chan struct{}
}

func (r *fakeRunner) Start(_ context.Context, argv, environment []string) (Process, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts++
	r.argv = append(r.argv, append([]string(nil), argv...))
	r.env = append(r.env, append([]string(nil), environment...))
	if r.startErr != nil {
		return nil, r.startErr
	}
	if r.process == nil {
		r.process = &fakeProcess{exit: make(chan error, 1)}
	}
	return r.process, nil
}

func (r *fakeRunner) Run(ctx context.Context, argv []string) error {
	r.mu.Lock()
	r.runs = append(r.runs, append([]string(nil), argv...))
	hook, delay, runErr, runFunc := r.runHook, r.runDelay, r.runErr, r.runFunc
	started, release := r.runStarted, r.runRelease
	r.mu.Unlock()
	if hook != nil {
		hook(argv)
	}
	if started != nil {
		started <- struct{}{}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if runFunc != nil {
		return runFunc(argv)
	}
	return runErr
}

func validBootstrap(root string) fuseprotocol.BootstrapConfig {
	return fuseprotocol.BootstrapConfig{
		Version: fuseprotocol.Version, RuntimeUID: "uid-a", Provider: "minio", Bucket: "bucket-a", Endpoint: "https://minio.example.com", Region: "us-east-1",
		Profile: "minio-sigv4-path-style-v1", AccessKeyFile: filepath.Join(root, "secrets", "accessKey"), SecretKeyFile: filepath.Join(root, "secrets", "secretKey"),
		PasswdFile: filepath.Join(root, "run", "passwd-s3fs"), CacheDir: filepath.Join(root, "cache"), MountPath: filepath.Join(root, "workspace"),
		PoolKey: strings.Repeat("a", 64), CacheLimitBytes: 1024,
		MountTimeoutSeconds: 2, FlushTimeoutSeconds: 2, UnmountTimeoutSeconds: 2,
	}
}

func newTestSupervisor(t *testing.T, runner *fakeRunner) (*Supervisor, fuseprotocol.BootstrapConfig) {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"secrets", "run", "cache/tmp", "workspace"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, dir), 0o700))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "secrets", "accessKey"), []byte("AK\n"), 0o400))
	require.NoError(t, os.WriteFile(filepath.Join(root, "secrets", "secretKey"), []byte("SK\n"), 0o400))
	bootstrap := validBootstrap(root)
	s := NewSupervisor(Config{
		RunDir: filepath.Join(root, "run"), CredentialRoot: filepath.Join(root, "secrets"), CacheRoot: filepath.Join(root, "cache"), MountPath: filepath.Join(root, "workspace"),
		CheckFuse: func() error { return nil }, CheckAnchor: func(string) error { return nil },
		MountInfo: func() (Mount, error) { return Mount{MountPoint: bootstrap.MountPath, FilesystemType: "tmpfs"}, nil },
	}, runner)
	require.NoError(t, s.Bootstrap(context.Background(), bootstrap, "uid-a"))
	return s, bootstrap
}

func validAuthorization() fuseprotocol.AuthorizeRequest {
	return fuseprotocol.AuthorizeRequest{Version: fuseprotocol.Version, RuntimeUID: "uid-a", PoolKey: strings.Repeat("a", 64), WorkspaceHash: strings.Repeat("b", 64), Prefix: "workspaces/a/", LeaseGeneration: 1, MountAttempt: 1}
}

func TestSupervisorConsumesAuthorizationOnce(t *testing.T) {
	runner := &fakeRunner{}
	s, bootstrap := newTestSupervisor(t, runner)
	auth := validAuthorization()
	require.NoError(t, s.Authorize(context.Background(), auth))
	require.ErrorIs(t, s.Authorize(context.Background(), auth), ErrAuthorizationConsumed)
	assert.Equal(t, 1, runner.starts)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(bootstrap.PasswdFile), mountGenerationFile))
	require.NoError(t, err)
	assert.Contains(t, string(raw), auth.WorkspaceHash)
	info, err := os.Stat(filepath.Join(filepath.Dir(bootstrap.PasswdFile), mountGenerationFile))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestSupervisorConcurrentAuthorizationHasOneWinner(t *testing.T) {
	runner := &fakeRunner{}
	s, _ := newTestSupervisor(t, runner)
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for range 100 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.Authorize(context.Background(), validAuthorization()) }()
	}
	wg.Wait()
	close(errs)
	winners := 0
	for err := range errs {
		if err == nil {
			winners++
		} else {
			assert.ErrorIs(t, err, ErrAuthorizationConsumed)
		}
	}
	assert.Equal(t, 1, winners)
	assert.Equal(t, 1, runner.starts)
}

func TestSupervisorNeverRestartsExitedS3FS(t *testing.T) {
	runner := &fakeRunner{process: &fakeProcess{exit: make(chan error, 1)}}
	s, _ := newTestSupervisor(t, runner)
	require.NoError(t, s.Authorize(context.Background(), validAuthorization()))
	runner.process.exit <- errors.New("s3fs exited")
	require.Eventually(t, func() bool { return s.State() == StateUnhealthy }, time.Second, time.Millisecond)
	assert.Equal(t, 1, runner.starts)
}

func TestSupervisorStaysLockedWhenGenerationMarkerExists(t *testing.T) {
	runDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(runDir, mountGenerationFile), []byte("{}"), 0o600))
	s := NewSupervisor(Config{RunDir: runDir}, &fakeRunner{})
	assert.Equal(t, StateRestartDetected, s.State())
	require.ErrorIs(t, s.Authorize(context.Background(), validAuthorization()), ErrAuthorizationConsumed)
}

func TestBootstrapWritesMode0600CredentialAndRejectsReplay(t *testing.T) {
	runner := &fakeRunner{}
	s, bootstrap := newTestSupervisor(t, runner)
	raw, err := os.ReadFile(bootstrap.PasswdFile)
	require.NoError(t, err)
	assert.Equal(t, "AK:SK\n", string(raw))
	info, err := os.Stat(bootstrap.PasswdFile)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	require.ErrorIs(t, s.Bootstrap(context.Background(), bootstrap, "uid-a"), ErrBootstrapConsumed)
}

func TestBootstrapRejectsMultilineCredentialsWithoutOutput(t *testing.T) {
	runner := &fakeRunner{}
	s, bootstrap := newTestSupervisor(t, runner)
	_ = s
	// A new supervisor sees the persisted bootstrap as consumed, so use a new root.
	root := t.TempDir()
	for _, dir := range []string{"secrets", "run", "cache/tmp", "workspace"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, dir), 0o700))
	}
	bootstrap = validBootstrap(root)
	require.NoError(t, os.WriteFile(bootstrap.AccessKeyFile, []byte("AK\nINJECT\n"), 0o400))
	require.NoError(t, os.WriteFile(bootstrap.SecretKeyFile, []byte("SK\n"), 0o400))
	bad := NewSupervisor(Config{RunDir: filepath.Join(root, "run"), CredentialRoot: filepath.Join(root, "secrets"), CacheRoot: filepath.Join(root, "cache"), MountPath: filepath.Join(root, "workspace"), CheckFuse: func() error { return nil }, CheckAnchor: func(string) error { return nil }}, runner)
	require.Error(t, bad.Bootstrap(context.Background(), bootstrap, "uid-a"))
	_, err := os.Stat(bootstrap.PasswdFile)
	assert.True(t, os.IsNotExist(err))
}

func TestAuthorizationMarkerPermanentlyConsumesStartFailure(t *testing.T) {
	runner := &fakeRunner{startErr: errors.New("start failed")}
	s, _ := newTestSupervisor(t, runner)
	require.Error(t, s.Authorize(context.Background(), validAuthorization()))
	require.ErrorIs(t, s.Authorize(context.Background(), validAuthorization()), ErrAuthorizationConsumed)
	assert.Equal(t, StateUnhealthy, s.State())
}

func TestAuthorizeBuildsArgvWithoutShellInterpolation(t *testing.T) {
	runner := &fakeRunner{}
	s, _ := newTestSupervisor(t, runner)
	require.NoError(t, s.Authorize(context.Background(), validAuthorization()))
	require.Len(t, runner.argv, 1)
	assert.Equal(t, "/usr/bin/s3fs", runner.argv[0][0])
	assert.Equal(t, "bucket-a:/workspaces/a", runner.argv[0][1])
	assert.Contains(t, runner.argv[0], "url=https://minio.example.com")
	assert.Contains(t, runner.argv[0], "endpoint=us-east-1")
	assert.Contains(t, runner.argv[0], "use_path_request_style")
}

func TestBootstrapCanonicalizesRuntimeUIDAndAllowsCacheRoot(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"secrets", "run", "cache/tmp", "workspace"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, dir), 0o700))
	}
	bootstrap := validBootstrap(root)
	bootstrap.RuntimeUID = ""
	require.NoError(t, os.WriteFile(bootstrap.AccessKeyFile, []byte("AK\n"), 0o400))
	require.NoError(t, os.WriteFile(bootstrap.SecretKeyFile, []byte("SK\n"), 0o400))
	s := NewSupervisor(Config{RunDir: filepath.Join(root, "run"), CredentialRoot: filepath.Join(root, "secrets"), CacheRoot: bootstrap.CacheDir, MountPath: bootstrap.MountPath, CheckFuse: func() error { return nil }, CheckAnchor: func(string) error { return nil }, MountInfo: func() (Mount, error) { return Mount{MountPoint: bootstrap.MountPath, FilesystemType: "tmpfs"}, nil }}, &fakeRunner{})
	require.NoError(t, s.Bootstrap(context.Background(), bootstrap, "uid-a"))
	status, err := s.PreparedStatus(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "uid-a", status.RuntimeUID)
	raw, err := os.ReadFile(filepath.Join(root, "run", "bootstrap.json"))
	require.NoError(t, err)
	var persisted fuseprotocol.BootstrapConfig
	require.NoError(t, fuseprotocol.Decode(raw, &persisted))
	assert.Equal(t, "uid-a", persisted.RuntimeUID)
}

func TestBootstrapSecuresRootOwnedEmptyDirCache(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"secrets", "run", "cache", "workspace"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, dir), 0o700))
	}
	cache := filepath.Join(root, "cache")
	require.NoError(t, os.Chmod(cache, 0o777), "model Kubernetes emptyDir mount root")
	bootstrap := validBootstrap(root)
	require.NoError(t, os.WriteFile(bootstrap.AccessKeyFile, []byte("AK\n"), 0o400))
	require.NoError(t, os.WriteFile(bootstrap.SecretKeyFile, []byte("SK\n"), 0o400))
	s := NewSupervisor(Config{
		RunDir: filepath.Join(root, "run"), CredentialRoot: filepath.Join(root, "secrets"), CacheRoot: cache, MountPath: bootstrap.MountPath,
		CheckFuse: func() error { return nil }, CheckAnchor: func(string) error { return nil },
		MountInfo: func() (Mount, error) { return Mount{MountPoint: bootstrap.MountPath, FilesystemType: "tmpfs"}, nil },
	}, &fakeRunner{})

	require.NoError(t, s.Bootstrap(context.Background(), bootstrap, "uid-a"))
	cacheInfo, err := os.Stat(cache)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), cacheInfo.Mode().Perm())
	tmpInfo, err := os.Stat(filepath.Join(cache, "tmp"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), tmpInfo.Mode().Perm())
}

func TestAuthorizeChildLifetimeDoesNotUseRequestCancellation(t *testing.T) {
	runner := &contextRecordingRunner{fakeRunner: fakeRunner{}}
	s, _ := newTestSupervisorWithRunner(t, runner)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, s.Authorize(ctx, validAuthorization()))
	cancel()
	assert.NoError(t, runner.startContext.Err())
}

func TestBootstrapAcceptsProjectedSecretSymlinkOnlyInsideTrustedRoot(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"secrets/..data", "run", "cache/tmp", "workspace"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, dir), 0o700))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "secrets", "..data", "access"), []byte("AK\n"), 0o400))
	require.NoError(t, os.WriteFile(filepath.Join(root, "secrets", "..data", "secret"), []byte("SK\n"), 0o400))
	require.NoError(t, os.Symlink(filepath.Join("..data", "access"), filepath.Join(root, "secrets", "accessKey")))
	require.NoError(t, os.Symlink(filepath.Join("..data", "secret"), filepath.Join(root, "secrets", "secretKey")))
	bootstrap := validBootstrap(root)
	s := NewSupervisor(Config{RunDir: filepath.Join(root, "run"), CredentialRoot: filepath.Join(root, "secrets"), CacheRoot: filepath.Join(root, "cache"), MountPath: bootstrap.MountPath, CheckFuse: func() error { return nil }, CheckAnchor: func(string) error { return nil }, MountInfo: func() (Mount, error) { return Mount{MountPoint: bootstrap.MountPath, FilesystemType: "tmpfs"}, nil }}, &fakeRunner{})
	require.NoError(t, s.Bootstrap(context.Background(), bootstrap, "uid-a"))
}

func TestBootstrapRejectsCredentialSymlinkEscapingTrustedRoot(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"secrets", "run", "cache/tmp", "workspace", "foreign"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, dir), 0o700))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "foreign", "access"), []byte("AK\n"), 0o400))
	require.NoError(t, os.WriteFile(filepath.Join(root, "secrets", "secretKey"), []byte("SK\n"), 0o400))
	require.NoError(t, os.Symlink(filepath.Join(root, "foreign", "access"), filepath.Join(root, "secrets", "accessKey")))
	bootstrap := validBootstrap(root)
	s := NewSupervisor(Config{RunDir: filepath.Join(root, "run"), CredentialRoot: filepath.Join(root, "secrets"), CacheRoot: filepath.Join(root, "cache"), MountPath: bootstrap.MountPath, CheckFuse: func() error { return nil }, CheckAnchor: func(string) error { return nil }}, &fakeRunner{})
	require.Error(t, s.Bootstrap(context.Background(), bootstrap, "uid-a"))
	_, err := os.Stat(bootstrap.PasswdFile)
	assert.True(t, os.IsNotExist(err))
}

func TestBootstrapRejectsWorldReadableCredentialButAllowsReadOnlyCA(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"secrets", "run", "cache/tmp", "workspace"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, dir), 0o700))
	}
	bootstrap := validBootstrap(root)
	require.NoError(t, os.WriteFile(bootstrap.AccessKeyFile, []byte("AK\n"), 0o444))
	require.NoError(t, os.WriteFile(bootstrap.SecretKeyFile, []byte("SK\n"), 0o400))
	bootstrap.CAFile = filepath.Join(root, "secrets", "ca.crt")
	require.NoError(t, os.WriteFile(bootstrap.CAFile, []byte("CA"), 0o444))
	config := Config{RunDir: filepath.Join(root, "run"), CredentialRoot: filepath.Join(root, "secrets"), CacheRoot: filepath.Join(root, "cache"), MountPath: bootstrap.MountPath, CheckFuse: func() error { return nil }, CheckAnchor: func(string) error { return nil }, MountInfo: func() (Mount, error) { return Mount{MountPoint: bootstrap.MountPath, FilesystemType: "tmpfs"}, nil }}
	s := NewSupervisor(config, &fakeRunner{})
	require.Error(t, s.Bootstrap(context.Background(), bootstrap, "uid-a"))

	require.NoError(t, os.Chmod(bootstrap.AccessKeyFile, 0o400))
	s = NewSupervisor(config, &fakeRunner{})
	require.NoError(t, s.Bootstrap(context.Background(), bootstrap, "uid-a"))
}

func TestAtomicPublishIgnoresStaleTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".tmp-publish"), []byte("stale"), 0o600))
	require.NoError(t, atomicPublish(filepath.Join(dir, "published"), []byte("ok"), 0o600))
	raw, err := os.ReadFile(filepath.Join(dir, "published"))
	require.NoError(t, err)
	assert.Equal(t, []byte("ok"), raw)
}

func TestStrongShutdownWaitsForChildExitAndEffectiveUnmount(t *testing.T) {
	process := &fakeProcess{exit: make(chan error, 1)}
	runner := &fakeRunner{process: process}
	s, _ := newVerifiedFlushSupervisor(t, runner)
	runner.runHook = func(argv []string) {
		if len(argv) > 0 && argv[0] == "/usr/bin/fusermount3" {
			process.exit <- nil
			s.config.MountInfo = func() (Mount, error) {
				return Mount{ID: 10, MountPoint: s.bootstrap.MountPath, FilesystemType: "tmpfs"}, nil
			}
		}
	}
	request := fuseprotocol.ControlRequest{Version: fuseprotocol.Version, RuntimeUID: "uid-a", Generation: 1}
	ack, err := s.Shutdown(context.Background(), &request)
	require.NoError(t, err)
	assert.True(t, ack.GracefulUnmount)
	assert.Equal(t, StateStopped, s.State())
	second, err := s.Shutdown(context.Background(), &request)
	require.NoError(t, err)
	assert.True(t, second.GracefulUnmount)
	assert.Len(t, runner.runs, 3)
}

func TestStrongShutdownLetsS3FSPerformGracefulUnmountAfterFlush(t *testing.T) {
	process := &fakeProcess{exit: make(chan error, 1)}
	runner := &fakeRunner{process: process}
	s, _ := newVerifiedFlushSupervisor(t, runner)
	process.signal = func(signal os.Signal) error {
		require.Equal(t, os.Signal(syscall.SIGTERM), signal)
		runner.mu.Lock()
		require.NotEmpty(t, runner.runs)
		assert.Equal(t, "/usr/bin/verified-flush", runner.runs[len(runner.runs)-1][0])
		runner.mu.Unlock()
		s.config.MountInfo = func() (Mount, error) {
			return Mount{ID: 10, MountPoint: s.bootstrap.MountPath, FilesystemType: "tmpfs"}, nil
		}
		process.exit <- nil
		return nil
	}
	request := fuseprotocol.ControlRequest{Version: fuseprotocol.Version, RuntimeUID: "uid-a", Generation: 1}
	ack, err := s.Shutdown(context.Background(), &request)
	require.NoError(t, err)
	assert.True(t, ack.GracefulUnmount)
	assert.Equal(t, StateStopped, s.State())
	runner.mu.Lock()
	defer runner.mu.Unlock()
	for _, argv := range runner.runs {
		assert.NotEqual(t, "/usr/bin/fusermount3", argv[0])
	}
}

func TestStrongShutdownRetriesAfterBoundedUnmountFailureWithoutRepeatingFlush(t *testing.T) {
	process := &fakeProcess{exit: make(chan error, 1)}
	runner := &fakeRunner{process: process}
	s, _ := newVerifiedFlushSupervisor(t, runner)
	unmountMaySucceed := false
	unmountAttempts := 0
	runner.runFunc = func(argv []string) error {
		if len(argv) == 0 || argv[0] != "/usr/bin/fusermount3" {
			return nil
		}
		unmountAttempts++
		if !unmountMaySucceed {
			return errors.New("temporarily busy")
		}
		process.exit <- nil
		s.config.MountInfo = func() (Mount, error) {
			return Mount{ID: 10, MountPoint: s.bootstrap.MountPath, FilesystemType: "tmpfs"}, nil
		}
		return nil
	}
	request := fuseprotocol.ControlRequest{Version: fuseprotocol.Version, RuntimeUID: "uid-a", Generation: 1}
	firstCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	first, err := s.Shutdown(firstCtx, &request)
	require.Error(t, err)
	assert.False(t, first.GracefulUnmount)

	unmountMaySucceed = true
	second, err := s.Shutdown(context.Background(), &request)
	require.NoError(t, err)
	assert.True(t, second.GracefulUnmount)
	assert.Equal(t, StateStopped, s.State())
	assert.GreaterOrEqual(t, unmountAttempts, 2)
	runner.mu.Lock()
	defer runner.mu.Unlock()
	flushes := 0
	for _, argv := range runner.runs {
		if len(argv) > 0 && argv[0] == "/usr/bin/verified-flush" {
			flushes++
		}
	}
	assert.Equal(t, 1, flushes)
}

func TestStrongShutdownRetryWaitsForAlreadyUnmountedChild(t *testing.T) {
	process := &fakeProcess{exit: make(chan error, 1)}
	runner := &fakeRunner{process: process}
	s, _ := newVerifiedFlushSupervisor(t, runner)
	unmountAttempts := 0
	runner.runFunc = func(argv []string) error {
		if len(argv) > 0 && argv[0] == "/usr/bin/fusermount3" {
			unmountAttempts++
			return errors.New("temporarily busy")
		}
		return nil
	}
	request := fuseprotocol.ControlRequest{Version: fuseprotocol.Version, RuntimeUID: "uid-a", Generation: 1}
	firstCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := s.Shutdown(firstCtx, &request)
	require.Error(t, err)
	firstAttempts := unmountAttempts

	s.config.MountInfo = func() (Mount, error) {
		return Mount{ID: 10, MountPoint: s.bootstrap.MountPath, FilesystemType: "tmpfs"}, nil
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		process.exit <- nil
	}()
	ack, err := s.Shutdown(context.Background(), &request)
	require.NoError(t, err)
	assert.True(t, ack.GracefulUnmount)
	assert.Equal(t, firstAttempts, unmountAttempts, "an already absent mount must not be unmounted again")
}

func TestStrongShutdownDoesNotRetryWhenDurableFlushFailed(t *testing.T) {
	runner := &fakeRunner{}
	s, _ := newVerifiedFlushSupervisor(t, runner)
	runner.runErr = errors.New("flush failed")
	request := fuseprotocol.ControlRequest{Version: fuseprotocol.Version, RuntimeUID: "uid-a", Generation: 1}
	_, err := s.Shutdown(context.Background(), &request)
	require.Error(t, err)
	runner.runErr = nil
	_, err = s.Shutdown(context.Background(), &request)
	require.Error(t, err)
	runner.mu.Lock()
	defer runner.mu.Unlock()
	for _, argv := range runner.runs {
		assert.NotEqual(t, "/usr/bin/fusermount3", argv[0])
	}
}

func TestStrongShutdownRetriesOrdinaryUnmountWithinDeadline(t *testing.T) {
	process := &fakeProcess{exit: make(chan error, 1)}
	runner := &fakeRunner{process: process}
	s, _ := newVerifiedFlushSupervisor(t, runner)
	unmountAttempts := 0
	runner.runFunc = func(argv []string) error {
		if len(argv) == 0 || argv[0] != "/usr/bin/fusermount3" {
			return nil
		}
		unmountAttempts++
		if unmountAttempts == 1 {
			return errors.New("temporarily busy")
		}
		process.exit <- nil
		s.config.MountInfo = func() (Mount, error) {
			return Mount{ID: 10, MountPoint: s.bootstrap.MountPath, FilesystemType: "tmpfs"}, nil
		}
		return nil
	}
	request := fuseprotocol.ControlRequest{Version: fuseprotocol.Version, RuntimeUID: "uid-a", Generation: 1}
	ack, err := s.Shutdown(context.Background(), &request)
	require.NoError(t, err)
	assert.True(t, ack.GracefulUnmount)
	assert.Equal(t, 2, unmountAttempts)
}

func TestStrongShutdownFailsClosedWhenChildDoesNotExit(t *testing.T) {
	runner := &fakeRunner{process: &fakeProcess{exit: make(chan error)}}
	s, _ := newVerifiedFlushSupervisor(t, runner)
	s.bootstrap.UnmountTimeoutSeconds = 0
	request := fuseprotocol.ControlRequest{Version: fuseprotocol.Version, RuntimeUID: "uid-a", Generation: 1}
	ack, err := s.Shutdown(context.Background(), &request)
	require.Error(t, err)
	assert.False(t, ack.GracefulUnmount)
	assert.Equal(t, StateUnhealthy, s.State())
}

func TestStrongShutdownRejectsUnverifiableUnmount(t *testing.T) {
	process := &fakeProcess{exit: make(chan error, 1)}
	runner := &fakeRunner{process: process}
	s, _ := newVerifiedFlushSupervisor(t, runner)
	runner.runHook = func(argv []string) {
		if len(argv) > 0 && argv[0] == "/usr/bin/fusermount3" {
			process.exit <- nil
		}
	}
	s.config.MountInfo = func() (Mount, error) { return Mount{}, errors.New("mountinfo unavailable") }
	request := fuseprotocol.ControlRequest{Version: fuseprotocol.Version, RuntimeUID: "uid-a", Generation: 1}
	ack, err := s.Shutdown(context.Background(), &request)
	require.Error(t, err)
	assert.False(t, ack.GracefulUnmount)
	assert.Equal(t, StateUnhealthy, s.State())
}

func TestBootstrapRejectsUnboundedTimeoutAndNonFixedPasswdPath(t *testing.T) {
	for _, mutate := range []func(*fuseprotocol.BootstrapConfig){
		func(value *fuseprotocol.BootstrapConfig) { value.MountTimeoutSeconds = 3601 },
		func(value *fuseprotocol.BootstrapConfig) { value.Region = "" },
		func(value *fuseprotocol.BootstrapConfig) {
			value.PasswdFile = filepath.Join(filepath.Dir(value.PasswdFile), "other")
		},
	} {
		root := t.TempDir()
		for _, dir := range []string{"secrets", "run", "cache/tmp", "workspace"} {
			require.NoError(t, os.MkdirAll(filepath.Join(root, dir), 0o700))
		}
		bootstrap := validBootstrap(root)
		require.NoError(t, os.WriteFile(bootstrap.AccessKeyFile, []byte("AK\n"), 0o400))
		require.NoError(t, os.WriteFile(bootstrap.SecretKeyFile, []byte("SK\n"), 0o400))
		mutate(&bootstrap)
		s := NewSupervisor(Config{RunDir: filepath.Join(root, "run"), CredentialRoot: filepath.Join(root, "secrets"), CacheRoot: filepath.Join(root, "cache"), MountPath: filepath.Join(root, "workspace"), CheckFuse: func() error { return nil }, CheckAnchor: func(string) error { return nil }, MountInfo: func() (Mount, error) { return Mount{}, nil }}, &fakeRunner{})
		require.Error(t, s.Bootstrap(context.Background(), bootstrap, "uid-a"))
	}
}

func TestBootstrapPreparesEmptyDirRunAndCacheLayout(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"secrets", "run", "cache", "workspace"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, dir), 0o755))
	}
	bootstrap := validBootstrap(root)
	require.NoError(t, os.WriteFile(bootstrap.AccessKeyFile, []byte("AK\n"), 0o400))
	require.NoError(t, os.WriteFile(bootstrap.SecretKeyFile, []byte("SK\n"), 0o400))
	s := NewSupervisor(Config{RunDir: filepath.Join(root, "run"), CredentialRoot: filepath.Join(root, "secrets"), CacheRoot: filepath.Join(root, "cache"), MountPath: bootstrap.MountPath, CheckFuse: func() error { return nil }, CheckAnchor: func(string) error { return nil }, MountInfo: func() (Mount, error) { return Mount{MountPoint: bootstrap.MountPath, FilesystemType: "tmpfs"}, nil }}, &fakeRunner{})
	require.NoError(t, s.Bootstrap(context.Background(), bootstrap, "uid-a"))
	for _, path := range []string{filepath.Join(root, "run"), filepath.Join(root, "cache", "tmp")} {
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	}
}

func TestAuthorizePassesOnlyValidatedCAEnvironmentToS3FS(t *testing.T) {
	runner := &fakeRunner{}
	s, bootstrap := newTestSupervisor(t, runner)
	caPath := filepath.Join(filepath.Dir(bootstrap.AccessKeyFile), "ca.crt")
	require.NoError(t, os.WriteFile(caPath, []byte("test-ca"), 0o400))
	s.bootstrap.CAFile = caPath
	require.NoError(t, s.Authorize(context.Background(), validAuthorization()))
	require.Equal(t, [][]string{{"CURL_CA_BUNDLE=" + caPath}}, runner.env)
}

func TestPreparedStatusRevalidatesFuseAnchorAndCredentialState(t *testing.T) {
	runner := &fakeRunner{}
	s, bootstrap := newTestSupervisor(t, runner)
	s.config.CheckFuse = func() error { return errors.New("fuse vanished") }
	_, err := s.PreparedStatus(context.Background())
	require.Error(t, err)

	s.config.CheckFuse = func() error { return nil }
	s.config.CheckAnchor = func(string) error { return errors.New("anchor changed") }
	_, err = s.PreparedStatus(context.Background())
	require.Error(t, err)

	s.config.CheckAnchor = func(string) error { return nil }
	require.NoError(t, os.Chmod(bootstrap.PasswdFile, 0o644))
	_, err = s.PreparedStatus(context.Background())
	require.Error(t, err)
}

func TestPreparedStatusRejectsNonEmptyCache(t *testing.T) {
	runner := &fakeRunner{}
	s, bootstrap := newTestSupervisor(t, runner)
	require.NoError(t, os.WriteFile(filepath.Join(bootstrap.CacheDir, "stale-cache"), []byte("stale"), 0o600))

	status, err := s.PreparedStatus(context.Background())
	require.ErrorContains(t, err, "cache")
	assert.Equal(t, int64(5), status.CacheBytes)
	assert.Equal(t, bootstrap.CacheLimitBytes, status.CacheLimitBytes)
}

func TestReadyStatusPoisonsRuntimeAtSoftCacheLimit(t *testing.T) {
	runner := &fakeRunner{}
	s, bootstrap := newTestSupervisor(t, runner)
	require.NoError(t, s.Authorize(context.Background(), validAuthorization()))
	s.config.MountInfo = func() (Mount, error) {
		return Mount{ID: 42, MountPoint: bootstrap.MountPath, FilesystemType: "fuse.s3fs"}, nil
	}
	require.NoError(t, os.WriteFile(filepath.Join(bootstrap.CacheDir, "cache-entry"), make([]byte, bootstrap.CacheLimitBytes), 0o600))

	status, err := s.ReadyStatus(context.Background())
	require.ErrorContains(t, err, "cache soft limit")
	assert.True(t, status.CacheExceeded)
	assert.Equal(t, StateUnhealthy, s.State())
}

func TestCacheUsageHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := cacheUsageBytes(ctx, t.TempDir())
	require.ErrorIs(t, err, context.Canceled)
}

type contextRecordingRunner struct {
	fakeRunner
	startContext context.Context
}

func (r *contextRecordingRunner) Start(ctx context.Context, argv, environment []string) (Process, error) {
	r.startContext = ctx
	return r.fakeRunner.Start(ctx, argv, environment)
}

func newTestSupervisorWithRunner(t *testing.T, runner Runner) (*Supervisor, fuseprotocol.BootstrapConfig) {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"secrets", "run", "cache/tmp", "workspace"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, dir), 0o700))
	}
	bootstrap := validBootstrap(root)
	require.NoError(t, os.WriteFile(bootstrap.AccessKeyFile, []byte("AK\n"), 0o400))
	require.NoError(t, os.WriteFile(bootstrap.SecretKeyFile, []byte("SK\n"), 0o400))
	s := NewSupervisor(Config{RunDir: filepath.Join(root, "run"), CredentialRoot: filepath.Join(root, "secrets"), CacheRoot: filepath.Join(root, "cache"), MountPath: filepath.Join(root, "workspace"), CheckFuse: func() error { return nil }, CheckAnchor: func(string) error { return nil }, MountInfo: func() (Mount, error) { return Mount{MountPoint: bootstrap.MountPath, FilesystemType: "tmpfs"}, nil }}, runner)
	require.NoError(t, s.Bootstrap(context.Background(), bootstrap, "uid-a"))
	return s, bootstrap
}

func TestReadyPollsStartupMountAndRecordsExactMountID(t *testing.T) {
	runner := &fakeRunner{}
	s, bootstrap := newTestSupervisor(t, runner)
	require.NoError(t, s.Authorize(context.Background(), validAuthorization()))
	calls := 0
	s.config.MountPollInterval = time.Millisecond
	s.config.MountInfo = func() (Mount, error) {
		calls++
		if calls < 3 {
			return Mount{ID: 10, MountPoint: bootstrap.MountPath, FilesystemType: "tmpfs"}, nil
		}
		return Mount{ID: 42, MountPoint: bootstrap.MountPath, FilesystemType: "fuse.s3fs"}, nil
	}
	status, err := s.ReadyStatus(context.Background())
	require.NoError(t, err)
	assert.Equal(t, StateReady, s.State())
	assert.Equal(t, int64(1), status.Generation)
	require.NotEmpty(t, runner.runs)
	assert.Equal(t, []string{"/bin/ls", "-U", "--", bootstrap.MountPath}, runner.runs[len(runner.runs)-1])
}

func TestReadyCallerCancellationDoesNotPoisonOrExtendMountDeadline(t *testing.T) {
	runner := &fakeRunner{}
	s, bootstrap := newTestSupervisor(t, runner)
	require.NoError(t, s.Authorize(context.Background(), validAuthorization()))
	s.config.MountPollInterval = time.Millisecond
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.ReadyStatus(canceled)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, StateMounting, s.State())

	s.config.MountInfo = func() (Mount, error) {
		return Mount{ID: 42, MountPoint: bootstrap.MountPath, FilesystemType: "fuse.s3fs"}, nil
	}
	_, err = s.ReadyStatus(context.Background())
	require.NoError(t, err)
	assert.Equal(t, StateReady, s.State())
}

func TestRecoverRestartedIdentityNeverRestartsS3FS(t *testing.T) {
	runner := &fakeRunner{}
	s, bootstrap := newTestSupervisor(t, runner)
	auth := validAuthorization()
	require.NoError(t, s.Authorize(context.Background(), auth))

	restarted := NewSupervisor(s.config, runner)
	require.Equal(t, StateRestartDetected, restarted.State())
	require.NoError(t, restarted.RecoverRestarted(bootstrap, "uid-a"))
	status := restarted.Status()
	assert.Equal(t, "uid-a", status.RuntimeUID)
	assert.Equal(t, int64(1), status.Generation)
	assert.True(t, status.RestartDetected)
	require.ErrorIs(t, restarted.Authorize(context.Background(), auth), ErrAuthorizationConsumed)
	assert.Equal(t, 1, runner.starts)
}

func newVerifiedFlushSupervisor(t *testing.T, runner *fakeRunner) (*Supervisor, *uint64) {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"secrets", "run", "cache/tmp", "workspace"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, dir), 0o700))
	}
	bootstrap := validBootstrap(root)
	require.NoError(t, os.WriteFile(bootstrap.AccessKeyFile, []byte("AK\n"), 0o400))
	require.NoError(t, os.WriteFile(bootstrap.SecretKeyFile, []byte("SK\n"), 0o400))
	profile := Profile{ID: bootstrap.Profile, Provider: "minio", Options: func(fuseprotocol.BootstrapConfig) ([]string, error) { return nil, nil }, Flush: func(fuseprotocol.BootstrapConfig) []string { return []string{"/usr/bin/verified-flush"} }}
	mountID := uint64(42)
	filesystemType := "tmpfs"
	s := NewSupervisor(Config{RunDir: filepath.Join(root, "run"), CredentialRoot: filepath.Join(root, "secrets"), CacheRoot: filepath.Join(root, "cache"), MountPath: bootstrap.MountPath, Profiles: StaticProfiles{profile.ID: profile}, CheckFuse: func() error { return nil }, CheckAnchor: func(string) error { return nil }, MountInfo: func() (Mount, error) {
		return Mount{ID: mountID, MountPoint: bootstrap.MountPath, FilesystemType: filesystemType}, nil
	}, MountPollInterval: time.Millisecond}, runner)
	require.NoError(t, s.Bootstrap(context.Background(), bootstrap, "uid-a"))
	require.NoError(t, s.Authorize(context.Background(), validAuthorization()))
	filesystemType = "fuse.s3fs"
	_, err := s.ReadyStatus(context.Background())
	require.NoError(t, err)
	return s, &mountID
}

func TestFlushRejectsMountIdentityChange(t *testing.T) {
	runner := &fakeRunner{}
	s, mountID := newVerifiedFlushSupervisor(t, runner)
	runner.runHook = func(argv []string) {
		if len(argv) > 0 && argv[0] == "/usr/bin/verified-flush" {
			*mountID = 43
		}
	}
	request := fuseprotocol.ControlRequest{Version: fuseprotocol.Version, RuntimeUID: "uid-a", Generation: 1}
	ack, err := s.Flush(context.Background(), request)
	require.Error(t, err)
	assert.False(t, ack.Accepted)
	assert.Equal(t, StateUnhealthy, s.State())
}

func TestStrongShutdownRequiresVerifiedFlushStrategy(t *testing.T) {
	runner := &fakeRunner{}
	s, bootstrap := newTestSupervisor(t, runner)
	require.NoError(t, s.Authorize(context.Background(), validAuthorization()))
	s.profile.Flush = nil
	s.mountID = 42
	s.state = StateReady
	s.config.MountInfo = func() (Mount, error) {
		return Mount{ID: 42, MountPoint: bootstrap.MountPath, FilesystemType: "fuse.s3fs"}, nil
	}
	request := fuseprotocol.ControlRequest{Version: fuseprotocol.Version, RuntimeUID: "uid-a", Generation: 1}
	ack, err := s.Shutdown(context.Background(), &request)
	require.ErrorIs(t, err, ErrFlushUnsupported)
	assert.False(t, ack.GracefulUnmount)
	assert.Empty(t, runner.runs)
}

func TestStrongShutdownFlushFailureDoesNotAttemptUnmount(t *testing.T) {
	runner := &fakeRunner{}
	s, _ := newVerifiedFlushSupervisor(t, runner)
	runner.runErr = errors.New("flush failed")
	request := fuseprotocol.ControlRequest{Version: fuseprotocol.Version, RuntimeUID: "uid-a", Generation: 1}
	ack, err := s.Shutdown(context.Background(), &request)
	require.Error(t, err)
	assert.False(t, ack.GracefulUnmount)
	runner.mu.Lock()
	defer runner.mu.Unlock()
	require.Len(t, runner.runs, 2)
	assert.Equal(t, "/usr/bin/verified-flush", runner.runs[1][0])
}

func TestStrongShutdownFlushesBeforeUnmount(t *testing.T) {
	process := &fakeProcess{exit: make(chan error, 1)}
	runner := &fakeRunner{process: process}
	s, _ := newVerifiedFlushSupervisor(t, runner)
	runner.runHook = func(argv []string) {
		if len(argv) > 0 && argv[0] == "/usr/bin/fusermount3" {
			process.exit <- nil
			s.config.MountInfo = func() (Mount, error) {
				return Mount{ID: 10, MountPoint: s.bootstrap.MountPath, FilesystemType: "tmpfs"}, nil
			}
		}
	}
	request := fuseprotocol.ControlRequest{Version: fuseprotocol.Version, RuntimeUID: "uid-a", Generation: 1}
	ack, err := s.Shutdown(context.Background(), &request)
	require.NoError(t, err)
	assert.True(t, ack.GracefulUnmount)
	require.GreaterOrEqual(t, len(runner.runs), 3)
	assert.Equal(t, "/usr/bin/verified-flush", runner.runs[len(runner.runs)-2][0])
	assert.Equal(t, "/usr/bin/fusermount3", runner.runs[len(runner.runs)-1][0])
}

func TestCanceledShutdownQueuedBehindFlushHasNoSideEffect(t *testing.T) {
	runner := &fakeRunner{}
	s, _ := newVerifiedFlushSupervisor(t, runner)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	runner.runStarted = started
	runner.runRelease = release
	request := fuseprotocol.ControlRequest{Version: fuseprotocol.Version, RuntimeUID: "uid-a", Generation: 1}
	flushDone := make(chan error, 1)
	go func() {
		_, err := s.Flush(context.Background(), request)
		flushDone <- err
	}()
	<-started
	canceled, cancel := context.WithCancel(context.Background())
	shutdownDone := make(chan error, 1)
	go func() {
		_, err := s.Shutdown(canceled, &request)
		shutdownDone <- err
	}()
	cancel()
	close(release)
	shutdownErr := <-shutdownDone
	require.ErrorIs(t, shutdownErr, context.Canceled)
	require.NoError(t, <-flushDone)
	runner.mu.Lock()
	defer runner.mu.Unlock()
	assert.Len(t, runner.runs, 2)
}

func TestImageContractIncludesBoundedRemoteReadBinary(t *testing.T) {
	assert.Contains(t, requiredImageBinaries, "/bin/ls")
}
