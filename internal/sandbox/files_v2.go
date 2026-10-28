package sandbox

import (
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
