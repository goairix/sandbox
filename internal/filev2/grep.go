package filev2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	grepBatchSize  = 32
	maxGrepControl = 8192
	maxGrepOutput  = 8 << 20
	maxGrepContext = 10
)

type grepRequest struct {
	Pattern       string   `json:"pattern"`
	Glob          string   `json:"glob,omitempty"`
	Types         []string `json:"file_types,omitempty"`
	IgnoreCase    bool     `json:"ignore_case,omitempty"`
	Multiline     bool     `json:"multiline,omitempty"`
	BeforeContext int      `json:"before_context,omitempty"`
	AfterContext  int      `json:"after_context,omitempty"`
	Limit         int      `json:"limit,omitempty"`
}

type fileGrepMatch struct {
	Path       string   `json:"path"`
	LineNumber int      `json:"line_number"`
	Content    string   `json:"content"`
	Before     []string `json:"before,omitempty"`
	After      []string `json:"after,omitempty"`
}

type fileGrepResult struct {
	Matches   []fileGrepMatch `json:"matches"`
	Truncated bool            `json:"truncated"`
}

func decodeGrepRequest(source io.Reader) (grepRequest, error) {
	var request grepRequest
	if source == nil {
		return request, ErrTextInvalid
	}
	body, err := io.ReadAll(io.LimitReader(source, maxGrepControl+1))
	if err != nil {
		return request, err
	}
	if len(body) > maxGrepControl {
		return request, ErrTextLimit
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, ErrTextInvalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return request, ErrTextInvalid
	}
	return request, validateGrepRequest(request)
}

// File types are a deliberately small, explicit subset of ripgrep's
// extension filters. Unknown types fail rather than silently broadening a scan.
var grepTypeExtensions = map[string][]string{
	"go": {".go"}, "py": {".py"}, "js": {".js", ".jsx", ".mjs", ".cjs"},
	"ts": {".ts", ".tsx", ".mts", ".cts"}, "json": {".json"},
	"md": {".md", ".markdown"}, "txt": {".txt"},
	"html": {".html", ".htm"}, "css": {".css"},
	"yaml": {".yaml", ".yml"}, "rs": {".rs"},
	"sh": {".sh", ".bash", ".zsh"},
}

func validateGrepRequest(request grepRequest) error {
	if request.Pattern == "" || len(request.Pattern) > 4096 || strings.IndexByte(request.Pattern, 0) >= 0 {
		return ErrTextInvalid
	}
	if request.Glob != "" {
		if err := ValidateGlobPattern(request.Glob); err != nil {
			return err
		}
	}
	if request.BeforeContext < 0 || request.BeforeContext > maxGrepContext ||
		request.AfterContext < 0 || request.AfterContext > maxGrepContext ||
		request.Limit < 0 || request.Limit > maxSearchResults || len(request.Types) > 16 {
		return ErrTextInvalid
	}
	for _, kind := range request.Types {
		if _, ok := grepTypeExtensions[kind]; !ok {
			return ErrTextInvalid
		}
	}
	return nil
}

func grepAcceptsType(name string, types []string) bool {
	if len(types) == 0 {
		return true
	}
	extension := path.Ext(name)
	for _, kind := range types {
		for _, allowed := range grepTypeExtensions[kind] {
			if extension == allowed {
				return true
			}
		}
	}
	return false
}

type pinnedGrepFile struct {
	file *os.File
	path string
}

type boundedGrepOutput struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedGrepOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		b.overflow = true
		return 0, ErrTextLimit
	}
	return b.Buffer.Write(p)
}

type rgJSONEvent struct {
	Type string `json:"type"`
	Data struct {
		Path struct {
			Text  *string `json:"text"`
			Bytes string  `json:"bytes"`
		} `json:"path"`
		Lines struct {
			Text  *string `json:"text"`
			Bytes string  `json:"bytes"`
		} `json:"lines"`
		LineNumber   *int            `json:"line_number"`
		BinaryOffset json.RawMessage `json:"binary_offset"`
	} `json:"data"`
}

func validateRipgrepPattern(ctx context.Context, binary string, request grepRequest) error {
	args := []string{"--json", "--no-config", "-e", request.Pattern, "-"}
	if request.Multiline {
		args = append([]string{"--multiline"}, args...)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdin = bytes.NewReader(nil)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if err == nil {
		return nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return nil // Valid regex, no match on empty input.
	}
	if errors.As(err, &exit) && exit.ExitCode() == 2 {
		return ErrTextInvalid
	}
	return err
}

func grepArguments(request grepRequest, files int) []string {
	args := []string{"--json", "--no-config", "--no-ignore", "--hidden", "--color", "never"}
	if request.IgnoreCase {
		args = append(args, "--ignore-case")
	}
	if request.Multiline {
		args = append(args, "--multiline")
	}
	if request.BeforeContext > 0 {
		args = append(args, "--before-context", strconv.Itoa(request.BeforeContext))
	}
	if request.AfterContext > 0 {
		args = append(args, "--after-context", strconv.Itoa(request.AfterContext))
	}
	args = append(args, "-e", request.Pattern, "--")
	for i := 0; i < files; i++ {
		args = append(args, "/proc/self/fd/"+strconv.Itoa(3+i))
	}
	return args
}

func parseGrepBatch(output []byte, paths map[string]string, request grepRequest) ([]fileGrepMatch, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(output))
	matches := make([]fileGrepMatch, 0)
	var current *fileGrepMatch
	var before []string
	currentPath := ""
	truncated := false
	for {
		var event rgJSONEvent
		if err := decoder.Decode(&event); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, false, fmt.Errorf("invalid ripgrep JSON: %w", err)
		}
		if event.Type == "summary" {
			continue
		}
		if event.Data.Path.Text == nil || event.Data.Path.Bytes != "" {
			return nil, false, errors.New("ripgrep returned a non-text path")
		}
		original, ok := paths[*event.Data.Path.Text]
		if !ok {
			return nil, false, errors.New("ripgrep returned an unexpected file")
		}
		if original != currentPath {
			currentPath, current, before = original, nil, nil
		}
		switch event.Type {
		case "begin":
			current, before = nil, nil
		case "match":
			if event.Data.Lines.Text == nil || event.Data.Lines.Bytes != "" ||
				event.Data.LineNumber == nil || *event.Data.LineNumber < 1 {
				return nil, false, errors.New("ripgrep returned invalid match text")
			}
			match := fileGrepMatch{Path: original, LineNumber: *event.Data.LineNumber, Content: *event.Data.Lines.Text}
			match.Before = append(match.Before, before...)
			matches = append(matches, match)
			current = &matches[len(matches)-1]
			before = nil
		case "context":
			if event.Data.Lines.Text == nil || event.Data.Lines.Bytes != "" {
				return nil, false, errors.New("ripgrep returned invalid context text")
			}
			line := *event.Data.Lines.Text
			if current != nil && len(current.After) < request.AfterContext {
				current.After = append(current.After, line)
			}
			if request.BeforeContext > 0 {
				before = append(before, line)
				if len(before) > request.BeforeContext {
					before = before[1:]
				}
			}
		case "end":
			if len(event.Data.BinaryOffset) > 0 && string(event.Data.BinaryOffset) != "null" {
				truncated = true
			}
			current, before = nil, nil
		default:
			return nil, false, errors.New("ripgrep returned unknown event")
		}
	}
	return matches, truncated, nil
}

// runGrepWithBinary sends only pinned regular-file descriptors to ripgrep.
// No user path is supplied to that subprocess, so a renamed symlink cannot
// redirect its reads outside the opened search root.
func runGrepWithBinary(ctx context.Context, rootFD int, relative, absolute string, request grepRequest, hideReserved bool, binary string, output io.Writer) error {
	if err := validateGrepRequest(request); err != nil {
		return err
	}
	searchCtx, cancel := context.WithTimeout(ctx, searchDeadline)
	defer cancel()
	if err := validateRipgrepPattern(searchCtx, binary, request); err != nil {
		return err
	}
	fd, err := openBeneath(rootFD, relative, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(fd), absolute)
	defer dir.Close()
	if request.Limit == 0 {
		request.Limit = maxSearchResults
	}
	result := fileGrepResult{Matches: make([]fileGrepMatch, 0)}
	batch := make([]pinnedGrepFile, 0, grepBatchSize)
	defer func() {
		for _, item := range batch {
			_ = item.file.Close()
		}
	}()
	contentBytes := int64(0)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		args := grepArguments(request, len(batch))
		command := exec.CommandContext(searchCtx, binary, args...)
		paths := make(map[string]string, len(batch))
		for i, item := range batch {
			command.ExtraFiles = append(command.ExtraFiles, item.file)
			paths["/proc/self/fd/"+strconv.Itoa(3+i)] = item.path
		}
		stdout := &boundedGrepOutput{limit: maxGrepOutput}
		command.Stdout, command.Stderr = stdout, io.Discard
		runErr := command.Run()
		for _, item := range batch {
			_ = item.file.Close()
		}
		batch = batch[:0]
		if stdout.overflow {
			return ErrTextLimit
		}
		var exit *exec.ExitError
		if runErr != nil && !(errors.As(runErr, &exit) && exit.ExitCode() == 1) {
			if searchCtx.Err() != nil {
				return ErrTextLimit
			}
			return fmt.Errorf("ripgrep search failed: %w", runErr)
		}
		found, partial, err := parseGrepBatch(stdout.Bytes(), paths, request)
		if err != nil {
			return err
		}
		result.Truncated = result.Truncated || partial
		result.Matches = append(result.Matches, found...)
		sort.Slice(result.Matches, func(i, j int) bool {
			if result.Matches[i].Path == result.Matches[j].Path {
				return result.Matches[i].LineNumber < result.Matches[j].LineNumber
			}
			return result.Matches[i].Path < result.Matches[j].Path
		})
		if len(result.Matches) > request.Limit {
			result.Truncated = true
			result.Matches = result.Matches[:request.Limit]
		}
		return nil
	}
	walker := pinnedSearchWalker{ctx: searchCtx, deadline: time.Now().Add(searchDeadline), hideReserved: hideReserved}
	walker.visit = func(file *os.File, childPath, childRelative string, info os.FileInfo) error {
		if request.Glob != "" && !matchGlob(request.Glob, childRelative) {
			return nil
		}
		if !grepAcceptsType(info.Name(), request.Types) {
			return nil
		}
		if info.Size() < 0 || info.Size() > maxSearchBytes-contentBytes {
			walker.truncated, walker.stopRequested = true, true
			return nil
		}
		contentBytes += info.Size()
		dup, err := unix.Dup(int(file.Fd()))
		if err != nil {
			return err
		}
		unix.CloseOnExec(dup)
		batch = append(batch, pinnedGrepFile{file: os.NewFile(uintptr(dup), childPath), path: childPath})
		if len(batch) == grepBatchSize {
			return flush()
		}
		return nil
	}
	if err := walker.walk(dir, absolute, "", 0); err != nil {
		if searchCtx.Err() != nil && ctx.Err() == nil {
			return ErrTextLimit
		}
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	result.Truncated = result.Truncated || walker.truncated
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if len(encoded) > maxGrepOutput {
		return ErrTextLimit
	}
	_, err = output.Write(append(encoded, '\n'))
	return err
}
