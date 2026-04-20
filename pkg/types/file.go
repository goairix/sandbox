package types

import "time"

type FileInfo struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	IsDir   bool      `json:"is_dir"`
	ModTime time.Time `json:"mod_time"`
}

type FileListResponse struct {
	Files []FileInfo `json:"files"`
	Path  string     `json:"path"`
}

type FileDownloadRequest struct {
	Path string `form:"path" binding:"required"`
}

type FileUploadResponse struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// ListFilesRecursiveRequest is the request body for POST /api/v1/sandboxes/:id/files/list-recursive.
type ListFilesRecursiveRequest struct {
	Path     string `json:"path" binding:"required"`
	MaxDepth int    `json:"max_depth,omitempty"`
	Page     int    `json:"page,omitempty"`
	PageSize int    `json:"page_size,omitempty"`
}

// ListFilesRecursiveResponse is returned by the list-recursive endpoint.
type ListFilesRecursiveResponse struct {
	Files      []FileInfo `json:"files"`
	Path       string     `json:"path"`
	TotalCount int        `json:"total_count"`
	Page       int        `json:"page"`
	PageSize   int        `json:"page_size"`
}

// ReadFileLinesRequest is the request body for POST /api/v1/sandboxes/:id/files/read-lines.
type ReadFileLinesRequest struct {
	Path      string `json:"path" binding:"required"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
}

// ReadFileLinesResponse is returned by the read-lines endpoint.
type ReadFileLinesResponse struct {
	Lines      []string `json:"lines"`
	StartLine  int      `json:"start_line"`
	EndLine    int      `json:"end_line"`
	TotalLines int      `json:"total_lines"`
}

// EditFileRequest is the request body for POST /api/v1/sandboxes/:id/files/edit.
type EditFileRequest struct {
	Path       string `json:"path" binding:"required"`
	OldStr     string `json:"old_str" binding:"required"`
	NewStr     string `json:"new_str"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

// EditFileLinesRequest is the request body for POST /api/v1/sandboxes/:id/files/edit-lines.
type EditFileLinesRequest struct {
	Path       string `json:"path" binding:"required"`
	StartLine  int    `json:"start_line" binding:"required,min=1"`
	EndLine    int    `json:"end_line,omitempty"`
	NewContent string `json:"new_content"`
}
