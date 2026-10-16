package sandboxfiles

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestDownloadCanonicalRequestAndLimits(t *testing.T) {
	wire := []byte(`{"path":"/tmp/a","max_bytes":2147483648}`)
	request, err := DecodeDownloadRequest(wire)
	require.NoError(t, err)
	require.Equal(t, DownloadRequest{Path: "/tmp/a", MaxBytes: 2147483648}, request)
	for _, wire := range []string{
		`{"path":"/tmp/a","max_bytes":0}`, `{"path":"/tmp/a","max_bytes":-1}`,
		`{"path":"/tmp/a","max_bytes":2147483649}`, `{"path":"/etc/passwd","max_bytes":1}`,
		`{"path":"/workspace/../tmp/a","max_bytes":1}`, `{"path":"relative","max_bytes":1}`,
		`{"path":"/tmp/a","max_bytes":1,"extra":true}`, `{"Path":"/tmp/a","max_bytes":1}`,
		`{"path":"/tmp/a","path":"/tmp/b","max_bytes":1}`, `{"path":null,"max_bytes":1}`,
		`{"path":"/tmp/a","max_bytes":null}`, `{"path":"/tmp/a","max_bytes":1} {}`,
		` {"path":"/tmp/a","max_bytes":1}`, `{"max_bytes":1,"path":"/tmp/a"}`,
		`{"path":"/tmp/a","max_bytes":1.0}`, `{"path":"/tmp/a","max_bytes":1e0}`,
		`{"path":"/tmp/\u0061","max_bytes":1}`, "null", "{}", strings.Repeat(" ", 16385),
	} {
		_, err := DecodeDownloadRequest([]byte(wire))
		require.Error(t, err, "wire=%q", wire)
	}
	for _, path := range []string{"/tmp/" + strings.Repeat("x", 4096), "/tmp/\x00", string([]byte{'/', 't', 'm', 'p', '/', 255})} {
		require.Error(t, ValidateDownload(DownloadRequest{Path: path, MaxBytes: 1}))
	}
}

// Detects loading a download through the legacy 512KiB reader or text encoding.
func TestDownloadStreamsLargeBinaryAndEmpty(t *testing.T) {
	dir, root := filesFixture(t)
	data := bytes.Repeat([]byte{0, 255, 128, '\n'}, (1<<18)+1)
	fileWrite(t, dir, "binary", data, 0600)
	output := &downloadCheckingWriter{}
	err := streamDownloadAt(DownloadRequest{Path: "/workspace/binary", MaxBytes: int64(len(data))}, root, output)
	require.NoError(t, err)
	require.Equal(t, data, output.Bytes())
	require.LessOrEqual(t, output.largest, 32768)
	fileWrite(t, dir, "empty", nil, 0600)
	output = &downloadCheckingWriter{}
	require.NoError(t, streamDownloadAt(DownloadRequest{Path: "/workspace/empty", MaxBytes: 1}, root, output))
	require.Empty(t, output.Bytes())
}

type downloadCheckingWriter struct {
	bytes.Buffer
	largest int
}

func (w *downloadCheckingWriter) Write(data []byte) (int, error) {
	if len(data) > w.largest {
		w.largest = len(data)
	}
	return w.Buffer.Write(data)
}

func TestDownloadRejectsInitialSizeBeforeWriting(t *testing.T) {
	dir, root := filesFixture(t)
	fileWrite(t, dir, "large", []byte("four"), 0600)
	var output bytes.Buffer
	err := streamDownloadAt(DownloadRequest{Path: "/workspace/large", MaxBytes: 3}, root, &output)
	require.Error(t, err)
	require.Equal(t, []byte("{\"code\":\"limit_exceeded\"}\n"), EncodeDownloadFailure(err))
	require.Empty(t, output.Bytes())
}

func TestDownloadRelativeSymlinkAndSpecialTypes(t *testing.T) {
	dir, root := filesFixture(t)
	fileWrite(t, dir, "ordinary", []byte("real"), 0600)
	require.NoError(t, os.Symlink("ordinary", filepath.Join(dir, "relative")))
	var output bytes.Buffer
	require.NoError(t, streamDownloadAt(DownloadRequest{Path: "/workspace/relative", MaxBytes: 4}, root, &output))
	require.Equal(t, "real", output.String())
	require.NoError(t, os.Symlink("/etc/passwd", filepath.Join(dir, "absolute")))
	require.NoError(t, os.Symlink("../outside", filepath.Join(dir, "escape")))
	require.NoError(t, unix.Mkfifo(filepath.Join(dir, "fifo"), 0600))
	for _, name := range []string{"absolute", "escape", "fifo", ".", "missing"} {
		done := make(chan error, 1)
		go func() {
			done <- streamDownloadAt(DownloadRequest{Path: "/workspace/" + name, MaxBytes: 100}, root, io.Discard)
		}()
		select {
		case err := <-done:
			require.Error(t, err, "name=%s", name)
		case <-time.After(time.Second):
			t.Fatalf("download blocked on nonregular target %s", name)
		}
	}
}

type changingDownloadWriter struct {
	once  func()
	fail  error
	short bool
}

func (w *changingDownloadWriter) Write(data []byte) (int, error) {
	if w.once != nil {
		f := w.once
		w.once = nil
		f()
	}
	if w.fail != nil {
		return 0, w.fail
	}
	if w.short {
		return len(data) - 1, nil
	}
	return len(data), nil
}

// The writer mutates the real still-open source after its first read.
func TestDownloadDetectsSourceGrowthShrinkAndWriteFailure(t *testing.T) {
	for _, scenario := range []string{"grow", "shrink", "write-error", "short-write"} {
		t.Run(scenario, func(t *testing.T) {
			dir, root := filesFixture(t)
			fileWrite(t, dir, "source", bytes.Repeat([]byte("x"), 65536), 0600)
			writer := &changingDownloadWriter{}
			switch scenario {
			case "grow":
				writer.once = func() { require.NoError(t, os.Truncate(filepath.Join(dir, "source"), 65537)) }
			case "shrink":
				writer.once = func() { require.NoError(t, os.Truncate(filepath.Join(dir, "source"), 1)) }
			case "write-error":
				writer.fail = errors.New("private sink failure")
			case "short-write":
				writer.short = true
			}
			err := streamDownloadAt(DownloadRequest{Path: "/workspace/source", MaxBytes: 100000}, root, writer)
			require.Error(t, err)
			require.Equal(t, []byte("{\"code\":\"io_error\"}\n"), EncodeDownloadFailure(err))
		})
	}
}

func TestDownloadFailureStrictCodesAndSanitization(t *testing.T) {
	for _, code := range []string{"invalid_request", "not_found", "limit_exceeded", "unsupported_type", "permission_denied", "io_error"} {
		failure, err := DecodeDownloadFailure([]byte(`{"code":"` + code + `"}` + "\n"))
		require.NoError(t, err)
		require.Equal(t, code, failure.Code)
	}
	for _, wire := range []string{
		`{"code":"conflict"}`, `{"code":"publish_uncertain"}`, `{"code":"unknown"}`,
		`{"code":"not_found","path":"/secret"}`, `{"Code":"not_found"}`, `{"code":null}`,
		`{"code":"not_found","code":"io_error"}`, `{"code":"not_found"}junk`,
		`{"error":{"code":"not_found"}}`, `{}`, `null`, strings.Repeat(" ", 257),
	} {
		_, err := DecodeDownloadFailure([]byte(wire))
		require.Error(t, err, "wire=%q", wire)
	}
	for _, tc := range []struct {
		err  error
		want string
	}{
		{os.ErrNotExist, "not_found"}, {os.ErrPermission, "permission_denied"},
		{errors.New("/secret/private/backend"), "io_error"}, {failureCode("conflict"), "io_error"},
	} {
		require.Equal(t, "{\"code\":\""+tc.want+"\"}\n", string(EncodeDownloadFailure(tc.err)))
	}
}
