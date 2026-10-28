package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/goairix/sandbox/internal/filev2"
	"github.com/goairix/sandbox/internal/fuseprotocol"
	"github.com/goairix/sandbox/internal/runtime"
)

const maxFileV2MetadataBytes = 8 << 20

var ErrFileV2Unsupported = errors.New("runtime does not support files v2")
var ErrFileV2InvalidPath = errors.New("invalid files v2 path")

type fileV2WireInfo struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	IsDir   bool      `json:"is_dir"`
	ModTime time.Time `json:"mod_time"`
}

func (info fileV2WireInfo) runtimeInfo() runtime.FileInfo {
	return runtime.FileInfo{
		Name: info.Name, Path: info.Path, Size: info.Size,
		IsDir: info.IsDir, ModTime: info.ModTime,
	}
}

func normalizeFileV2Path(input string) (string, error) {
	if input == "" || len(input) > 4096 || strings.IndexByte(input, 0) >= 0 {
		return "", ErrFileV2InvalidPath
	}
	for _, part := range strings.Split(input, "/") {
		if part == ".." {
			return "", ErrFileV2InvalidPath
		}
	}
	if !strings.HasPrefix(input, "/") {
		input = "/workspace/" + input
	}
	clean := path.Clean(input)
	if clean != "/workspace" && !strings.HasPrefix(clean, "/workspace/") && clean != "/tmp" && !strings.HasPrefix(clean, "/tmp/") {
		return "", ErrFileV2InvalidPath
	}
	return clean, nil
}

func (m *Manager) fileV2Operation(ctx context.Context, id string) (*Sandbox, context.Context, func(), runtime.FileV2Transport, error) {
	sb, operationCtx, release, err := m.acquireSandboxOperation(ctx, id)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	transport, ok := m.runtime.(runtime.FileV2Transport)
	if !ok {
		release()
		return nil, nil, nil, nil, ErrFileV2Unsupported
	}
	return sb, operationCtx, release, transport, nil
}

func readFileV2Metadata(rc io.ReadCloser, result any) error {
	if rc == nil {
		return errors.New("file helper returned no reader")
	}
	body, readErr := io.ReadAll(io.LimitReader(rc, maxFileV2MetadataBytes+1))
	closeErr := rc.Close() // Close joins the sandbox-side helper.
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	if len(body) > maxFileV2MetadataBytes {
		return errors.New("file metadata exceeds limit")
	}
	if err := json.Unmarshal(body, result); err != nil {
		return fmt.Errorf("invalid file helper metadata: %w", err)
	}
	return nil
}

// StatFileV2 reads one sandbox-side file record under the ordinary data gate.
func (m *Manager) StatFileV2(ctx context.Context, id, filePath string) (runtime.FileInfo, error) {
	canonical, err := normalizeFileV2Path(filePath)
	if err != nil {
		return runtime.FileInfo{}, err
	}
	sb, operationCtx, release, transport, err := m.fileV2Operation(ctx, id)
	if err != nil {
		return runtime.FileInfo{}, err
	}
	defer release()
	if err := validatePublicFUSEFilePath(sb, canonical); err != nil {
		return runtime.FileInfo{}, err
	}
	rc, err := transport.RunFileOp(operationCtx, m.sandboxRuntimeID(sb), []string{"stat", canonical}, nil)
	if err != nil {
		return runtime.FileInfo{}, err
	}
	var wire fileV2WireInfo
	if err := readFileV2Metadata(rc, &wire); err != nil {
		return runtime.FileInfo{}, err
	}
	if wire.Path != canonical || wire.Name != path.Base(canonical) || wire.Size < 0 {
		return runtime.FileInfo{}, errors.New("file helper returned invalid stat")
	}
	return wire.runtimeInfo(), nil
}

// ListFilesV2 returns a bounded, lexically ordered page. The cursor is a
// position hint; concurrent directory changes are not hidden as a snapshot.
func (m *Manager) ListFilesV2(ctx context.Context, id, dirPath, cursor string, limit int) (runtime.FileListPage, error) {
	if dirPath == "" {
		dirPath = "/workspace"
	}
	canonical, err := normalizeFileV2Path(dirPath)
	if err != nil {
		return runtime.FileListPage{}, err
	}
	if limit == 0 {
		limit = 200
	}
	if limit < 1 || limit > 1000 {
		return runtime.FileListPage{}, errors.New("invalid file list limit")
	}
	if cursor != "" && (path.Clean(cursor) != cursor || path.Dir(cursor) != canonical) {
		return runtime.FileListPage{}, ErrFileV2InvalidPath
	}
	sb, operationCtx, release, transport, err := m.fileV2Operation(ctx, id)
	if err != nil {
		return runtime.FileListPage{}, err
	}
	defer release()
	if err := validatePublicFUSEFilePath(sb, canonical); err != nil {
		return runtime.FileListPage{}, err
	}
	if cursor != "" {
		if err := validatePublicFUSEFilePath(sb, cursor); err != nil {
			return runtime.FileListPage{}, err
		}
	}
	argv := []string{"list", canonical, cursor, strconv.Itoa(limit)}
	if sb.Workspace != nil && sb.Workspace.MountType == WorkspaceMountFUSE {
		argv = append(argv, "hide-reserved")
	}
	rc, err := transport.RunFileOp(operationCtx, m.sandboxRuntimeID(sb), argv, nil)
	if err != nil {
		return runtime.FileListPage{}, err
	}
	var wire struct {
		Files      []fileV2WireInfo `json:"files"`
		NextCursor string           `json:"next_cursor"`
	}
	if err := readFileV2Metadata(rc, &wire); err != nil {
		return runtime.FileListPage{}, err
	}
	if len(wire.Files) > limit || (wire.NextCursor != "" && path.Dir(wire.NextCursor) != canonical) {
		return runtime.FileListPage{}, errors.New("file helper returned invalid page")
	}
	if wire.NextCursor != "" && sb.Workspace != nil && sb.Workspace.MountType == WorkspaceMountFUSE &&
		fuseprotocol.IsReservedProbeObjectName(path.Base(wire.NextCursor)) {
		return runtime.FileListPage{}, errors.New("file helper returned reserved cursor")
	}
	page := runtime.FileListPage{Files: make([]runtime.FileInfo, 0, len(wire.Files)), NextCursor: wire.NextCursor}
	last := cursor
	for _, info := range wire.Files {
		if path.Dir(info.Path) != canonical || info.Name != path.Base(info.Path) || info.Path <= last || info.Size < 0 {
			return runtime.FileListPage{}, errors.New("file helper returned invalid listing")
		}
		page.Files = append(page.Files, info.runtimeInfo())
		last = info.Path
	}
	if page.NextCursor != "" && page.NextCursor != last {
		return runtime.FileListPage{}, errors.New("file helper returned invalid cursor")
	}
	page.Files = hideReservedFUSEFileInfos(sb, page.Files)
	return page, nil
}

// GlobFilesV2 searches paths in one sandbox operation. The helper's bounded
// result is revalidated before it becomes public metadata.
func (m *Manager) GlobFilesV2(ctx context.Context, id string, request runtime.FileV2GlobRequest) (runtime.FileV2SearchPage, error) {
	if request.Path == "" {
		request.Path = "/workspace"
	}
	canonical, err := normalizeFileV2Path(request.Path)
	if err != nil {
		return runtime.FileV2SearchPage{}, err
	}
	if err := filev2.ValidateGlobPattern(request.Pattern); err != nil {
		return runtime.FileV2SearchPage{}, runtime.ErrFileInvalidText
	}
	if request.Limit == 0 {
		request.Limit = 1000
	}
	if request.Limit < 1 || request.Limit > 1000 {
		return runtime.FileV2SearchPage{}, runtime.ErrFileLimitExceeded
	}
	sb, operationCtx, release, transport, err := m.fileV2Operation(ctx, id)
	if err != nil {
		return runtime.FileV2SearchPage{}, err
	}
	defer release()
	if err := validatePublicFUSEFilePath(sb, canonical); err != nil {
		return runtime.FileV2SearchPage{}, err
	}
	searchCtx, cancel := context.WithTimeout(operationCtx, 6*time.Second)
	defer cancel()
	argv := []string{"glob", canonical, request.Pattern}
	if sb.Workspace != nil && sb.Workspace.MountType == WorkspaceMountFUSE {
		argv = append(argv, "hide-reserved")
	}
	rc, err := transport.RunFileOp(searchCtx, m.sandboxRuntimeID(sb), argv, nil)
	if err != nil {
		return runtime.FileV2SearchPage{}, err
	}
	var wire struct {
		Files     []fileV2WireInfo `json:"files"`
		Truncated bool             `json:"truncated"`
	}
	if err := readFileV2Metadata(rc, &wire); err != nil {
		return runtime.FileV2SearchPage{}, err
	}
	if len(wire.Files) > 1000 {
		return runtime.FileV2SearchPage{}, errors.New("file helper returned oversized search")
	}
	page := runtime.FileV2SearchPage{Files: make([]runtime.FileInfo, 0, min(len(wire.Files), request.Limit)), Truncated: wire.Truncated}
	last := ""
	for _, info := range wire.Files {
		if !strings.HasPrefix(info.Path, canonical+"/") || path.Clean(info.Path) != info.Path ||
			info.Name != path.Base(info.Path) || info.Path <= last || info.Size < 0 || info.IsDir {
			return runtime.FileV2SearchPage{}, errors.New("file helper returned invalid search result")
		}
		if sb.Workspace != nil && sb.Workspace.MountType == WorkspaceMountFUSE && fuseprotocol.IsReservedProbeObjectName(info.Name) {
			return runtime.FileV2SearchPage{}, errors.New("file helper returned reserved search result")
		}
		if len(page.Files) < request.Limit {
			page.Files = append(page.Files, info.runtimeInfo())
		} else {
			page.Truncated = true
		}
		last = info.Path
	}
	return page, nil
}

// GrepFilesV2 searches through the same operation gate as all other file
// methods. Its sandbox-side helper owns the directory scan and fixed rg call.
func (m *Manager) GrepFilesV2(ctx context.Context, id string, request runtime.FileV2GrepRequest) (runtime.FileV2GrepResult, error) {
	if request.Path == "" {
		request.Path = "/workspace"
	}
	canonical, err := normalizeFileV2Path(request.Path)
	if err != nil {
		return runtime.FileV2GrepResult{}, err
	}
	if request.Pattern == "" || len(request.Pattern) > 4096 || strings.IndexByte(request.Pattern, 0) >= 0 ||
		request.BeforeContext < 0 || request.BeforeContext > 10 || request.AfterContext < 0 || request.AfterContext > 10 ||
		request.Limit < 0 || request.Limit > 1000 {
		return runtime.FileV2GrepResult{}, runtime.ErrFileInvalidText
	}
	if request.Glob != "" {
		if err := filev2.ValidateGlobPattern(request.Glob); err != nil {
			return runtime.FileV2GrepResult{}, runtime.ErrFileInvalidText
		}
	}
	if request.Limit == 0 {
		request.Limit = 1000
	}
	control, err := json.Marshal(struct {
		Pattern       string   `json:"pattern"`
		Glob          string   `json:"glob,omitempty"`
		Types         []string `json:"file_types,omitempty"`
		IgnoreCase    bool     `json:"ignore_case,omitempty"`
		Multiline     bool     `json:"multiline,omitempty"`
		BeforeContext int      `json:"before_context,omitempty"`
		AfterContext  int      `json:"after_context,omitempty"`
		Limit         int      `json:"limit"`
	}{request.Pattern, request.Glob, request.FileTypes, request.IgnoreCase, request.Multiline, request.BeforeContext, request.AfterContext, request.Limit})
	if err != nil || len(control) > 8192 {
		return runtime.FileV2GrepResult{}, runtime.ErrFileInvalidText
	}
	sb, operationCtx, release, transport, err := m.fileV2Operation(ctx, id)
	if err != nil {
		return runtime.FileV2GrepResult{}, err
	}
	defer release()
	if err := validatePublicFUSEFilePath(sb, canonical); err != nil {
		return runtime.FileV2GrepResult{}, err
	}
	searchCtx, cancel := context.WithTimeout(operationCtx, 6*time.Second)
	defer cancel()
	argv := []string{"grep", canonical}
	if sb.Workspace != nil && sb.Workspace.MountType == WorkspaceMountFUSE {
		argv = append(argv, "hide-reserved")
	}
	rc, err := transport.RunFileOp(searchCtx, m.sandboxRuntimeID(sb), argv, bytes.NewReader(control))
	if err != nil {
		return runtime.FileV2GrepResult{}, err
	}
	var result runtime.FileV2GrepResult
	if err := readFileV2Metadata(rc, &result); err != nil {
		return runtime.FileV2GrepResult{}, err
	}
	if len(result.Matches) > request.Limit {
		return runtime.FileV2GrepResult{}, errors.New("file helper returned oversized grep result")
	}
	lastPath, lastLine := "", 0
	for _, match := range result.Matches {
		if !strings.HasPrefix(match.Path, canonical+"/") || path.Clean(match.Path) != match.Path ||
			match.LineNumber < 1 || match.Path < lastPath || (match.Path == lastPath && match.LineNumber < lastLine) ||
			!utf8.ValidString(match.Content) || len(match.Before) > request.BeforeContext || len(match.After) > request.AfterContext {
			return runtime.FileV2GrepResult{}, errors.New("file helper returned invalid grep result")
		}
		if sb.Workspace != nil && sb.Workspace.MountType == WorkspaceMountFUSE && fuseprotocol.IsReservedProbeObjectName(path.Base(match.Path)) {
			return runtime.FileV2GrepResult{}, errors.New("file helper returned reserved grep result")
		}
		for _, line := range append(append([]string(nil), match.Before...), match.After...) {
			if !utf8.ValidString(line) {
				return runtime.FileV2GrepResult{}, errors.New("file helper returned invalid grep context")
			}
		}
		lastPath, lastLine = match.Path, match.LineNumber
	}
	return result, nil
}

type fileV2GatedStream struct {
	reader  io.ReadCloser
	release func()
	once    sync.Once
	err     error
}

func (s *fileV2GatedStream) Read(p []byte) (int, error) {
	n, err := s.reader.Read(p)
	if err != nil {
		if closeErr := s.Close(); closeErr != nil {
			return n, errors.Join(err, closeErr)
		}
	}
	return n, err
}

func (s *fileV2GatedStream) Close() error {
	s.once.Do(func() {
		s.err = s.reader.Close()
		s.release()
	})
	return s.err
}

// ReadFileRangeV2 starts a raw byte stream. The caller must close it; EOF
// also closes the helper and only then releases the existing operation gate.
func (m *Manager) ReadFileRangeV2(ctx context.Context, id, filePath string, start, length int64) (io.ReadCloser, error) {
	canonical, err := normalizeFileV2Path(filePath)
	if err != nil {
		return nil, err
	}
	if start < 0 || length < -1 {
		return nil, errors.New("invalid file range")
	}
	sb, operationCtx, release, transport, err := m.fileV2Operation(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := validatePublicFUSEFilePath(sb, canonical); err != nil {
		release()
		return nil, err
	}
	argv := []string{"read", canonical, strconv.FormatInt(start, 10), strconv.FormatInt(length, 10)}
	rc, err := transport.RunFileOp(operationCtx, m.sandboxRuntimeID(sb), argv, nil)
	if err != nil {
		release()
		return nil, err
	}
	if rc == nil {
		release()
		return nil, errors.New("file helper returned no reader")
	}
	return &fileV2GatedStream{reader: rc, release: release}, nil
}

// WriteFileV2 sends raw bytes to one helper process. Its empty stdout and
// successful terminal status are both required before the gate is released.
func (m *Manager) WriteFileV2(ctx context.Context, id, filePath string, size int64, createParents bool, src io.Reader) error {
	canonical, err := normalizeFileV2Path(filePath)
	if err != nil {
		return err
	}
	if size < 0 || src == nil {
		return runtime.ErrInvalidUploadSize
	}
	sb, operationCtx, release, transport, err := m.fileV2Operation(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	if err := validatePublicFUSEFilePath(sb, canonical); err != nil {
		return err
	}
	argv := []string{"write", canonical, strconv.FormatInt(size, 10)}
	if createParents {
		argv = append(argv, "create-parents")
	}
	rc, err := transport.RunFileOp(operationCtx, m.sandboxRuntimeID(sb), argv, src)
	if err != nil {
		return err
	}
	if rc == nil {
		return errors.New("file helper returned no reader")
	}
	output, readErr := io.ReadAll(io.LimitReader(rc, 4097))
	closeErr := rc.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	if len(output) != 0 {
		return errors.New("file helper returned unexpected write output")
	}
	return nil
}

func (m *Manager) ReadLinesV2(ctx context.Context, id, filePath string, start, count int) (string, error) {
	canonical, err := normalizeFileV2Path(filePath)
	if err != nil {
		return "", err
	}
	if start < 1 || count < 0 {
		return "", runtime.ErrFileInvalidText
	}
	sb, operationCtx, release, transport, err := m.fileV2Operation(ctx, id)
	if err != nil {
		return "", err
	}
	defer release()
	if err := validatePublicFUSEFilePath(sb, canonical); err != nil {
		return "", err
	}
	argv := []string{"read-lines", canonical, strconv.Itoa(start), strconv.Itoa(count)}
	rc, err := transport.RunFileOp(operationCtx, m.sandboxRuntimeID(sb), argv, nil)
	if err != nil {
		return "", err
	}
	var result struct {
		Content string `json:"content"`
	}
	if err := readFileV2Metadata(rc, &result); err != nil {
		return "", err
	}
	if len(result.Content) > 1<<20 || !utf8.ValidString(result.Content) {
		return "", runtime.ErrFileLimitExceeded
	}
	return result.Content, nil
}

func (m *Manager) EditTextV2(ctx context.Context, id, filePath, oldText, newText string, replaceAll bool) error {
	canonical, err := normalizeFileV2Path(filePath)
	if err != nil {
		return err
	}
	if oldText == "" || oldText == newText || len(oldText) > 8<<20 || len(newText) > 8<<20 ||
		!utf8.ValidString(oldText) || !utf8.ValidString(newText) {
		return runtime.ErrFileInvalidText
	}
	body, err := json.Marshal(struct {
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}{oldText, newText, replaceAll})
	if err != nil || len(body) > 16<<20 {
		return runtime.ErrFileLimitExceeded
	}
	sb, operationCtx, release, transport, err := m.fileV2Operation(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	if err := validatePublicFUSEFilePath(sb, canonical); err != nil {
		return err
	}
	rc, err := transport.RunFileOp(operationCtx, m.sandboxRuntimeID(sb), []string{"edit", canonical}, bytes.NewReader(body))
	if err != nil {
		return err
	}
	var result struct {
		Replacements int `json:"replacements"`
	}
	if err := readFileV2Metadata(rc, &result); err != nil {
		return err
	}
	if result.Replacements < 1 {
		return errors.New("file helper returned invalid edit result")
	}
	return nil
}
