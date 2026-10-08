//go:build linux

package launcher

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func identityFixture(t *testing.T) *KernelBoundary {
	t.Helper()
	b := requireNativeBoundary(t)
	if err := os.Chmod("/workspace", 0777); err != nil {
		t.Fatal(err)
	}
	identityHelper(t, "prepare")
	t.Cleanup(func() { identityHelper(t, "clean") })
	for _, path := range []string{"/workspace/private"} {
		var st unix.Stat_t
		if err := unix.Stat(path, &st); err != nil {
			t.Fatal(err)
		}
		t.Logf("actual user fixture path=%s uid=%d gid=%d mode=%o", path, st.Uid, st.Gid, st.Mode&0777)
		if st.Uid != 1000 || st.Gid != 1000 || (path == "/workspace/private" && st.Mode&0777 != 0700) || (path != "/workspace/private" && st.Mode&0777 != 0600) {
			t.Fatal("incorrect private fixture")
		}
	}
	return b
}

func identityHelper(t *testing.T, action string) {
	t.Helper()
	cmd := exec.Command("/archive-user", action)
	cmd.Env = []string{"GOMAXPROCS=2"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000, Groups: []uint32{}}}
	// The parent is the sole waiter; no child exists at the later census.
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("joined fixture %s: %v %s", action, err, out)
	} else {
		t.Logf("joined fixture %s: %s", action, out)
	}
}

// An all-reject constructor or an open as root breaks actual private content.
func TestWorkspaceArchiveIdentityNativePrivateContent(t *testing.T) {
	b := identityFixture(t)
	o, err := b.ObserveNoUserDescendants(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s, err := b.OpenWorkspaceArchiveAsUser(context.Background(), o, 1000, 1000)
	if err != nil {
		t.Fatalf("user private source must open: %v", err)
	}
	defer s.Close()
	var out bytes.Buffer
	groups, err := unix.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	callbacks := 0
	summary, err := s.WriteArchive(context.Background(), archiveTestWriter(func(p []byte) (int, error) {
		callbacks++
		if err := b.ValidateCurrent(); err != nil {
			return 0, err
		}
		now, err := unix.Getgroups()
		if err != nil || !reflect.DeepEqual(now, groups) {
			return 0, fmt.Errorf("writer groups not restored")
		}
		return out.Write(p)
	}))
	if err != nil || summary.EntryCount != 3 || summary.ArchiveBytes != uint64(out.Len()) {
		t.Fatalf("private archive %+v: %v", summary, err)
	}
	if callbacks == 0 || summary.SHA256 != sha256.Sum256(out.Bytes()) {
		t.Fatal("no restored callback/digest")
	}
	if err := b.ValidateCurrent(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteArchive(context.Background(), io.Discard); err == nil {
		t.Fatal("reused source")
	}
	r := tar.NewReader(bytes.NewReader(out.Bytes()))
	found := false
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == "workspace/private/content" {
			body, err := io.ReadAll(r)
			if err != nil || string(body) != "user private physical content\n" {
				t.Fatalf("private body %q: %v", body, err)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("private physical content absent")
	}
}

func identityObservation(t *testing.T, b *KernelBoundary) *UserNamespaceQuiescenceObservation {
	t.Helper()
	o, err := b.ObserveNoUserDescendants(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func identitySource(t *testing.T, b *KernelBoundary, uid uint32) *WorkspaceArchiveSource {
	t.Helper()
	s, err := b.OpenWorkspaceArchiveAsUser(context.Background(), identityObservation(t, b), uid, uid)
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

func identityReject(t *testing.T, s *WorkspaceArchiveSource, ctx context.Context, dst io.Writer, want error) {
	t.Helper()
	fd := int(s.root.Fd())
	got, err := s.WriteArchive(ctx, dst)
	if err == nil || got != (WorkspaceArchiveSummary{}) || (want != nil && !errors.Is(err, want)) {
		t.Fatalf("rejection summary=%+v error=%v wanted=%v", got, err, want)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); !errors.Is(err, unix.EBADF) {
		t.Fatalf("descriptor retained: %v", err)
	}
	if _, err := s.WriteArchive(context.Background(), io.Discard); err == nil {
		t.Fatal("failed source reused")
	}
	if err := s.origin.ValidateCurrent(); err != nil {
		t.Fatal(err)
	}
	t.Logf("zero-summary restored rejection: %v", err)
}

// Opening as root, the wrong user, or retaining group0 must fail actual DAC.
func TestWorkspaceArchiveIdentityNativeDenials(t *testing.T) {
	b := identityFixture(t)
	t.Run("root-private", func(t *testing.T) {
		s, err := b.OpenWorkspaceArchive(context.Background(), identityObservation(t, b))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		identityReject(t, s, context.Background(), io.Discard, unix.EACCES)
	})
	t.Run("wrong-user", func(t *testing.T) {
		identityReject(t, identitySource(t, b, 2000), context.Background(), io.Discard, unix.EACCES)
	})
	identityHelper(t, "clean")
	// Fixed root-owned group0 file: root can read it, user1000 must not gain
	// access through PID1's supplementary group0, even in a world-readable dir.
	if err := os.WriteFile("/workspace/group-only", []byte("ROOT-GROUP-ONLY"), 0040); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove("/workspace/group-only"); err != nil {
			t.Error(err)
		}
	})
	groups, err := unix.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	rootGroup := false
	for _, g := range groups {
		if g == 0 {
			rootGroup = true
		}
	}
	if !rootGroup {
		t.Fatal("fixture must begin with real supplementary group0")
	}
	var st unix.Stat_t
	if err := unix.Stat("/workspace/group-only", &st); err != nil || st.Uid != 0 || st.Gid != 0 || st.Mode&0777 != 0040 {
		t.Fatalf("root group fixture: %+v %v", st, err)
	}
	t.Run("cleared-groups", func(t *testing.T) {
		var out bytes.Buffer
		identityReject(t, identitySource(t, b, 1000), context.Background(), &out, unix.EACCES)
		if bytes.Contains(out.Bytes(), []byte("ROOT-GROUP-ONLY")) {
			t.Fatal("group-only bytes escaped")
		}
		after, err := unix.Getgroups()
		if err != nil || !reflect.DeepEqual(after, groups) {
			t.Fatal("supplementary groups not restored")
		}
	})
}

// Invalid originals must reject before taking a copied/foreign boundary mutex.
func TestWorkspaceArchiveIdentityNativeInvalid(t *testing.T) {
	b := identityFixture(t)
	o := identityObservation(t, b)
	copiedB := &KernelBoundary{self: b, role: b.role, pid: b.pid, seal: b.seal}
	copiedB.mu.Lock()
	defer copiedB.mu.Unlock()
	foreign := &KernelBoundary{role: b.role, pid: b.pid, seal: b.seal}
	foreign.self = foreign
	foreign.mu.Lock()
	defer foreign.mu.Unlock()
	copiedO := &UserNamespaceQuiescenceObservation{self: o, origin: b, pid: o.pid, seal: o.seal}
	for _, tc := range []struct {
		name     string
		b        *KernelBoundary
		o        *UserNamespaceQuiescenceObservation
		ctx      context.Context
		uid, gid uint32
	}{
		{"nil-boundary", nil, o, context.Background(), 1000, 1000},
		{"copy-boundary", copiedB, o, context.Background(), 1000, 1000},
		{"foreign-boundary", foreign, o, context.Background(), 1000, 1000},
		{"nil-observation", b, nil, context.Background(), 1000, 1000},
		{"copy-observation", b, copiedO, context.Background(), 1000, 1000},
		{"nil-context", b, o, nil, 1000, 1000},
		{"zero-uid", b, o, context.Background(), 0, 1000},
		{"zero-gid", b, o, context.Background(), 1000, 0},
		{"large-uid", b, o, context.Background(), 2147483648, 1000},
		{"large-gid", b, o, context.Background(), 1000, 2147483648},
		{"query-id", b, o, context.Background(), 4294967295, 4294967295},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if s, err := tc.b.OpenWorkspaceArchiveAsUser(tc.ctx, tc.o, tc.uid, tc.gid); s != nil || err == nil {
				t.Fatalf("invalid accepted: %v %v", s, err)
			}
		})
	}
	if err := b.ValidateCurrent(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceArchiveIdentityNativeFailures(t *testing.T) {
	b := identityFixture(t)
	t.Run("writer-panic", func(t *testing.T) {
		s := identitySource(t, b, 1000)
		fd := int(s.root.Fd())
		defer func() {
			if recovered := recover(); recovered != "fixed writer panic" {
				t.Fatalf("writer panic=%v", recovered)
			}
			if err := b.ValidateCurrent(); err != nil {
				t.Fatal(err)
			}
			var st unix.Stat_t
			if err := unix.Fstat(fd, &st); !errors.Is(err, unix.EBADF) {
				t.Fatalf("panic descriptor retained: %v", err)
			}
		}()
		_, _ = s.WriteArchive(context.Background(), archiveTestWriter(func([]byte) (int, error) {
			if err := b.ValidateCurrent(); err != nil {
				t.Fatal(err)
			}
			panic("fixed writer panic")
		}))
	})
	t.Run("canceled-constructor", func(t *testing.T) {
		o := identityObservation(t, b)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if s, err := b.OpenWorkspaceArchiveAsUser(ctx, o, 1000, 1000); s != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled constructor: %v %v", s, err)
		}
	})
	for _, kind := range []string{"canceled-stream", "cancel-at-writer", "writer-error", "short-writer", "nil-context", "nil-writer", "copy", "closed"} {
		t.Run(kind, func(t *testing.T) {
			s := identitySource(t, b, 1000)
			ctx := context.Background()
			var dst io.Writer = io.Discard
			var want error
			switch kind {
			case "cancel-at-writer":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				dst = archiveTestWriter(func(p []byte) (int, error) {
					if err := b.ValidateCurrent(); err != nil {
						return 0, err
					}
					cancel()
					return len(p), nil
				})
				want = context.Canceled
			case "canceled-stream":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			case "writer-error":
				dst = archiveTestWriter(func([]byte) (int, error) {
					if err := b.ValidateCurrent(); err != nil {
						return 0, err
					}
					return 0, io.ErrClosedPipe
				})
				want = io.ErrClosedPipe
			case "short-writer":
				dst = archiveTestWriter(func(p []byte) (int, error) {
					if len(p) == 0 {
						return 0, nil
					}
					return len(p) - 1, nil
				})
				want = io.ErrShortWrite
			case "nil-context":
				ctx = nil
			case "nil-writer":
				dst = nil
			case "copy":
				copy := &WorkspaceArchiveSource{self: s, origin: b, observation: s.observation, root: s.root, readIdentity: s.readIdentity}
				if got, err := copy.WriteArchive(ctx, dst); got != (WorkspaceArchiveSummary{}) || err == nil {
					t.Fatal("copy accepted")
				}
				if err := copy.Close(); err == nil {
					t.Fatal("copy close accepted")
				}
				return
			case "closed":
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				if got, err := s.WriteArchive(ctx, dst); got != (WorkspaceArchiveSummary{}) || err == nil {
					t.Fatal("closed source accepted")
				}
				return
			}
			identityReject(t, s, ctx, dst, want)
		})
	}
	if err := b.ValidateCurrent(); err != nil {
		t.Fatal(err)
	}
}

// A real NNP/TSYNC filter denies only the target setfsuid argument. Query -1
// remains available, so silent setter failure cannot masquerade as success.
func identityDenyFSUID(t *testing.T, target, refusal uint32) {
	t.Helper()
	filter := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SYS_SETFSUID, Jf: 3},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 16},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: target, Jf: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | refusal},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
	}
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	result, _, errno := syscall.Syscall6(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&program)), 0, 0, 0)
	runtime.KeepAlive(&program)
	runtime.KeepAlive(filter)
	if errno != 0 || result != 0 {
		t.Fatalf("real filter result=%d errno=%v", result, errno)
	}
}

func TestWorkspaceArchiveIdentityNativeEntryRefusal(t *testing.T) {
	b := identityFixture(t)
	o := identityObservation(t, b)
	groups, err := unix.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	// ERRNO|0 synthesizes a successful return without executing the setter.
	// Only the queried FSUID detects this actual silent kernel refusal.
	identityDenyFSUID(t, 1000, 0)
	if s, err := b.OpenWorkspaceArchiveAsUser(context.Background(), o, 1000, 1000); s != nil || !errors.Is(err, ErrUnsafeKernel) {
		t.Fatalf("entry refusal accepted: %v %v", s, err)
	}
	identityDenyFSUID(t, 1001, uint32(unix.EPERM))
	if s, err := b.OpenWorkspaceArchiveAsUser(context.Background(), o, 1001, 1001); s != nil || !errors.Is(err, ErrUnsafeKernel) {
		t.Fatalf("errno entry refusal accepted: %v %v", s, err)
	}
	if err := b.ValidateCurrent(); err != nil {
		t.Fatal(err)
	}
	after, err := unix.Getgroups()
	if err != nil || !reflect.DeepEqual(after, groups) {
		t.Fatal("entry failure did not restore exact groups")
	}
	t.Log("real silent and EPERM entry refusals; no source delivered; full root profile restored")
}

func TestWorkspaceArchiveIdentityNativeFatalRestore(t *testing.T) {
	b := identityFixture(t)
	o := identityObservation(t, b)
	identityDenyFSUID(t, 0, uint32(unix.EPERM))
	fmt.Println("restoration-refusal armed; next scope must terminate PID1")
	s, err := b.OpenWorkspaceArchiveAsUser(context.Background(), o, 1000, 1000)
	fmt.Println("RETURNED_FROM_FATAL", err)
	if s != nil {
		_, _ = s.WriteArchive(context.Background(), archiveTestWriter(func([]byte) (int, error) { fmt.Println("WRITER_AFTER_FATAL"); return 0, io.ErrClosedPipe }))
	}
	t.Fatal("restoration refusal returned instead of actual PID1 exit70")
}
