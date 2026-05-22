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

// GlobFilesRequest is the request body for POST /api/v1/sandboxes/:id/files/glob.
type GlobFilesRequest struct {
	Path     string `json:"path" binding:"required"`
	Pattern  string `json:"pattern" binding:"required"`
	Page     int    `json:"page,omitempty"`
	PageSize int    `json:"page_size,omitempty"`
}

// GlobFilesResponse is returned by the glob endpoint.
type GlobFilesResponse struct {
	Files      []FileInfo `json:"files"`
	Path       string     `json:"path"`
	Pattern    string     `json:"pattern"`
	TotalCount int        `json:"total_count"`
	Page       int        `json:"page"`
	PageSize   int        `json:"page_size"`
}

// ReadFileRequest is the request body for POST /api/v1/sandboxes/:id/files/read.
type ReadFileRequest struct {
	Path string `json:"path" binding:"required"`
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

// ReadFileResponse is returned by the read file endpoint.
type ReadFileResponse struct {
	Path    string `json:"path"`
	Content string `json:"content"`
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

// EditFileResponse is returned by the edit and edit-lines endpoints.
type EditFileResponse struct {
	Message string `json:"message"`
}

// MultipartInitRequest is the request body for POST /api/v1/sandboxes/:id/files/upload/init.
type MultipartInitRequest struct {
	Path        string `json:"path" binding:"required"`
	TotalChunks int    `json:"total_chunks" binding:"required,min=1,max=10000"`
}

// MultipartInitResponse is returned by the init endpoint.
type MultipartInitResponse struct {
	UploadID string `json:"upload_id"`
}

// MultipartChunkResponse is returned after each chunk upload.
type MultipartChunkResponse struct {
	Received int `json:"received"`
	Total    int `json:"total"`
}

// MultipartStatusResponse is returned by the status endpoint.
type MultipartStatusResponse struct {
	UploadID       string    `json:"upload_id"`
	Path           string    `json:"path"`
	TotalChunks    int       `json:"total_chunks"`
	ReceivedChunks int       `json:"received_chunks"`
	CreatedAt      time.Time `json:"created_at"`
}

// MultipartCompleteRequest is the request body for POST /api/v1/sandboxes/:id/files/upload/complete.
type MultipartCompleteRequest struct {
	UploadID string `json:"upload_id" binding:"required"`
}

// MultipartCompleteResponse is returned after a successful merge.
type MultipartCompleteResponse struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// MultipartCancelRequest is the request body for DELETE /api/v1/sandboxes/:id/files/upload/cancel.
type MultipartCancelRequest struct {
	UploadID string `form:"upload_id" binding:"required"`
}
