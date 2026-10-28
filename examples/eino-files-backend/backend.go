// Package einofiles adapts the generic Sandbox Files v2 API to Eino's
// filesystem.Backend. It is deliberately separate from the service and SDK.
package einofiles

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/cloudwego/eino/adk/filesystem"
	sandbox "github.com/goairix/sandbox/sdk/go"
)

const (
	workspaceRoot = "/workspace"
	listPageSize  = 1000
	maxListPages  = 10
)

// ErrIncompleteSearch prevents an agent from treating a bounded scan as a
// complete answer. Both GlobInfo and GrepRaw use this error.
var ErrIncompleteSearch = errors.New("sandbox files search is incomplete")

// FileClient is the Files v2 surface used by this adapter. The production SDK
// handle implements it; keeping it small makes the adapter contract testable.
type FileClient interface {
	List(context.Context, string, string, int) (sandbox.FileV2ListPage, error)
	Glob(context.Context, sandbox.FileV2GlobRequest) (sandbox.FileV2SearchPage, error)
	Grep(context.Context, sandbox.FileV2GrepRequest) (sandbox.FileV2GrepResult, error)
	ReadLines(context.Context, string, int, int) (string, error)
	Write(context.Context, string, io.Reader, int64, bool) error
	Edit(context.Context, string, string, string, bool) error
}

type Backend struct{ files FileClient }

var _ filesystem.Backend = (*Backend)(nil)
var _ FileClient = (*sandbox.Files)(nil)

func NewBackend(files FileClient) *Backend { return &Backend{files: files} }

// sandboxPath maps Eino's virtual / to the sandbox workspace without allowing
// parent traversal to be normalized away before validation.
func sandboxPath(name string) (string, error) {
	if strings.IndexByte(name, 0) >= 0 {
		return "", errors.New("invalid file path")
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", errors.New("parent traversal is not allowed")
		}
	}
	clean := path.Clean("/" + strings.TrimPrefix(name, "/"))
	if clean == "/" {
		return workspaceRoot, nil
	}
	return workspaceRoot + clean, nil
}

func virtualPath(name string) (string, error) {
	if name == workspaceRoot {
		return "/", nil
	}
	if !strings.HasPrefix(name, workspaceRoot+"/") {
		return "", fmt.Errorf("unexpected sandbox path %q", name)
	}
	return strings.TrimPrefix(name, workspaceRoot), nil
}

func convertInfo(info sandbox.FileInfo) (filesystem.FileInfo, error) {
	virtual, err := virtualPath(info.Path)
	if err != nil {
		return filesystem.FileInfo{}, err
	}
	return filesystem.FileInfo{
		Path: virtual, IsDir: info.IsDir, Size: info.Size,
		ModifiedAt: info.ModTime.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
	}, nil
}

func (b *Backend) LsInfo(ctx context.Context, req *filesystem.LsInfoRequest) ([]filesystem.FileInfo, error) {
	if req == nil {
		return nil, errors.New("missing list request")
	}
	root, err := sandboxPath(req.Path)
	if err != nil {
		return nil, err
	}
	result := make([]filesystem.FileInfo, 0)
	cursor := ""
	seen := make(map[string]struct{})
	for pageNumber := 0; pageNumber < maxListPages; pageNumber++ {
		page, err := b.files.List(ctx, root, cursor, listPageSize)
		if err != nil {
			return nil, err
		}
		for _, info := range page.Files {
			converted, err := convertInfo(info)
			if err != nil {
				return nil, err
			}
			result = append(result, converted)
		}
		if page.NextCursor == "" {
			return result, nil
		}
		if _, duplicate := seen[page.NextCursor]; duplicate {
			return nil, ErrIncompleteSearch
		}
		seen[page.NextCursor] = struct{}{}
		cursor = page.NextCursor
	}
	return nil, ErrIncompleteSearch
}

func (b *Backend) Read(ctx context.Context, req *filesystem.ReadRequest) (*filesystem.FileContent, error) {
	if req == nil || req.Limit < 0 {
		return nil, errors.New("invalid read request")
	}
	name, err := sandboxPath(req.FilePath)
	if err != nil {
		return nil, err
	}
	start := req.Offset
	if start < 1 {
		start = 1
	}
	content, err := b.files.ReadLines(ctx, name, start, req.Limit)
	if err != nil {
		return nil, err
	}
	return &filesystem.FileContent{Content: content}, nil
}

func (b *Backend) GrepRaw(ctx context.Context, req *filesystem.GrepRequest) ([]filesystem.GrepMatch, error) {
	page, err := b.grepDetailed(ctx, req)
	if err != nil {
		return nil, err
	}
	result := make([]filesystem.GrepMatch, 0, len(page.Matches))
	for _, match := range page.Matches {
		result = append(result, filesystem.GrepMatch{Path: match.Path, Line: match.LineNumber, Content: match.Content})
	}
	return result, nil
}

// grepDetailed retains Files v2 context lines for the richer Eino grep tool.
// The standard Backend interface has no fields for that context.
func (b *Backend) grepDetailed(ctx context.Context, req *filesystem.GrepRequest) (sandbox.FileV2GrepResult, error) {
	if req == nil {
		return sandbox.FileV2GrepResult{}, errors.New("missing grep request")
	}
	root, err := sandboxPath(req.Path)
	if err != nil {
		return sandbox.FileV2GrepResult{}, err
	}
	fileTypes := []string(nil)
	if req.FileType != "" {
		kind := req.FileType
		if kind == "rust" {
			kind = "rs"
		}
		fileTypes = []string{kind}
	}
	page, err := b.files.Grep(ctx, sandbox.FileV2GrepRequest{
		Path: root, Pattern: req.Pattern, Glob: req.Glob, FileTypes: fileTypes,
		IgnoreCase: req.CaseInsensitive, Multiline: req.EnableMultiline,
		BeforeContext: req.BeforeLines, AfterContext: req.AfterLines,
	})
	if err != nil {
		return sandbox.FileV2GrepResult{}, err
	}
	if page.Truncated {
		return sandbox.FileV2GrepResult{}, ErrIncompleteSearch
	}
	// Do not mutate a client-owned result: test doubles and SDK wrappers may
	// reuse the same slice across calls.
	page.Matches = append([]sandbox.FileV2Match(nil), page.Matches...)
	for i := range page.Matches {
		virtual, err := virtualPath(page.Matches[i].Path)
		if err != nil {
			return sandbox.FileV2GrepResult{}, err
		}
		page.Matches[i].Path = virtual
	}
	return page, nil
}

func (b *Backend) GlobInfo(ctx context.Context, req *filesystem.GlobInfoRequest) ([]filesystem.FileInfo, error) {
	if req == nil {
		return nil, errors.New("missing glob request")
	}
	root, err := sandboxPath(req.Path)
	if err != nil {
		return nil, err
	}
	page, err := b.files.Glob(ctx, sandbox.FileV2GlobRequest{Path: root, Pattern: req.Pattern})
	if err != nil {
		return nil, err
	}
	if page.Truncated {
		return nil, ErrIncompleteSearch
	}
	result := make([]filesystem.FileInfo, 0, len(page.Files))
	for _, info := range page.Files {
		converted, err := convertInfo(info)
		if err != nil {
			return nil, err
		}
		result = append(result, converted)
	}
	return result, nil
}

func (b *Backend) Write(ctx context.Context, req *filesystem.WriteRequest) error {
	if req == nil {
		return errors.New("missing write request")
	}
	name, err := sandboxPath(req.FilePath)
	if err != nil {
		return err
	}
	return b.files.Write(ctx, name, strings.NewReader(req.Content), int64(len(req.Content)), true)
}

func (b *Backend) Edit(ctx context.Context, req *filesystem.EditRequest) error {
	if req == nil {
		return errors.New("missing edit request")
	}
	name, err := sandboxPath(req.FilePath)
	if err != nil {
		return err
	}
	return b.files.Edit(ctx, name, req.OldString, req.NewString, req.ReplaceAll)
}
