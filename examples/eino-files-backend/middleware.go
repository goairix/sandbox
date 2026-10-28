package einofiles

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/filesystem"
	einoFS "github.com/cloudwego/eino/adk/middlewares/filesystem"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/pmezard/go-difflib/difflib"
)

const (
	maxDiffSnapshotBytes = 256 << 10
	maxDiffOutputBytes   = 64 << 10
)

// DiffFileClient adds raw reads only for the optional diff-producing edit tool.
// Implementations of the standard six-tool FileClient contract need no change.
type DiffFileClient interface {
	FileClient
	Read(context.Context, string) (io.ReadCloser, error)
}

// MiddlewareOption adds one execution mode to the six existing file tools.
// Eino exposes a single execute tool, so Shell and StreamingShell are exclusive.
type MiddlewareOption func(*einoFS.MiddlewareConfig) error

func WithShell(shell filesystem.Shell) MiddlewareOption {
	return func(config *einoFS.MiddlewareConfig) error {
		if shell == nil || config.StreamingShell != nil || config.Shell != nil {
			return errors.New("invalid or duplicate shell configuration")
		}
		config.Shell = shell
		return nil
	}
}

func WithStreamingShell(shell filesystem.StreamingShell) MiddlewareOption {
	return func(config *einoFS.MiddlewareConfig) error {
		if shell == nil || config.Shell != nil || config.StreamingShell != nil {
			return errors.New("invalid or duplicate streaming shell configuration")
		}
		config.StreamingShell = shell
		return nil
	}
}

func applyMiddlewareOptions(config *einoFS.MiddlewareConfig, options []MiddlewareOption) error {
	for _, option := range options {
		if option == nil {
			return errors.New("nil middleware option")
		}
		if err := option(config); err != nil {
			return err
		}
	}
	return nil
}

// NewMiddleware exposes Eino's six filesystem tools through Files v2. The ls
// and grep tools include native-style metadata and matching lines by default.
func NewMiddleware(ctx context.Context, files FileClient, options ...MiddlewareOption) (adk.ChatModelAgentMiddleware, error) {
	if files == nil {
		return nil, errors.New("missing sandbox files client")
	}
	backend := NewBackend(files)
	ls, grep, err := nativeToolConfigs(backend)
	if err != nil {
		return nil, err
	}
	config := &einoFS.MiddlewareConfig{Backend: backend, LsToolConfig: ls, GrepToolConfig: grep}
	if err := applyMiddlewareOptions(config, options); err != nil {
		return nil, err
	}
	return einoFS.New(ctx, config)
}

// NewDiffMiddleware replaces only Eino's edit_file tool. Eino's default tool
// returns a fixed success sentence; this variant returns a bounded unified
// diff after a successful Files v2 edit and an independent readback.
func NewDiffMiddleware(ctx context.Context, files DiffFileClient, options ...MiddlewareOption) (adk.ChatModelAgentMiddleware, error) {
	if files == nil {
		return nil, errors.New("missing sandbox files client")
	}
	backend := NewBackend(files)
	ls, grep, err := nativeToolConfigs(backend)
	if err != nil {
		return nil, err
	}
	edit, err := utils.InferTool(einoFS.ToolNameEditFile,
		"Replace exact text in a file and show a unified diff of the observed change.",
		func(ctx context.Context, input diffEditRequest) (string, error) {
			return editWithDiff(ctx, files, backend, input)
		})
	if err != nil {
		return nil, err
	}
	config := &einoFS.MiddlewareConfig{
		Backend:            backend,
		LsToolConfig:       ls,
		GrepToolConfig:     grep,
		EditFileToolConfig: &einoFS.ToolConfig{CustomTool: edit},
	}
	if err := applyMiddlewareOptions(config, options); err != nil {
		return nil, err
	}
	return einoFS.New(ctx, config)
}

type diffEditRequest struct {
	FilePath   string `json:"file_path" jsonschema:"description=The path to the file to modify"`
	OldString  string `json:"old_string" jsonschema:"description=The exact text to replace"`
	NewString  string `json:"new_string" jsonschema:"description=The replacement text"`
	ReplaceAll bool   `json:"replace_all" jsonschema:"description=Replace all occurrences (default false),default=false"`
}

func editWithDiff(ctx context.Context, files DiffFileClient, backend *Backend, input diffEditRequest) (string, error) {
	name, err := sandboxPath(input.FilePath)
	if err != nil {
		return "", err
	}
	if input.OldString == "" || input.OldString == input.NewString {
		return "", errors.New("old_string must be nonempty and differ from new_string")
	}
	before, err := readDiffSnapshot(ctx, files, name)
	if err != nil {
		return "", err // No edit has been sent yet.
	}
	if err := backend.Edit(ctx, &filesystem.EditRequest{
		FilePath: input.FilePath, OldString: input.OldString,
		NewString: input.NewString, ReplaceAll: input.ReplaceAll,
	}); err != nil {
		return "", err
	}
	after, err := readDiffSnapshot(ctx, files, name)
	if err != nil {
		return fmt.Sprintf("Edit applied to %s; diff unavailable: %v", input.FilePath, err), nil
	}
	expected := strings.Replace(before, input.OldString, input.NewString, 1)
	if input.ReplaceAll {
		expected = strings.ReplaceAll(before, input.OldString, input.NewString)
	}
	if after != expected {
		return fmt.Sprintf("Edit applied to %s; observed content differs from the requested replacement, so no diff is claimed", input.FilePath), nil
	}
	result, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A: difflib.SplitLines(before), B: difflib.SplitLines(after),
		FromFile: input.FilePath, ToFile: input.FilePath, Context: 3,
	})
	if err != nil || len(result) > maxDiffOutputBytes {
		return fmt.Sprintf("Edit applied to %s; diff unavailable or exceeds %d bytes", input.FilePath, maxDiffOutputBytes), nil
	}
	return result, nil
}

// readDiffSnapshot closes every stream before returning so the sandbox's file
// operation gate is released. A large or non-UTF-8 file is rejected before
// editing; readback failures after Edit are reported as applied/unknown diff.
func readDiffSnapshot(ctx context.Context, files DiffFileClient, name string) (string, error) {
	stream, err := files.Read(ctx, name)
	if err != nil {
		return "", err
	}
	data, readErr := io.ReadAll(io.LimitReader(stream, maxDiffSnapshotBytes+1))
	closeErr := stream.Close()
	if readErr != nil {
		return "", readErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if len(data) > maxDiffSnapshotBytes {
		return "", fmt.Errorf("file exceeds %d-byte diff snapshot limit", maxDiffSnapshotBytes)
	}
	if !utf8.Valid(data) {
		return "", errors.New("diff requires UTF-8 text")
	}
	return string(data), nil
}
