//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/api/etcdgateway"
	"github.com/goairix/sandbox/internal/provisioning"
	"github.com/goairix/sandbox/internal/provisioning/deployment"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/sandboxfiles"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestLargeDownloadNative(t *testing.T) {
	if os.Getenv("SANDBOX_LARGE_DOWNLOAD_NATIVE") != "1" {
		t.Skip("requires exact owned large-download Linux fixture")
	}
	runColdCreateServiceFixtureNative(t, coldServiceNativeVariant{largeDownload: true})
}

type largeDownloadNativeService struct {
	*httpNativeService
	t         *testing.T
	downloads int
}

func (s *largeDownloadNativeService) Exec(ctx context.Context, sandboxID, requestID string, request p.ExecutionRequest, stdout, stderr io.Writer) (provisioning.PlainExecResult, error) {
	download := len(request.Argv) == 2 && request.Argv[1] == "file-download"
	if download {
		s.downloads++
		require.Equal(s.t, []string{"/usr/local/bin/sandbox-launcher", "file-download"}, request.Argv)
		require.Equal(s.t, uint32(1000), request.UID)
		require.Equal(s.t, uint32(1000), request.GID)
		require.Equal(s.t, map[string]string{"PATH": "/usr/bin:/bin"}, request.Env)
		require.Equal(s.t, "/workspace", request.WorkDir)
		require.Equal(s.t, uint32(10), request.TimeoutSeconds)
		require.False(s.t, request.RequiresNetwork)
		require.False(s.t, request.TTY)
		var input sandboxfiles.DownloadRequest
		require.NoError(s.t, json.Unmarshal(request.Stdin, &input))
		require.Contains(s.t, []string{"/tmp/large-download/binary.bin", "/tmp/large-download/empty", "/tmp/large-download/oversize", "/tmp/large-download/fifo", "/tmp/large-download/escape"}, input.Path)
		require.Equal(s.t, int64(4194304), input.MaxBytes)
		require.Equal(s.t, fmt.Sprintf(`{"path":%q,"max_bytes":4194304}`, input.Path), string(request.Stdin))
	}
	result, err := s.httpNativeService.Exec(ctx, sandboxID, requestID, request, stdout, stderr)
	if download {
		descriptor, descriptorErr := p.NewExecutionDescriptor(request)
		require.NoError(s.t, descriptorErr)
		require.Equal(s.t, descriptor.Digest(), result.Receipt.DescriptorDigest(), "original signed receipt binds exact fixed helper argv/stdin/profile")
		require.Equal(s.t, sandboxID, result.Receipt.Context().SandboxID)
	}
	return result, err
}

func exerciseLargeDownloadNative(t *testing.T, ctx context.Context, realService *provisioning.PlainColdService, sandboxID string) {
	t.Helper()
	service := &largeDownloadNativeService{httpNativeService: &httpNativeService{PlainColdService: realService, results: make(chan execServiceNativeAnswer, 2)}, t: t}
	var seedOut, seedErr bytes.Buffer
	seed := `set -eu
test "$(id -u)" = 1000
test "$(id -g)" = 1000
test ! -r /run/sandbox-control/config.json
mkdir -p /tmp/large-download
dd if=/dev/zero of=/tmp/large-download/binary.bin bs=1048613 count=1 status=none
printf '\000\377\200x' | dd of=/tmp/large-download/binary.bin bs=1 conv=notrunc status=none
printf z | dd of=/tmp/large-download/binary.bin bs=1 seek=1048612 conv=notrunc status=none
: > /tmp/large-download/empty
dd if=/dev/zero of=/tmp/large-download/oversize bs=1 count=0 seek=4194305 status=none
mkfifo /tmp/large-download/fifo
ln -s /etc/passwd /tmp/large-download/escape
printf 'uid1000 gid1000 control-unreadable large-files-created\n'
`
	_, err := service.Exec(ctx, sandboxID, uuid.NewString(), nativeExecRequest(seed, 5), &seedOut, &seedErr)
	require.NoError(t, err)
	require.Empty(t, seedErr.Bytes())
	require.Equal(t, "uid1000 gid1000 control-unreadable large-files-created\n", seedOut.String())
	seedResult := requireHTTPNativeExec(t, ctx, service.httpNativeService, true)
	require.Zero(t, seedResult.Receipt.RootWaitStatus())
	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0700))
	settings := &deployment.HTTPSettings{APIKey: []byte("native-http-key"), Env: map[string]string{"PATH": "/usr/bin:/bin"}, ExecTimeoutSeconds: 5, MaxExecTimeoutSeconds: 15, MaxActive: 4, RequestsPerSecond: 100, UID: 1000, GID: 1000, FileTransfer: &deployment.FileTransferSettings{Directory: directory, MaxFileBytes: 4 << 20, MaxTotalBytes: 8 << 20, MaxActive: 1, TimeoutSeconds: 10}}
	gateway, err := etcdgateway.New(service, settings)
	require.NoError(t, err)
	settings.Zero()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serverCtx, cancelServer := context.WithCancel(ctx)
	serverDone := make(chan error, 1)
	go func() { serverDone <- etcdgateway.Serve(serverCtx, listener, gateway) }()
	joined := false
	t.Cleanup(func() {
		cancelServer()
		if !joined {
			require.NoError(t, <-serverDone)
		}
	})
	client := &http.Client{Timeout: 15 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	origin := "http://" + listener.Addr().String()
	base := "/api/v1/sandboxes/" + sandboxID + "/files/download?path="
	want := make([]byte, 1048613)
	copy(want, []byte{0, 255, 128, 'x'})
	want[len(want)-1] = 'z'
	for _, tc := range []struct {
		name   string
		status int
		code   string
	}{
		{"binary.bin", 200, ""}, {"empty", 200, ""}, {"oversize", 413, "limit_exceeded"}, {"fifo", 422, "unsupported_type"}, {"escape", 500, "io_error"},
	} {
		before := service.downloads
		response := fileNativeRequest(t, client, origin, "GET", base+url.QueryEscape("/tmp/large-download/"+tc.name), nil)
		if tc.status == 200 {
			expected := int64(0)
			if tc.name == "binary.bin" {
				expected = 1048613
			}
			require.Equal(t, expected, response.ContentLength)
			require.Equal(t, "application/octet-stream", response.Header.Get("Content-Type"))
			require.Contains(t, response.Header.Get("Content-Disposition"), "attachment;")
		}
		wire := httpNativeRead(t, response, tc.status, nil)
		if tc.name == "binary.bin" {
			require.Len(t, wire, 1048613)
			require.Equal(t, sha256.Sum256(want), sha256.Sum256(wire))
		} else if tc.name == "empty" {
			require.Empty(t, wire)
		} else {
			require.Contains(t, string(wire), `"code":"`+tc.code+`"`)
		}
		result := requireHTTPNativeExec(t, ctx, service.httpNativeService, true)
		if tc.status == 200 {
			require.Zero(t, result.Receipt.RootWaitStatus())
		} else {
			require.Equal(t, uint32(256), result.Receipt.RootWaitStatus())
		}
		require.Equal(t, before+1, service.downloads, "one original Exec per HTTP download")
		entries, err := os.ReadDir(directory)
		require.NoError(t, err)
		require.Empty(t, entries, "the actual HTTP/spool owner cleaned up before the next request")
	}
	require.Equal(t, 5, service.downloads)
	require.Empty(t, service.results)
	cancelServer()
	select {
	case err := <-serverDone:
		joined = true
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("large download HTTP/store owners failed physical join")
	}
	require.NoError(t, gateway.Close())
	t.Logf("large download actual1048613-byte binary sha256=%x, empty200/oversize413/FIFO422/escape-signed-io_error500; UID/GID1000 protected-control denial, exact descriptors, signed terminal/drain, original Wait/IO and private spool cleanup", sha256.Sum256(want))
}
