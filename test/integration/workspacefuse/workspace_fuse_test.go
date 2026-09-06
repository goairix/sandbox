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
	_, err := c.execOutput(ctx, sandboxID, shell)
	return err
}

func (c *apiClient) execOutput(ctx context.Context, sandboxID, shell string) (string, error) {
	var result struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	_, err := c.request(ctx, http.MethodPost, "/api/v1/sandboxes/"+sandboxID+"/exec", map[string]any{
		"language": "bash", "code": shell, "timeout": 110,
	}, &result)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("exec failed: exit=%d stderr=%s", result.ExitCode, result.Stderr)
	}
	return result.Stdout, nil
}

type sandboxRef struct {
	ID                 string `json:"id"`
	WorkspaceMountMode string `json:"workspace_mount_mode"`
}

var requiredAPIPaths = []string{
	"ephemeral_no_workspace",
	"ephemeral_sync_workspace",
	"persistent_sync_workspace",
	"ephemeral_fuse_workspace",
	"persistent_fuse_workspace",
	"same_prefix_sync_fuse_conflict",
	"different_prefix_sync_fuse_concurrency",
}

func createSandbox(ctx context.Context, c *apiClient, mode, workspacePath, mountMode string) (sandboxRef, int, error) {
	input := map[string]any{"mode": mode, "timeout": -1}
	if workspacePath != "" {
		input["workspace_path"] = workspacePath
	}
	// Sync is intentionally omitted: it verifies the release default remains
	// backward-compatible. FUSE must always be an explicit request choice.
	if mountMode == "fuse" {
		input["workspace_mount_mode"] = "fuse"
	}
	var result sandboxRef
	status, err := c.request(ctx, http.MethodPost, "/api/v1/sandboxes", input, &result)
	if err == nil && workspacePath != "" {
		expected := mountMode
		if expected == "" {
			expected = "sync"
		}
		if result.WorkspaceMountMode != expected {
			return result, status, fmt.Errorf("create selected mount mode %q, want %q", result.WorkspaceMountMode, expected)
		}
	}
	return result, status, err
}

func destroySandbox(ctx context.Context, c *apiClient, sandboxID string) error {
	if sandboxID == "" {
		return nil
	}
	_, err := c.request(ctx, http.MethodDelete, "/api/v1/sandboxes/"+sandboxID, nil, nil)
	return err
}

func flushWorkspace(ctx context.Context, c *apiClient, sandboxID string) error {
	_, err := c.request(ctx, http.MethodPost, "/api/v1/sandboxes/"+sandboxID+"/workspace/sync", map[string]string{"direction": "from_container"}, nil)
	return err
}

func verifyPersisted(ctx context.Context, c *apiClient, workspacePath, mountMode, expected string) error {
	sandbox, _, err := createSandbox(ctx, c, "persistent", workspacePath, mountMode)
	if err != nil {
		return err
	}
	defer func() { _ = destroySandbox(context.Background(), c, sandbox.ID) }()
	stdout, err := c.execOutput(ctx, sandbox.ID, "cat /workspace/lifecycle-value")
	if err != nil {
		return err
	}
	if strings.TrimSpace(stdout) != expected {
		return fmt.Errorf("persisted workspace content=%q, want %q", strings.TrimSpace(stdout), expected)
	}
	return destroySandbox(ctx, c, sandbox.ID)
}

func presetForProfile(profile string) string {
	switch profile {
	case "minio-sigv4-path-style-v1":
		return "minio"
	case "huawei-obs-public-v1":
		return "huawei-obs-public"
	case "huawei-obs-private-2023-v1":
		return "huawei-obs-private"
	default:
		return ""
	}
}

func objectCounterSnapshot(ctx context.Context, prefix string) (string, bool, error) {
	command := os.Getenv("WORKSPACE_FUSE_OBJECT_COUNTER_CMD")
	if command == "" {
		if os.Getenv("WORKSPACE_FUSE_REQUIRE_OBJECT_COUNTER") == "1" {
			return "", false, fmt.Errorf("WORKSPACE_FUSE_OBJECT_COUNTER_CMD is required")
		}
		return "", false, nil
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Env = append(os.Environ(), "WORKSPACE_FUSE_COUNTER_PREFIX="+prefix)
	output, err := cmd.Output()
	if err != nil {
		return "", true, fmt.Errorf("object counter command failed")
	}
	return strings.TrimSpace(string(output)), true, nil
}

func writeAPIEvidence(t *testing.T, results map[string]bool) {
	t.Helper()
	output := os.Getenv("WORKSPACE_FUSE_EVIDENCE_OUTPUT")
	if output == "" {
		return
	}
	for _, path := range requiredAPIPaths {
		if !results[path] {
			t.Fatalf("cannot record incomplete API evidence: %s did not pass", path)
		}
	}
	evidence := struct {
		SchemaVersion      int             `json:"schema_version"`
		Passed             bool            `json:"passed"`
		Preset             string          `json:"preset"`
		ProfileID          string          `json:"profile_id"`
		Runtime            string          `json:"runtime"`
		MounterImageDigest string          `json:"mounter_image_digest"`
		DockerImageDigest  string          `json:"docker_image_digest"`
		APIPaths           map[string]bool `json:"api_paths"`
	}{
		SchemaVersion: 1, Passed: true, Preset: presetForProfile(*profileID), ProfileID: *profileID,
		Runtime: *runtimeName, MounterImageDigest: os.Getenv("WORKSPACE_FUSE_MOUNTER_IMAGE_DIGEST"),
		DockerImageDigest: os.Getenv("WORKSPACE_FUSE_DOCKER_IMAGE_DIGEST"), APIPaths: results,
	}
	if evidence.Preset == "" || evidence.MounterImageDigest == "" || evidence.DockerImageDigest == "" {
		t.Fatal("evidence output requires preset and both common image digests")
	}
	raw, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(output, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceFUSE(t *testing.T) {
	c := integrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	runRoot := fmt.Sprintf("matrix/%s/%s/%d", *profileID, *runtimeName, time.Now().UnixNano())
	results := make(map[string]bool, len(requiredAPIPaths))

	results["ephemeral_no_workspace"] = t.Run("ephemeral no workspace", func(t *testing.T) {
		counterPrefix := runRoot + "/no-workspace-counter"
		before, counterEnabled, err := objectCounterSnapshot(ctx, counterPrefix)
		if err != nil {
			t.Fatal(err)
		}
		sandbox, _, err := createSandbox(ctx, c, "ephemeral", "", "")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = destroySandbox(context.Background(), c, sandbox.ID) }()
		var info struct {
			Mounted bool `json:"mounted"`
		}
		if _, err := c.request(ctx, http.MethodGet, "/api/v1/sandboxes/"+sandbox.ID+"/workspace/info", nil, &info); err != nil {
			t.Fatal(err)
		}
		if info.Mounted {
			t.Fatal("workspace-less ephemeral sandbox unexpectedly reports a mount")
		}
		if err := c.exec(ctx, sandbox.ID, "printf transient >/workspace/no-workspace-value"); err != nil {
			t.Fatal(err)
		}
		if err := destroySandbox(ctx, c, sandbox.ID); err != nil {
			t.Fatal(err)
		}
		after, afterEnabled, err := objectCounterSnapshot(ctx, counterPrefix)
		if err != nil {
			t.Fatal(err)
		}
		if counterEnabled != afterEnabled || (counterEnabled && before != after) {
			t.Fatal("workspace-less execution changed object-store marker/list counters")
		}
	})

	runPersistence := func(t *testing.T, mode, mountMode, path, content string, manualFlush bool) {
		sandbox, _, err := createSandbox(ctx, c, mode, path, mountMode)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = destroySandbox(context.Background(), c, sandbox.ID) }()
		if err := c.exec(ctx, sandbox.ID, "printf '"+content+"' >/workspace/lifecycle-value"); err != nil {
			t.Fatal(err)
		}
		if manualFlush {
			if err := flushWorkspace(ctx, c, sandbox.ID); err != nil {
				t.Fatal(err)
			}
		}
		if err := destroySandbox(ctx, c, sandbox.ID); err != nil {
			t.Fatal(err)
		}
		if err := verifyPersisted(ctx, c, path, mountMode, content); err != nil {
			t.Fatal(err)
		}
	}

	results["ephemeral_sync_workspace"] = t.Run("ephemeral sync finalization", func(t *testing.T) {
		runPersistence(t, "ephemeral", "", runRoot+"/ephemeral-sync", "ephemeral-sync", false)
	})
	results["persistent_sync_workspace"] = t.Run("persistent sync manual sync", func(t *testing.T) {
		runPersistence(t, "persistent", "", runRoot+"/persistent-sync", "persistent-sync", true)
	})
	results["ephemeral_fuse_workspace"] = t.Run("ephemeral FUSE finalization", func(t *testing.T) {
		runPersistence(t, "ephemeral", "fuse", runRoot+"/ephemeral-fuse", "ephemeral-fuse", false)
	})
	results["persistent_fuse_workspace"] = t.Run("persistent FUSE durable flush", func(t *testing.T) {
		runPersistence(t, "persistent", "fuse", runRoot+"/persistent-fuse", "persistent-fuse", true)
	})

	results["same_prefix_sync_fuse_conflict"] = t.Run("same prefix cross-mode conflict both orders", func(t *testing.T) {
		path := runRoot + "/conflict"
		first, _, err := createSandbox(ctx, c, "persistent", path, "")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = destroySandbox(context.Background(), c, first.ID) }()
		if _, status, err := createSandbox(ctx, c, "persistent", path, "fuse"); err == nil || status != http.StatusConflict {
			t.Fatalf("sync then FUSE conflict status=%d err=%v", status, err)
		}
		if err := destroySandbox(ctx, c, first.ID); err != nil {
			t.Fatal(err)
		}
		second, _, err := createSandbox(ctx, c, "persistent", path, "fuse")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = destroySandbox(context.Background(), c, second.ID) }()
		if _, status, err := createSandbox(ctx, c, "persistent", path, ""); err == nil || status != http.StatusConflict {
			t.Fatalf("FUSE then sync conflict status=%d err=%v", status, err)
		}
		if err := destroySandbox(ctx, c, second.ID); err != nil {
			t.Fatal(err)
		}
	})

	results["different_prefix_sync_fuse_concurrency"] = t.Run("different prefix cross-mode concurrency", func(t *testing.T) {
		syncSandbox, _, err := createSandbox(ctx, c, "persistent", runRoot+"/concurrent-sync", "")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = destroySandbox(context.Background(), c, syncSandbox.ID) }()
		fuseSandbox, _, err := createSandbox(ctx, c, "persistent", runRoot+"/concurrent-fuse", "fuse")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = destroySandbox(context.Background(), c, fuseSandbox.ID) }()
		if err := c.exec(ctx, syncSandbox.ID, "printf sync >/workspace/concurrent"); err != nil {
			t.Fatal(err)
		}
		if err := c.exec(ctx, fuseSandbox.ID, "printf fuse >/workspace/concurrent"); err != nil {
			t.Fatal(err)
		}
	})

	if os.Getenv("WORKSPACE_FUSE_SKIP_STRESS") != "1" {
		t.Run("FUSE filesystem stress", func(t *testing.T) {
			runFUSEStress(t, c, ctx, runRoot+"/stress")
		})
	}
	if !t.Failed() {
		writeAPIEvidence(t, results)
	}
}

func runFUSEStress(t *testing.T, c *apiClient, ctx context.Context, workspacePath string) {
	t.Helper()
	var sandbox struct {
		ID string `json:"id"`
	}
	_, err := c.request(ctx, http.MethodPost, "/api/v1/sandboxes", map[string]any{
		"mode": "persistent", "timeout": -1, "workspace_path": workspacePath, "workspace_mount_mode": "fuse",
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
	smallFileCount := int64(10000)
	if override := os.Getenv("WORKSPACE_FUSE_SMALL_FILE_COUNT"); override != "" {
		smallFileCount, err = strconv.ParseInt(override, 10, 64)
		if err != nil || smallFileCount <= 0 {
			t.Fatal("WORKSPACE_FUSE_SMALL_FILE_COUNT must be positive")
		}
	}
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
	rm -rf /workspace/tree /workspace/git-source /workspace/git-checkout`); err != nil {
		t.Fatal(err)
	}
	const smallFileBatchSize int64 = 100
	for start := int64(0); start < smallFileCount; start += smallFileBatchSize {
		end := min(start+smallFileBatchSize, smallFileCount)
		if err := c.exec(ctx, sandbox.ID, fmt.Sprintf(`set -e
	mkdir -p /workspace/small
	i=%d; while [ "$i" -lt %d ]; do : >"/workspace/small/f-$i"; i=$((i+1)); done
	test -f /workspace/small/f-%d`, start, end, end-1)); err != nil {
			t.Fatal(err)
		}
	}
	for start := int64(0); start < smallFileCount; start += smallFileBatchSize {
		end := min(start+smallFileBatchSize, smallFileCount)
		if err := c.exec(ctx, sandbox.ID, fmt.Sprintf(`set -e
	i=%d; while [ "$i" -lt %d ]; do rm -f "/workspace/small/f-$i"; i=$((i+1)); done
	test ! -e /workspace/small/f-%d`, start, end, end-1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.exec(ctx, sandbox.ID, `rmdir /workspace/small`); err != nil {
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
