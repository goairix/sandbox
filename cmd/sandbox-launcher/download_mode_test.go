package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/goairix/sandbox/internal/sandboxfiles"
	"github.com/stretchr/testify/require"
)

func TestDownloadModeProcessChild(t *testing.T) {
	if os.Getenv("SANDBOX_DOWNLOAD_MODE_CHILD") != "1" {
		return
	}
	os.Exit(launcherExitCode(run([]string{"file-download"}), os.Stderr))
}

func runDownloadModeChild(t *testing.T, input []byte) (int, []byte, []byte) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestDownloadModeProcessChild$")
	command.Env = append(os.Environ(), "SANDBOX_DOWNLOAD_MODE_CHILD=1")
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	exit := 0
	if err != nil {
		var processErr *exec.ExitError
		require.ErrorAs(t, err, &processErr)
		exit = processErr.ExitCode()
	}
	return exit, stdout.Bytes(), stderr.Bytes()
}

// Detects a helper mode that corrupts binary stdout or adds a logging prefix to
// signed application failures. This runs the real fixed-mode dispatcher.
func TestDownloadModeBinaryEmptyAndQuietApplicationFailure(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "sandbox-download-cli-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	data := bytes.Repeat([]byte{0, 255, 128, '\n'}, 150000)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "binary"), data, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "empty"), nil, 0600))
	for _, tc := range []struct {
		name    string
		want    []byte
		status  int
		failure string
	}{
		{"binary", data, 0, ""}, {"empty", nil, 0, ""}, {"missing", nil, 1, "not_found"},
	} {
		wire, err := json.Marshal(sandboxfiles.DownloadRequest{Path: filepath.Join(dir, tc.name), MaxBytes: 700000})
		require.NoError(t, err)
		exit, stdout, stderr := runDownloadModeChild(t, wire)
		require.Equal(t, tc.status, exit)
		require.True(t, bytes.Equal(tc.want, stdout), "binary output differs")
		if tc.failure == "" {
			require.Empty(t, stderr)
		} else {
			require.Equal(t, "{\"code\":\"not_found\"}\n", string(stderr))
		}
	}
	exit, stdout, stderr := runDownloadModeChild(t, []byte(`{"path":"/tmp/a","max_bytes":1,"extra":true}`))
	require.Equal(t, 1, exit)
	require.Empty(t, stdout)
	require.Equal(t, "{\"code\":\"invalid_request\"}\n", string(stderr))
}

func TestDownloadModeBoundedInputAndSinkErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := fileDownload(bytes.NewReader(bytes.Repeat([]byte("x"), 16385)), &stdout, &stderr)
	require.Error(t, err)
	require.Equal(t, 1, launcherExitCode(err, &stderr))
	require.Empty(t, stdout.Bytes())
	require.Equal(t, "{\"code\":\"limit_exceeded\"}\n", stderr.String())
	stderr.Reset()
	err = fileDownload(bytes.NewReader([]byte("bad")), &stdout, failingDownloadStderr{})
	require.Error(t, err)
	require.Equal(t, 1, launcherExitCode(err, &stderr))
	require.Empty(t, stderr.Bytes(), "typed failure stays quiet even if its original stderr sink failed")
}

type failingDownloadStderr struct{}

func (failingDownloadStderr) Write([]byte) (int, error) { return 0, errors.New("closed stderr") }

func TestDownloadModePreservesOtherModeErrorLogging(t *testing.T) {
	var stderr bytes.Buffer
	require.Equal(t, 1, launcherExitCode(errors.New("old mode failure"), &stderr))
	require.Equal(t, "sandbox-launcher: old mode failure\n", stderr.String())
	stderr.Reset()
	require.Zero(t, launcherExitCode(nil, &stderr))
	require.Empty(t, stderr.Bytes())
}
