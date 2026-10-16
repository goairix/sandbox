package etcdgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/goairix/sandbox/internal/provisioning"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/sandboxfiles"
	"github.com/goairix/sandbox/pkg/types"
	"github.com/google/uuid"
)

func fileRoute(method, operation string) bool {
	if method == http.MethodGet {
		return operation == "list" || operation == "download"
	}
	if method != http.MethodPost {
		return false
	}
	switch operation {
	case "list-recursive", "glob", "read", "read-lines", "edit", "edit-lines":
		return true
	}
	return false
}

func fileRequest(w http.ResponseWriter, r *http.Request, operation string) (sandboxfiles.Request, bool) {
	request := sandboxfiles.Request{Operation: operation}
	if r.Method == http.MethodGet {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1))
		if err != nil || len(body) != 0 || len(r.URL.RawQuery) > 3*4096+100 {
			writeError(w, 400, "invalid_request", "invalid file query")
			return request, false
		}
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(query) > 1 || len(query) == 1 && len(query["path"]) != 1 {
			writeError(w, 400, "invalid_request", "invalid file query")
			return request, false
		}
		request.Path = query.Get("path")
		if len(query) == 0 && operation == "list" {
			request.Path = "/workspace"
		}
		return request, true
	}
	switch operation {
	case "read":
		var dto types.ReadFileRequest
		if !readRequest(w, r, &dto) {
			return request, false
		}
		request.Path = dto.Path
	case "list-recursive":
		var dto types.ListFilesRecursiveRequest
		if !readRequest(w, r, &dto) {
			return request, false
		}
		request.Path, request.MaxDepth, request.Page, request.PageSize = dto.Path, dto.MaxDepth, dto.Page, dto.PageSize
	case "glob":
		var dto types.GlobFilesRequest
		if !readRequest(w, r, &dto) {
			return request, false
		}
		request.Path, request.Pattern, request.Page, request.PageSize = dto.Path, dto.Pattern, dto.Page, dto.PageSize
	case "read-lines":
		var dto types.ReadFileLinesRequest
		if !readRequest(w, r, &dto) {
			return request, false
		}
		request.Path, request.StartLine, request.EndLine = dto.Path, dto.StartLine, dto.EndLine
	case "edit":
		var dto types.EditFileRequest
		if !readRequest(w, r, &dto) {
			return request, false
		}
		request.Path, request.OldStr, request.NewStr, request.ReplaceAll = dto.Path, dto.OldStr, dto.NewStr, dto.ReplaceAll
	case "edit-lines":
		var dto types.EditFileLinesRequest
		if !readRequest(w, r, &dto) {
			return request, false
		}
		request.Path, request.StartLine, request.EndLine, request.NewContent = dto.Path, dto.StartLine, dto.EndLine, dto.NewContent
	}
	return request, true
}

func (g *Gateway) files(w http.ResponseWriter, r *http.Request, id, operation string) {
	for name := range r.Header {
		if strings.EqualFold(name, "Idempotency-Key") {
			writeError(w, 400, "invalid_request", "Idempotency-Key is not supported for file operations")
			return
		}
	}
	requestID, valid := validHeaderValue(r.Header, "X-Request-ID", 128, true)
	if !valid {
		writeError(w, 400, "invalid_request", "invalid X-Request-ID")
		return
	}
	if requestID == "" {
		requestID = uuid.NewString()
	}
	w.Header().Set("X-Request-ID", requestID)
	request, valid := fileRequest(w, r, operation)
	if !valid {
		return
	}
	if err := sandboxfiles.Validate(request); err != nil {
		status, message := sandboxfiles.ErrorStatus(err.Error())
		writeError(w, status, err.Error(), message)
		return
	}
	if operation == "download" && g.transfers != nil {
		g.download(w, r, id, requestID, request.Path)
		return
	}
	stdin, err := json.Marshal(request)
	if err != nil || len(stdin) > sandboxfiles.MaxInputBytes {
		writeError(w, 413, "limit_exceeded", "file request exceeds helper input limit")
		return
	}
	resolved := p.ExecutionRequest{Argv: []string{"/usr/local/bin/sandbox-launcher", "file-operation"}, Env: g.settings.Env, UID: g.settings.UID, GID: g.settings.GID, WorkDir: "/workspace", TimeoutSeconds: uint32(g.settings.ExecTimeoutSeconds), Stdin: stdin, RequiresNetwork: false}
	descriptor, err := p.NewExecutionDescriptor(resolved)
	if err != nil {
		writeError(w, 400, "invalid_request", "invalid file execution descriptor")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(g.settings.ExecTimeoutSeconds)*time.Second+30*time.Second)
	defer cancel()
	budget := &fileOutputBudget{cancel: cancel}
	stdout := &fileOutput{budget: budget, limit: sandboxfiles.MaxOutputBytes}
	stderr := &fileOutput{budget: budget, limit: sandboxfiles.MaxStderrBytes}
	// Synchronous return physically joins the original service Wait, renewal and
	// IO owners, including when either bounded writer cancels the context.
	result, err := g.service.Exec(ctx, id, requestID, descriptor.Request(), stdout, stderr)
	if budget.overflow {
		writeJSON(w, 502, executionUnknown(result, err))
		return
	}
	if exit, terminal := terminalExit(result, err); terminal {
		if exit != 0 {
			writeError(w, 502, "file_helper_failed", "file helper failed; do not automatically retry")
			return
		}
		answer, decodeErr := sandboxfiles.DecodeResult(stdout.buffer.Bytes())
		if decodeErr != nil || answer.Error == nil && !fileResultMatches(request, answer) {
			writeError(w, 502, "file_result_invalid", "file result unavailable; do not automatically retry")
			return
		}
		if answer.Error != nil {
			status, message := sandboxfiles.ErrorStatus(answer.Error.Code)
			writeError(w, status, answer.Error.Code, message)
			return
		}
		writeFileResult(w, request, answer)
		return
	}
	if result.State == provisioning.PlainExecRejected && err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, 502, executionUnknown(result, err))
}

type fileOutputBudget struct {
	mu       sync.Mutex
	overflow bool
	cancel   context.CancelFunc
}

type fileOutput struct {
	budget *fileOutputBudget
	limit  int
	buffer bytes.Buffer
}

func (w *fileOutput) Write(data []byte) (int, error) {
	w.budget.mu.Lock()
	defer w.budget.mu.Unlock()
	// Separate stream caps also bound their combined allocation to 1MiB+64KiB.
	if w.budget.overflow || len(data) > w.limit-w.buffer.Len() {
		w.budget.overflow = true
		w.budget.cancel()
		return 0, errors.New("file output limit")
	}
	return w.buffer.Write(data)
}

func fileResultMatches(request sandboxfiles.Request, result sandboxfiles.Result) bool {
	clean := result
	switch request.Operation {
	case "read", "download":
		clean.Content = nil
	case "edit", "edit-lines":
	case "list", "list-recursive", "glob":
		if len(result.Files) > result.TotalCount {
			return false
		}
		clean.Files, clean.TotalCount = nil, 0
		if request.Operation == "list" {
			if result.TotalCount != len(result.Files) {
				return false
			}
		} else {
			page, size := request.Page, request.PageSize
			if page == 0 {
				page = 1
			}
			if size == 0 {
				size = 100
			}
			if result.Page != page || result.PageSize != size || len(result.Files) > size {
				return false
			}
			clean.Page, clean.PageSize = 0, 0
		}
	case "read-lines":
		start, end := request.StartLine, request.EndLine
		if start == 0 {
			start = 1
		}
		if end == 0 || end > result.TotalLines {
			end = result.TotalLines
		}
		count := 0
		if end >= start {
			count = end - start + 1
		}
		if result.StartLine != start || result.EndLine != end || len(result.Lines) != count {
			return false
		}
		clean.Lines, clean.StartLine, clean.EndLine, clean.TotalLines = nil, 0, 0, 0
	}
	return reflect.DeepEqual(clean, sandboxfiles.Result{})
}

func writeFileResult(w http.ResponseWriter, request sandboxfiles.Request, result sandboxfiles.Result) {
	switch request.Operation {
	case "read", "download":
		if request.Operation == "read" {
			name := request.Path
			if strings.IndexFunc(name, unicode.IsControl) >= 0 {
				name = url.PathEscape(name)
				w.Header().Set("X-File-Path-Encoding", "percent-encoded")
			}
			w.Header().Set("X-File-Path", name)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		} else {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(request.Path)}))
		}
		w.WriteHeader(200)
		_, _ = w.Write(result.Content)
	case "list":
		if result.Files == nil {
			result.Files = []types.FileInfo{}
		}
		writeJSON(w, 200, types.FileListResponse{Files: result.Files, Path: request.Path})
	case "list-recursive":
		if result.Files == nil {
			result.Files = []types.FileInfo{}
		}
		writeJSON(w, 200, types.ListFilesRecursiveResponse{Files: result.Files, Path: request.Path, TotalCount: result.TotalCount, Page: result.Page, PageSize: result.PageSize})
	case "glob":
		if result.Files == nil {
			result.Files = []types.FileInfo{}
		}
		writeJSON(w, 200, types.GlobFilesResponse{Files: result.Files, Path: request.Path, Pattern: request.Pattern, TotalCount: result.TotalCount, Page: result.Page, PageSize: result.PageSize})
	case "read-lines":
		if result.Lines == nil {
			result.Lines = []string{}
		}
		writeJSON(w, 200, types.ReadFileLinesResponse{Lines: result.Lines, StartLine: result.StartLine, EndLine: result.EndLine, TotalLines: result.TotalLines})
	case "edit", "edit-lines":
		writeJSON(w, 200, types.EditFileResponse{Message: "ok"})
	}
}
