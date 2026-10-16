package deployment

import (
	"strings"

	"github.com/goairix/sandbox/internal/transferspool"
)

type fileTransferManifest struct {
	Directory      string `json:"directory"`
	MaxFileBytes   *int64 `json:"max_file_bytes,omitempty"`
	MaxTotalBytes  *int64 `json:"max_total_bytes,omitempty"`
	MaxActive      *int   `json:"max_active,omitempty"`
	TimeoutSeconds *int   `json:"timeout_seconds,omitempty"`
}

// FileTransferSettings is an optional, caller-owned mounted download budget.
// Loading it has no filesystem effects; only the gateway opens its directory.
type FileTransferSettings struct {
	Directory                   string
	MaxFileBytes, MaxTotalBytes int64
	MaxActive, TimeoutSeconds   int
}

func loadFileTransfer(input *fileTransferManifest, maxTimeout int) (*FileTransferSettings, error) {
	if input == nil {
		return nil, nil
	}
	settings := &FileTransferSettings{Directory: strings.Clone(input.Directory), MaxFileBytes: 2 << 30, MaxTotalBytes: 4 << 30, MaxActive: 2, TimeoutSeconds: 300}
	if input.MaxFileBytes != nil {
		settings.MaxFileBytes = *input.MaxFileBytes
	}
	if input.MaxTotalBytes != nil {
		settings.MaxTotalBytes = *input.MaxTotalBytes
	}
	if input.MaxActive != nil {
		settings.MaxActive = *input.MaxActive
	}
	if input.TimeoutSeconds != nil {
		settings.TimeoutSeconds = *input.TimeoutSeconds
	}
	options := transferspool.Options{Directory: settings.Directory, MaxFileBytes: settings.MaxFileBytes, MaxTotalBytes: settings.MaxTotalBytes, MaxActive: settings.MaxActive}
	if options.Validate() != nil || settings.TimeoutSeconds < 1 || settings.TimeoutSeconds > maxTimeout {
		return nil, fileError("HTTP file transfer budgets")
	}
	return settings, nil
}
