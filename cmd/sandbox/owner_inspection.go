package main

import (
	"github.com/goairix/fs"
	"github.com/goairix/sandbox/internal/config"
	"github.com/goairix/sandbox/internal/storage"
)

// Owner inspection uses identity metadata only. Provider constructors may
// create buckets/directories, which is inappropriate even before a read-only
// audit discovers that this configuration has no remote workspace owners.
func newFilesystemForMode(cfg config.FileSystemConfig, storageIdentity string, inspectOwners bool) (fs.FileSystem, *storage.FileSystemMeta, error) {
	if !inspectOwners {
		return storage.NewFileSystemFromConfiguredCredentials(cfg, storageIdentity)
	}
	return nil, &storage.FileSystemMeta{
		Provider: storage.StorageProvider(cfg.Provider), Bucket: cfg.Bucket,
		Region: cfg.Region, Endpoint: cfg.Endpoint, SubPath: cfg.SubPath,
		UseSSL: cfg.UseSSL, LocalPath: cfg.LocalPath, StorageIdentity: storageIdentity,
	}, nil
}
