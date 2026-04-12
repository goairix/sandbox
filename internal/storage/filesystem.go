package storage

import (
	"fmt"

	"github.com/dysodeng/fs"
	"github.com/dysodeng/fs/driver/alioss"
	"github.com/dysodeng/fs/driver/hwobs"
	"github.com/dysodeng/fs/driver/local"
	"github.com/dysodeng/fs/driver/minio"
	"github.com/dysodeng/fs/driver/s3"
	"github.com/dysodeng/fs/driver/txcos"

	"github.com/goairix/sandbox/internal/config"
)

// StorageProvider identifies the type of filesystem backend.
type StorageProvider string

const (
	ProviderLocal StorageProvider = "local"
	ProviderS3    StorageProvider = "s3"
	ProviderCOS   StorageProvider = "cos"
	ProviderOSS   StorageProvider = "oss"
	ProviderOBS   StorageProvider = "obs"
	ProviderMinIO StorageProvider = "minio"
)

// FileSystemMeta holds metadata about the created filesystem.
type FileSystemMeta struct {
	Provider  StorageProvider
	LocalPath string // non-empty only when Provider == ProviderLocal
}

// NewFileSystem creates a fs.FileSystem from the given configuration.
func NewFileSystem(cfg config.FileSystemConfig) (fs.FileSystem, *FileSystemMeta, error) {
	meta := &FileSystemMeta{Provider: StorageProvider(cfg.Provider)}

	switch cfg.Provider {
	case "local":
		if cfg.LocalPath == "" {
			return nil, nil, fmt.Errorf("storage: local provider requires local_path")
		}
		meta.LocalPath = cfg.LocalPath
		fsys, err := local.New(local.Config{
			RootPath: cfg.LocalPath,
			SubPath:  cfg.SubPath,
		})
		return fsys, meta, err

	case "s3":
		fsys, err := s3.New(s3.Config{
			Region:          cfg.Region,
			Endpoint:        cfg.Endpoint,
			AccessKeyID:     cfg.AccessKey,
			SecretAccessKey: cfg.SecretKey,
			BucketName:      cfg.Bucket,
			SubPath:         cfg.SubPath,
		})
		return fsys, meta, err

	case "cos":
		fsys, err := txcos.New(txcos.Config{
			BucketURL: cfg.Endpoint,
			SecretID:  cfg.AccessKey,
			SecretKey: cfg.SecretKey,
			SubPath:   cfg.SubPath,
		})
		return fsys, meta, err

	case "oss":
		fsys, err := alioss.New(alioss.Config{
			Endpoint:        cfg.Endpoint,
			AccessKeyID:     cfg.AccessKey,
			SecretAccessKey: cfg.SecretKey,
			BucketName:      cfg.Bucket,
			SubPath:         cfg.SubPath,
		})
		return fsys, meta, err

	case "obs":
		fsys, err := hwobs.New(hwobs.Config{
			Endpoint:        cfg.Endpoint,
			AccessKeyID:     cfg.AccessKey,
			SecretAccessKey: cfg.SecretKey,
			BucketName:      cfg.Bucket,
			SubPath:         cfg.SubPath,
		})
		return fsys, meta, err

	case "minio":
		fsys, err := minio.New(minio.Config{
			Endpoint:        cfg.Endpoint,
			AccessKeyID:     cfg.AccessKey,
			SecretAccessKey: cfg.SecretKey,
			UseSSL:          cfg.UseSSL,
			BucketName:      cfg.Bucket,
			SubPath:         cfg.SubPath,
		})
		return fsys, meta, err

	default:
		return nil, nil, fmt.Errorf("storage: unsupported filesystem provider: %q", cfg.Provider)
	}
}
