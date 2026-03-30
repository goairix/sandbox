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
