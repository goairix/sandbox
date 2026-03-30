package local

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocalStore_PutAndGet(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	ctx := context.Background()

	data := []byte("hello sandbox")
	err := store.Put(ctx, "test/file.txt", bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)

	reader, err := store.Get(ctx, "test/file.txt")
	require.NoError(t, err)
	defer reader.Close()

	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, data, got)
}

func TestLocalStore_Delete(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	ctx := context.Background()

	data := []byte("to delete")
	err := store.Put(ctx, "del.txt", bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)

	err = store.Delete(ctx, "del.txt")
	require.NoError(t, err)

	exists, err := store.Exists(ctx, "del.txt")
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestLocalStore_List(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	ctx := context.Background()

	files := []string{"prefix/a.txt", "prefix/b.txt", "other/c.txt"}
	for _, f := range files {
		data := []byte("content")
		err := store.Put(ctx, f, bytes.NewReader(data), int64(len(data)))
		require.NoError(t, err)
	}

	objs, err := store.List(ctx, "prefix/")
	require.NoError(t, err)
	assert.Len(t, objs, 2)
}

func TestLocalStore_Exists(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	ctx := context.Background()

	exists, err := store.Exists(ctx, "nope.txt")
	require.NoError(t, err)
	assert.False(t, exists)

	data := []byte("yes")
	err = store.Put(ctx, "yes.txt", bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)

	exists, err = store.Exists(ctx, "yes.txt")
	require.NoError(t, err)
	assert.True(t, exists)
}

func TestLocalStore_GetNotFound(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	ctx := context.Background()

	_, err := store.Get(ctx, "nonexistent.txt")
	assert.Error(t, err)
}

func TestLocalStore_PresignedURLsNotSupported(t *testing.T) {
	dir := t.TempDir()
	store := New(filepath.Join(dir, "store"))
	ctx := context.Background()

	_, err := store.PresignedPutURL(ctx, "key", 0)
	assert.Error(t, err)

	_, err = store.PresignedGetURL(ctx, "key", 0)
	assert.Error(t, err)
}
