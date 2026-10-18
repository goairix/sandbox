//go:build linux

package mounter

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestNativeMounterReadinessChannelPID1 authenticates an actual independent
// helper process. Only its mount/process backend is the existing model: this
// proves neither a /dev/fuse mount nor access to an object store. Samples are
// produced by the original resident refresh/cache owners, never injected.
func TestNativeMounterReadinessChannelPID1(t *testing.T) {
	if os.Getenv("SANDBOX_MOUNTER_CHANNEL_PID1") != "1" {
		t.Skip("requires original enrolled launcher and independent native helper fixture")
	}
	require.Equal(t, 1, os.Getpid())
	require.NoError(t, unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""))
	require.NoError(t, unix.Unmount("/proc", unix.MNT_DETACH))
	require.NoError(t, unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""))
	uid := os.Getenv("SANDBOX_MOUNTER_CHANNEL_UID")
	require.NotEmpty(t, uid)
	root := t.TempDir()
	for _, dir := range []string{"run", "secrets", "cache/tmp", "workspace"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, dir), 0700))
	}
	bootstrap := validBootstrap(root)
	bootstrap.RuntimeUID, bootstrap.CacheLimitBytes = uid, 128<<20
	runner := &fakeRunner{}
	var ready bool
	supervisor := NewSupervisor(Config{
		RunDir: filepath.Join(root, "run"), CredentialRoot: filepath.Join(root, "secrets"), CacheRoot: filepath.Join(root, "cache"), MountPath: bootstrap.MountPath,
		CheckFuse: func() error { return nil }, CheckAnchor: func(string) error { return nil },
		MountInfo: func() (Mount, error) {
			kind, id := "tmpfs", uint64(0)
			if ready {
				kind, id = "fuse.s3fs", 41
			}
			return Mount{ID: id, MountPoint: bootstrap.MountPath, FilesystemType: kind}, nil
		},
	}, runner)
	attachTestNativeOwner(t, supervisor)
	require.NoError(t, supervisor.Bootstrap(context.Background(), bootstrap, uid))
	authorization := validAuthorization()
	authorization.RuntimeUID = uid
	// The modeled mount still names the workspace in the original signed slot.
	authorization.WorkspaceHash = strings.Repeat("a", 64)
	require.NoError(t, supervisor.Authorize(context.Background(), authorization))
	ready = true
	_, err := supervisor.ReadyStatus(context.Background())
	require.NoError(t, err)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	ctx, deadline := context.WithTimeout(ctx, 55*time.Second)
	defer deadline()
	channel := &ReadinessChannel{Supervisor: supervisor, SocketPath: readinessObserverSocket, PodName: os.Getenv("SANDBOX_MOUNTER_CHANNEL_POD"), NodeUID: os.Getenv("SANDBOX_MOUNTER_CHANNEL_NODE"), NodeBootID: os.Getenv("SANDBOX_MOUNTER_CHANNEL_BOOT"), ContractDigest: os.Getenv("SANDBOX_MOUNTER_CHANNEL_CONTRACT")}
	readiness := &ReadinessServer{Supervisor: supervisor, Address: "127.0.0.1:0"}
	joined := make(chan error, 2)
	go func() { joined <- readiness.Serve(ctx) }()
	go func() { joined <- channel.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		var joinedErr error
		for range 2 {
			joinedErr = errors.Join(joinedErr, <-joined)
		}
		// End only the explicitly modeled filesystem child after all actual
		// channel/sampler IO has returned; native PID1 handles remain owned.
		runner.process.exit <- nil
		<-supervisor.processDone
		ready = false
		shutdown, finish := context.WithTimeout(context.Background(), 3*time.Second)
		defer finish()
		_, err := supervisor.Shutdown(shutdown, nil)
		closeErr := supervisor.Close()
		require.NoError(t, errors.Join(joinedErr, err, closeErr), "original channel/readiness/cache/model child owners joined before native close")
	})
	<-ctx.Done()
	require.NotEqual(t, context.DeadlineExceeded, ctx.Err(), "parent must explicitly retire the original helper")
}

// TestNativeKernelPID1 requires the test binary itself to be PID1. Root runs
// this selector in a fresh Linux PID namespace; host model tests are not a
// substitute. No /dev/fuse, object store, launcher enrollment or Pod is assumed.
func TestNativeKernelPID1(t *testing.T) {
	if os.Getenv("SANDBOX_MOUNTER_NATIVE_TEST") != "1" {
		t.Skip("requires an explicitly selected native PID1 execution")
	}
	require.Equal(t, 1, os.Getpid(), "native execution must use the original test process as PID1")

	t.Run("factory_preserves_locked_start_without_precreated_directories", func(t *testing.T) {
		root := t.TempDir()
		s, err := NewNativeSupervisor(NativeConfig{
			RunDir:         filepath.Join(root, "run"),
			CredentialRoot: filepath.Join(root, "credentials"),
			CacheRoot:      filepath.Join(root, "cache"),
			MountPath:      filepath.Join(root, "workspace"),
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		require.Equal(t, StateLocked, s.State())
		require.NotNil(t, s.nativeOwner)
		require.NoError(t, s.nativeOwner.validateCurrent())
		entries, err := os.ReadDir(root)
		require.NoError(t, err)
		require.Empty(t, entries)

		require.NoError(t, s.Close())
		require.NoError(t, s.Close())
		require.Error(t, s.nativeOwner.validateCurrent())
	})

	t.Run("identity_mismatch_revokes_without_releasing_original_handles", func(t *testing.T) {
		owner := captureTestNativeOwner(t)
		original := owner.identity.startTime
		owner.identity.startTime++
		require.Error(t, owner.validateCurrent())

		// Restoring the saved value must not resurrect revoked admission.
		owner.identity.startTime = original
		require.Error(t, owner.validateCurrent())
		for _, file := range nativeOwnerFiles(owner) {
			_, err := file.Stat()
			require.NoError(t, err, "identity loss must retain original resources until final close")
		}
	})

	t.Run("close_is_idempotent_after_a_partial_handle_failure", func(t *testing.T) {
		owner := captureTestNativeOwner(t)
		files := nativeOwnerFiles(owner)
		fds := make([]uintptr, len(files))
		for index, file := range files {
			fds[index] = file.Fd()
		}

		require.NoError(t, owner.executable.Close())
		require.Error(t, owner.validateCurrent())
		// An already closed owned file may be reported, but it must not stop
		// cleanup of the remaining files or cause a later raw-FD double close.
		_ = owner.close()
		for _, fd := range fds {
			_, err := unix.FcntlInt(fd, unix.F_GETFD, 0)
			require.ErrorIs(t, err, unix.EBADF)
		}

		sentinel, err := os.Open(os.DevNull)
		require.NoError(t, err)
		defer sentinel.Close()
		_ = owner.close()
		_, err = sentinel.Stat()
		require.NoError(t, err, "repeated close must not close a subsequently reused descriptor")
	})

	t.Run("bootstrap_revalidates_before_publication", func(t *testing.T) {
		// The existing model supplies filesystem/process doubles only. The
		// owner still comes exclusively from this real PID1 kernel process.
		runner := &fakeRunner{}
		prepared, bootstrap := newTestSupervisor(t, runner)
		require.NoError(t, os.Remove(filepath.Join(prepared.config.RunDir, "bootstrap.json")))
		s := NewSupervisor(prepared.config, runner)
		attachTestNativeOwner(t, s)
		s.config.CheckFuse = func() error {
			s.nativeOwner.identity.startTime++
			return nil
		}

		require.Error(t, s.Bootstrap(context.Background(), bootstrap, "uid-a"))
		require.False(t, s.bootstrapped)
		require.NoFileExists(t, filepath.Join(s.config.RunDir, "bootstrap.json"))
		require.Equal(t, 0, runner.starts)
	})

	t.Run("authorize_revalidation_prevents_credentials_marker_and_process", func(t *testing.T) {
		runner := &fakeRunner{}
		s, bootstrap := newTestSupervisor(t, runner)
		attachTestNativeOwner(t, s)
		s.config.CheckFuse = func() error {
			s.nativeOwner.identity.startTime++
			return nil
		}

		require.Error(t, s.Authorize(context.Background(), validAuthorization()))
		require.NoFileExists(t, bootstrap.PasswdFile)
		require.NoFileExists(t, filepath.Join(s.config.RunDir, mountGenerationFile))
		require.Equal(t, 0, runner.starts)
		require.False(t, s.consumed)
	})

	t.Run("identity_loss_preserves_original_shutdown_cleanup", func(t *testing.T) {
		runner := &fakeRunner{}
		s, _ := newTestSupervisor(t, runner)
		attachTestNativeOwner(t, s)
		require.NoError(t, s.Authorize(context.Background(), validAuthorization()))
		s.nativeOwner.identity.startTime++
		require.Error(t, s.nativeOwner.validateCurrent())

		runner.process.signal = func(signal os.Signal) error {
			require.Equal(t, syscall.SIGTERM, signal)
			_, err := s.nativeOwner.proc.Stat()
			require.NoError(t, err, "native resources must outlive the original shutdown")
			runner.process.exit <- nil
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := s.Shutdown(ctx, nil)
		require.NoError(t, err)
		require.Equal(t, StateStopped, s.State())
		require.NoError(t, s.Close())
	})

	t.Run("busy_close_seals_admission_and_retains_handles_until_original_process_joins", func(t *testing.T) {
		runner := &fakeRunner{}
		s, _ := newTestSupervisor(t, runner)
		attachTestNativeOwner(t, s)
		require.NoError(t, s.Authorize(context.Background(), validAuthorization()))
		files := nativeOwnerFiles(s.nativeOwner)
		fds := make([]uintptr, len(files))
		for index, file := range files {
			fds[index] = file.Fd()
		}

		// A live original child forbids successful release. Closing admission
		// still must not discard the handles needed by the original cleanup.
		require.Error(t, s.Close())
		require.Error(t, s.nativeOwner.validateCurrent())
		for _, file := range files {
			_, err := file.Stat()
			require.NoError(t, err)
		}

		runner.process.signal = func(os.Signal) error {
			for _, file := range files {
				_, err := file.Stat()
				require.NoError(t, err, "pending Close must preserve handles through original Shutdown")
			}
			runner.process.exit <- nil
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := s.Shutdown(ctx, nil)
		require.NoError(t, err)
		select {
		case <-s.processDone:
		default:
			t.Fatal("Shutdown returned before the original process joined")
		}

		require.NoError(t, s.Close())
		for _, fd := range fds {
			_, err := unix.FcntlInt(fd, unix.F_GETFD, 0)
			require.ErrorIs(t, err, unix.EBADF)
		}
		sentinel, err := os.Open(os.DevNull)
		require.NoError(t, err)
		defer sentinel.Close()
		require.NoError(t, s.Close())
		_, err = sentinel.Stat()
		require.NoError(t, err, "retrying Close must not close an unrelated reused descriptor")
	})

	t.Run("copied_supervisor_cannot_consume_or_close_original_owner", func(t *testing.T) {
		runner := &fakeRunner{}
		s, bootstrap := newTestSupervisor(t, runner)
		attachTestNativeOwner(t, s)
		files := nativeOwnerFiles(s.nativeOwner)

		// Deliberately reproduce an accidental value copy without teaching
		// callers to copy a mutex. Production must reject the copied address
		// before it touches either copied state or the shared native owner.
		var copied Supervisor
		reflect.ValueOf(&copied).Elem().Set(reflect.ValueOf(s).Elem())
		require.Error(t, copied.Close())
		require.Error(t, copied.Authorize(context.Background(), validAuthorization()))
		require.NoError(t, s.nativeOwner.validateCurrent(), "copy.Close must not revoke the original owner")
		for _, file := range files {
			_, err := file.Stat()
			require.NoError(t, err, "copy.Close must not close the original owner's handles")
		}
		require.NoFileExists(t, bootstrap.PasswdFile)
		require.NoFileExists(t, filepath.Join(s.config.RunDir, mountGenerationFile))
		require.Equal(t, 0, runner.starts)
		require.False(t, s.consumed)

		// The original instance retains its single-use authorization and its
		// cleanup path after both rejected operations on the copied value.
		require.NoError(t, s.Authorize(context.Background(), validAuthorization()))
		runner.process.signal = func(os.Signal) error {
			runner.process.exit <- nil
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := s.Shutdown(ctx, nil)
		require.NoError(t, err)
		require.NoError(t, s.Close())
	})

	t.Run("partial_capture_closes_every_opened_descriptor", testNativeCaptureDescriptorExhaustion)
}

func captureTestNativeOwner(t *testing.T) *nativeOwner {
	t.Helper()
	owner, err := captureNativeOwner()
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.close() })
	require.NoError(t, owner.validateCurrent())
	return owner
}

func nativeOwnerFiles(owner *nativeOwner) []*os.File {
	return []*os.File{owner.proc, owner.pidfd, owner.pidNamespace, owner.mountNamespace, owner.executable}
}

// attachTestNativeOwner mirrors the factory's one-time pointer binding while
// keeping external process/filesystem doubles out of the production factory.
func attachTestNativeOwner(t *testing.T, supervisor *Supervisor) {
	t.Helper()
	owner := captureTestNativeOwner(t)
	owner.supervisor = supervisor
	supervisor.nativeOwner = owner
}

// Descriptor exhaustion exercises real partial acquisition, with no fake
// opener or constructor callback. Each failed capture must return all slots it
// used. Keep this serial: the process-wide limit belongs to this test only.
func testNativeCaptureDescriptorExhaustion(t *testing.T) {
	var previous unix.Rlimit
	require.NoError(t, unix.Getrlimit(unix.RLIMIT_NOFILE, &previous))
	entries, err := os.ReadDir("/proc/self/fd")
	require.NoError(t, err)
	maximum := 2
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		require.NoError(t, err)
		if fd > maximum {
			maximum = fd
		}
	}

	limited := previous
	limited.Cur = uint64(maximum + 32)
	if limited.Cur > previous.Cur {
		limited.Cur = previous.Cur
	}
	require.NoError(t, unix.Setrlimit(unix.RLIMIT_NOFILE, &limited))
	var fillers []int
	defer func() {
		require.NoError(t, unix.Setrlimit(unix.RLIMIT_NOFILE, &previous))
		for _, fd := range fillers {
			require.NoError(t, unix.Close(fd))
		}
	}()

	fill := func() (int, error) {
		opened := 0
		for {
			fd, err := unix.Open(os.DevNull, unix.O_RDONLY|unix.O_CLOEXEC, 0)
			if errors.Is(err, unix.EMFILE) {
				return opened, nil
			}
			if err != nil {
				return opened, err
			}
			fillers = append(fillers, fd)
			opened++
		}
	}
	_, err = fill()
	require.NoError(t, err)

	// Five original handles are required. Fewer available slots must fail
	// and leave exactly the same number available for an unrelated open.
	for available := 0; available < 5; available++ {
		for released := 0; released < available; released++ {
			last := len(fillers) - 1
			require.NoError(t, unix.Close(fillers[last]))
			fillers = fillers[:last]
		}

		owner, captureErr := captureNativeOwner()
		if owner != nil {
			_ = owner.close()
		}
		opened, fillErr := fill()
		require.Error(t, captureErr, "capture accepted only %d available descriptor slots", available)
		require.Nil(t, owner)
		require.NoError(t, fillErr)
		require.Equal(t, available, opened, "partial capture leaked a descriptor")
	}
}
