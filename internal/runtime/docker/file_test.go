package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/runtime"
)

func TestReservedFileCountPipelinePropagatesFindFailure(t *testing.T) {
	binDir := writeFakeFind(t, "printf '.\\n'\nexit 7\n")
	command := exec.Command("sh", "-c", reservedFileCountCommand("/workspace", 0, ""))
	command.Env = append(os.Environ(), "PATH="+binDir+":/usr/bin:/bin")
	output, err := command.CombinedOutput()
	require.Error(t, err, "find failure must not be masked by the downstream counter: %s", output)
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	require.Equal(t, 7, exitErr.ExitCode())
}

func TestReservedFileCountPipelineReturnsNumericCount(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "zero", body: "exit 0\n", want: "0"},
		{name: "multiple", body: "printf '.\\n.\\n.\\n'\n", want: "3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binDir := writeFakeFind(t, tc.body)
			command := exec.Command("sh", "-c", reservedFileCountCommand("/workspace", 0, ""))
			command.Env = append(os.Environ(), "PATH="+binDir+":/usr/bin:/bin")
			output, err := command.CombinedOutput()
			require.NoError(t, err, "%s", output)
			require.Equal(t, tc.want, strings.TrimSpace(string(output)))
		})
	}
}

func writeFakeFind(t *testing.T, body string) string {
	t.Helper()
	binDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "find"), []byte("#!/bin/sh\n"+body), 0o755))
	return binDir
}

func TestWriteSizedTarStreamsExactBody(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, writeSizedTar(&out, "a.txt", 0o644, 1000, 1000, 5, strings.NewReader("hello")))
	tr := tar.NewReader(&out)
	header, err := tr.Next()
	require.NoError(t, err)
	assert.Equal(t, int64(5), header.Size)
	content, err := io.ReadAll(tr)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(content))
}

func TestWriteSizedTarRejectsInvalidBodyLengths(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int64
		body string
	}{
		{name: "negative", size: -1},
		{name: "short", size: 5, body: "four"},
		{name: "excess", size: 4, body: "extra"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := writeSizedTar(io.Discard, "a.txt", 0o644, 0, 0, tc.size, strings.NewReader(tc.body))
			require.ErrorIs(t, err, runtime.ErrInvalidUploadSize)
		})
	}
}

func TestUploadExecResultRejectsNonZeroExit(t *testing.T) {
	err := uploadExecResult("publish uploaded file", &runtime.ExecResult{ExitCode: 1, Stderr: "destination is a directory"}, nil)
	require.ErrorContains(t, err, "destination is a directory")
}

func TestUploadPublishCommandRejectsDirectoryDestination(t *testing.T) {
	command := uploadPublishCommand("/workspace/.sandbox-upload-temp", "/workspace/target")
	assert.Contains(t, command, "test ! -d")
	assert.Contains(t, command, "mv -f --")
}

func TestLegacyUploadTemporaryFileBelongsToSandboxUser(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	fake.mu.Lock()
	fake.containers["legacy-runtime"] = &fakeContainer{
		config:  &container.Config{User: "1000", Labels: map[string]string{"sandbox.managed": "true", "sandbox.id": "legacy"}},
		name:    "legacy",
		running: true,
	}
	fake.mu.Unlock()

	require.NoError(t, rt.UploadFile(context.Background(), "legacy-runtime", "/tmp/a.txt", 5, strings.NewReader("hello")))

	fake.mu.Lock()
	defer fake.mu.Unlock()
	assert.Equal(t, 1000, fake.lastCopyToUID)
	assert.Equal(t, 1000, fake.lastCopyToGID)
	assert.True(t, fake.lastCopyToOptions.CopyUIDGID)
}

func TestUploadSizeErrorPreservesTypeAndReportsCleanupFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		exitCode   int
		inspectErr error
		want       string
	}{
		{name: "nonzero exit", exitCode: 9, want: "exit code 9"},
		{name: "exec error", inspectErr: errors.New("inspect cleanup failed"), want: "inspect cleanup failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, fake := newFakeDockerRuntime(t)
			fake.mu.Lock()
			fake.containers["legacy-runtime"] = &fakeContainer{
				config:  &container.Config{User: "1000", Labels: map[string]string{"sandbox.managed": "true", "sandbox.id": "legacy"}},
				name:    "legacy",
				running: true,
			}
			fake.removeExecExitCode = tc.exitCode
			fake.removeExecInspectErr = tc.inspectErr
			fake.mu.Unlock()

			err := rt.UploadFile(context.Background(), "legacy-runtime", "/tmp/a.txt", 5, strings.NewReader("four"))

			require.ErrorIs(t, err, runtime.ErrInvalidUploadSize)
			require.ErrorContains(t, err, "cleanup partial upload")
			require.ErrorContains(t, err, tc.want)
		})
	}
}
