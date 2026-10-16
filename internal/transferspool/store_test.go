//go:build linux || darwin

package transferspool

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func spoolOptions(t *testing.T) Options {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0700))
	return Options{Directory: dir, MaxFileBytes: 65536, MaxTotalBytes: 131072, MaxActive: 2}
}

func openSpool(t *testing.T, options Options) *Store {
	t.Helper()
	s, err := Open(options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func reserveSpool(t *testing.T, s *Store) *Job {
	t.Helper()
	j, err := s.Reserve(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, j.Close()) })
	return j
}

// Detects admission after cancellation, uncharged files, and early quota refund.
func TestTransferSpoolAdmissionBeforeFilesAndFullReservation(t *testing.T) {
	options := spoolOptions(t)
	options.MaxActive = 3
	s := openSpool(t, options)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.Reserve(ctx)
	require.ErrorIs(t, err, context.Canceled)
	entries, err := os.ReadDir(options.Directory)
	require.NoError(t, err)
	require.Empty(t, entries)
	j := reserveSpool(t, s)
	second := reserveSpool(t, s)
	_, err = s.Reserve(context.Background())
	require.ErrorIs(t, err, ErrCapacity, "two empty jobs still reserve the full disk budget")
	entries, err = os.ReadDir(options.Directory)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	for _, entry := range entries {
		require.Regexp(t, `^http-download-[0-9a-f]{32}$`, entry.Name())
		info, err := entry.Info()
		require.NoError(t, err)
		require.True(t, info.Mode().IsRegular())
		require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	}
	require.NoError(t, j.Close())
	third := reserveSpool(t, s)
	require.NoError(t, second.Close())
	require.NoError(t, third.Close())
	require.NoError(t, s.Err())
}

func TestTransferSpoolActiveLimitAndOptionsValidation(t *testing.T) {
	options := spoolOptions(t)
	options.MaxActive = 1
	s := openSpool(t, options)
	reserveSpool(t, s)
	_, err := s.Reserve(context.Background())
	require.ErrorIs(t, err, ErrCapacity)
	for _, mutate := range []func(*Options){
		func(o *Options) { o.Directory = "relative" },
		func(o *Options) { o.Directory += "/../child" },
		func(o *Options) { o.MaxFileBytes = 0 },
		func(o *Options) { o.MaxFileBytes = (2 << 30) + 1 },
		func(o *Options) { o.MaxTotalBytes = o.MaxFileBytes - 1 },
		func(o *Options) { o.MaxTotalBytes = (16 << 30) + 1 },
		func(o *Options) { o.MaxActive = 0 },
		func(o *Options) { o.MaxActive = 9 },
	} {
		bad := options
		mutate(&bad)
		require.Error(t, bad.Validate())
	}
	boundary := Options{Directory: options.Directory, MaxFileBytes: 2 << 30, MaxTotalBytes: 16 << 30, MaxActive: 8}
	require.NoError(t, boundary.Validate())
}

// Detects exposing unsealed bytes, corrupt binary copies, and mutable options.
func TestTransferSpoolSealBinaryAndEmpty(t *testing.T) {
	options := spoolOptions(t)
	s := openSpool(t, options)
	options.MaxFileBytes = 1
	options.Directory = "/must-not-be-used"
	j := reserveSpool(t, s)
	_, err := j.Read(make([]byte, 1))
	require.Error(t, err, "unsealed bytes cannot be consumed")
	data := bytes.Repeat([]byte{0, 255, 128, '\n'}, 16384)
	n, err := j.Write(data)
	require.NoError(t, err)
	require.Equal(t, 65536, n)
	size, err := j.Seal()
	require.NoError(t, err)
	require.Equal(t, int64(65536), size)
	got, err := io.ReadAll(j)
	require.NoError(t, err)
	require.Equal(t, data, got)
	_, err = j.Write([]byte("late"))
	require.Error(t, err)
	require.NoError(t, j.Close())
	empty := reserveSpool(t, s)
	size, err = empty.Seal()
	require.NoError(t, err)
	require.Zero(t, size)
	n, err = empty.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF)
	require.Zero(t, n)
}

func TestTransferSpoolOverLimitCancelsAndLatchesUnavailable(t *testing.T) {
	options := spoolOptions(t)
	options.MaxFileBytes, options.MaxTotalBytes = 3, 3
	s, err := Open(options)
	require.NoError(t, err)
	j, err := s.Reserve(context.Background())
	require.NoError(t, err)
	_, err = j.Write([]byte("four"))
	require.Error(t, err)
	require.ErrorIs(t, j.Context().Err(), context.Canceled)
	_, err = j.Seal()
	require.Error(t, err)
	_ = j.Close()
	require.Error(t, s.Err())
	_, err = s.Reserve(context.Background())
	require.ErrorIs(t, err, ErrUnavailable)
	require.Error(t, s.Close())
}

func TestTransferSpoolSealRejectsExternalSizeAndIdentityChanges(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprintf("replacement-%t", replacement), func(t *testing.T) {
			options := spoolOptions(t)
			s, err := Open(options)
			require.NoError(t, err)
			j, err := s.Reserve(context.Background())
			require.NoError(t, err)
			_, err = j.Write([]byte("original"))
			require.NoError(t, err)
			entries, err := os.ReadDir(options.Directory)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			name := filepath.Join(options.Directory, entries[0].Name())
			if replacement {
				require.NoError(t, os.Rename(name, name+"-original"))
			}
			require.NoError(t, os.WriteFile(name, []byte("changed"), 0600))
			_, err = j.Seal()
			require.Error(t, err)
			_ = j.Close()
			require.Error(t, s.Err())
			_, err = s.Reserve(context.Background())
			require.ErrorIs(t, err, ErrUnavailable)
			require.Error(t, s.Close())
			if replacement {
				got, err := os.ReadFile(name)
				require.NoError(t, err)
				require.Equal(t, "changed", string(got), "cleanup must preserve a replacement identity")
			}
		})
	}
}

// Invalidating the actual job FD causes real write/close errors, not a mock.
func TestTransferSpoolIOFailureRetainsChargeAndCloseError(t *testing.T) {
	options := spoolOptions(t)
	s, err := Open(options)
	require.NoError(t, err)
	j, err := s.Reserve(context.Background())
	require.NoError(t, err)
	require.NoError(t, j.file.Close())
	_, err = j.Write([]byte("x"))
	require.Error(t, err)
	require.ErrorIs(t, j.Context().Err(), context.Canceled)
	closeErr := j.Close()
	require.Error(t, closeErr)
	require.Equal(t, closeErr, j.Close())
	_, err = s.Reserve(context.Background())
	require.ErrorIs(t, err, ErrUnavailable)
	storeErr := s.Close()
	require.Error(t, storeErr)
	require.Equal(t, storeErr, s.Close())
	entries, err := os.ReadDir(options.Directory)
	require.NoError(t, err)
	require.Len(t, entries, 1, "failed cleanup must not pretend bytes were removed")
}

// Detects Store.Close closing descriptors or reporting completion before owner join.
func TestTransferSpoolCloseWaitsForOriginalOwner(t *testing.T) {
	options := spoolOptions(t)
	s, err := Open(options)
	require.NoError(t, err)
	j, err := s.Reserve(context.Background())
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case <-j.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("Store.Close did not cancel original owner")
	}
	select {
	case err := <-done:
		t.Fatalf("Store.Close returned before original owner: %v", err)
	default:
	}
	_, err = j.file.Stat()
	require.NoError(t, err, "store must keep unjoined owner's FD alive")
	_, err = s.Reserve(context.Background())
	require.ErrorIs(t, err, ErrUnavailable)
	require.NoError(t, j.Close())
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Store.Close did not observe physical owner release")
	}
	require.NoError(t, s.Close())
	entries, err := os.ReadDir(options.Directory)
	require.NoError(t, err)
	require.Empty(t, entries)
	opened, err := Open(options)
	require.NoError(t, err, "directory lock must release only after owners joined")
	require.NoError(t, opened.Close())
}

func TestTransferSpoolDirectoryChecksAndLock(t *testing.T) {
	options := spoolOptions(t)
	s := openSpool(t, options)
	_, err := Open(options)
	require.Error(t, err)
	require.NoError(t, s.Close())
	require.NoError(t, os.Chmod(options.Directory, 0750))
	_, err = Open(options)
	require.Error(t, err)
	require.NoError(t, os.Chmod(options.Directory, 0700))
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(options.Directory, link))
	options.Directory = link
	_, err = Open(options)
	require.Error(t, err)
	fifo := filepath.Join(t.TempDir(), "fifo")
	require.NoError(t, unix.Mkfifo(fifo, 0600))
	options.Directory = fifo
	done := make(chan error, 1)
	go func() { _, err := Open(options); done <- err }()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("directory opening blocked on FIFO")
	}
}

func TestTransferSpoolStaleValidationBeforeDeletion(t *testing.T) {
	for _, scenario := range []string{"valid", "unknown", "nine", "mode", "hardlink", "symlink", "directory", "fifo"} {
		t.Run(scenario, func(t *testing.T) {
			options := spoolOptions(t)
			name := filepath.Join(options.Directory, "http-download-"+strings.Repeat("a", 32))
			require.NoError(t, os.WriteFile(name, []byte("discardable"), 0600))
			switch scenario {
			case "unknown":
				require.NoError(t, os.WriteFile(filepath.Join(options.Directory, "operator-data"), nil, 0600))
			case "nine":
				for i := 0; i < 8; i++ {
					require.NoError(t, os.WriteFile(filepath.Join(options.Directory, fmt.Sprintf("http-download-%032x", i)), nil, 0600))
				}
			case "mode":
				require.NoError(t, os.Chmod(name, 0640))
			case "hardlink":
				require.NoError(t, os.Link(name, filepath.Join(t.TempDir(), "second-link")))
			case "symlink", "directory", "fifo":
				require.NoError(t, os.Remove(name))
				if scenario == "symlink" {
					require.NoError(t, os.Symlink("/etc/passwd", name))
				} else if scenario == "directory" {
					require.NoError(t, os.Mkdir(name, 0700))
				} else {
					require.NoError(t, unix.Mkfifo(name, 0600))
				}
			}
			s, err := Open(options)
			if scenario == "valid" {
				require.NoError(t, err)
				require.NoError(t, s.Close())
				entries, err := os.ReadDir(options.Directory)
				require.NoError(t, err)
				require.Empty(t, entries)
			} else {
				require.Error(t, err)
				_, err = os.Lstat(name)
				require.NoError(t, err, "validation must finish before any stale deletion")
			}
		})
	}
}

func TestTransferSpoolCrashChild(t *testing.T) {
	if os.Getenv("TRANSFER_SPOOL_CRASH_CHILD") != "1" {
		return
	}
	options := Options{Directory: os.Getenv("TRANSFER_SPOOL_CRASH_DIRECTORY"), MaxFileBytes: 65536, MaxTotalBytes: 131072, MaxActive: 2}
	s, err := Open(options)
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.Reserve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = j.Write([]byte("actual child crash data")); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(os.Stdout, "SPOOL_CHILD_READY")
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(97) // Parent must kill us; normal exit is a fixture failure.
}

func TestTransferSpoolActualProcessCrashReleasesLockAndRecovers(t *testing.T) {
	options := spoolOptions(t)
	command := exec.Command(os.Args[0], "-test.run=^TestTransferSpoolCrashChild$")
	command.Env = append(os.Environ(), "TRANSFER_SPOOL_CRASH_CHILD=1", "TRANSFER_SPOOL_CRASH_DIRECTORY="+options.Directory)
	stdin, err := command.StdinPipe()
	require.NoError(t, err)
	stdout, err := command.StdoutPipe()
	require.NoError(t, err)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	require.NoError(t, command.Start())
	joined := false
	t.Cleanup(func() {
		stdin.Close()
		if !joined {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		require.Equal(t, "SPOOL_CHILD_READY\n", line)
	case <-time.After(5 * time.Second):
		t.Fatal("actual child did not reach reserved/written state")
	}
	_, err = Open(options)
	require.Error(t, err, "another live process owns directory flock")
	entries, err := os.ReadDir(options.Directory)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.True(t, regexp.MustCompile(`^http-download-[0-9a-f]{32}$`).MatchString(entries[0].Name()))
	require.NoError(t, command.Process.Kill())
	err = command.Wait()
	joined = true
	require.Empty(t, stderr.String(), "stderr is read only after original Cmd.Wait joined its copier")
	var exit *exec.ExitError
	require.True(t, errors.As(err, &exit), "actual killed process must be physically waited")
	s := openSpool(t, options)
	entries, err = os.ReadDir(options.Directory)
	require.NoError(t, err)
	require.Empty(t, entries, "crashed process's bounded stale job must be removed")
	j := reserveSpool(t, s)
	_, err = j.Write([]byte("new owner"))
	require.NoError(t, err)
}
