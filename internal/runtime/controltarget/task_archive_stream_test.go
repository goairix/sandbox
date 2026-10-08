//go:build linux || darwin

package controltarget

import (
	"context"
	"crypto/ed25519"
	"github.com/goairix/sandbox/internal/runtime/launcher"
	"github.com/stretchr/testify/require"
	"io"
	"os"
	"testing"
	"time"
)

func archiveHostAccepted(t *testing.T) (archiveFixture, *AcceptedWorkspaceArchive) {
	t.Helper()
	f := archiveHostSetup(t)
	a, err := f.j.AcceptWorkspaceArchive(context.Background(), f.archive, f.q, f.quiescedReceipt)
	require.NoError(t, err)
	return f, a
}

func TestTaskArchiveStreamSealedSessionAuthentication(t *testing.T) {
	for _, mode := range []string{"past-admission", "archive-missing", "quiescence-missing", "key-seed", "clock", "birth", "copied-terminal"} {
		t.Run(mode, func(t *testing.T) {
			f, a := archiveHostAccepted(t)
			key := f.runtimeKey
			want := a.record
			switch mode {
			case "past-admission":
				f.clock.now = f.archive.NotAfter().Add(time.Second)
			case "archive-missing":
				require.NoError(t, os.Remove(f.o.Directory+"/workspace-archive.json"))
			case "quiescence-missing":
				require.NoError(t, os.Remove(f.o.Directory+"/users-quiesce.json"))
			case "key-seed":
				key = append(ed25519.PrivateKey(nil), key...)
				key[0] ^= 1
			case "clock":
				f.clock.err = io.ErrClosedPipe
			case "birth":
				f.j.birth.UID++
			case "copied-terminal":
				want.State = "archive_complete"
				want.ArchiveBytes = 1536
				want.EntryCount = 1
				want.SHA256 = f.archive.Digest()
			}
			f.j.mu.Lock()
			err := f.j.archiveLiveLocked(context.Background(), a, want, key, true)
			f.j.mu.Unlock()
			if mode == "past-admission" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

// A labeled host registration exercises mutex/cancellation and FD lifetime;
// the native test separately proves the actual source/writer owner joins.
func TestTaskArchiveCloseRegisteredOwner(t *testing.T) {
	f, a := archiveHostAccepted(t)
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel, a.done = cancel, make(chan struct{})
	fd := f.j.root
	done1, done2 := make(chan error, 1), make(chan error, 1)
	go func() { done1 <- f.j.Close() }()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel registered owner")
	}
	go func() { done2 <- f.j.Close() }()
	for _, done := range []chan error{done1, done2} {
		select {
		case err := <-done:
			t.Fatalf("Close returned before owner: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	_, err := fd.Stat()
	require.NoError(t, err)
	require.True(t, f.j.Status().Closed)
	f.j.mu.Lock()
	close(a.done)
	a.done, a.cancel = nil, nil
	f.j.mu.Unlock()
	for _, done := range []chan error{done1, done2} {
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatal("Close did not join")
		}
	}
	_, err = fd.Stat()
	require.Error(t, err)
}

func TestTaskArchiveStreamOriginalRefusals(t *testing.T) {
	for _, mode := range []string{"nil", "copy", "foreign", "nil-source", "nil-writer", "canceled", "key", "deadline", "session", "retained", "quiescence"} {
		t.Run(mode, func(t *testing.T) {
			f, a := archiveHostAccepted(t)
			original := a
			ctx := context.Background()
			key := f.runtimeKey
			var dst io.Writer = io.Discard
			source := &launcher.WorkspaceArchiveSource{}
			switch mode {
			case "nil":
				a = nil
			case "copy":
				copied := *a
				a = &copied
			case "foreign":
				_, a = archiveHostAccepted(t)
			case "nil-source":
				source = nil
			case "nil-writer":
				dst = nil
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "key":
				key = ed25519.PrivateKey(make([]byte, 64))
			case "deadline":
				a.deadline = time.Now().Add(-time.Second)
			case "session":
				f.clock.now = f.archive.SessionNotAfter()
			case "retained":
				require.NoError(t, f.j.root.Chmod(0755))
			case "quiescence":
				f.q.terminal = nil
			}
			wire, err := f.j.StreamWorkspaceArchive(ctx, a, source, dst, key)
			require.Error(t, err)
			require.Empty(t, wire)
			if mode == "nil" || mode == "copy" || mode == "foreign" {
				require.False(t, original.consumed)
				require.False(t, f.j.Status().Poisoned)
			} else {
				require.True(t, original.consumed)
				require.True(t, f.j.Status().Poisoned)
				wire, err = f.j.StreamWorkspaceArchive(context.Background(), original, source, io.Discard, f.runtimeKey)
				require.Error(t, err)
				require.Empty(t, wire)
			}
			wire, err = f.j.SignWorkspaceArchiveReceipt(context.Background(), original, nil, f.runtimeKey)
			require.Error(t, err)
			require.Empty(t, wire)
		})
	}
}

func TestTaskArchiveDeadlineOriginal(t *testing.T) {
	f, a := archiveHostAccepted(t)
	d, err := a.Deadline()
	require.NoError(t, err)
	require.Equal(t, a.deadline, d)
	copy := *a
	_, err = copy.Deadline()
	require.Error(t, err)
	require.NoError(t, f.j.Close())
	d, err = a.Deadline()
	require.NoError(t, err)
	require.Equal(t, a.deadline, d)
}
