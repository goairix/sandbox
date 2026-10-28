package handler

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/sandbox"
	"github.com/goairix/sandbox/pkg/types"
)

func fileV2Error(c *gin.Context, err error) {
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		c.JSON(http.StatusRequestEntityTooLarge, types.ErrorResponse{Code: "FILE_LIMIT_EXCEEDED", Message: "file exceeds upload limit"})
	case errors.Is(err, sandbox.ErrFileV2InvalidPath), errors.Is(err, sandbox.ErrReservedFUSEWorkspacePath):
		c.JSON(http.StatusForbidden, types.ErrorResponse{Code: "FILE_PATH_FORBIDDEN", Message: "file path is forbidden"})
	case errors.Is(err, runtime.ErrFileNotFound):
		c.JSON(http.StatusNotFound, types.ErrorResponse{Code: "FILE_NOT_FOUND", Message: "file not found"})
	case errors.Is(err, runtime.ErrInvalidUploadSize):
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Code: "FILE_INVALID", Message: "invalid file size"})
	case errors.Is(err, sandbox.ErrSandboxNotReady), errors.Is(err, sandbox.ErrFileV2Unsupported):
		c.JSON(http.StatusServiceUnavailable, types.ErrorResponse{Code: "SANDBOX_NOT_READY", Message: "sandbox file operation is unavailable"})
	default:
		c.JSON(http.StatusServiceUnavailable, types.ErrorResponse{Code: "FILE_OUTCOME_UNKNOWN", Message: "file operation outcome is unknown"})
	}
}

// parseFileV2Range accepts one RFC 9110 byte range and returns its concrete
// start and length for the helper. No Range header means the complete file.
func parseFileV2Range(header string, size int64) (start, length int64, partial bool, err error) {
	if header == "" {
		return 0, size, false, nil
	}
	if size <= 0 || !strings.HasPrefix(header, "bytes=") || strings.Contains(header, ",") {
		return 0, 0, false, errors.New("invalid byte range")
	}
	first, last, ok := strings.Cut(strings.TrimPrefix(header, "bytes="), "-")
	if !ok {
		return 0, 0, false, errors.New("invalid byte range")
	}
	if first == "" {
		suffix, parseErr := strconv.ParseInt(last, 10, 64)
		if parseErr != nil || suffix <= 0 {
			return 0, 0, false, errors.New("invalid byte range")
		}
		if suffix > size {
			suffix = size
		}
		return size - suffix, suffix, true, nil
	}
	start, err = strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false, errors.New("invalid byte range")
	}
	end := size - 1
	if last != "" {
		end, err = strconv.ParseInt(last, 10, 64)
		if err != nil || end < start {
			return 0, 0, false, errors.New("invalid byte range")
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end - start + 1, true, nil
}

// ReadFileContentV2 streams raw bytes. A one-byte preflight catches helper
// failures before HTTP headers are sent; later transport errors truncate the
// declared Content-Length so the client cannot mistake a partial body for OK.
func (h *Handler) ReadFileContentV2(c *gin.Context) {
	filePath := c.Query("path")
	if filePath == "" {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Code: "FILE_INVALID", Message: "path is required"})
		return
	}
	info, err := h.manager.StatFileV2(c.Request.Context(), c.Param("id"), filePath)
	if err != nil {
		fileV2Error(c, err)
		return
	}
	if info.IsDir || info.Size < 0 {
		c.JSON(http.StatusConflict, types.ErrorResponse{Code: "FILE_TYPE_MISMATCH", Message: "path is not a regular file"})
		return
	}
	start, length, partial, err := parseFileV2Range(c.GetHeader("Range"), info.Size)
	if err != nil {
		c.Header("Content-Range", fmt.Sprintf("bytes */%d", info.Size))
		c.JSON(http.StatusRequestedRangeNotSatisfiable, types.ErrorResponse{Code: "FILE_INVALID", Message: "invalid byte range"})
		return
	}
	stream, err := h.manager.ReadFileRangeV2(c.Request.Context(), c.Param("id"), filePath, start, length)
	if err != nil {
		fileV2Error(c, err)
		return
	}
	var first [1]byte
	n, readErr := stream.Read(first[:])
	if readErr != nil && !(errors.Is(readErr, io.EOF) && length == 0) {
		_ = stream.Close()
		fileV2Error(c, readErr)
		return
	}
	if length > 0 && n == 0 {
		_ = stream.Close()
		fileV2Error(c, io.ErrUnexpectedEOF)
		return
	}
	c.Header("Accept-Ranges", "bytes")
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Length", strconv.FormatInt(length, 10))
	if partial {
		c.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+length-1, info.Size))
		c.Status(http.StatusPartialContent)
	} else {
		c.Status(http.StatusOK)
	}
	if n > 0 {
		if _, err := c.Writer.Write(first[:n]); err != nil {
			_ = stream.Close()
			return
		}
	}
	if _, err := io.CopyN(c.Writer, stream, length-int64(n)); err != nil {
		_ = stream.Close()
		return
	}
	_ = stream.Close()
}

func (h *Handler) WriteFileContentV2(c *gin.Context) {
	filePath := c.Query("path")
	if filePath == "" || c.Request.ContentLength < 0 {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Code: "FILE_INVALID", Message: "path and Content-Length are required"})
		return
	}
	if c.Request.ContentLength > h.maxUploadBytes {
		c.JSON(http.StatusRequestEntityTooLarge, types.ErrorResponse{Code: "FILE_LIMIT_EXCEEDED", Message: "file exceeds upload limit"})
		return
	}
	createParents := false
	switch c.Query("create_parents") {
	case "", "false":
	case "true":
		createParents = true
	default:
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Code: "FILE_INVALID", Message: "invalid create_parents"})
		return
	}
	if err := h.manager.WriteFileV2(c.Request.Context(), c.Param("id"), filePath, c.Request.ContentLength, createParents, c.Request.Body); err != nil {
		fileV2Error(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
