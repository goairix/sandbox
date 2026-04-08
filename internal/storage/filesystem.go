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

// NewFileSystem creates a fs.FileSystem from the given configuration.
func NewFileSystem(cfg config.FileSystemConfig) (fs.FileSystem, error) {
	switch cfg.Provider {
	case "local":
		if cfg.LocalPath == "" {
			return nil, fmt.Errorf("storage: local provider requires local_path")
		}
		return local.New(local.Config{
			RootPath: cfg.LocalPath,
			SubPath:  cfg.SubPath,
		})

	case "s3":
		return s3.New(s3.Config{
			Region:          cfg.Region,
			Endpoint:        cfg.Endpoint,
			AccessKeyID:     cfg.AccessKey,
			SecretAccessKey: cfg.SecretKey,
			BucketName:      cfg.Bucket,
			SubPath:         cfg.SubPath,
		})

	case "cos":
		return txcos.New(txcos.Config{
			BucketURL: cfg.Endpoint,
			SecretID:  cfg.AccessKey,
			SecretKey: cfg.SecretKey,
			SubPath:   cfg.SubPath,
		})

	case "oss":
		return alioss.New(alioss.Config{
			Endpoint:        cfg.Endpoint,
			AccessKeyID:     cfg.AccessKey,
			SecretAccessKey: cfg.SecretKey,
			BucketName:      cfg.Bucket,
			SubPath:         cfg.SubPath,
		})

	case "obs":
		return hwobs.New(hwobs.Config{
			Endpoint:        cfg.Endpoint,
			AccessKeyID:     cfg.AccessKey,
			SecretAccessKey: cfg.SecretKey,
			BucketName:      cfg.Bucket,
			SubPath:         cfg.SubPath,
		})

	case "minio":
		return minio.New(minio.Config{
			Endpoint:        cfg.Endpoint,
			AccessKeyID:     cfg.AccessKey,
			SecretAccessKey: cfg.SecretKey,
			UseSSL:          cfg.UseSSL,
			BucketName:      cfg.Bucket,
			SubPath:         cfg.SubPath,
		})

	default:
		return nil, fmt.Errorf("storage: unsupported filesystem provider: %q", cfg.Provider)
	}
}
