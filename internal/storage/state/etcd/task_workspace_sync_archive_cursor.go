package etcd

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
)

// The cursor is mechanical: its reader is registered and guarded by its owner.
// Body offsets count actual TAR/PAX consumption; no header seek is accepted.
type taskWorkspaceSyncArchiveCursor struct {
	reader                *tar.Reader
	readAt                func(context.Context, []byte, int64) (int, error)
	ctx                   context.Context
	offset                int64
	count, bytes, entries uint64
	index                 []byte
	plan                  TaskWorkspaceSyncPlan
}

type workspaceSyncArchiveEntry struct {
	workspaceSyncLocalFile
	kind byte
}

func newWorkspaceSyncArchiveCursor(bytes, entries uint64, index []byte, plan TaskWorkspaceSyncPlan, readAt func(context.Context, []byte, int64) (int, error)) *taskWorkspaceSyncArchiveCursor {
	c := &taskWorkspaceSyncArchiveCursor{bytes: bytes, entries: entries, index: index, plan: plan, readAt: readAt}
	c.reader = tar.NewReader(c)
	return c
}

func (c *taskWorkspaceSyncArchiveCursor) Read(data []byte) (int, error) {
	if c.offset == int64(c.bytes) {
		return 0, io.EOF
	}
	if int64(len(data)) > int64(c.bytes)-c.offset {
		data = data[:int64(c.bytes)-c.offset]
	}
	n, err := c.readAt(c.ctx, data, c.offset)
	c.offset += int64(n)
	return n, err
}

func (c *taskWorkspaceSyncArchiveCursor) next(ctx context.Context) (*workspaceSyncArchiveEntry, error) {
	c.ctx = ctx
	var buffer [32768]byte
	for {
		h, err := c.reader.Next()
		if err == io.EOF {
			if c.count != c.entries || c.offset != int64(c.bytes) {
				return nil, errTaskArchive
			}
			return nil, io.EOF
		}
		if err != nil {
			return nil, err
		}
		if c.count >= c.entries || h.Size < 0 || h.Size > int64(c.bytes)-c.offset || h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
			return nil, errTaskArchive
		}
		name, err := taskArchivePath(h, c.count == 0)
		if err != nil {
			return nil, err
		}
		kind, err := taskArchiveIndex(ctx, c.index, name, 0, false)
		if err != nil || kind != h.Typeflag {
			return nil, errTaskArchive
		}
		offset := c.offset
		hash := sha256.New()
		n, err := io.CopyBuffer(hash, c.reader, buffer[:])
		if err != nil || n != h.Size {
			return nil, errors.Join(errTaskArchive, err)
		}
		c.count++
		if name == "workspace" {
			continue
		}
		path := strings.TrimPrefix(name, "workspace/")
		excluded, err := c.plan.Excludes(path)
		if err != nil {
			return nil, err
		}
		if excluded {
			continue
		}
		return &workspaceSyncArchiveEntry{workspaceSyncLocalFile{path, h.Mode, h.Size, offset, hex.EncodeToString(hash.Sum(nil))}, h.Typeflag}, nil
	}
}
