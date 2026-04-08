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
// Object-storage drivers (s3, cos, oss, obs, minio) are wrapped with
// NewFixedListFS to work around a bug in their List implementation that
// strips trailing "/" from the prefix, breaking directory content listing.
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
		fsys, err := s3.New(s3.Config{
			Region:          cfg.Region,
			Endpoint:        cfg.Endpoint,
			AccessKeyID:     cfg.AccessKey,
			SecretAccessKey: cfg.SecretKey,
			BucketName:      cfg.Bucket,
			SubPath:         cfg.SubPath,
		})
		if err != nil {
			return nil, err
		}
		return NewFixedListFS(fsys), nil

	case "cos":
		fsys, err := txcos.New(txcos.Config{
			BucketURL: cfg.Endpoint,
			SecretID:  cfg.AccessKey,
			SecretKey: cfg.SecretKey,
			SubPath:   cfg.SubPath,
		})
		if err != nil {
			return nil, err
		}
		return NewFixedListFS(fsys), nil

	case "oss":
		fsys, err := alioss.New(alioss.Config{
			Endpoint:        cfg.Endpoint,
			AccessKeyID:     cfg.AccessKey,
			SecretAccessKey: cfg.SecretKey,
			BucketName:      cfg.Bucket,
			SubPath:         cfg.SubPath,
		})
		if err != nil {
			return nil, err
		}
		return NewFixedListFS(fsys), nil

	case "obs":
		fsys, err := hwobs.New(hwobs.Config{
			Endpoint:        cfg.Endpoint,
			AccessKeyID:     cfg.AccessKey,
			SecretAccessKey: cfg.SecretKey,
			BucketName:      cfg.Bucket,
			SubPath:         cfg.SubPath,
		})
		if err != nil {
			return nil, err
		}
		return NewFixedListFS(fsys), nil

	case "minio":
		fsys, err := minio.New(minio.Config{
			Endpoint:        cfg.Endpoint,
			AccessKeyID:     cfg.AccessKey,
			SecretAccessKey: cfg.SecretKey,
			UseSSL:          cfg.UseSSL,
			BucketName:      cfg.Bucket,
			SubPath:         cfg.SubPath,
		})
		if err != nil {
			return nil, err
		}
		return NewFixedListFS(fsys), nil

	default:
		return nil, fmt.Errorf("storage: unsupported filesystem provider: %q", cfg.Provider)
	}
}
