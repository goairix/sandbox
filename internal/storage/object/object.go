package object

import (
	"context"
	"io"
	"time"
)

// ObjectInfo holds metadata about a stored object.
type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// Store is the abstraction over object storage backends.
type Store interface {
	// Put uploads an object.
	Put(ctx context.Context, key string, reader io.Reader, size int64) error
	// Get downloads an object.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes an object.
	Delete(ctx context.Context, key string) error
	// List returns objects with the given prefix.
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
	// Exists checks if an object exists.
	Exists(ctx context.Context, key string) (bool, error)
	// PresignedPutURL returns a pre-signed URL for uploading.
	PresignedPutURL(ctx context.Context, key string, expires time.Duration) (string, error)
	// PresignedGetURL returns a pre-signed URL for downloading.
	PresignedGetURL(ctx context.Context, key string, expires time.Duration) (string, error)
}
