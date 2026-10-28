package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/pkg/types"
)

func decodeFileV2JSON(source io.Reader, limit int64, target any) error {
	body, err := io.ReadAll(io.LimitReader(source, limit+1))
	if err != nil {
		return err
	}
	if int64(len(body)) > limit {
		return runtime.ErrFileLimitExceeded
	}
	if !utf8.Valid(body) {
		return runtime.ErrFileInvalidText
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return runtime.ErrFileInvalidText
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return runtime.ErrFileInvalidText
	}
	return nil
}

func (h *Handler) ReadFileLinesV2(c *gin.Context) {
	var request struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		LineCount int    `json:"line_count"`
	}
	if err := decodeFileV2JSON(c.Request.Body, 4096, &request); err != nil {
		fileV2Error(c, err)
		return
	}
	if request.Path == "" {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Code: "FILE_INVALID", Message: "path is required"})
		return
	}
	content, err := h.manager.ReadLinesV2(c.Request.Context(), c.Param("id"), request.Path, request.StartLine, request.LineCount)
	if err != nil {
		fileV2Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"content": content})
}

func (h *Handler) EditFileTextV2(c *gin.Context) {
	var request struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := decodeFileV2JSON(c.Request.Body, 16<<20, &request); err != nil {
		fileV2Error(c, err)
		return
	}
	if request.Path == "" {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Code: "FILE_INVALID", Message: "path is required"})
		return
	}
	if err := h.manager.EditTextV2(c.Request.Context(), c.Param("id"), request.Path, request.OldString, request.NewString, request.ReplaceAll); err != nil {
		fileV2Error(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
