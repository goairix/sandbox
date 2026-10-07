//go:build linux

package launcher

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Missing physical copying, a wrong archive name, or a false summary breaks
// this assertion against the fixed native volume, rather than a fake tree.
func TestWorkspaceArchiveNativeContent(t *testing.T) {
	b := requireNativeBoundary(t)
	if err := os.Mkdir("/workspace/nested", 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("/workspace/nested/hello.txt", []byte("physical workspace\n"), 0640); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll("/workspace/nested"); err != nil {
			t.Error(err)
		}
	})
	o, err := b.ObserveNoUserDescendants(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s, err := b.OpenWorkspaceArchive(context.Background(), o)
	if err != nil {
		t.Fatalf("open real workspace: %v", err)
	}
	defer s.Close()
	copied := &WorkspaceArchiveSource{self: s, root: s.root, origin: s.origin, observation: s.observation}
	copied.mu.Lock()
	if summary, err := copied.WriteArchive(context.Background(), io.Discard); err == nil || summary != (WorkspaceArchiveSummary{}) {
		t.Fatal("copy accepted")
	}
	if err := copied.Close(); err == nil {
		t.Fatal("copy close accepted")
	}
	copied.mu.Unlock()
	var out bytes.Buffer
	summary, err := s.WriteArchive(context.Background(), &out)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ArchiveBytes != uint64(out.Len()) || summary.EntryCount != 3 || summary.SHA256 != sha256.Sum256(out.Bytes()) {
		t.Fatalf("summary %+v, bytes=%d", summary, out.Len())
	}
	reader := tar.NewReader(bytes.NewReader(out.Bytes()))
	names := map[string]string{}
	for {
		h, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		names[h.Name] = string(body)
		if h.Uid != 0 || h.Gid != 0 || h.Linkname != "" || len(h.Xattrs) != 0 {
			t.Fatalf("authority metadata: %+v", h)
		}
	}
	if len(names) != 3 || names["workspace/nested/hello.txt"] != "physical workspace\n" {
		t.Fatalf("entries: %v", names)
	}
	if _, err := s.WriteArchive(context.Background(), &out); err == nil {
		t.Fatal("second stream accepted")
	}
}

func nativeArchiveSource(t *testing.T) *WorkspaceArchiveSource {
	t.Helper()
	b := requireNativeBoundary(t)
	o, err := b.ObserveNoUserDescendants(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s, err := b.OpenWorkspaceArchive(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func nativeArchiveReject(t *testing.T, s *WorkspaceArchiveSource, ctx context.Context, dst io.Writer) {
	t.Helper()
	rootFD := int(s.root.Fd())
	summary, err := s.WriteArchive(ctx, dst)
	if err == nil || summary != (WorkspaceArchiveSummary{}) {
		t.Fatalf("incomplete accepted: %+v %v", summary, err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(rootFD, &stat); !errors.Is(err, unix.EBADF) {
		t.Fatalf("root descriptor retained: %v", err)
	}
	if summary, err := s.WriteArchive(context.Background(), io.Discard); err == nil || summary != (WorkspaceArchiveSummary{}) {
		t.Fatal("failed stream reused")
	}
	t.Logf("zero-summary terminal rejection: %v", err)
}

func TestWorkspaceArchiveNativeUnsafeObjects(t *testing.T) {
	requireNativeBoundary(t)
	for _, kind := range []string{"symlink", "hardlink", "fifo", "socket", "invalid-name"} {
		t.Run(kind, func(t *testing.T) {
			path := "/workspace/item"
			var socket int = -1
			switch kind {
			case "symlink":
				if err := os.WriteFile("/tmp/outside", []byte("DO-NOT-READ-OUTSIDE"), 0600); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Remove("/tmp/outside") })
				if err := os.Symlink("/tmp/outside", path); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.WriteFile(path, []byte("hardlinked"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(path, "/workspace/other"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Remove("/workspace/other") })
			case "fifo":
				if err := unix.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "socket":
				var err error
				socket, err = unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
				if err != nil {
					t.Fatal(err)
				}
				if err := unix.Bind(socket, &unix.SockaddrUnix{Name: path}); err != nil {
					unix.Close(socket)
					t.Fatal(err)
				}
				t.Cleanup(func() { unix.Close(socket) })
			case "invalid-name":
				path = "/workspace/line\n"
				if err := os.WriteFile(path, []byte("invalid"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() {
				if err := os.Remove(path); err != nil {
					t.Error(err)
				}
			})
			var out bytes.Buffer
			nativeArchiveReject(t, nativeArchiveSource(t), context.Background(), &out)
			if bytes.Contains(out.Bytes(), []byte("DO-NOT-READ-OUTSIDE")) {
				t.Fatal("symlink target leaked")
			}
		})
	}
}

func TestWorkspaceArchiveNativeFailuresAndLimits(t *testing.T) {
	requireNativeBoundary(t)
	for _, kind := range []string{"canceled", "writer-error", "short-writer", "close-padding-cap", "entry-cap", "depth-cap", "path-cap", "root-pin-entry", "root-pin-completion"} {
		t.Run(kind, func(t *testing.T) {
			if err := os.Mkdir("/workspace/dir", 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile("/workspace/dir/file", []byte("payload"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.RemoveAll("/workspace/dir"); err != nil {
					t.Error(err)
				}
			})
			s := nativeArchiveSource(t)
			ctx := context.Background()
			var out bytes.Buffer
			var dst io.Writer = &out
			switch kind {
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			case "writer-error":
				dst = archiveTestWriter(func([]byte) (int, error) { return 0, io.ErrClosedPipe })
			case "short-writer":
				dst = archiveTestWriter(func(p []byte) (int, error) {
					if len(p) == 0 {
						return 0, nil
					}
					return len(p) - 1, nil
				})
			case "close-padding-cap":
				// Compute completed size through a distinct original source, then
				// allow all but the final tar block on the tested source.
				baseline := nativeArchiveSource(t)
				full, err := baseline.WriteArchive(ctx, io.Discard)
				if err != nil {
					t.Fatal(err)
				}
				s.limits.bytes = full.ArchiveBytes - 1
			case "entry-cap":
				s.limits.entries = 2
			case "depth-cap":
				s.limits.depth = 1
			case "path-cap":
				s.limits.pathBytes = 15
			case "root-pin-entry":
				s.ino++
			case "root-pin-completion":
				dst = archiveTestWriter(func(p []byte) (int, error) { s.ino++; return out.Write(p) })
			}
			nativeArchiveReject(t, s, ctx, dst)
		})
	}
}

func TestWorkspaceArchiveNativeMutationAndNewProcess(t *testing.T) {
	requireNativeBoundary(t)
	for _, kind := range []string{"truncate", "same-size-write", "new-process"} {
		t.Run(kind, func(t *testing.T) {
			path := "/workspace/change.txt"
			if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.Remove(path); err != nil {
					t.Error(err)
				}
			})
			s := nativeArchiveSource(t)
			changed := false
			var child *os.Process
			t.Cleanup(func() {
				if child != nil {
					if _, err := child.Wait(); err != nil {
						t.Error(err)
					}
				}
			})
			dst := archiveTestWriter(func(p []byte) (int, error) {
				if !changed && bytes.Contains(p, []byte("workspace/change.txt")) {
					changed = true
					var err error
					switch kind {
					case "truncate":
						err = os.Truncate(path, 0)
					case "same-size-write":
						err = os.WriteFile(path, []byte("modified"), 0600)
					case "new-process":
						child, err = os.StartProcess("/workspace-archive.test", []string{"/workspace-archive.test", "-test.run", "^$"}, &os.ProcAttr{Env: os.Environ(), Files: []*os.File{nil, nil, nil}})
					}
					if err != nil {
						return 0, err
					}
				}
				return len(p), nil
			})
			nativeArchiveReject(t, s, context.Background(), dst)
			if !changed {
				t.Fatal("mutation not triggered")
			}
		})
	}
}

func TestWorkspaceArchiveNativeObservationAndClose(t *testing.T) {
	b := requireNativeBoundary(t)
	o, err := b.ObserveNoUserDescendants(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	copyObservation := *o
	foreign := &KernelBoundary{self: b, role: rolePID1, pid: 1, seal: b.seal}
	for _, tc := range []struct {
		b *KernelBoundary
		o *UserNamespaceQuiescenceObservation
	}{{b, nil}, {b, &copyObservation}, {foreign, o}, {b, &UserNamespaceQuiescenceObservation{origin: b}}} {
		if s, err := tc.b.OpenWorkspaceArchive(context.Background(), tc.o); err == nil || s != nil {
			t.Fatal("foreign observation/boundary accepted")
		}
	}
	s := nativeArchiveSource(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if summary, err := s.WriteArchive(context.Background(), io.Discard); err == nil || summary != (WorkspaceArchiveSummary{}) {
		t.Fatal("closed source streamed")
	}
}

func TestWorkspaceArchiveNativePhysicalReplacementUnavailable(t *testing.T) {
	requireNativeBoundary(t)
	// Under this fixture's immutable mount topology, replacement is blocked.
	// This does not prove privileged replacement handling; pin sensitivity is
	// tested separately, and a privileged physical replacement is CannotVerify.
	err := os.Rename("/workspace", "/workspace-moved")
	if err == nil {
		if restore := os.Rename("/workspace-moved", "/workspace"); restore != nil {
			t.Fatal(restore)
		}
		t.Fatal("unexpected replaceable fixture root")
	}
	if !errors.Is(err, unix.EBUSY) && !errors.Is(err, unix.EROFS) {
		t.Fatalf("unexpected root rename: %v", err)
	}
	t.Logf("CannotVerify privileged physical root replacement: %v; root pin tests check internal identity only", err)
}

func TestWorkspaceArchiveNativeCrossMountResolution(t *testing.T) {
	requireNativeBoundary(t)
	// /proc supplies an actual nested mount for the same native opener. No
	// extra mount capability is added to place another mount in /workspace.
	root, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(root)
	file, err := openArchiveChild(root, "proc")
	if file != nil {
		file.Close()
		t.Fatal("cross-mount opened")
	}
	if !errors.Is(err, unix.EXDEV) {
		t.Fatalf("mount rejection: %v", err)
	}
	for _, path := range []string{"../tmp", "/tmp"} {
		if file, err := openArchiveChild(root, path); err == nil || file != nil {
			if file != nil {
				file.Close()
			}
			t.Fatalf("escape %q accepted", path)
		}
	}
	if file, err := openArchiveChild(root, strings.Repeat("a", 4097)); err == nil || file != nil {
		if file != nil {
			file.Close()
		}
		t.Fatal("oversize path accepted")
	}
}

func TestWorkspaceArchiveNativeBlockedWriterOwnership(t *testing.T) {
	requireNativeBoundary(t)
	s := nativeArchiveSource(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	stream := make(chan error, 1)
	closed := make(chan error, 1)
	first := true
	dst := archiveTestWriter(func(p []byte) (int, error) {
		if first {
			first = false
			close(entered)
			<-release
		}
		return len(p), nil
	})
	go func() {
		summary, err := s.WriteArchive(ctx, dst)
		if summary != (WorkspaceArchiveSummary{}) {
			stream <- errors.New("blocked canceled writer claimed summary")
			return
		}
		stream <- err
	}()
	<-entered
	cancel()
	go func() { closed <- s.Close() }()
	select {
	case err := <-stream:
		close(release)
		t.Fatalf("stream returned while writer blocked: %v", err)
	case err := <-closed:
		close(release)
		t.Fatalf("Close returned while writer blocked: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-stream; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled stream: %v", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	t.Log("cancellation and Close retained ownership until blocked caller writer physically returned")
}
