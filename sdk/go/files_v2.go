package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// Files is the generic v2 file API. Existing Sandbox file methods keep their
// v1 contract while callers migrate deliberately to this handle.
type Files struct {
	client *Client
	id     string
}

type FileV2ListPage struct {
	Files      []FileInfo `json:"files"`
	NextCursor string     `json:"next_cursor"`
}

type FileV2GlobRequest struct {
	Path    string `json:"path,omitempty"`
	Pattern string `json:"pattern"`
	Limit   int    `json:"limit,omitempty"`
}

type FileV2SearchPage struct {
	Files     []FileInfo `json:"files"`
	Truncated bool       `json:"truncated"`
}

type FileV2GrepRequest struct {
	Path          string   `json:"path,omitempty"`
	Pattern       string   `json:"pattern"`
	Glob          string   `json:"glob,omitempty"`
	FileTypes     []string `json:"file_types,omitempty"`
	IgnoreCase    bool     `json:"ignore_case,omitempty"`
	Multiline     bool     `json:"multiline,omitempty"`
	BeforeContext int      `json:"before_context,omitempty"`
	AfterContext  int      `json:"after_context,omitempty"`
	Limit         int      `json:"limit,omitempty"`
}

type FileV2Match struct {
	Path       string   `json:"path"`
	LineNumber int      `json:"line_number"`
	Content    string   `json:"content"`
	Before     []string `json:"before,omitempty"`
	After      []string `json:"after,omitempty"`
}

type FileV2GrepResult struct {
	Matches   []FileV2Match `json:"matches"`
	Truncated bool          `json:"truncated"`
}

func (f *Files) base() string {
	return "/api/v2/sandboxes/" + url.PathEscape(f.id) + "/files"
}

func (f *Files) Stat(ctx context.Context, path string) (FileInfo, error) {
	var info FileInfo
	query := url.Values{"path": {path}}
	err := f.client.do(ctx, http.MethodGet, f.base()+"/stat?"+query.Encode(), nil, &info)
	return info, err
}

func (f *Files) List(ctx context.Context, path, cursor string, limit int) (FileV2ListPage, error) {
	var page FileV2ListPage
	query := url.Values{"path": {path}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	if limit != 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	err := f.client.do(ctx, http.MethodGet, f.base()+"/list?"+query.Encode(), nil, &page)
	return page, err
}

func (f *Files) Glob(ctx context.Context, request FileV2GlobRequest) (FileV2SearchPage, error) {
	var page FileV2SearchPage
	err := f.client.do(ctx, http.MethodPost, f.base()+"/glob", request, &page)
	return page, err
}

func (f *Files) Grep(ctx context.Context, request FileV2GrepRequest) (FileV2GrepResult, error) {
	var result FileV2GrepResult
	err := f.client.do(ctx, http.MethodPost, f.base()+"/grep", request, &result)
	return result, err
}

// Read returns a raw byte stream. The caller must close it, including when
// decoding stops early, so the HTTP connection and server-side gate can join.
// Streaming duration is controlled by ctx rather than the unary HTTP timeout.
func (f *Files) Read(ctx context.Context, path string) (io.ReadCloser, error) {
	query := url.Values{"path": {path}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.client.baseURL+f.base()+"/content?"+query.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("sandbox: build file read: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+f.client.apiKey)
	streamClient := *f.client.httpClient
	streamClient.Timeout = 0
	resp, err := streamClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sandbox: file read: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, f.client.decodeError(resp)
	}
	return resp.Body, nil
}

// Write sends exactly the declared byte count as a raw request body. The
// source and size must be supplied by the caller; no base64 buffer is built.
// Streaming duration is controlled by ctx rather than the unary HTTP timeout.
func (f *Files) Write(ctx context.Context, path string, source io.Reader, size int64, createParents bool) error {
	if source == nil || size < 0 {
		return errors.New("sandbox: invalid file source or size")
	}
	query := url.Values{"path": {path}}
	if createParents {
		query.Set("create_parents", "true")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, f.client.baseURL+f.base()+"/content?"+query.Encode(), source)
	if err != nil {
		return fmt.Errorf("sandbox: build file write: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+f.client.apiKey)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = size
	streamClient := *f.client.httpClient
	streamClient.Timeout = 0
	resp, err := streamClient.Do(req)
	if err != nil {
		return fmt.Errorf("sandbox: file write: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return f.client.decodeError(resp)
	}
	return nil
}

func (f *Files) ReadLines(ctx context.Context, path string, startLine, lineCount int) (string, error) {
	var response struct {
		Content string `json:"content"`
	}
	err := f.client.do(ctx, http.MethodPost, f.base()+"/text/read-lines", map[string]any{
		"path": path, "start_line": startLine, "line_count": lineCount,
	}, &response)
	return response.Content, err
}

func (f *Files) Edit(ctx context.Context, path, oldText, newText string, replaceAll bool) error {
	return f.client.do(ctx, http.MethodPost, f.base()+"/text/edit", map[string]any{
		"path": path, "old_string": oldText, "new_string": newText, "replace_all": replaceAll,
	}, nil)
}
