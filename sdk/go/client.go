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
// The provided client is copied to avoid mutating the caller's value.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		copied := *hc
		c.httpClient = &copied
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
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
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

// sandboxBase returns the base path for a sandbox, with the id properly escaped.
func (c *Client) sandboxBase(id string) string {
	return "/api/v1/sandboxes/" + url.PathEscape(id)
}

// CreateSandbox creates a new sandbox. POST /api/v1/sandboxes
func (c *Client) CreateSandbox(ctx context.Context, req CreateSandboxRequest) (SandboxResponse, error) {
	var resp SandboxResponse
	return resp, c.do(ctx, http.MethodPost, "/api/v1/sandboxes", req, &resp)
}

// GetSandbox retrieves sandbox details. GET /api/v1/sandboxes/:id
func (c *Client) GetSandbox(ctx context.Context, id string) (SandboxResponse, error) {
	var resp SandboxResponse
	return resp, c.do(ctx, http.MethodGet, c.sandboxBase(id), nil, &resp)
}

// DestroySandbox destroys a sandbox. DELETE /api/v1/sandboxes/:id
func (c *Client) DestroySandbox(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, c.sandboxBase(id), nil, nil)
}

// UpdateNetwork updates network config. PUT /api/v1/sandboxes/:id/network
func (c *Client) UpdateNetwork(ctx context.Context, id string, req UpdateNetworkRequest) (UpdateNetworkResponse, error) {
	var resp UpdateNetworkResponse
	return resp, c.do(ctx, http.MethodPut, c.sandboxBase(id)+"/network", req, &resp)
}

// Exec executes code in a sandbox. POST /api/v1/sandboxes/:id/exec
func (c *Client) Exec(ctx context.Context, id string, req ExecRequest) (ExecResponse, error) {
	var resp ExecResponse
	return resp, c.do(ctx, http.MethodPost, c.sandboxBase(id)+"/exec", req, &resp)
}

// Execute runs a one-shot execution. POST /api/v1/execute
func (c *Client) Execute(ctx context.Context, req ExecuteRequest) (ExecResponse, error) {
	var resp ExecResponse
	return resp, c.do(ctx, http.MethodPost, "/api/v1/execute", req, &resp)
}

// UploadFile uploads a file to the sandbox. POST /api/v1/sandboxes/:id/files/upload
// The file content is streamed via io.Pipe to avoid buffering the entire file in memory.
func (c *Client) UploadFile(ctx context.Context, id, remotePath string, r io.Reader) (FileUploadResponse, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		fw, err := mw.CreateFormFile("file", filepath.Base(remotePath))
		if err != nil {
			pw.CloseWithError(fmt.Errorf("sandbox: create form file: %w", err))
			return
		}
		if _, err := io.Copy(fw, r); err != nil {
			pw.CloseWithError(fmt.Errorf("sandbox: copy file: %w", err))
			return
		}
		if err := mw.WriteField("path", remotePath); err != nil {
			pw.CloseWithError(fmt.Errorf("sandbox: write field: %w", err))
			return
		}
		pw.CloseWithError(mw.Close())
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+c.sandboxBase(id)+"/files/upload", pr)
	if err != nil {
		pr.CloseWithError(err)
		return FileUploadResponse{}, fmt.Errorf("sandbox: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
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
	u := c.baseURL + c.sandboxBase(id) + "/files/download?path=" + url.QueryEscape(remotePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("sandbox: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
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
	path := c.sandboxBase(id) + "/files/list?path=" + url.QueryEscape(dir)
	return resp, c.do(ctx, http.MethodGet, path, nil, &resp)
}

// MountWorkspace mounts a workspace. POST /api/v1/sandboxes/:id/workspace/mount
func (c *Client) MountWorkspace(ctx context.Context, id string, req MountWorkspaceRequest) (MountWorkspaceResponse, error) {
	var resp MountWorkspaceResponse
	return resp, c.do(ctx, http.MethodPost, c.sandboxBase(id)+"/workspace/mount", req, &resp)
}

// UnmountWorkspace unmounts the workspace. POST /api/v1/sandboxes/:id/workspace/unmount
func (c *Client) UnmountWorkspace(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, c.sandboxBase(id)+"/workspace/unmount", nil, nil)
}

// SyncWorkspace syncs the workspace. POST /api/v1/sandboxes/:id/workspace/sync
func (c *Client) SyncWorkspace(ctx context.Context, id string, req SyncWorkspaceRequest) (SyncWorkspaceResponse, error) {
	var resp SyncWorkspaceResponse
	return resp, c.do(ctx, http.MethodPost, c.sandboxBase(id)+"/workspace/sync", req, &resp)
}

// GetWorkspaceInfo returns workspace status. GET /api/v1/sandboxes/:id/workspace/info
func (c *Client) GetWorkspaceInfo(ctx context.Context, id string) (WorkspaceInfoResponse, error) {
	var resp WorkspaceInfoResponse
	return resp, c.do(ctx, http.MethodGet, c.sandboxBase(id)+"/workspace/info", nil, &resp)
}

// ListSkills lists all agent skills in the sandbox. GET /api/v1/sandboxes/:id/skills
func (c *Client) ListSkills(ctx context.Context, id string) (SkillListResponse, error) {
	var resp SkillListResponse
	return resp, c.do(ctx, http.MethodGet, c.sandboxBase(id)+"/skills", nil, &resp)
}

// GetSkill returns the full content and file list of a skill. GET /api/v1/sandboxes/:id/skills/:name
func (c *Client) GetSkill(ctx context.Context, id, name string) (SkillResponse, error) {
	var resp SkillResponse
	return resp, c.do(ctx, http.MethodGet, c.sandboxBase(id)+"/skills/"+url.PathEscape(name), nil, &resp)
}

// GetSkillFile returns the raw content of an attached skill file. GET /api/v1/sandboxes/:id/skills/:name/files/*filepath
// Caller is responsible for closing the returned ReadCloser.
func (c *Client) GetSkillFile(ctx context.Context, id, name, filePath string) (io.ReadCloser, error) {
	u := c.baseURL + c.sandboxBase(id) + "/skills/" + url.PathEscape(name) + "/files/" + filePath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("sandbox: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
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

// ListFilesRecursive lists files recursively in a sandbox directory.
func (c *Client) ListFilesRecursive(ctx context.Context, id string, req ListFilesRecursiveRequest) (ListFilesRecursiveResponse, error) {
	var resp ListFilesRecursiveResponse
	err := c.do(ctx, http.MethodPost, c.sandboxBase(id)+"/files/list-recursive", req, &resp)
	return resp, err
}

// ReadFileLines reads a range of lines from a file in a sandbox.
func (c *Client) ReadFileLines(ctx context.Context, id string, req ReadFileLinesRequest) (ReadFileLinesResponse, error) {
	var resp ReadFileLinesResponse
	err := c.do(ctx, http.MethodPost, c.sandboxBase(id)+"/files/read-lines", req, &resp)
	return resp, err
}

// EditFile performs a string replacement in a file in a sandbox.
func (c *Client) EditFile(ctx context.Context, id string, req EditFileRequest) error {
	return c.do(ctx, http.MethodPost, c.sandboxBase(id)+"/files/edit", req, nil)
}

// EditFileLines replaces a range of lines in a file in a sandbox.
func (c *Client) EditFileLines(ctx context.Context, id string, req EditFileLinesRequest) error {
	return c.do(ctx, http.MethodPost, c.sandboxBase(id)+"/files/edit-lines", req, nil)
}
