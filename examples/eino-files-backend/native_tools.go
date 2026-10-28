package einofiles

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/cloudwego/eino/adk/filesystem"
	einoFS "github.com/cloudwego/eino/adk/middlewares/filesystem"
	"github.com/cloudwego/eino/components/tool/utils"
	sandbox "github.com/goairix/sandbox/sdk/go"
)

const maxNativeToolOutput = 64 << 10

type nativeLsArgs struct {
	Path string `json:"path" jsonschema:"description=Directory to list; defaults to /"`
}

// Keep Eino's existing grep argument names so callers can switch middleware
// without changing tool calls. Only the default presentation changes.
type nativeGrepArgs struct {
	Pattern         string  `json:"pattern" jsonschema:"description=Regular expression to search for"`
	Path            *string `json:"path,omitempty"`
	Glob            *string `json:"glob,omitempty"`
	OutputMode      string  `json:"output_mode,omitempty" jsonschema:"description=Choose content by default or files_with_matches or count,enum=content,enum=files_with_matches,enum=count"`
	Context         *int    `json:"-C,omitempty"`
	BeforeLines     *int    `json:"-B,omitempty"`
	AfterLines      *int    `json:"-A,omitempty"`
	ShowLineNumbers *bool   `json:"-n,omitempty"`
	CaseInsensitive *bool   `json:"-i,omitempty"`
	FileType        *string `json:"type,omitempty"`
	HeadLimit       *int    `json:"head_limit,omitempty"`
	Offset          *int    `json:"offset,omitempty"`
	Multiline       *bool   `json:"multiline,omitempty"`
}

func nativeToolConfigs(backend *Backend) (*einoFS.ToolConfig, *einoFS.ToolConfig, error) {
	ls, err := utils.InferTool(einoFS.ToolNameLs,
		"List files like ls -alh: mode, links, numeric owner/group, human-readable size, UTC modification time, and path. Hidden entries are included.",
		func(ctx context.Context, input nativeLsArgs) (string, error) {
			return nativeLs(ctx, backend, input)
		})
	if err != nil {
		return nil, nil, err
	}
	grep, err := utils.InferTool(einoFS.ToolNameGrep,
		"Search file contents like rg -n: path, line number, and matching text. Supports -A/-B/-C, -i, glob, type, count, and files_with_matches.",
		func(ctx context.Context, input nativeGrepArgs) (string, error) {
			return nativeGrep(ctx, backend, input)
		})
	if err != nil {
		return nil, nil, err
	}
	return &einoFS.ToolConfig{CustomTool: ls}, &einoFS.ToolConfig{CustomTool: grep}, nil
}

func nativeLs(ctx context.Context, backend *Backend, input nativeLsArgs) (string, error) {
	infos, err := backend.listDetailed(ctx, &filesystem.LsInfoRequest{Path: input.Path})
	if err != nil {
		return "", err
	}
	// Keep the virtual absolute path so an agent can use it in the next tool
	// call. SDK responses from older servers lack Unix details and show ?.
	sort.Slice(infos, func(i, j int) bool { return infos[i].Path < infos[j].Path })
	type row struct {
		mode, links, owner, group, size, modified, name string
	}
	rows := make([]row, 0, len(infos))
	linkWidth, ownerWidth, groupWidth, sizeWidth := 0, 0, 0, 0
	for _, info := range infos {
		name, err := virtualPath(info.Path)
		if err != nil {
			return "", err
		}
		if info.IsDir {
			name = strings.TrimSuffix(name, "/") + "/"
		}
		if strings.IndexFunc(name, unicode.IsControl) >= 0 {
			name = strconv.Quote(name)
		}
		links, owner, group := "?", "?", "?"
		if info.Nlink > 0 {
			links = strconv.FormatUint(info.Nlink, 10)
			owner = strconv.FormatUint(uint64(info.UID), 10)
			group = strconv.FormatUint(uint64(info.GID), 10)
		}
		modified := "-"
		if !info.ModTime.IsZero() {
			modified = info.ModTime.UTC().Format("Jan _2 15:04 2006")
		}
		entry := row{lsMode(info), links, owner, group, humanSize(info.Size), modified, name}
		rows = append(rows, entry)
		linkWidth = max(linkWidth, len(links))
		ownerWidth = max(ownerWidth, len(owner))
		groupWidth = max(groupWidth, len(group))
		sizeWidth = max(sizeWidth, len(entry.size))
	}
	lines := make([]string, 0, len(rows))
	for _, entry := range rows {
		lines = append(lines, fmt.Sprintf("%s %*s %-*s %-*s %*s %s %s",
			entry.mode, linkWidth, entry.links, ownerWidth, entry.owner,
			groupWidth, entry.group, sizeWidth, entry.size, entry.modified, entry.name))
	}
	return strings.Join(lines, "\n"), nil
}

func lsMode(info sandbox.FileInfo) string {
	if len(info.Mode) == 10 && strings.IndexFunc(info.Mode, unicode.IsControl) < 0 {
		if info.Mode[0] == 'L' {
			return "l" + info.Mode[1:]
		}
		return info.Mode
	}
	if info.IsDir {
		return "d?????????"
	}
	return "-?????????"
}

func humanSize(size int64) string {
	if size < 1024 {
		return strconv.FormatInt(size, 10)
	}
	value := float64(size)
	for _, unit := range []string{"K", "M", "G", "T", "P", "E"} {
		value /= 1024
		if value < 1024 || unit == "E" {
			return fmt.Sprintf("%.1f%s", math.Ceil(value*10)/10, unit)
		}
	}
	return strconv.FormatInt(size, 10)
}

func nativeGrep(ctx context.Context, backend *Backend, input nativeGrepArgs) (string, error) {
	mode := input.OutputMode
	if mode == "" {
		mode = "content"
	}
	if mode != "content" && mode != "files_with_matches" && mode != "count" {
		return "", fmt.Errorf("unsupported grep output_mode %q", mode)
	}
	offset, limit := intOrZero(input.Offset), intOrZero(input.HeadLimit)
	if offset < 0 || limit < 0 {
		return "", errors.New("grep offset and head_limit must be nonnegative")
	}
	before, after := intOrZero(input.BeforeLines), intOrZero(input.AfterLines)
	if input.Context != nil {
		before, after = *input.Context, *input.Context
	}
	if before < 0 || after < 0 || before > 10 || after > 10 {
		return "", errors.New("grep context must be between 0 and 10 lines")
	}
	if mode != "content" {
		before, after = 0, 0
	}
	page, err := backend.grepDetailed(ctx, &filesystem.GrepRequest{
		Pattern: input.Pattern, Path: stringOrEmpty(input.Path), Glob: stringOrEmpty(input.Glob),
		FileType: stringOrEmpty(input.FileType), CaseInsensitive: boolOrFalse(input.CaseInsensitive),
		EnableMultiline: boolOrFalse(input.Multiline), BeforeLines: before, AfterLines: after,
	})
	if err != nil {
		return "", err
	}
	sort.SliceStable(page.Matches, func(i, j int) bool {
		if page.Matches[i].Path != page.Matches[j].Path {
			return page.Matches[i].Path < page.Matches[j].Path
		}
		return page.Matches[i].LineNumber < page.Matches[j].LineNumber
	})
	switch mode {
	case "files_with_matches", "count":
		counts := make(map[string]int)
		for _, match := range page.Matches {
			counts[match.Path]++
		}
		paths := make([]string, 0, len(counts))
		for path := range counts {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		paths = paginate(paths, offset, limit)
		if mode == "count" {
			for i, path := range paths {
				paths[i] = path + ":" + strconv.Itoa(counts[path])
			}
		}
		return boundedToolOutput(paths)
	default:
		lines := grepContentLines(page.Matches, input.ShowLineNumbers == nil || *input.ShowLineNumbers)
		return boundedToolOutput(paginate(lines, offset, limit))
	}
}

type grepOutputLine struct {
	path    string
	line    int
	content string
	match   bool
}

func grepContentLines(matches []sandbox.FileV2Match, lineNumbers bool) []string {
	rows := make(map[string]grepOutputLine)
	for _, match := range matches {
		for i, content := range match.Before {
			addGrepLine(rows, grepOutputLine{match.Path, match.LineNumber - len(match.Before) + i, content, false})
		}
		parts := strings.Split(strings.TrimSuffix(match.Content, "\n"), "\n")
		for i, content := range parts {
			addGrepLine(rows, grepOutputLine{match.Path, match.LineNumber + i, content, true})
		}
		for i, content := range match.After {
			addGrepLine(rows, grepOutputLine{match.Path, match.LineNumber + len(parts) + i, content, false})
		}
	}
	ordered := make([]grepOutputLine, 0, len(rows))
	for _, row := range rows {
		ordered = append(ordered, row)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].path != ordered[j].path {
			return ordered[i].path < ordered[j].path
		}
		return ordered[i].line < ordered[j].line
	})
	lines := make([]string, 0, len(ordered))
	for _, row := range ordered {
		separator := "-"
		if row.match {
			separator = ":"
		}
		if lineNumbers {
			lines = append(lines, row.path+separator+strconv.Itoa(row.line)+separator+strings.TrimSuffix(row.content, "\n"))
		} else {
			lines = append(lines, row.path+separator+strings.TrimSuffix(row.content, "\n"))
		}
	}
	return lines
}

func addGrepLine(rows map[string]grepOutputLine, row grepOutputLine) {
	key := row.path + "\x00" + strconv.Itoa(row.line)
	// Adjacent matches can share context lines. The actual match takes
	// precedence when the same path and line appears more than once.
	if previous, exists := rows[key]; !exists || !previous.match {
		rows[key] = row
	}
}

func paginate[T any](items []T, offset, limit int) []T {
	if offset >= len(items) {
		return nil
	}
	items = items[offset:]
	if limit > 0 && limit < len(items) {
		items = items[:limit]
	}
	return items
}

func boundedToolOutput(lines []string) (string, error) {
	result := strings.Join(lines, "\n")
	if len(result) > maxNativeToolOutput {
		return "", fmt.Errorf("tool output exceeds %d bytes; narrow the path or use head_limit", maxNativeToolOutput)
	}
	return result, nil
}

func intOrZero(value *int) int {
	if value != nil {
		return *value
	}
	return 0
}

func stringOrEmpty(value *string) string {
	if value != nil {
		return *value
	}
	return ""
}

func boolOrFalse(value *bool) bool {
	return value != nil && *value
}
