package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goairix/sandbox/pkg/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Explicit opt-in: callers supply three per-replica endpoints/port-forwards.
// This suite never changes a Deployment, Helm release, pool, or Redis data.
type apiSuite struct {
	t      *testing.T
	ctx    context.Context
	client *http.Client
	urls   []string
	key    string
}

func (s *apiSuite) request(replica int, method, path string, payload any, expected int) []byte {
	s.t.Helper()
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		require.NoError(s.t, err)
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(s.ctx, method, s.urls[replica]+"/api/v1"+path, body)
	require.NoError(s.t, err)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.key != "" {
		req.Header.Set("Authorization", "Bearer "+s.key)
	}
	response, err := s.client.Do(req)
	require.NoError(s.t, err)
	defer func() { require.NoError(s.t, response.Body.Close()) }()
	data, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	require.NoError(s.t, err, "HTTP body must terminate cleanly")
	require.LessOrEqual(s.t, len(data), 2<<20)
	require.Equal(s.t, expected, response.StatusCode, "request %s %s: %s", method, path, data)
	return data
}

func (s *apiSuite) create(cfg types.CreateSandboxRequest) types.SandboxResponse {
	s.t.Helper()
	var sandbox types.SandboxResponse
	require.NoError(s.t, json.Unmarshal(s.request(0, http.MethodPost, "/sandboxes", cfg, http.StatusCreated), &sandbox))
	require.NotEmpty(s.t, sandbox.ID)
	s.t.Cleanup(func() {
		// Independent bounded cleanup still runs if the test/request was canceled.
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		for attempt := range 3 {
			req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.urls[attempt%len(s.urls)]+"/api/v1/sandboxes/"+sandbox.ID, nil)
			if err != nil {
				s.t.Errorf("cleanup: %v", err)
				return
			}
			if s.key != "" {
				req.Header.Set("Authorization", "Bearer "+s.key)
			}
			response, err := s.client.Do(req)
			if err != nil {
				continue
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK || response.StatusCode == http.StatusNoContent || response.StatusCode == http.StatusNotFound {
				return
			}
		}
		s.t.Errorf("cleanup remains pending for this test sandbox: %s", sandbox.ID)
	})
	return sandbox
}

func (s *apiSuite) exec(replica int, id, language, code, stdin string) types.ExecResponse {
	s.t.Helper()
	var result types.ExecResponse
	data := s.request(replica, http.MethodPost, "/sandboxes/"+id+"/exec", types.ExecRequest{Language: language, Code: code, Stdin: stdin, Timeout: 10}, http.StatusOK)
	require.NoError(s.t, json.Unmarshal(data, &result))
	require.Zero(s.t, result.ExitCode, "stderr: %s", result.Stderr)
	return result
}

func (s *apiSuite) info(replica int, id string) types.WorkspaceInfoResponse {
	s.t.Helper()
	var info types.WorkspaceInfoResponse
	require.NoError(s.t, json.Unmarshal(s.request(replica, http.MethodGet, "/sandboxes/"+id+"/workspace/info", nil, http.StatusOK), &info))
	return info
}

func TestDeployedAPIRemediation(t *testing.T) {
	raw := os.Getenv("SANDBOX_API_TEST_URLS")
	if raw == "" {
		t.Skip("opt-in deployed suite: SANDBOX_API_TEST_URLS")
	}
	urls := strings.Split(raw, ",")
	require.GreaterOrEqual(t, len(urls), 3, "supply three direct per-replica URLs, not three copies of a load-balanced URL")
	for i := range urls {
		urls[i] = strings.TrimRight(strings.TrimSpace(urls[i]), "/")
		parsed, err := url.Parse(urls[i])
		require.NoError(t, err)
		require.Contains(t, []string{"http", "https"}, parsed.Scheme)
		require.NotEmpty(t, parsed.Host)
		require.Empty(t, parsed.User, "credentials belong in SANDBOX_API_TEST_KEY")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	s := &apiSuite{t: t, ctx: ctx, client: &http.Client{Timeout: 45 * time.Second}, urls: urls, key: os.Getenv("SANDBOX_API_TEST_KEY")}
	root := "validation/remediation/" + uuid.NewString()
	sb := s.create(types.CreateSandboxRequest{Mode: "persistent", Timeout: 600})
	for i := range 3 {
		require.Equal(t, "hello\n", s.exec(i, sb.ID, "python", "import sys; print(sys.stdin.read().strip())", "hello\n").Stdout)
		require.Equal(t, "hello", s.exec(i, sb.ID, "nodejs", "process.stdout.write(require('fs').readFileSync(0,'utf8'))", "hello").Stdout)
		errorBody := s.request(i, http.MethodGet, "/sandboxes/"+sb.ID+"/files/download?path=/workspace/no-such-file-"+uuid.NewString(), nil, http.StatusNotFound)
		require.Contains(t, string(errorBody), "FILE_NOT_FOUND")
	}
	s.request(1, http.MethodPost, "/sandboxes/"+sb.ID+"/workspace/mount", types.MountWorkspaceRequest{RootPath: root + "/dynamic"}, http.StatusOK)
	for i := range 3 {
		require.True(t, s.info(i, sb.ID).Mounted)
	}
	s.exec(2, sb.ID, "bash", "printf test-content > /workspace/validation.txt; : > /workspace/empty.txt", "")
	require.Empty(t, s.request(0, http.MethodGet, "/sandboxes/"+sb.ID+"/files/download?path=/workspace/empty.txt", nil, http.StatusOK))
	s.request(0, http.MethodPost, "/sandboxes/"+sb.ID+"/workspace/sync", types.SyncWorkspaceRequest{Direction: "from_container"}, http.StatusOK)
	s.exec(1, sb.ID, "bash", "rm -f /workspace/validation.txt", "")
	s.request(2, http.MethodPost, "/sandboxes/"+sb.ID+"/workspace/sync", types.SyncWorkspaceRequest{Direction: "to_container"}, http.StatusOK)
	require.Equal(t, "test-content", s.exec(0, sb.ID, "bash", "cat /workspace/validation.txt", "").Stdout)
	s.exec(1, sb.ID, "bash", "rm -f /workspace/validation.txt /workspace/empty.txt", "")
	s.request(2, http.MethodPost, "/sandboxes/"+sb.ID+"/workspace/unmount", nil, http.StatusOK)
	for i := range 3 {
		require.False(t, s.info(i, sb.ID).Mounted)
	}
	limited := s.create(types.CreateSandboxRequest{Mode: "ephemeral", Timeout: 300, Resources: &types.ResourceLimits{Memory: "128Mi", CPU: "100m"}})
	actual := s.exec(1, limited.ID, "bash", "cat /sys/fs/cgroup/memory.max; cat /sys/fs/cgroup/cpu.max", "").Stdout
	fields := strings.Fields(actual)
	require.Len(t, fields, 3, "requires cgroupv2 resource verification")
	require.Equal(t, "134217728", fields[0])
	quota, err := strconv.ParseFloat(fields[1], 64)
	require.NoError(t, err)
	period, err := strconv.ParseFloat(fields[2], 64)
	require.NoError(t, err)
	require.InDelta(t, .1, quota/period, .001)
	if target := os.Getenv("SANDBOX_API_INTERNAL_TARGET"); target != "" {
		internalURL := os.Getenv("SANDBOX_API_INTERNAL_URL")
		require.NotEmpty(t, internalURL, "provide a known backend URL, preferably ServiceIP to avoid DNS dependency")
		s.request(1, http.MethodPut, "/sandboxes/"+sb.ID+"/network", types.UpdateNetworkRequest{Enabled: true, Whitelist: []string{target}}, http.StatusOK)
		for i := range 3 {
			s.exec(i, sb.ID, "python", fmt.Sprintf("import urllib.request; r=urllib.request.urlopen(%q, timeout=5); print(r.status)", internalURL), "")
		}
		if deniedURL := os.Getenv("SANDBOX_API_DENIED_URL"); deniedURL != "" {
			result := s.exec(2, sb.ID, "python", fmt.Sprintf("import urllib.request\ntry:\n urllib.request.urlopen(%q,timeout=5)\n print('UNEXPECTED_ALLOWED')\nexcept Exception:\n print('BLOCKED')", deniedURL), "")
			require.Contains(t, result.Stdout, "BLOCKED")
			require.NotContains(t, result.Stdout, "UNEXPECTED_ALLOWED")
		}
	}
	for i := range 3 {
		stream := s.request(i, http.MethodPost, "/execute/stream", types.ExecuteRequest{Language: "python", Code: "import sys; print(sys.stdin.read())", Stdin: "hello", Timeout: 10}, http.StatusOK)
		require.Contains(t, string(stream), "event: done")
		require.NotContains(t, string(stream), "event: error")
	}
	if os.Getenv("SANDBOX_API_TEST_FUSE") == "1" {
		n := 3
		if raw := os.Getenv("SANDBOX_API_FUSE_SAMPLES"); raw != "" {
			var err error
			n, err = strconv.Atoi(raw)
			require.NoError(t, err)
		}
		require.GreaterOrEqual(t, n, 1)
		require.LessOrEqual(t, n, 100)
		var durations []time.Duration
		for sample := range n {
			started := time.Now()
			fuse := s.create(types.CreateSandboxRequest{Mode: "persistent", Timeout: 120, WorkspacePath: fmt.Sprintf("%s/fuse-%d", root, sample), WorkspaceMountMode: types.WorkspaceMountFUSE})
			durations = append(durations, time.Since(started))
			s.exec(1, fuse.ID, "bash", "printf fuse-content > /workspace/validation.txt", "")
			s.request(2, http.MethodPost, "/sandboxes/"+fuse.ID+"/workspace/sync", types.SyncWorkspaceRequest{Direction: "from_container"}, http.StatusOK)
			for i := range 3 {
				info := s.info(i, fuse.ID)
				require.True(t, info.Flushed)
				require.NotNil(t, info.LastFlushedAt)
				require.False(t, info.LastSyncedAt.IsZero())
			}
			s.request(1, http.MethodPost, "/sandboxes/"+fuse.ID+"/workspace/unmount", nil, http.StatusConflict)
			s.exec(0, fuse.ID, "bash", "rm -f /workspace/validation.txt", "")
			s.request(2, http.MethodDelete, "/sandboxes/"+fuse.ID, nil, http.StatusOK)
		}
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		percentile := func(p int) time.Duration { index := (len(durations)*p+99)/100 - 1; return durations[index] }
		t.Logf("FUSE create n=%d p50=%s p95=%s p99=%s; sample successes only, failures fail the suite", n, percentile(50), percentile(95), percentile(99))
	}
}
