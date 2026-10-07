package launcher

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"runtime"
	"testing"
)

func TestWorkspaceArchiveInvalidHandles(t *testing.T) {
	for _, s := range []*WorkspaceArchiveSource{nil, {}, {self: &WorkspaceArchiveSource{}}} {
		if summary, err := s.WriteArchive(context.Background(), io.Discard); err == nil || summary != (WorkspaceArchiveSummary{}) {
			t.Fatalf("invalid handle: %+v %v", summary, err)
		}
		if err := s.Close(); err == nil {
			t.Fatal("invalid close accepted")
		}
	}
	for _, b := range []*KernelBoundary{nil, {}, {self: &KernelBoundary{}}} {
		if s, err := b.OpenWorkspaceArchive(context.Background(), nil); s != nil || err == nil {
			t.Fatalf("invalid boundary: %v %v", s, err)
		}
	}
	if runtime.GOOS != "linux" {
		if s, err := (&KernelBoundary{}).OpenWorkspaceArchive(context.Background(), nil); s != nil || !errors.Is(err, ErrUnsupported) {
			t.Fatalf("unsupported native archive: %v %v", s, err)
		}
	}
}

func TestWorkspaceArchiveCopyRejectsBeforeLock(t *testing.T) {
	root, err := os.CreateTemp(t.TempDir(), "unused")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	original := &WorkspaceArchiveSource{root: root}
	original.self = original
	copied := &WorkspaceArchiveSource{self: original, root: root}
	copied.mu.Lock()
	defer copied.mu.Unlock()
	if summary, err := copied.WriteArchive(context.Background(), io.Discard); err == nil || summary != (WorkspaceArchiveSummary{}) {
		t.Fatal("copy streamed")
	}
	if err := copied.Close(); err == nil {
		t.Fatal("copy closed")
	}
	if _, err := root.Write([]byte("original descriptor remains open")); err != nil {
		t.Fatal(err)
	}
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceArchiveNames(t *testing.T) {
	for _, name := range []string{"", ".", "..", "a/b", "a\\b", "zero\x00", "line\n", "del\x7f", "next\u0085", string([]byte{0xff})} {
		if archiveComponentValid(name) {
			t.Fatalf("unsafe name accepted: %q", name)
		}
	}
	for _, name := range []string{"hello.txt", "你好", "..hidden", "space name"} {
		if !archiveComponentValid(name) {
			t.Fatalf("valid name rejected: %q", name)
		}
	}
}

func TestWorkspaceArchiveTraversalBudget(t *testing.T) {
	limit := archiveLimits{depth: 2, pathBytes: 20, entries: 3, bytes: 100}
	if err := limit.checkEntry(2, 20, 2); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		depth, path int
		count       uint64
	}{{3, 20, 2}, {2, 21, 2}, {2, 20, 3}} {
		if err := limit.checkEntry(tc.depth, tc.path, tc.count); err == nil {
			t.Fatal("overflow accepted")
		}
	}
}

type archiveTestWriter func([]byte) (int, error)

func (f archiveTestWriter) Write(p []byte) (int, error) { return f(p) }

func TestWorkspaceArchiveByteBudgetAndWriterErrors(t *testing.T) {
	var out bytes.Buffer
	w := archiveOutput{ctx: context.Background(), dst: &out, hash: sha256.New(), limit: 4}
	if n, err := w.Write([]byte("four")); n != 4 || err != nil {
		t.Fatalf("exact cap: %d %v", n, err)
	}
	if n, err := w.Write([]byte("x")); n != 0 || err == nil || out.String() != "four" {
		t.Fatalf("cap overflow: %d %v %q", n, err, out.String())
	}
	for _, result := range []struct {
		n   int
		err error
	}{{1, nil}, {-1, nil}, {3, nil}, {0, io.ErrClosedPipe}} {
		w := archiveOutput{ctx: context.Background(), dst: archiveTestWriter(func(p []byte) (int, error) { return result.n, result.err }), hash: sha256.New(), limit: 20}
		if _, err := w.Write([]byte("ok")); err == nil {
			t.Fatal("writer failure accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.ctx = ctx
	if _, err := w.Write(nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
