package etcd

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTaskWorkspaceSyncSourceRejectsUnoriginal(t *testing.T) {
	var b *Backend
	s, err := b.PrepareTaskWorkspaceSyncSource(context.Background(), nil, TaskWorkspaceSyncPlan{})
	require.Error(t, err)
	require.Nil(t, s)
}

// Lower-level host fixture supplies local seals only. It never admits through
// the public source API and establishes no current-native ownership evidence.
func syncSourceHostIO(t *testing.T, data []byte) (*Backend, *PreparedTaskWorkspaceSyncSource) {
	t.Helper()
	store, err := newTaskArchiveStore(archiveTestDirectory(t), archiveTestLimits())
	require.NoError(t, err)
	r, err := store.reserve(context.Background())
	require.NoError(t, err)
	_, err = r.file.Write(data)
	require.NoError(t, err)
	b := &Backend{archiveStore: store, archiveLife: &taskArchiveBackend{}}
	c := &TaskClaim{origin: b, parentCtx: context.Background(), reference: TaskClaimReference{LeaseID: 1, CreateRevision: 1}}
	c.self = c
	f := &taskFinalSyncDraft{}
	d := &taskArchiveDraft{finalSync: f}
	p := &PreparedTaskWorkspaceArchive{origin: b, claim: c, finalSync: f, draft: d}
	p.self = p
	d.preparedArchive = p
	f.archive = d
	c.finalSyncDraft = f
	o := &taskArchiveDelivery{backend: b, claim: c, prepared: p, spool: r, done: make(chan struct{}), wake: make(chan struct{}, 1)}
	close(o.done)
	deadline := time.Now().Add(time.Minute)
	o.deadline.Store(&deadline)
	c.archiveDelivery = o
	archive := &ReceivedTaskWorkspaceArchive{origin: b, owner: o}
	archive.self = archive
	ctx, cancel := context.WithCancel(context.Background())
	s := &PreparedTaskWorkspaceSyncSource{archive: archive, ctx: ctx, cancel: cancel, done: make(chan struct{}), diagnostics: taskArchiveDiagnostics{Bytes: uint64(len(data))}}
	s.self = s
	r.consumer = s
	go s.own()
	t.Cleanup(func() { _ = s.Close(); _ = b.Close() })
	return b, s
}

func TestTaskWorkspaceSyncSourceHostBoundedStreamUSTARAndPAX(t *testing.T) {
	for _, format := range []tar.Format{tar.FormatUSTAR, tar.FormatPAX} {
		t.Run(format.String(), func(t *testing.T) {
			large := strings.Repeat("x", 130001)
			name := "workspace/large"
			if format == tar.FormatPAX {
				name = "workspace/" + strings.Repeat("p", 140)
			}
			data := archiveTarBytes(t, []*tar.Header{{Name: "workspace", Typeflag: tar.TypeDir, Mode: 0700, Format: format}, {Name: "workspace/empty", Typeflag: tar.TypeReg, Mode: 0600, Format: format}, {Name: name, Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(large)), Format: format}}, []string{"", "", large})
			_, source := syncSourceHostIO(t, data)
			require.NoError(t, source.begin(context.Background()))
			defer source.end()
			stream := &taskWorkspaceSyncStream{source: source, ctx: source.ctx}
			reader := tar.NewReader(stream)
			for i := 0; i < 3; i++ {
				h, err := reader.Next()
				require.NoError(t, err)
				offset := stream.offset
				var buffer [32768]byte
				hash := sha256.New()
				n, err := io.CopyBuffer(hash, reader, buffer[:])
				require.NoError(t, err)
				require.Equal(t, h.Size, n)
				if i == 2 {
					require.Greater(t, offset, int64(1024))
					sum := sha256.Sum256([]byte(large))
					require.Equal(t, sum[:], hash.Sum(nil))
					actual := make([]byte, 4)
					_, err = source.archive.owner.spool.file.ReadAt(actual, offset)
					require.NoError(t, err)
					require.Equal(t, "xxxx", string(actual))
				}
			}
			_, err := reader.Next()
			require.ErrorIs(t, err, io.EOF)
			require.EqualValues(t, len(data), stream.offset)
		})
	}
}

func TestTaskWorkspaceSyncSourceHostBackendCancelsBeforeDeliveryJoin(t *testing.T) {
	b, source := syncSourceHostIO(t, bytes.Repeat([]byte{1}, 1536))
	deliveryCtx, deliveryCancel := context.WithCancel(context.Background())
	deliveryDone := make(chan struct{})
	b.archiveLife.owners = map[*taskArchiveDelivery]struct{}{{ctx: deliveryCtx, cancel: deliveryCancel, done: deliveryDone}: {}}
	require.NoError(t, source.begin(source.ctx))
	closed := make(chan error, 1)
	go func() { closed <- b.Close() }()
	select {
	case <-source.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("source cancellation queued behind delivery join")
	}
	select {
	case <-deliveryCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("delivery cancellation absent")
	}
	require.Error(t, source.begin(context.Background()), "sealed IO cannot be newly admitted")
	b.archiveStore.mu.Lock()
	require.EqualValues(t, 1, b.archiveStore.jobs)
	b.archiveStore.mu.Unlock()
	select {
	case <-closed:
		t.Fatal("Backend closed before held delivery and source owners joined")
	default:
	}
	close(deliveryDone)
	select {
	case <-closed:
		t.Fatal("Backend closed before source admitted operation joined")
	case <-time.After(20 * time.Millisecond):
	}
	source.end()
	require.NoError(t, <-closed)
	require.Zero(t, b.archiveStore.jobs)
}

func TestTaskWorkspaceSyncSourceHostExclusionBoundary(t *testing.T) {
	plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspaceSyncIdentity(t, "s3", "test-storage", "bucket", "team/project/"), Exclude: []string{"cache"}})
	require.NoError(t, err)
	for _, path := range []string{"cache", "cache/file", "cache/"} {
		excluded, err := plan.Excludes(path)
		require.NoError(t, err)
		require.True(t, excluded)
	}
	for _, path := range []string{"cache-other", "cache2/file", "other/cache"} {
		excluded, err := plan.Excludes(path)
		require.NoError(t, err)
		require.False(t, excluded)
	}
}
