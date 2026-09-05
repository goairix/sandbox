package workspacefuse_test

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

var (
	runtimeName = flag.String("runtime", "", "docker or kubernetes")
	profileID   = flag.String("profile", "", "immutable workspace FUSE profile ID")
)

type apiClient struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

func integrationClient(t *testing.T) *apiClient {
	t.Helper()
	baseURL := strings.TrimRight(os.Getenv("WORKSPACE_FUSE_API_URL"), "/")
	apiKey := os.Getenv("WORKSPACE_FUSE_API_KEY")
	if baseURL == "" || apiKey == "" {
		t.Skip("WORKSPACE_FUSE_API_URL and WORKSPACE_FUSE_API_KEY are required")
	}
	if *runtimeName != "docker" && *runtimeName != "kubernetes" {
		t.Fatalf("invalid -runtime %q", *runtimeName)
	}
	if *profileID == "" {
		t.Fatal("-profile is required")
	}
	return &apiClient{baseURL: baseURL, apiKey: apiKey, client: &http.Client{Timeout: 45 * time.Minute}}
}

func (c *apiClient) request(ctx context.Context, method, path string, input, output any) (int, error) {
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		limited, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, fmt.Errorf("%s %s: status=%d body=%s", method, path, resp.StatusCode, strings.TrimSpace(string(limited)))
	}
	if output != nil {
		return resp.StatusCode, json.NewDecoder(resp.Body).Decode(output)
	}
	return resp.StatusCode, nil
}

func (c *apiClient) exec(ctx context.Context, sandboxID, shell string) error {
	var result struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	_, err := c.request(ctx, http.MethodPost, "/api/v1/sandboxes/"+sandboxID+"/exec", map[string]any{
		"language": "bash", "code": shell, "timeout": 1800,
	}, &result)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("exec failed: exit=%d stderr=%s", result.ExitCode, result.Stderr)
	}
	return nil
}

func TestWorkspaceFUSE(t *testing.T) {
	c := integrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	workspacePath := fmt.Sprintf("matrix/%s/%s/%d", *profileID, *runtimeName, time.Now().UnixNano())
	var sandbox struct {
		ID string `json:"id"`
	}
	_, err := c.request(ctx, http.MethodPost, "/api/v1/sandboxes", map[string]any{
		"mode": "persistent", "timeout": -1, "workspace_path": workspacePath,
	}, &sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.ID == "" {
		t.Fatal("create returned an empty sandbox ID")
	}
	destroyed := false
	defer func() {
		if !destroyed {
			_, _ = c.request(context.Background(), http.MethodDelete, "/api/v1/sandboxes/"+sandbox.ID, nil, nil)
		}
	}()

	// Filesystem semantics: new empty prefix, directories, overwrite, append,
	// truncate, rename, delete, local git checkout and a bounded small-file set.
	if err := c.exec(ctx, sandbox.ID, `set -e
	findmnt -rn -o FSTYPE -T /workspace | grep -qx fuse.s3fs
mkdir -p /workspace/tree/a
printf first >/workspace/tree/a/file
printf second >/workspace/tree/a/file
printf '+append' >>/workspace/tree/a/file
test "$(cat /workspace/tree/a/file)" = 'second+append'
truncate -s 3 /workspace/tree/a/file
test "$(cat /workspace/tree/a/file)" = sec
mv /workspace/tree/a/file /workspace/tree/a/renamed
rm /workspace/tree/a/renamed
rmdir /workspace/tree/a
mkdir /workspace/git-source
git -C /workspace/git-source init -q
git -C /workspace/git-source config user.email matrix@example.invalid
git -C /workspace/git-source config user.name matrix
printf tracked >/workspace/git-source/tracked
git -C /workspace/git-source add tracked
git -C /workspace/git-source commit -qm initial
git clone -q /workspace/git-source /workspace/git-checkout
test "$(cat /workspace/git-checkout/tracked)" = tracked
mkdir /workspace/small
i=0; while [ "$i" -lt 10000 ]; do : >"/workspace/small/f-$i"; i=$((i+1)); done
test "$(find /workspace/small -type f | wc -l)" -eq 10000
rm -rf /workspace/tree /workspace/git-source /workspace/git-checkout /workspace/small`); err != nil {
		t.Fatal(err)
	}

	largeSize := int64(1 << 30)
	if override := os.Getenv("WORKSPACE_FUSE_LARGE_UPLOAD_BYTES"); override != "" {
		largeSize, err = strconv.ParseInt(override, 10, 64)
		if err != nil || largeSize <= 0 {
			t.Fatal("WORKSPACE_FUSE_LARGE_UPLOAD_BYTES must be positive")
		}
	}
	if err := uploadSized(ctx, c, sandbox.ID, "/workspace/large.bin", largeSize); err != nil {
		t.Fatal(err)
	}
	if err := c.exec(ctx, sandbox.ID, fmt.Sprintf("test \"$(stat -c %%s /workspace/large.bin)\" -eq %d && rm /workspace/large.bin", largeSize)); err != nil {
		t.Fatal(err)
	}

	// A second owner for the same canonical prefix must be rejected.
	status, conflictErr := c.request(ctx, http.MethodPost, "/api/v1/sandboxes", map[string]any{
		"mode": "persistent", "timeout": -1, "workspace_path": workspacePath,
	}, nil)
	if conflictErr == nil || status != http.StatusConflict {
		t.Fatalf("lease conflict status=%d err=%v", status, conflictErr)
	}

	_, err = c.request(ctx, http.MethodPost, "/api/v1/sandboxes/"+sandbox.ID+"/workspace/sync", map[string]string{"direction": "from_container"}, nil)
	if err != nil {
		t.Fatalf("graceful flush failed: %v", err)
	}
	var workspaceInfo struct {
		Flushed bool `json:"flushed"`
	}
	_, err = c.request(ctx, http.MethodGet, "/api/v1/sandboxes/"+sandbox.ID+"/workspace/info", nil, &workspaceInfo)
	if err != nil || !workspaceInfo.Flushed {
		t.Fatalf("workspace did not report a durable flush: flushed=%v err=%v", workspaceInfo.Flushed, err)
	}
	if _, err := c.request(ctx, http.MethodDelete, "/api/v1/sandboxes/"+sandbox.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	destroyed = true
}

func uploadSized(ctx context.Context, c *apiClient, sandboxID, path string, size int64) error {
	reader, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	go func() {
		part, err := multipartWriter.CreateFormFile("file", "large.bin")
		if err == nil {
			_, err = io.CopyN(part, zeroReader{}, size)
		}
		if closeErr := multipartWriter.Close(); err == nil {
			err = closeErr
		}
		_ = writer.CloseWithError(err)
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/sandboxes/"+sandboxID+"/files/upload?path="+url.QueryEscape(path), reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	req.Header.Set("X-Sandbox-File-Size", strconv.FormatInt(size, 10))
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("large upload status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

type zeroReader struct{}

func (zeroReader) Read(buffer []byte) (int, error) {
	clear(buffer)
	return len(buffer), nil
}

func TestWorkspaceFUSEFaultMatrix(t *testing.T) {
	integrationClient(t)
	driver := os.Getenv("WORKSPACE_FUSE_CHAOS_DRIVER")
	if driver == "" {
		if os.Getenv("WORKSPACE_FUSE_REQUIRE_FAULTS") == "1" {
			t.Fatal("WORKSPACE_FUSE_CHAOS_DRIVER is required for release verification")
		}
		t.Skip("no platform fault-injection driver configured")
	}
	scenarios := []string{
		"endpoint-outage", "s3fs-death", "cache-pressure", "redis-outage", "runtime-restart", "graceful-flush",
		"api-crash-preparing", "api-crash-prepared", "api-crash-reserved", "api-crash-binding", "api-crash-consumed", "api-crash-cleanup",
		"force-delete-without-proof", "cleanup-with-matching-proof",
	}
	for _, scenario := range scenarios {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, driver, *runtimeName, *profileID, scenario)
			cmd.Env = os.Environ()
			output, err := cmd.Output()
			if err != nil {
				t.Fatalf("fault driver failed: %v", err)
			}
			var evidence struct {
				Passed           bool `json:"passed"`
				CleanupConfirmed bool `json:"cleanup_confirmed"`
			}
			if err := json.Unmarshal(bytes.TrimSpace(output), &evidence); err != nil || !evidence.Passed || !evidence.CleanupConfirmed {
				t.Fatalf("invalid fault evidence (details intentionally not logged): %v", err)
			}
		})
	}
}
