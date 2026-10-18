// Package sandboxfiles implements finite, unprivileged sandbox file operations.
package sandboxfiles

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/goairix/sandbox/pkg/types"
)

const (
	MaxInputBytes  = 1 << 20
	MaxOutputBytes = 1 << 20
	MaxStderrBytes = 64 << 10
	// MaxContentBytes limits individual file payloads to 512 KiB. Binary upload
	// uses JSON base64 within MaxInputBytes (1 MiB), also the signed Exec stdin
	// ceiling; neither this helper nor HTTP admission offers a large-file stream.
	MaxContentBytes = 512 << 10
	MaxEntries      = 10000
	MaxDepth        = 64
)

// Request is one finite unprivileged file operation. It conveys no control,
// ownership or reusable execution capability; the original signed Exec binds
// its complete encoded bytes before the helper can access the filesystem.
type Request struct {
	Operation  string `json:"operation"`
	Path       string `json:"path"`
	Pattern    string `json:"pattern,omitempty"`
	MaxDepth   int    `json:"max_depth,omitempty"`
	Page       int    `json:"page,omitempty"`
	PageSize   int    `json:"page_size,omitempty"`
	StartLine  int    `json:"start_line,omitempty"`
	EndLine    int    `json:"end_line,omitempty"`
	OldStr     string `json:"old_str,omitempty"`
	NewStr     string `json:"new_str,omitempty"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
	NewContent string `json:"new_content,omitempty"`
	// Content is binary input for upload only. JSON encodes it as base64; an
	// absent value on upload denotes an empty file, not an omitted operation.
	Content []byte `json:"content,omitempty"`
}

type Failure struct {
	Code string `json:"code"`
}

type Result struct {
	Error      *Failure         `json:"error,omitempty"`
	Content    []byte           `json:"content,omitempty"`
	Files      []types.FileInfo `json:"files,omitempty"`
	TotalCount int              `json:"total_count,omitempty"`
	Page       int              `json:"page,omitempty"`
	PageSize   int              `json:"page_size,omitempty"`
	Lines      []string         `json:"lines,omitempty"`
	StartLine  int              `json:"start_line,omitempty"`
	EndLine    int              `json:"end_line,omitempty"`
	TotalLines int              `json:"total_lines,omitempty"`
}

type failureCode string

func (e failureCode) Error() string { return string(e) }

func ErrorStatus(code string) (int, string) {
	switch code {
	case "invalid_request":
		return 400, "invalid file request"
	case "not_found":
		return 404, "file or directory not found"
	case "conflict":
		return 409, "file does not contain the requested text"
	case "limit_exceeded":
		return 413, "file operation exceeds a resource limit"
	case "unsupported_type":
		return 422, "unsupported file type or symbolic link"
	case "permission_denied":
		return 422, "file permission denied"
	case "publish_uncertain":
		return 500, "file publication may have occurred; do not automatically retry"
	default:
		return 500, "file operation failed"
	}
}

func knownCode(code string) bool {
	switch code {
	case "invalid_request", "not_found", "conflict", "limit_exceeded", "unsupported_type", "permission_denied", "io_error", "publish_uncertain":
		return true
	}
	return false
}

func Validate(req Request) error {
	invalid := failureCode("invalid_request")
	if !utf8.ValidString(req.Path) || len(req.Path) > 4096 || strings.ContainsRune(req.Path, 0) || !path.IsAbs(req.Path) {
		return invalid
	}
	for _, part := range strings.Split(req.Path, "/") {
		if part == ".." {
			return invalid
		}
	}
	clean := path.Clean(req.Path)
	if clean != "/workspace" && !strings.HasPrefix(clean, "/workspace/") && clean != "/tmp" && !strings.HasPrefix(clean, "/tmp/") {
		return invalid
	}
	switch req.Operation {
	case "list", "list-recursive", "glob", "read", "download", "read-lines", "edit", "edit-lines", "upload":
	default:
		return invalid
	}
	if req.Page < 0 || req.PageSize < 0 || req.PageSize > 1000 || req.MaxDepth < 0 || req.MaxDepth > MaxDepth || req.StartLine < 0 || req.EndLine < 0 {
		return invalid
	}
	if req.EndLine != 0 && req.EndLine < req.StartLine {
		return invalid
	}
	if req.Operation == "edit-lines" && req.StartLine < 1 || req.Operation == "edit" && req.OldStr == "" {
		return invalid
	}
	if len(req.OldStr) > MaxContentBytes || len(req.NewStr) > MaxContentBytes || len(req.NewContent) > MaxContentBytes {
		return failureCode("limit_exceeded")
	}
	if len(req.Content) > MaxContentBytes {
		return failureCode("limit_exceeded")
	}
	if req.Operation != "upload" && len(req.Content) != 0 {
		return invalid
	}
	if req.Operation == "upload" && (req.Pattern != "" || req.MaxDepth != 0 || req.Page != 0 || req.PageSize != 0 || req.StartLine != 0 || req.EndLine != 0 || req.OldStr != "" || req.NewStr != "" || req.ReplaceAll || req.NewContent != "") {
		return invalid
	}
	if req.Operation == "glob" {
		if req.Pattern == "" || len(req.Pattern) > 4096 || !utf8.ValidString(req.Pattern) || strings.ContainsRune(req.Pattern, 0) || path.IsAbs(req.Pattern) {
			return invalid
		}
		for _, part := range strings.Split(req.Pattern, "/") {
			if part == ".." {
				return invalid
			}
		}
		if _, err := path.Match(req.Pattern, ""); err != nil {
			return invalid
		}
	}
	return nil
}

// decodeStrict validates exact field names and duplicates before normal decoding.
// It also rejects null, since absence and zero values are the protocol defaults.
func decodeStrict(wire []byte, out any) error {
	if !utf8.Valid(wire) {
		return errors.New("invalid JSON")
	}
	d := json.NewDecoder(bytes.NewReader(wire))
	d.UseNumber()
	if err := checkJSON(d, reflect.TypeOf(out).Elem(), 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	d = json.NewDecoder(bytes.NewReader(wire))
	d.DisallowUnknownFields()
	return d.Decode(out)
}

func checkJSON(d *json.Decoder, shape reflect.Type, depth int) error {
	if depth > 32 {
		return errors.New("JSON depth")
	}
	for shape.Kind() == reflect.Pointer {
		shape = shape.Elem()
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return errors.New("null JSON")
	}
	delim, container := token.(json.Delim)
	if !container {
		return nil
	}
	switch delim {
	case '{':
		if shape.Kind() != reflect.Struct {
			return errors.New("JSON object")
		}
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			name, ok := key.(string)
			if err != nil || !ok || seen[name] {
				return errors.New("JSON field")
			}
			seen[name] = true
			var child reflect.Type
			for i := 0; i < shape.NumField(); i++ {
				f := shape.Field(i)
				if strings.Split(f.Tag.Get("json"), ",")[0] == name {
					child = f.Type
					break
				}
			}
			if child == nil {
				return errors.New("unknown JSON field")
			}
			if err := checkJSON(d, child, depth+1); err != nil {
				return err
			}
		}
	case '[':
		if shape.Kind() != reflect.Slice {
			return errors.New("JSON array")
		}
		for d.More() {
			if err := checkJSON(d, shape.Elem(), depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("JSON container")
	}
	_, err = d.Token()
	return err
}

func DecodeResult(wire []byte) (Result, error) {
	var result Result
	invalid := errors.New("invalid file result")
	if len(wire) > MaxOutputBytes || decodeStrict(wire, &result) != nil {
		return Result{}, invalid
	}
	if result.Error != nil {
		copy := result
		copy.Error = nil
		if !knownCode(result.Error.Code) || !reflect.DeepEqual(copy, Result{}) {
			return Result{}, invalid
		}
		return result, nil
	}
	if len(result.Content) > MaxContentBytes || len(result.Files) > MaxEntries || result.TotalCount < 0 || result.TotalCount > MaxEntries || result.Page < 0 || result.PageSize < 0 || result.PageSize > 1000 || result.StartLine < 0 || result.EndLine < 0 || result.TotalLines < 0 || result.TotalLines > MaxContentBytes {
		return Result{}, invalid
	}
	groups := 0
	if result.Content != nil {
		groups++
	}
	if result.Files != nil || result.Page != 0 || result.PageSize != 0 || result.TotalCount != 0 {
		groups++
	}
	if result.Lines != nil || result.StartLine != 0 || result.EndLine != 0 || result.TotalLines != 0 {
		groups++
	}
	if groups > 1 {
		return Result{}, invalid
	}
	return result, nil
}
