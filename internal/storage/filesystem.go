package storage

import (
	"fmt"
	"os"
	"runtime"

	"github.com/goairix/fs"
	"github.com/goairix/fs/driver/alioss"
	"github.com/goairix/fs/driver/hwobs"
	"github.com/goairix/fs/driver/local"
	"github.com/goairix/fs/driver/minio"
	"github.com/goairix/fs/driver/s3"
	"github.com/goairix/fs/driver/txcos"

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
	Provider        StorageProvider
	LocalPath       string // non-empty only when Provider == ProviderLocal
	Bucket          string
	Region          string
	Endpoint        string
	SubPath         string
	UseSSL          bool
	StorageIdentity string
}

// NewFileSystem creates a fs.FileSystem from the given configuration.
func NewFileSystem(cfg config.FileSystemConfig) (fs.FileSystem, *FileSystemMeta, error) {
	return NewFileSystemWithStorageIdentity(cfg, "")
}

// NewFileSystemWithStorageIdentity creates a legacy sync-mode filesystem and
// records the explicitly configured identity of its physical object namespace.
func NewFileSystemWithStorageIdentity(cfg config.FileSystemConfig, storageIdentity string) (fs.FileSystem, *FileSystemMeta, error) {
	meta := &FileSystemMeta{
		Provider:        StorageProvider(cfg.Provider),
		Bucket:          cfg.Bucket,
		Region:          cfg.Region,
		Endpoint:        cfg.Endpoint,
		SubPath:         cfg.SubPath,
		UseSSL:          cfg.UseSSL,
		StorageIdentity: storageIdentity,
	}

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

// FileSystemCredentials owns the credential buffers returned by
// LoadFileSystemCredentials. Callers should zero them immediately after client
// construction.
type FileSystemCredentials struct {
	AccessKey []byte
	SecretKey []byte
}

// Zero overwrites and releases both owned credential buffers.
func (c *FileSystemCredentials) Zero() {
	if c == nil {
		return
	}
	zeroBytes(c.AccessKey)
	zeroBytes(c.SecretKey)
	c.AccessKey = nil
	c.SecretKey = nil
}

func zeroBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

// LoadFileSystemCredentials loads one complete AK/SK pair from either inline
// configuration or credential files. The two sources cannot be mixed.
func LoadFileSystemCredentials(cfg config.FileSystemConfig) (FileSystemCredentials, error) {
	files := cfg.CredentialFiles
	if cfg.SessionToken != "" || cfg.CredentialExpiry != "" || files.SessionTokenFile != "" || files.CredentialExpiryFile != "" {
		return FileSystemCredentials{}, fmt.Errorf("storage: temporary credentials are not supported")
	}

	hasInline := cfg.AccessKey != "" || cfg.SecretKey != ""
	hasFiles := files.AccessKeyFile != "" || files.SecretKeyFile != ""
	if hasInline && hasFiles {
		return FileSystemCredentials{}, fmt.Errorf("storage: inline and file credential sources are mutually exclusive")
	}

	if hasInline {
		if cfg.AccessKey == "" || cfg.SecretKey == "" {
			return FileSystemCredentials{}, fmt.Errorf("storage: inline access key and secret key are both required")
		}
		return FileSystemCredentials{
			AccessKey: []byte(cfg.AccessKey),
			SecretKey: []byte(cfg.SecretKey),
		}, nil
	}

	if !hasFiles || files.AccessKeyFile == "" || files.SecretKeyFile == "" {
		return FileSystemCredentials{}, fmt.Errorf("storage: access key and secret key credential files are both required")
	}

	accessKey, err := readCredentialFile(files.AccessKeyFile, "access key")
	if err != nil {
		return FileSystemCredentials{}, err
	}
	secretKey, err := readCredentialFile(files.SecretKeyFile, "secret key")
	if err != nil {
		zeroBytes(accessKey)
		return FileSystemCredentials{}, err
	}
	return FileSystemCredentials{AccessKey: accessKey, SecretKey: secretKey}, nil
}

func readCredentialFile(path, name string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("storage: inspect %s credential file: %w", name, err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o044 != 0 {
		return nil, fmt.Errorf("storage: %s credential file permissions allow group or world read access", name)
	}

	value, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("storage: read %s credential file: %w", name, err)
	}
	if len(value) > 0 && value[len(value)-1] == '\n' {
		value = value[:len(value)-1]
	}
	if len(value) == 0 {
		return nil, fmt.Errorf("storage: %s credential file is empty", name)
	}
	return value, nil
}
