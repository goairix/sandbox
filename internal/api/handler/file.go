package handler

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/goairix/sandbox/internal/logger"
	"github.com/goairix/sandbox/internal/telemetry/trace"
	"github.com/goairix/sandbox/pkg/types"
)

var errStreamingMultipartPath = errors.New("streaming upload path must only be provided in the query")

type declaredUploadPartReader struct {
	part      io.Reader
	multipart *multipart.Reader
	done      bool
}

func (r *declaredUploadPartReader) Read(p []byte) (int, error) {
	n, err := r.part.Read(p)
	if n > 0 {
		return n, nil
	}
	if !errors.Is(err, io.EOF) || r.done {
		return n, err
	}
	r.done = true
	for {
		part, nextErr := r.multipart.NextPart()
		if errors.Is(nextErr, io.EOF) {
			return 0, io.EOF
		}
		if nextErr != nil {
			return 0, fmt.Errorf("validate trailing multipart data: %w", nextErr)
		}
		if part.FormName() == "path" {
			_ = part.Close()
			return 0, errStreamingMultipartPath
		}
		_ = part.Close()
	}
}

// validateSandboxPath checks that the path does not contain ".." and starts with
// an allowed prefix (/workspace/ or /tmp/). This prevents path traversal attacks.
func validateSandboxPath(p string) error {
	cleaned := filepath.Clean(p)
	if strings.Contains(cleaned, "..") {
		return fmt.Errorf("path must not contain '..'")
	}
	if !(cleaned == "/workspace" || strings.HasPrefix(cleaned, "/workspace/")) &&
		!(cleaned == "/tmp" || strings.HasPrefix(cleaned, "/tmp/")) {
		return fmt.Errorf("path must start with /workspace/ or /tmp/")
	}
	return nil
}

func (h *Handler) UploadFile(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.file.UploadFile")
	defer span.End()

	id := c.Param("id")
	declaredHeader := strings.TrimSpace(c.GetHeader("X-Sandbox-File-Size"))
	hasDeclaredSize := declaredHeader != ""
	var declaredSize int64
	var err error
	if hasDeclaredSize {
		declaredSize, err = strconv.ParseInt(declaredHeader, 10, 64)
		if err != nil || declaredSize < 0 {
			c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: "X-Sandbox-File-Size must be a non-negative integer"})
			return
		}
		if declaredSize > h.maxUploadBytes {
			c.JSON(http.StatusRequestEntityTooLarge, types.ErrorResponse{Message: "declared file size exceeds upload limit"})
			return
		}
	}

	multipartReader, err := c.Request.MultipartReader()
	if err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: "multipart form data is required"})
		return
	}

	sb, err := h.manager.Get(spanCtx, id)
	if err != nil {
		internalError(c, err)
		return
	}
	_ = sb // used to confirm sandbox exists

	destPath := c.Query("path")
	if hasDeclaredSize && destPath == "" {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: "streaming upload requires path query parameter"})
		return
	}
	if destPath == "" {
		destPath = "/workspace/"
	}
	var legacyFile *os.File
	var legacyFilename string
	defer func() {
		if legacyFile != nil {
			name := legacyFile.Name()
			_ = legacyFile.Close()
			_ = os.Remove(name)
		}
	}()
	for {
		part, nextErr := multipartReader.NextPart()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: "invalid multipart upload"})
			return
		}
		if part.FormName() == "path" {
			if hasDeclaredSize {
				_ = part.Close()
				c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: errStreamingMultipartPath.Error()})
				return
			}
			value, readErr := io.ReadAll(io.LimitReader(part, 4097))
			_ = part.Close()
			if readErr != nil || len(value) > 4096 || len(value) == 0 {
				c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: "invalid upload path"})
				return
			}
			destPath = string(value)
			continue
		}
		if part.FormName() != "file" || part.FileName() == "" {
			_ = part.Close()
			continue
		}

		safeFilename := filepath.Base(part.FileName())
		if safeFilename == "." || safeFilename == "/" {
			_ = part.Close()
			c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: "invalid filename"})
			return
		}
		if hasDeclaredSize {
			fullPath, pathErr := uploadDestination(destPath, safeFilename)
			if pathErr != nil {
				_ = part.Close()
				c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: pathErr.Error()})
				return
			}
			stream := &declaredUploadPartReader{part: part, multipart: multipartReader}
			if err := h.manager.UploadFile(spanCtx, id, fullPath, declaredSize, stream); err != nil {
				_ = part.Close()
				if errors.Is(err, errStreamingMultipartPath) {
					c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: errStreamingMultipartPath.Error()})
					return
				}
				internalError(c, err)
				return
			}
			_ = part.Close()
			c.JSON(http.StatusOK, types.FileUploadResponse{Path: fullPath, Size: declaredSize})
			return
		}

		if legacyFile != nil {
			_ = part.Close()
			c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: "only one file part is allowed"})
			return
		}
		legacyFile, err = os.CreateTemp("", "sandbox-upload-*")
		if err != nil {
			_ = part.Close()
			internalError(c, fmt.Errorf("create upload spool: %w", err))
			return
		}
		legacyLimit := int64(64 << 20)
		if h.maxUploadBytes < legacyLimit {
			legacyLimit = h.maxUploadBytes
		}
		declaredSize, err = io.Copy(legacyFile, io.LimitReader(part, legacyLimit+1))
		_ = part.Close()
		if err != nil {
			internalError(c, fmt.Errorf("spool upload: %w", err))
			return
		}
		if declaredSize > legacyLimit {
			c.JSON(http.StatusRequestEntityTooLarge, types.ErrorResponse{Message: "uploads larger than 64 MiB require X-Sandbox-File-Size"})
			return
		}
		legacyFilename = safeFilename
	}
	if legacyFile == nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: "file is required"})
		return
	}
	fullPath, err := uploadDestination(destPath, legacyFilename)
	if err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}
	if _, err := legacyFile.Seek(0, io.SeekStart); err != nil {
		internalError(c, fmt.Errorf("rewind upload spool: %w", err))
		return
	}
	if err := h.manager.UploadFile(spanCtx, id, fullPath, declaredSize, legacyFile); err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, types.FileUploadResponse{Path: fullPath, Size: declaredSize})
}

func uploadDestination(destPath, filename string) (string, error) {
	if destPath == "" {
		return "", fmt.Errorf("path must not be empty")
	}
	fullPath := destPath
	if fullPath[len(fullPath)-1] == '/' {
		fullPath += filename
	}
	if err := validateSandboxPath(fullPath); err != nil {
		return "", err
	}
	return fullPath, nil
}

func (h *Handler) DownloadFile(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.file.DownloadFile")
	defer span.End()

	id := c.Param("id")
	path := c.Query("path")
	if path == "" {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Message: "path is required",
		})
		return
	}

	if err := validateSandboxPath(path); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Message: err.Error(),
		})
		return
	}
	tarStream, err := h.manager.DownloadFile(spanCtx, id, path)
	if err != nil {
		internalError(c, err)
		return
	}
	defer func() { _ = tarStream.Close() }()

	tr := tar.NewReader(tarStream)
	if _, err := tr.Next(); err != nil {
		internalError(c, err)
		return
	}

	safeName := filepath.Base(path)
	ext := strings.ToLower(filepath.Ext(safeName))
	if ext == ".html" || ext == ".htm" {
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.Header("Content-Disposition", "inline")
	} else {
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", safeName))
		c.Header("Content-Type", "application/octet-stream")
	}
	if _, err := io.Copy(c.Writer, tr); err != nil {
		logger.Warn(spanCtx, "error copying file to response",
			logger.AddField("sandbox_id", id),
			logger.AddField("path", path),
			logger.ErrorField(err),
		)
	}
}

func (h *Handler) ReadFile(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.file.ReadFile")
	defer span.End()

	id := c.Param("id")

	var req types.ReadFileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Message: "path is required",
		})
		return
	}

	path := req.Path
	if err := validateSandboxPath(path); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Message: err.Error(),
		})
		return
	}
	reader, err := h.manager.ReadFileContent(spanCtx, id, path)
	if err != nil {
		internalError(c, err)
		return
	}
	defer func() { _ = reader.Close() }()

	c.Header("X-File-Path", path)
	c.DataFromReader(http.StatusOK, -1, "text/plain; charset=utf-8", reader, nil)
}

func (h *Handler) ListFiles(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.file.ListFiles")
	defer span.End()

	id := c.Param("id")
	dir := c.DefaultQuery("path", "/workspace")

	if err := validateSandboxPath(dir); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Message: err.Error(),
		})
		return
	}
	files, err := h.manager.ListFiles(spanCtx, id, dir)
	if err != nil {
		internalError(c, err)
		return
	}

	var fileInfos []types.FileInfo
	for _, f := range files {
		fileInfos = append(fileInfos, types.FileInfo{
			Name:    f.Name,
			Path:    f.Path,
			Size:    f.Size,
			IsDir:   f.IsDir,
			ModTime: f.ModTime,
		})
	}
	c.JSON(http.StatusOK, types.FileListResponse{
		Files: fileInfos,
		Path:  dir,
	})
}

func (h *Handler) ListFilesRecursive(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.file.ListFilesRecursive")
	defer span.End()

	id := c.Param("id")

	var req types.ListFilesRecursiveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}

	if err := validateSandboxPath(req.Path); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}
	page := req.Page
	if page < 1 {
		page = 1
	}
	pageSize := req.PageSize
	if pageSize <= 0 {
		pageSize = 100
	} else if pageSize > 1000 {
		pageSize = 1000
	}

	result, err := h.manager.ListFilesRecursive(spanCtx, id, req.Path, req.MaxDepth, page, pageSize)
	if err != nil {
		internalError(c, err)
		return
	}

	var fileInfos []types.FileInfo
	for _, f := range result.Files {
		fileInfos = append(fileInfos, types.FileInfo{
			Name:    f.Name,
			Path:    f.Path,
			Size:    f.Size,
			IsDir:   f.IsDir,
			ModTime: f.ModTime,
		})
	}
	c.JSON(http.StatusOK, types.ListFilesRecursiveResponse{
		Files:      fileInfos,
		Path:       req.Path,
		TotalCount: result.TotalCount,
		Page:       result.Page,
		PageSize:   result.PageSize,
	})
}

func (h *Handler) GlobFiles(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.file.GlobFiles")
	defer span.End()

	id := c.Param("id")

	var req types.GlobFilesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}

	if err := validateSandboxPath(req.Path); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}
	page := req.Page
	if page < 1 {
		page = 1
	}
	pageSize := req.PageSize
	if pageSize <= 0 {
		pageSize = 100
	} else if pageSize > 1000 {
		pageSize = 1000
	}

	result, err := h.manager.GlobFiles(spanCtx, id, req.Path, req.Pattern, page, pageSize)
	if err != nil {
		internalError(c, err)
		return
	}

	var fileInfos []types.FileInfo
	for _, f := range result.Files {
		fileInfos = append(fileInfos, types.FileInfo{
			Name:    f.Name,
			Path:    f.Path,
			Size:    f.Size,
			IsDir:   f.IsDir,
			ModTime: f.ModTime,
		})
	}
	c.JSON(http.StatusOK, types.GlobFilesResponse{
		Files:      fileInfos,
		Path:       req.Path,
		Pattern:    req.Pattern,
		TotalCount: result.TotalCount,
		Page:       result.Page,
		PageSize:   result.PageSize,
	})
}

func (h *Handler) ReadFileLines(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.file.ReadFileLines")
	defer span.End()

	id := c.Param("id")

	var req types.ReadFileLinesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}

	if err := validateSandboxPath(req.Path); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}
	result, err := h.manager.ReadFileLines(spanCtx, id, req.Path, req.StartLine, req.EndLine)
	if err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, types.ReadFileLinesResponse{
		Lines:      result.Lines,
		StartLine:  result.StartLine,
		EndLine:    result.EndLine,
		TotalLines: result.TotalLines,
	})
}

func (h *Handler) EditFile(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.file.EditFile")
	defer span.End()

	id := c.Param("id")

	var req types.EditFileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}

	if err := validateSandboxPath(req.Path); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}
	if err := h.manager.EditFile(spanCtx, id, req.Path, req.OldStr, req.NewStr, req.ReplaceAll); err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, types.EditFileResponse{Message: "ok"})
}

func (h *Handler) EditFileLines(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.file.EditFileLines")
	defer span.End()

	id := c.Param("id")

	var req types.EditFileLinesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}

	if err := validateSandboxPath(req.Path); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}
	if err := h.manager.EditFileLines(spanCtx, id, req.Path, req.StartLine, req.EndLine, req.NewContent); err != nil {
		internalError(c, err)
		return
	}

	c.JSON(http.StatusOK, types.EditFileResponse{Message: "ok"})
}

func (h *Handler) InitMultipartUpload(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.file.InitMultipartUpload")
	defer span.End()

	id := c.Param("id")

	var req types.MultipartInitRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}
	if err := validateSandboxPath(req.Path); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}
	uploadID, err := h.manager.InitMultipartUpload(spanCtx, id, req.Path, req.TotalChunks)
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, types.MultipartInitResponse{UploadID: uploadID})
}

func (h *Handler) UploadChunk(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.file.UploadChunk")
	defer span.End()

	id := c.Param("id")
	uploadID := c.PostForm("upload_id")
	if uploadID == "" {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: "upload_id is required"})
		return
	}
	chunkIndexStr := c.PostForm("chunk_index")
	chunkIndex, err := strconv.Atoi(chunkIndexStr)
	if err != nil || chunkIndex < 0 {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: "chunk_index must be a non-negative integer"})
		return
	}

	file, header, err := c.Request.FormFile("chunk")
	if err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: "file chunk is required"})
		return
	}
	defer func() { _ = file.Close() }()

	received, total, err := h.manager.UploadChunk(spanCtx, id, uploadID, chunkIndex, header.Size, file)
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, types.MultipartChunkResponse{Received: received, Total: total})
}

func (h *Handler) GetMultipartStatus(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.file.GetMultipartStatus")
	defer span.End()

	id := c.Param("id")
	uploadID := c.Query("upload_id")
	if uploadID == "" {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: "upload_id is required"})
		return
	}

	st, err := h.manager.GetMultipartStatus(spanCtx, id, uploadID)
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, types.MultipartStatusResponse{
		UploadID:       st.UploadID,
		Path:           st.DestPath,
		TotalChunks:    st.TotalChunks,
		ReceivedChunks: st.ReceivedChunks,
		CreatedAt:      st.CreatedAt,
	})
}

func (h *Handler) CompleteMultipartUpload(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.file.CompleteMultipartUpload")
	defer span.End()

	id := c.Param("id")

	var req types.MultipartCompleteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}

	path, size, err := h.manager.CompleteMultipartUpload(spanCtx, id, req.UploadID)
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, types.MultipartCompleteResponse{Path: path, Size: size})
}

func (h *Handler) CancelMultipartUpload(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.file.CancelMultipartUpload")
	defer span.End()

	id := c.Param("id")

	var req types.MultipartCancelRequest
	if err := c.ShouldBindQuery(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}

	if err := h.manager.CancelMultipartUpload(spanCtx, id, req.UploadID); err != nil {
		internalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
