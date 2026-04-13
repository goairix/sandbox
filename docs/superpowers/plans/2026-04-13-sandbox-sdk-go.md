# Sandbox SDK (Go) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为 Sandbox 服务实现独立的 Go SDK，封装全部 HTTP API，提供底层 1:1 Client 和高层 Sandbox 对象两层接口。

**Architecture:** SDK 作为独立 Go module（`github.com/goairix/sandbox-sdk-go`），不依赖服务端代码，自定义镜像服务端 `pkg/types` 的类型。底层 `Client` 1:1 映射所有 HTTP 端点，高层 `Sandbox` 对象封装常见工作流。Streaming 方法（ExecStream/ExecuteStream）后续迭代，本计划暂跳过。

**Tech Stack:** Go 1.21+, `net/http`, `encoding/json`, functional options pattern, `errors.Is/As`

---

## File Structure

```
sdk/go/
├── go.mod              # module: github.com/goairix/sandbox-sdk-go
├── types.go            # SDK 专用类型（镜像 pkg/types，不依赖服务端）
├── errors.go           # SandboxError + 预定义错误变量
├── client.go           # 底层 Client，1:1 映射所有同步 HTTP 端点
├── sandbox.go          # 高层 Sandbox 对象
├── types_test.go       # types 单元测试（零值、常量）
├── errors_test.go      # errors.Is / errors.As 行为测试
├── client_test.go      # Client 方法测试（httptest.Server）
└── sandbox_test.go     # Sandbox 高层接口测试
sdk/python/
└── .gitkeep
```

---

## Task 1: 初始化 Go module

**Files:**
- Create: `sdk/go/go.mod`
- Create: `sdk/python/.gitkeep`

- [ ] **Step 1: 创建目录并初始化 module**

```bash
mkdir -p sdk/go sdk/python
touch sdk/python/.gitkeep
cd sdk/go
go mod init github.com/goairix/sandbox-sdk-go
```

`sdk/go/go.mod` 内容：

```
module github.com/goairix/sandbox-sdk-go

go 1.21
```

- [ ] **Step 2: 验证 module 文件正确**

```bash
cat sdk/go/go.mod
```

Expected: 显示 module 名和 go 版本，无报错。

- [ ] **Step 3: Commit**

```bash
git add sdk/
git commit -m "chore(sdk): init go module github.com/goairix/sandbox-sdk-go"
```

---

## Task 2: types.go — SDK 类型定义

**Files:**
- Create: `sdk/go/types.go`
- Create: `sdk/go/types_test.go`

- [ ] **Step 1: 写失败测试**

`sdk/go/types_test.go`:

```go
package sandbox_test

import (
	"testing"

	sandbox "github.com/goairix/sandbox-sdk-go"
)

func TestModeConstants(t *testing.T) {
	if sandbox.ModeEphemeral != "ephemeral" {
		t.Errorf("ModeEphemeral = %q, want %q", sandbox.ModeEphemeral, "ephemeral")
	}
	if sandbox.ModePersistent != "persistent" {
		t.Errorf("ModePersistent = %q, want %q", sandbox.ModePersistent, "persistent")
	}
	// compile-time type identity check
	var _ sandbox.Mode = sandbox.ModeEphemeral
	var _ sandbox.Mode = sandbox.ModePersistent
}

func TestCreateSandboxRequestDefaults(t *testing.T) {
	req := sandbox.CreateSandboxRequest{}
	if req.Mode != "" {
		t.Errorf("zero Mode should be empty string, got %q", req.Mode)
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

```bash
cd sdk/go && go test ./... -run TestModeConstants
```

Expected: FAIL — `package not found` 或 `undefined`

- [ ] **Step 3: 实现 types.go**

`sdk/go/types.go`:

```go
// Package sandbox provides a Go SDK for the Sandbox execution service.
package sandbox

import "time"

// Mode represents the sandbox lifecycle mode.
type Mode string

const (
	ModeEphemeral  Mode = "ephemeral"
	ModePersistent Mode = "persistent"
)

// ResourceLimits specifies resource constraints for a sandbox.
type ResourceLimits struct {
	Memory string `json:"memory,omitempty"`
	CPU    string `json:"cpu,omitempty"`
	Disk   string `json:"disk,omitempty"`
}

// NetworkConfig controls network access for a sandbox.
type NetworkConfig struct {
	Enabled   bool     `json:"enabled"`
	Whitelist []string `json:"whitelist,omitempty"`
}

// DependencySpec describes a single package dependency.
// Manager must be "pip" or "npm".
type DependencySpec struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Manager string `json:"manager"` // "pip" | "npm"
}

// CreateSandboxRequest is the request body for POST /api/v1/sandboxes.
// Mode is required; use ModeEphemeral or ModePersistent.
type CreateSandboxRequest struct {
	Mode          Mode             `json:"mode"`
	Timeout       int              `json:"timeout,omitempty"`
	Resources     *ResourceLimits  `json:"resources,omitempty"`
	Network       *NetworkConfig   `json:"network,omitempty"`
	Dependencies  []DependencySpec `json:"dependencies,omitempty"`
	WorkspacePath string           `json:"workspace_path,omitempty"`
}

// SandboxResponse is returned by sandbox lifecycle endpoints.
type SandboxResponse struct {
	ID        string    `json:"id"`
	Mode      Mode      `json:"mode"`
	State     string    `json:"state"`
	RuntimeID string    `json:"runtime_id"`
	CreatedAt time.Time `json:"created_at"`
}

// UpdateNetworkRequest is the request body for PUT /api/v1/sandboxes/:id/network.
type UpdateNetworkRequest struct {
	Enabled   bool     `json:"enabled"`
	Whitelist []string `json:"whitelist,omitempty"`
}

// UpdateNetworkResponse is returned by the update network endpoint.
type UpdateNetworkResponse struct {
	Enabled   bool     `json:"enabled"`
	Whitelist []string `json:"whitelist"`
}

// ExecRequest is the request body for POST /api/v1/sandboxes/:id/exec.
type ExecRequest struct {
	Language string            `json:"language"`
	Code     string            `json:"code"`
	Stdin    string            `json:"stdin,omitempty"`
	Timeout  int               `json:"timeout,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
}

// ExecResponse is returned by the exec endpoint.
// Duration is in seconds (float64).
type ExecResponse struct {
	ExitCode int     `json:"exit_code"`
	Stdout   string  `json:"stdout"`
	Stderr   string  `json:"stderr"`
	Duration float64 `json:"duration"`
}

// ExecuteRequest is the request body for POST /api/v1/execute (one-shot).
type ExecuteRequest struct {
	Language     string            `json:"language"`
	Code         string            `json:"code"`
	Stdin        string            `json:"stdin,omitempty"`
	Timeout      int               `json:"timeout,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	Resources    *ResourceLimits   `json:"resources,omitempty"`
	Network      *NetworkConfig    `json:"network,omitempty"`
	Dependencies []DependencySpec  `json:"dependencies,omitempty"`
}

// FileInfo describes a file or directory entry.
type FileInfo struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	IsDir   bool      `json:"is_dir"`
	ModTime time.Time `json:"mod_time"`
}

// FileListResponse is returned by the list files endpoint.
type FileListResponse struct {
	Files []FileInfo `json:"files"`
	Path  string     `json:"path"`
}

// FileUploadResponse is returned by the upload file endpoint.
type FileUploadResponse struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// MountWorkspaceRequest is the request body for POST /api/v1/sandboxes/:id/workspace/mount.
type MountWorkspaceRequest struct {
	RootPath string `json:"root_path"`
}

// MountWorkspaceResponse is returned by the mount workspace endpoint.
type MountWorkspaceResponse struct {
	RootPath  string    `json:"root_path"`
	MountedAt time.Time `json:"mounted_at"`
}

// SyncWorkspaceRequest is the request body for POST /api/v1/sandboxes/:id/workspace/sync.
type SyncWorkspaceRequest struct {
	Direction string   `json:"direction"` // "to_container" | "from_container"
	Exclude   []string `json:"exclude,omitempty"`
}

// SyncWorkspaceResponse is returned by the sync workspace endpoint.
type SyncWorkspaceResponse struct {
	Direction string `json:"direction"`
	Message   string `json:"message"`
}

// WorkspaceInfoResponse is returned by the workspace info endpoint.
type WorkspaceInfoResponse struct {
	Mounted      bool      `json:"mounted"`
	RootPath     string    `json:"root_path,omitempty"`
	MountedAt    time.Time `json:"mounted_at,omitempty"`
	LastSyncedAt time.Time `json:"last_synced_at,omitempty"`
}

// NOTE: SSEEvent and streaming sub-types (SSEStdoutData, SSEStderrData, etc.)
// are intentionally omitted here. They will be added in the streaming iteration
// alongside ExecStream / ExecuteStream on Client.
```

- [ ] **Step 4: 运行测试，确认通过**

```bash
cd sdk/go && go test ./... -run TestModeConstants -v
```

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add sdk/go/types.go sdk/go/types_test.go
git commit -m "feat(sdk): add types.go with all SDK type definitions"
```

---

## Task 3: errors.go — 错误类型

**Files:**
- Create: `sdk/go/errors.go`
- Create: `sdk/go/errors_test.go`

- [ ] **Step 1: 写失败测试**

`sdk/go/errors_test.go`:

```go
package sandbox_test

import (
	"errors"
	"testing"

	sandbox "github.com/goairix/sandbox-sdk-go"
)

func TestSandboxErrorIs(t *testing.T) {
	err := &sandbox.SandboxError{StatusCode: 404, Code: "SANDBOX_NOT_FOUND", Message: "not found"}
	if !errors.Is(err, sandbox.ErrNotFound) {
		t.Error("errors.Is should match ErrNotFound by StatusCode+Code")
	}
}

func TestSandboxErrorIsNegative(t *testing.T) {
	err := &sandbox.SandboxError{StatusCode: 500, Code: "INTERNAL", Message: "oops"}
	if errors.Is(err, sandbox.ErrNotFound) {
		t.Error("500 error should not match ErrNotFound")
	}
}

func TestSandboxErrorIsEmptyCodeSentinel(t *testing.T) {
	// ErrUnauthorized has no Code — any 401 should match regardless of Code.
	err := &sandbox.SandboxError{StatusCode: 401, Code: "SOME_OTHER_CODE", Message: "bad key"}
	if !errors.Is(err, sandbox.ErrUnauthorized) {
		t.Error("any 401 error should match ErrUnauthorized (empty-Code sentinel)")
	}
}

func TestSandboxErrorAs(t *testing.T) {
	err := &sandbox.SandboxError{StatusCode: 401, Code: "UNAUTHORIZED", Message: "bad key"}
	var se *sandbox.SandboxError
	if !errors.As(err, &se) {
		t.Fatal("errors.As should unwrap to *SandboxError")
	}
	if se.Message != "bad key" {
		t.Errorf("Message = %q, want %q", se.Message, "bad key")
	}
}

func TestSandboxErrorError(t *testing.T) {
	err := &sandbox.SandboxError{StatusCode: 429, Code: "RATE_LIMITED", Message: "slow down"}
	if err.Error() == "" {
		t.Error("Error() should return non-empty string")
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

```bash
cd sdk/go && go test ./... -run TestSandboxError
```

Expected: FAIL — `undefined: SandboxError`

- [ ] **Step 3: 实现 errors.go**

`sdk/go/errors.go`:

```go
package sandbox

import "fmt"

// SandboxError represents an error returned by the Sandbox API.
type SandboxError struct {
	StatusCode int    // HTTP status code
	Code       string // server-side error code
	Message    string // human-readable message
}

func (e *SandboxError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("sandbox: %s (HTTP %d): %s", e.Code, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("sandbox: HTTP %d: %s", e.StatusCode, e.Message)
}

// Is reports whether target matches this error by StatusCode and Code.
// If target.Code is empty, only StatusCode is compared.
func (e *SandboxError) Is(target error) bool {
	t, ok := target.(*SandboxError)
	if !ok {
		return false
	}
	if e.StatusCode != t.StatusCode {
		return false
	}
	if t.Code != "" && e.Code != t.Code {
		return false
	}
	return true
}

// Predefined sentinel errors for common HTTP status codes.
var (
	ErrNotFound       = &SandboxError{StatusCode: 404, Code: "SANDBOX_NOT_FOUND"}
	ErrUnauthorized   = &SandboxError{StatusCode: 401}
	ErrRateLimited    = &SandboxError{StatusCode: 429}
	ErrTimeout        = &SandboxError{StatusCode: 408}
	ErrInvalidRequest = &SandboxError{StatusCode: 400}
)
```

- [ ] **Step 4: 运行测试，确认通过**

```bash
cd sdk/go && go test ./... -run TestSandboxError -v
```

Expected: 4 tests PASS

- [ ] **Step 5: Commit**

```bash
git add sdk/go/errors.go sdk/go/errors_test.go
git commit -m "feat(sdk): add errors.go with SandboxError and sentinel errors"
```

---

## Task 4: client.go — 底层 HTTP Client

**Files:**
- Create: `sdk/go/client.go`
- Create: `sdk/go/client_test.go`

- [ ] **Step 1: 写失败测试（CreateSandbox + GetSandbox）**

`sdk/go/client_test.go`:

```go
package sandbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sandbox "github.com/goairix/sandbox-sdk-go"
)

func newTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *sandbox.Client) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := sandbox.NewClient(srv.URL, "test-key")
	return srv, client
}

func TestClientCreateSandbox(t *testing.T) {
	want := sandbox.SandboxResponse{
		ID:        "sb-123",
		Mode:      sandbox.ModeEphemeral,
		State:     "running",
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sandboxes" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Error("missing X-API-Key header")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(want)
	})

	got, err := client.CreateSandbox(context.Background(), sandbox.CreateSandboxRequest{Mode: sandbox.ModeEphemeral})
	if err != nil {
		t.Fatalf("CreateSandbox error: %v", err)
	}
	if got.ID != want.ID {
		t.Errorf("ID = %q, want %q", got.ID, want.ID)
	}
}

func TestClientGetSandbox(t *testing.T) {
	want := sandbox.SandboxResponse{ID: "sb-456", Mode: sandbox.ModePersistent, State: "idle"}
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sandboxes/sb-456" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(want)
	})

	got, err := client.GetSandbox(context.Background(), "sb-456")
	if err != nil {
		t.Fatalf("GetSandbox error: %v", err)
	}
	if got.ID != "sb-456" {
		t.Errorf("ID = %q, want %q", got.ID, "sb-456")
	}
}

func TestClientErrorResponse(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"code": "SANDBOX_NOT_FOUND", "message": "not found"})
	})

	_, err := client.GetSandbox(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

```bash
cd sdk/go && go test ./... -run TestClient
```

Expected: FAIL — `undefined: NewClient`

- [ ] **Step 3: 实现 client.go（第一部分：结构体 + 初始化 + 辅助方法）**

`sdk/go/client.go`:

```go
// Package sandbox provides a Go SDK for the Sandbox execution service.
package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// Client is the low-level HTTP client that maps 1:1 to all Sandbox API endpoints.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithTimeout sets the HTTP client timeout.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		c.httpClient.Timeout = d
	}
}

// WithHTTPClient replaces the underlying *http.Client.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		c.httpClient = hc
	}
}

// NewClient creates a new Client with the given base URL and API key.
func NewClient(baseURL, apiKey string, opts ...Option) *Client {
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// do executes an HTTP request and decodes the JSON response into out.
// If the server returns a non-2xx status, it returns a *SandboxError.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("sandbox: marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("sandbox: build request: %w", err)
	}
	req.Header.Set("X-API-Key", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sandbox: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.decodeError(resp)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("sandbox: decode response: %w", err)
		}
	}
	return nil
}

// decodeError reads an error response body and returns a *SandboxError.
func (c *Client) decodeError(resp *http.Response) error {
	var payload struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	return &SandboxError{
		StatusCode: resp.StatusCode,
		Code:       payload.Code,
		Message:    payload.Message,
	}
}
```

- [ ] **Step 4: 实现 client.go（第二部分：Sandbox 生命周期方法）**

在 `sdk/go/client.go` 末尾追加：

```go
// CreateSandbox creates a new sandbox. POST /api/v1/sandboxes
func (c *Client) CreateSandbox(ctx context.Context, req CreateSandboxRequest) (SandboxResponse, error) {
	var resp SandboxResponse
	return resp, c.do(ctx, http.MethodPost, "/api/v1/sandboxes", req, &resp)
}

// GetSandbox retrieves sandbox details. GET /api/v1/sandboxes/:id
func (c *Client) GetSandbox(ctx context.Context, id string) (SandboxResponse, error) {
	var resp SandboxResponse
	return resp, c.do(ctx, http.MethodGet, "/api/v1/sandboxes/"+id, nil, &resp)
}

// DestroySandbox destroys a sandbox. DELETE /api/v1/sandboxes/:id
func (c *Client) DestroySandbox(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/sandboxes/"+id, nil, nil)
}

// UpdateNetwork updates network config. PUT /api/v1/sandboxes/:id/network
func (c *Client) UpdateNetwork(ctx context.Context, id string, req UpdateNetworkRequest) (UpdateNetworkResponse, error) {
	var resp UpdateNetworkResponse
	return resp, c.do(ctx, http.MethodPut, "/api/v1/sandboxes/"+id+"/network", req, &resp)
}

// Exec executes code in a sandbox. POST /api/v1/sandboxes/:id/exec
func (c *Client) Exec(ctx context.Context, id string, req ExecRequest) (ExecResponse, error) {
	var resp ExecResponse
	return resp, c.do(ctx, http.MethodPost, "/api/v1/sandboxes/"+id+"/exec", req, &resp)
}

// Execute runs a one-shot execution. POST /api/v1/execute
func (c *Client) Execute(ctx context.Context, req ExecuteRequest) (ExecResponse, error) {
	var resp ExecResponse
	return resp, c.do(ctx, http.MethodPost, "/api/v1/execute", req, &resp)
}
```

- [ ] **Step 5: 实现 client.go（第三部分：文件操作方法）**

在 `sdk/go/client.go` 末尾追加：

```go
// UploadFile uploads a file to the sandbox. POST /api/v1/sandboxes/:id/files/upload
func (c *Client) UploadFile(ctx context.Context, id, remotePath string, r io.Reader) (FileUploadResponse, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filepath.Base(remotePath))
	if err != nil {
		return FileUploadResponse{}, fmt.Errorf("sandbox: create form file: %w", err)
	}
	if _, err := io.Copy(fw, r); err != nil {
		return FileUploadResponse{}, fmt.Errorf("sandbox: copy file: %w", err)
	}
	_ = mw.WriteField("path", remotePath)
	if err := mw.Close(); err != nil {
		return FileUploadResponse{}, fmt.Errorf("sandbox: close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/v1/sandboxes/"+id+"/files/upload", &buf)
	if err != nil {
		return FileUploadResponse{}, fmt.Errorf("sandbox: build request: %w", err)
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return FileUploadResponse{}, fmt.Errorf("sandbox: http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return FileUploadResponse{}, c.decodeError(resp)
	}
	var out FileUploadResponse
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

// DownloadFile downloads a file from the sandbox. GET /api/v1/sandboxes/:id/files/download
// Caller is responsible for closing the returned ReadCloser.
func (c *Client) DownloadFile(ctx context.Context, id, remotePath string) (io.ReadCloser, error) {
	u := c.baseURL + "/api/v1/sandboxes/" + id + "/files/download?path=" + url.QueryEscape(remotePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("sandbox: build request: %w", err)
	}
	req.Header.Set("X-API-Key", c.apiKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sandbox: http: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, c.decodeError(resp)
	}
	return resp.Body, nil
}

// ListFiles lists files in a directory. GET /api/v1/sandboxes/:id/files/list
func (c *Client) ListFiles(ctx context.Context, id, dir string) (FileListResponse, error) {
	var resp FileListResponse
	path := "/api/v1/sandboxes/" + id + "/files/list?path=" + url.QueryEscape(dir)
	return resp, c.do(ctx, http.MethodGet, path, nil, &resp)
}
```

- [ ] **Step 6: 实现 client.go（第四部分：Workspace 方法）**

在 `sdk/go/client.go` 末尾追加：

```go
// MountWorkspace mounts a workspace. POST /api/v1/sandboxes/:id/workspace/mount
func (c *Client) MountWorkspace(ctx context.Context, id string, req MountWorkspaceRequest) (MountWorkspaceResponse, error) {
	var resp MountWorkspaceResponse
	return resp, c.do(ctx, http.MethodPost, "/api/v1/sandboxes/"+id+"/workspace/mount", req, &resp)
}

// UnmountWorkspace unmounts the workspace. POST /api/v1/sandboxes/:id/workspace/unmount
func (c *Client) UnmountWorkspace(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/sandboxes/"+id+"/workspace/unmount", nil, nil)
}

// SyncWorkspace syncs the workspace. POST /api/v1/sandboxes/:id/workspace/sync
func (c *Client) SyncWorkspace(ctx context.Context, id string, req SyncWorkspaceRequest) (SyncWorkspaceResponse, error) {
	var resp SyncWorkspaceResponse
	return resp, c.do(ctx, http.MethodPost, "/api/v1/sandboxes/"+id+"/workspace/sync", req, &resp)
}

// GetWorkspaceInfo returns workspace status. GET /api/v1/sandboxes/:id/workspace/info
func (c *Client) GetWorkspaceInfo(ctx context.Context, id string) (WorkspaceInfoResponse, error) {
	var resp WorkspaceInfoResponse
	return resp, c.do(ctx, http.MethodGet, "/api/v1/sandboxes/"+id+"/workspace/info", nil, &resp)
}
```

- [ ] **Step 7: 运行测试，确认通过**

```bash
cd sdk/go && go test ./... -run TestClient -v -race
```

Expected: 3 tests PASS, no race conditions

- [ ] **Step 8: Commit**

```bash
git add sdk/go/client.go sdk/go/client_test.go
git commit -m "feat(sdk): add client.go with all synchronous HTTP endpoint methods"
```

---

## Task 5: sandbox.go — 高层 Sandbox 对象

**Files:**
- Create: `sdk/go/sandbox.go`
- Create: `sdk/go/sandbox_test.go`

- [ ] **Step 1: 写失败测试**

`sdk/go/sandbox_test.go`:

```go
package sandbox_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sandbox "github.com/goairix/sandbox-sdk-go"
)

func newSandboxTestServer(t *testing.T, sandboxID string) (*httptest.Server, *sandbox.Client) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sandbox.SandboxResponse{ID: sandboxID, Mode: sandbox.ModeEphemeral, State: "running"})
	})
	mux.HandleFunc("/api/v1/sandboxes/"+sandboxID+"/exec", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sandbox.ExecResponse{ExitCode: 0, Stdout: "hello\n"})
	})
	mux.HandleFunc("/api/v1/sandboxes/"+sandboxID, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, sandbox.NewClient(srv.URL, "test-key")
}

func TestNewSandbox(t *testing.T) {
	_, client := newSandboxTestServer(t, "sb-abc")
	sb, err := client.NewSandbox(context.Background(), sandbox.SandboxOptions{})
	if err != nil {
		t.Fatalf("NewSandbox error: %v", err)
	}
	if sb.ID() != "sb-abc" {
		t.Errorf("ID = %q, want %q", sb.ID(), "sb-abc")
	}
}

func TestSandboxRun(t *testing.T) {
	_, client := newSandboxTestServer(t, "sb-abc")
	sb, _ := client.NewSandbox(context.Background(), sandbox.SandboxOptions{})

	result, err := sb.Run(context.Background(), "python", `print("hello")`)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if result.Stdout != "hello\n" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "hello\n")
	}
}

func TestSandboxClose(t *testing.T) {
	_, client := newSandboxTestServer(t, "sb-abc")
	sb, _ := client.NewSandbox(context.Background(), sandbox.SandboxOptions{})
	if err := sb.Close(context.Background()); err != nil {
		t.Fatalf("Close error: %v", err)
	}
}

func TestClientRun(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/execute", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sandbox.ExecResponse{ExitCode: 0, Stdout: "42\n"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := sandbox.NewClient(srv.URL, "test-key")

	result, err := client.Run(context.Background(), "python", `print(42)`)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if result.Stdout != "42\n" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "42\n")
	}
}

func TestSandboxUploadDownload(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sandbox.SandboxResponse{ID: "sb-xyz", Mode: sandbox.ModeEphemeral, State: "running"})
	})
	mux.HandleFunc("/api/v1/sandboxes/sb-xyz/files/upload", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sandbox.FileUploadResponse{Path: "/workspace/main.py", Size: 10})
	})
	mux.HandleFunc("/api/v1/sandboxes/sb-xyz/files/download", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "file content")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := sandbox.NewClient(srv.URL, "test-key")
	sb, _ := client.NewSandbox(context.Background(), sandbox.SandboxOptions{})

	if err := sb.UploadFile(context.Background(), "/workspace/main.py", strings.NewReader("print(1)")); err != nil {
		t.Fatalf("UploadFile error: %v", err)
	}
	rc, err := sb.DownloadFile(context.Background(), "/workspace/main.py")
	if err != nil {
		t.Fatalf("DownloadFile error: %v", err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "file content" {
		t.Errorf("content = %q, want %q", string(data), "file content")
	}
}
```

- [ ] **Step 2: 运行测试，确认失败**

```bash
cd sdk/go && go test ./... -run "TestNewSandbox|TestSandboxRun|TestSandboxClose|TestClientRun"
```

Expected: FAIL — `undefined: SandboxOptions`

- [ ] **Step 3: 实现 sandbox.go**

`sdk/go/sandbox.go`:

```go
package sandbox

import (
	"context"
	"io"
)

// SandboxOptions configures a new sandbox created via NewSandbox.
type SandboxOptions struct {
	Mode         Mode
	Timeout      int
	Resources    *ResourceLimits
	Network      *NetworkConfig
	Dependencies []DependencySpec
}

// Sandbox is a high-level handle to a running sandbox instance.
type Sandbox struct {
	client *Client
	id     string
}

// ID returns the sandbox identifier.
func (s *Sandbox) ID() string { return s.id }

// NewSandbox creates a new sandbox and returns a high-level Sandbox handle.
// If opts.Mode is empty, ModeEphemeral is used.
func (c *Client) NewSandbox(ctx context.Context, opts SandboxOptions) (*Sandbox, error) {
	mode := opts.Mode
	if mode == "" {
		mode = ModeEphemeral
	}
	req := CreateSandboxRequest{
		Mode:         mode,
		Timeout:      opts.Timeout,
		Resources:    opts.Resources,
		Network:      opts.Network,
		Dependencies: opts.Dependencies,
	}
	resp, err := c.CreateSandbox(ctx, req)
	if err != nil {
		return nil, err
	}
	return &Sandbox{client: c, id: resp.ID}, nil
}

// Close destroys the sandbox. Suitable for use with defer.
func (s *Sandbox) Close(ctx context.Context) error {
	return s.client.DestroySandbox(ctx, s.id)
}

// Run executes code in the sandbox and returns the result.
func (s *Sandbox) Run(ctx context.Context, language, code string) (ExecResponse, error) {
	return s.client.Exec(ctx, s.id, ExecRequest{Language: language, Code: code})
}

// UploadFile uploads a file to the sandbox at remotePath.
func (s *Sandbox) UploadFile(ctx context.Context, remotePath string, r io.Reader) error {
	_, err := s.client.UploadFile(ctx, s.id, remotePath, r)
	return err
}

// DownloadFile downloads a file from the sandbox. Caller must close the returned ReadCloser.
func (s *Sandbox) DownloadFile(ctx context.Context, remotePath string) (io.ReadCloser, error) {
	return s.client.DownloadFile(ctx, s.id, remotePath)
}

// ListFiles lists files in a directory inside the sandbox.
func (s *Sandbox) ListFiles(ctx context.Context, dir string) (FileListResponse, error) {
	return s.client.ListFiles(ctx, s.id, dir)
}

// MountWorkspace mounts a workspace by root path.
func (s *Sandbox) MountWorkspace(ctx context.Context, rootPath string) error {
	_, err := s.client.MountWorkspace(ctx, s.id, MountWorkspaceRequest{RootPath: rootPath})
	return err
}

// UnmountWorkspace unmounts the current workspace.
func (s *Sandbox) UnmountWorkspace(ctx context.Context) error {
	return s.client.UnmountWorkspace(ctx, s.id)
}

// Sync syncs the workspace from container to host (from_container direction).
func (s *Sandbox) Sync(ctx context.Context) (SyncWorkspaceResponse, error) {
	return s.client.SyncWorkspace(ctx, s.id, SyncWorkspaceRequest{Direction: "from_container"})
}

// SyncTo syncs the workspace from host to container (to_container direction).
func (s *Sandbox) SyncTo(ctx context.Context) (SyncWorkspaceResponse, error) {
	return s.client.SyncWorkspace(ctx, s.id, SyncWorkspaceRequest{Direction: "to_container"})
}

// WorkspaceInfo returns the current workspace status.
func (s *Sandbox) WorkspaceInfo(ctx context.Context) (WorkspaceInfoResponse, error) {
	return s.client.GetWorkspaceInfo(ctx, s.id)
}

// EnableNetwork enables network access with the given whitelist.
func (s *Sandbox) EnableNetwork(ctx context.Context, whitelist []string) error {
	_, err := s.client.UpdateNetwork(ctx, s.id, UpdateNetworkRequest{Enabled: true, Whitelist: whitelist})
	return err
}

// DisableNetwork disables network access.
func (s *Sandbox) DisableNetwork(ctx context.Context) error {
	_, err := s.client.UpdateNetwork(ctx, s.id, UpdateNetworkRequest{Enabled: false})
	return err
}

// Run is a convenience method on Client for one-shot execution without pre-creating a sandbox.
func (c *Client) Run(ctx context.Context, language, code string) (ExecResponse, error) {
	return c.Execute(ctx, ExecuteRequest{Language: language, Code: code})
}
```

- [ ] **Step 4: 运行全部测试**

```bash
cd sdk/go && go test ./... -v -race
```

Expected: 全部 PASS，无 race condition

- [ ] **Step 5: Commit**

```bash
git add sdk/go/sandbox.go sdk/go/sandbox_test.go
git commit -m "feat(sdk): add sandbox.go high-level Sandbox object and Client.Run convenience method"
```

---

## Task 6: 最终验证

**Files:** 无新文件，验证整体

- [ ] **Step 1: 运行全部测试（含 race detector）**

```bash
cd sdk/go && go test ./... -v -race -count=1
```

Expected: 全部 PASS

- [ ] **Step 2: 静态检查**

```bash
cd sdk/go && go vet ./...
```

Expected: 无输出（无警告）

- [ ] **Step 3: 确认 module 无多余依赖**

```bash
cd sdk/go && go mod tidy && git diff sdk/go/go.mod
```

Expected: go.mod 无变化（仅标准库依赖）

- [ ] **Step 4: 最终 Commit**

```bash
git add sdk/
git commit -m "feat(sdk): complete Go SDK v1 — types, errors, client, sandbox"
```

---
