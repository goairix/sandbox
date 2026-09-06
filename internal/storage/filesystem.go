package storage

import (
	"errors"
	"fmt"
	"io"
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

// NewFileSystemFromConfiguredCredentials constructs the sync filesystem from
// either inline or file-backed credentials. Credential bytes are owned only
// for the duration of provider construction and are zeroed before return.
// Local storage never attempts to load object-store credentials.
func NewFileSystemFromConfiguredCredentials(cfg config.FileSystemConfig, storageIdentity string) (fs.FileSystem, *FileSystemMeta, error) {
	if cfg.Provider == "local" {
		return NewFileSystemWithStorageIdentity(cfg, storageIdentity)
	}
	credentials, err := LoadFileSystemCredentials(cfg)
	if err != nil {
		return nil, nil, err
	}
	defer credentials.Zero()

	providerConfig := cfg
	providerConfig.AccessKey = string(credentials.AccessKey)
	providerConfig.SecretKey = string(credentials.SecretKey)
	providerConfig.CredentialFiles = config.FileSystemCredentialFileConfig{}
	return NewFileSystemWithStorageIdentity(providerConfig, storageIdentity)
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

	return loadCredentialFiles(files, credentialLoadHooks{})
}

const credentialLoadMaxAttempts = 3

type credentialLoadHooks struct {
	afterOpen     func(attempt int)
	afterValidate func(attempt int)
}

func loadCredentialFiles(files config.FileSystemCredentialFileConfig, hooks credentialLoadHooks) (FileSystemCredentials, error) {
	for attempt := 0; attempt < credentialLoadMaxAttempts; attempt++ {
		credentials, retry, err := loadCredentialFilesAttempt(files, hooks, attempt)
		if err != nil {
			return FileSystemCredentials{}, err
		}
		if !retry {
			return credentials, nil
		}
	}
	return FileSystemCredentials{}, fmt.Errorf("storage: credential files changed while loading")
}

func loadCredentialFilesAttempt(files config.FileSystemCredentialFileConfig, hooks credentialLoadHooks, attempt int) (FileSystemCredentials, bool, error) {
	accessFile, err := os.Open(files.AccessKeyFile)
	if err != nil {
		return FileSystemCredentials{}, false, fmt.Errorf("storage: open access key credential file: %w", err)
	}
	defer accessFile.Close()

	secretFile, err := os.Open(files.SecretKeyFile)
	if err != nil {
		return FileSystemCredentials{}, false, fmt.Errorf("storage: open secret key credential file: %w", err)
	}
	defer secretFile.Close()

	if hooks.afterOpen != nil {
		hooks.afterOpen(attempt)
	}

	accessInfo, err := accessFile.Stat()
	if err != nil {
		return FileSystemCredentials{}, false, fmt.Errorf("storage: inspect open access key credential file: %w", err)
	}
	secretInfo, err := secretFile.Stat()
	if err != nil {
		return FileSystemCredentials{}, false, fmt.Errorf("storage: inspect open secret key credential file: %w", err)
	}
	if err := validateCredentialFilePermissions(accessInfo, "access key"); err != nil {
		return FileSystemCredentials{}, false, err
	}
	if err := validateCredentialFilePermissions(secretInfo, "secret key"); err != nil {
		return FileSystemCredentials{}, false, err
	}

	accessCurrent, accessRetry, err := currentCredentialFileInfo(files.AccessKeyFile, "access key")
	if err != nil {
		return FileSystemCredentials{}, false, err
	}
	secretCurrent, secretRetry, err := currentCredentialFileInfo(files.SecretKeyFile, "secret key")
	if err != nil {
		return FileSystemCredentials{}, false, err
	}
	if accessRetry || secretRetry || !os.SameFile(accessInfo, accessCurrent) || !os.SameFile(secretInfo, secretCurrent) {
		return FileSystemCredentials{}, true, nil
	}

	if hooks.afterValidate != nil {
		hooks.afterValidate(attempt)
	}

	accessKey, err := readOpenCredentialFile(accessFile, "access key")
	if err != nil {
		return FileSystemCredentials{}, false, err
	}
	secretKey, err := readOpenCredentialFile(secretFile, "secret key")
	if err != nil {
		zeroBytes(accessKey)
		return FileSystemCredentials{}, false, err
	}
	return FileSystemCredentials{AccessKey: accessKey, SecretKey: secretKey}, false, nil
}

func currentCredentialFileInfo(path, name string) (os.FileInfo, bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("storage: inspect current %s credential file: %w", name, err)
	}
	return info, false, nil
}

func validateCredentialFilePermissions(info os.FileInfo, name string) error {
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o044 != 0 {
		return fmt.Errorf("storage: %s credential file permissions allow group or world read access", name)
	}
	return nil
}

func readOpenCredentialFile(file *os.File, name string) ([]byte, error) {
	return readCredentialValue(file, name)
}

func readCredentialValue(reader io.Reader, name string) ([]byte, error) {
	value, err := io.ReadAll(reader)
	if err != nil {
		zeroBytes(value)
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
