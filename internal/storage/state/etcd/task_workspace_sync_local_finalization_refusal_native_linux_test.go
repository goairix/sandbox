//go:build linux

package etcd

import (
	"archive/tar"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestTaskWorkspaceSyncLocalFinalizationNativeRefusal(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	for _, kind := range []string{"protected-type", "symlink", "hardlink", "non-dfs", "leased-progress", "root-aba"} {
		t.Run(kind, func(t *testing.T) {
			headers := []*tar.Header{{Name: "workspace", Typeflag: tar.TypeDir, Mode: 0700}, {Name: "workspace/file", Typeflag: tar.TypeReg, Mode: 0600, Size: 3}}
			bodies := []string{"", "new"}
			if kind == "non-dfs" {
				headers = []*tar.Header{{Name: "workspace", Typeflag: tar.TypeDir, Mode: 0700}, {Name: "workspace/a", Typeflag: tar.TypeDir, Mode: 0700}, {Name: "workspace/b", Typeflag: tar.TypeDir, Mode: 0700}, {Name: "workspace/a/late", Typeflag: tar.TypeReg, Mode: 0600, Size: 3}}
				bodies = []string{"", "", "", "new"}
			}
			var exclude []string
			if kind == "protected-type" {
				exclude = []string{"file/keep"}
			}
			f, wire, root := finalizationNativeCold(t, archiveTarBytes(t, headers, bodies), uint64(len(headers)), exclude, false, func(root string) {
				switch kind {
				case "protected-type":
					require.NoError(t, os.MkdirAll(filepath.Join(root, "file/keep"), 0700))
					require.NoError(t, os.WriteFile(filepath.Join(root, "file/keep/original"), []byte("old"), 0600))
				case "symlink":
					require.NoError(t, os.WriteFile(filepath.Join(root, "other"), []byte("old"), 0600))
					require.NoError(t, os.Symlink("other", filepath.Join(root, "file")))
				case "hardlink":
					require.NoError(t, os.WriteFile(filepath.Join(root, "other"), []byte("old"), 0600))
					require.NoError(t, os.Link(filepath.Join(root, "other"), filepath.Join(root, "file")))
				}
			})
			if kind == "leased-progress" {
				key, _, err := f.b.namespace.workspaceSyncLocalKeys(f.c.reference.Task)
				require.NoError(t, err)
				lease, err := f.raw.Grant(f.ctx, 30)
				require.NoError(t, err)
				_, err = f.raw.Put(f.ctx, key, "{}", clientv3.WithLease(lease.ID))
				require.NoError(t, err)
			}
			s, err := f.b.prepareTaskWorkspaceSyncRecoveryMode(f.ctx, f.c, 0, wire.destination, workspaceSyncLocalNativeWorker, true)
			require.NoError(t, err)
			if kind == "root-aba" {
				require.NoError(t, os.Rename(root, root+"-old"))
				require.NoError(t, os.Mkdir(root, 0700))
				t.Cleanup(func() { _ = os.RemoveAll(root); _ = os.Rename(root+"-old", root) })
			}
			writes := 0
			s.localIOComplete = func(string) { writes++ }
			r, err := s.finalizeLocal(f.ctx)
			require.Error(t, err)
			require.False(t, r.LocalComplete)
			require.Zero(t, writes, "refusal precedes target mutation")
			require.NoError(t, s.Close())
			if kind == "protected-type" {
				got, err := os.ReadFile(filepath.Join(root, "file/keep/original"))
				require.NoError(t, err)
				require.Equal(t, "old", string(got))
			}
			if kind == "hardlink" || kind == "symlink" {
				got, err := os.ReadFile(filepath.Join(root, "other"))
				require.NoError(t, err)
				require.Equal(t, "old", string(got))
			}
		})
	}
}

func TestTaskWorkspaceSyncLocalFinalizationNativeOriginalRefusal(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	f, prepared, wire, _ := recoveryNativePrepared(t)
	key, _, err := f.b.namespace.workspaceSyncLocalKeys(f.c.reference.Task)
	require.NoError(t, err)
	_, err = f.raw.Put(f.ctx, key, "{}")
	require.NoError(t, err)
	_, err = f.b.ReceiveTaskWorkspaceArchive(f.ctx, prepared)
	require.ErrorIs(t, err, ErrConflict)
	require.Zero(t, wire.sends.Load())
}

func TestTaskWorkspaceSyncLocalFinalizationNativePreparedRefusal(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	f, prepared, wire, plan := recoveryNativePrepared(t)
	done := deliveryBegin(f, prepared)
	deliveryWaitSent(t, wire, done)
	close(wire.body)
	r := deliveryWait(t, done)
	require.NoError(t, r.err)
	require.NoError(t, <-wire.done)
	key, _, err := f.b.namespace.workspaceSyncLocalKeys(f.c.reference.Task)
	require.NoError(t, err)
	_, err = f.raw.Put(f.ctx, key, "{}")
	require.NoError(t, err)
	source, err := f.b.PrepareTaskWorkspaceSyncSource(f.ctx, r.archive, plan)
	require.ErrorIs(t, err, ErrConflict)
	require.Nil(t, source)
	spool := r.archive.owner.spool
	spool.store.mu.Lock()
	consumer := spool.consumer
	spool.store.mu.Unlock()
	require.NotNil(t, consumer, "refusal retains original registered owner through cleanup")
	select {
	case <-consumer.done:
	default:
		t.Fatal("refused prepared source did not join its actual owner")
	}
	require.NoError(t, consumer.Close())
}

func TestTaskWorkspaceSyncLocalFinalizationNativeBounds(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	headers := []*tar.Header{{Name: "workspace", Typeflag: tar.TypeDir, Mode: 0700}, {Name: "workspace/" + strings.Repeat("<", 255), Typeflag: tar.TypeReg, Mode: 0600, Size: 1}}
	bodies := []string{"", "x"}
	path := ""
	for i := 0; i < 128; i++ {
		if path != "" {
			path += "/"
		}
		path += "d"
		headers = append(headers, &tar.Header{Name: "workspace/" + path, Typeflag: tar.TypeDir, Mode: 0555})
		bodies = append(bodies, "")
	}
	data := archiveTarBytes(t, headers, bodies)
	f, wire, root := finalizationNativeCold(t, data, uint64(len(headers)), nil, true, func(root string) {
		for i := 0; i < 256; i++ {
			require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("extra-%03d", i)), nil, 0600))
		}
	}, 64)
	s, err := f.b.prepareTaskWorkspaceSyncRecoveryMode(f.ctx, f.c, 0, wire.destination, workspaceSyncLocalNativeWorker, true)
	require.NoError(t, err)
	readBytes := 0
	s.localArchiveReadComplete = func(n int) { readBytes += n }
	r, err := s.finalizeLocal(f.ctx)
	require.NoError(t, err)
	require.True(t, r.LocalComplete)
	require.EqualValues(t, 256, r.RemovedEntries)
	require.NoError(t, s.Close())
	require.LessOrEqual(t, readBytes, 4*len(data)+1, "archive passes stay fixed at order+regular+directories+modes")
	for _, entry := range headers[2:] {
		require.NoError(t, os.Chmod(filepath.Join(root, strings.TrimPrefix(entry.Name, "workspace/")), 0700))
	}
	_, err = os.Stat(filepath.Join(root, path))
	require.NoError(t, err)
}
