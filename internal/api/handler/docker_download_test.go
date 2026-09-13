package handler_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/goairix/sandbox/internal/runtime/docker"
	"github.com/stretchr/testify/require"
)

// This exercises the Docker Engine adapter and the shared public handler
// together against a local fake API, without contacting a daemon or cluster.
func TestDockerDownloadMissingFileAndEmptyFileThroughPublicHandler(t *testing.T) {
	runDockerPublicDownloadContract(t, false)
}

func TestDockerFUSEDownloadMissingFileAndEmptyFileThroughPublicHandler(t *testing.T) {
	runDockerPublicDownloadContract(t, true)
}

func runDockerPublicDownloadContract(t *testing.T, fuse bool) {
	t.Helper()
	var execMu sync.Mutex
	execs := make(map[string]container.ExecOptions)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.URL.Path == "/_ping":
			w.Header().Set("API-Version", "1.47")
			_, _ = w.Write([]byte("OK"))
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/networks"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{"Id": "isolated-id", "Name": "sandbox-isolated"}, {"Id": "open-id", "Name": "sandbox-open"}})
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/containers/json"):
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/networks/isolated-id"):
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "isolated-id", "Name": "sandbox-isolated"})
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/containers/create"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "runtime-a"})
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/containers/runtime-a/exec"):
			var opts container.ExecOptions
			if err := json.NewDecoder(req.Body).Decode(&opts); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if opts.User != "1000:1000" {
				t.Errorf("FUSE file exec user = %q", opts.User)
			}
			execMu.Lock()
			id := fmt.Sprintf("exec-%d", len(execs)+1)
			execs[id] = opts
			execMu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": id})
		case req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/exec/") && strings.HasSuffix(req.URL.Path, "/start"):
			if _, err := io.Copy(io.Discard, req.Body); err != nil {
				t.Error(err)
				return
			}
			parts := strings.Split(req.URL.Path, "/")
			execMu.Lock()
			opts := execs[parts[len(parts)-2]]
			execMu.Unlock()
			var stdout, stderr bytes.Buffer
			if len(opts.Cmd) == 3 && opts.Cmd[0] == "sh" {
				if strings.Contains(opts.Cmd[2], "missing.txt") {
					stderr.WriteString("stat: cannot statx '/workspace/missing.txt': No such file or directory\n")
				} else {
					stdout.WriteString("regular empty file\n")
				}
			} else {
				tw := tar.NewWriter(&stdout)
				if err := tw.WriteHeader(&tar.Header{Name: "empty.txt", Mode: 0o644}); err != nil {
					t.Error(err)
				}
				if err := tw.Close(); err != nil {
					t.Error(err)
				}
			}
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = conn.Close() }()
			if _, err := rw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"); err != nil {
				t.Error(err)
				return
			}
			if _, err := stdcopy.NewStdWriter(rw, stdcopy.Stdout).Write(stdout.Bytes()); err != nil {
				t.Error(err)
			}
			if _, err := stdcopy.NewStdWriter(rw, stdcopy.Stderr).Write(stderr.Bytes()); err != nil {
				t.Error(err)
			}
			if err := rw.Flush(); err != nil {
				t.Error(err)
			}
		case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/exec/") && strings.HasSuffix(req.URL.Path, "/json"):
			parts := strings.Split(req.URL.Path, "/")
			execMu.Lock()
			opts := execs[parts[len(parts)-2]]
			execMu.Unlock()
			code := 0
			if len(opts.Cmd) == 3 && opts.Cmd[0] == "sh" && strings.Contains(opts.Cmd[2], "missing.txt") {
				code = 1
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Running": false, "ExitCode": code})
		case req.Method == http.MethodPost && (strings.HasSuffix(req.URL.Path, "/start") || strings.HasSuffix(req.URL.Path, "/rename")):
			w.WriteHeader(http.StatusNoContent)
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/containers/runtime-a/json"):
			labels := make(map[string]string)
			if fuse {
				labels["sandbox.managed"], labels["sandbox.role"] = "true", "fuse-runtime"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "runtime-a", "Config": map[string]any{"User": "1000", "Labels": labels}, "State": map[string]any{"Running": true}})
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/containers/runtime-a/archive"):
			if req.URL.Query().Get("path") == "/workspace/missing.txt" {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "Could not find the file /workspace/missing.txt in container runtime-a"})
				return
			}
			w.Header().Set("Content-Type", "application/x-tar")
			w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString([]byte(`{"name":"empty.txt","size":0,"mode":420}`)))
			tw := tar.NewWriter(w)
			if err := tw.WriteHeader(&tar.Header{Name: "empty.txt", Mode: 0o644}); err != nil {
				t.Error(err)
			}
			if err := tw.Close(); err != nil {
				t.Error(err)
			}
		default:
			http.Error(w, "unexpected fake Docker request: "+req.Method+" "+req.URL.Path, http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	rt, err := docker.New(context.Background(), strings.Replace(server.URL, "http://", "tcp://", 1), "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	router, id := downloadTestRouter(t, rt)
	for _, tc := range []struct {
		name   string
		path   string
		status int
	}{
		{name: "missing", path: "/workspace/missing.txt", status: http.StatusNotFound},
		{name: "empty", path: "/workspace/empty.txt", status: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/sandboxes/"+id+"/files/download?path="+tc.path, nil))
			require.Equal(t, tc.status, recorder.Code, recorder.Body.String())
			if tc.status == http.StatusNotFound {
				require.Contains(t, recorder.Body.String(), "FILE_NOT_FOUND")
				require.Empty(t, recorder.Header().Get("Content-Disposition"))
			} else {
				require.Empty(t, recorder.Body.String())
			}
		})
	}
}
