package filev2

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

const maxLineResultBytes = 1 << 20
const maxEditFileBytes = 8 << 20

var ErrTextLimit = errors.New("text limit exceeded")
var ErrTextInvalid = errors.New("invalid UTF-8 text")
var ErrEditConflict = errors.New("text replacement is not unique")

type fileEditRequest struct {
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func decodeEditRequest(source io.Reader) (fileEditRequest, error) {
	var request fileEditRequest
	if source == nil {
		return request, ErrTextInvalid
	}
	body, err := io.ReadAll(io.LimitReader(source, (16<<20)+1))
	if err != nil || len(body) > 16<<20 {
		return request, ErrTextLimit
	}
	if !utf8.Valid(body) {
		return request, ErrTextInvalid
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
	return request, nil
}

// readLines keeps the source newline bytes, including CRLF. It scans in
// bounded chunks so a single untrusted line cannot allocate without limit.
func readLines(ctx context.Context, source io.Reader, start, count int) (string, error) {
	if start < 1 || count < 0 {
		return "", ErrTextInvalid
	}
	reader := bufio.NewReaderSize(source, 32<<10)
	result := make([]byte, 0, 4096)
	line, selected := 1, 0
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		chunk, readErr := reader.ReadSlice('\n')
		if line >= start {
			if len(chunk) > maxLineResultBytes-len(result) {
				return "", ErrTextLimit
			}
			result = append(result, chunk...)
		}
		switch readErr {
		case nil:
			if line >= start {
				selected++
				if count > 0 && selected >= count {
					if !utf8.Valid(result) {
						return "", ErrTextInvalid
					}
					return string(result), nil
				}
			}
			line++
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			if !utf8.Valid(result) {
				return "", ErrTextInvalid
			}
			return string(result), nil
		default:
			return "", readErr
		}
	}
}

// editText validates the complete replacement before any caller writes.
func editText(original []byte, oldText, newText string, replaceAll bool) ([]byte, int, error) {
	if len(original) > maxEditFileBytes || len(oldText) > maxEditFileBytes || len(newText) > maxEditFileBytes {
		return nil, 0, ErrTextLimit
	}
	if !utf8.Valid(original) || !utf8.ValidString(oldText) || !utf8.ValidString(newText) || oldText == "" || oldText == newText {
		return nil, 0, ErrTextInvalid
	}
	matches := strings.Count(string(original), oldText)
	if matches == 0 || (!replaceAll && matches != 1) {
		return nil, 0, ErrEditConflict
	}
	if !replaceAll {
		matches = 1
	}
	finalSize := int64(len(original)) + int64(matches)*(int64(len(newText))-int64(len(oldText)))
	if finalSize < 0 || finalSize > maxEditFileBytes {
		return nil, 0, ErrTextLimit
	}
	if replaceAll {
		return []byte(strings.ReplaceAll(string(original), oldText, newText)), matches, nil
	}
	return []byte(strings.Replace(string(original), oldText, newText, 1)), 1, nil
}
