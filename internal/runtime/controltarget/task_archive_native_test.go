//go:build linux && (amd64 || arm64)

package controltarget

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/launcher"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type archiveStreamWriter func([]byte) (int, error)

func (w archiveStreamWriter) Write(b []byte) (int, error) { return w(b) }

func TestTaskArchiveProducerNative(t *testing.T) {
	if os.Getenv("SANDBOX_TASK_ARCHIVE_NATIVE") != "1" {
		t.Skip("requires restricted original Linux PID1 producer fixture")
	}
	require.Equal(t, 1, os.Getpid())
	kernel, err := launcher.BootstrapPID1()
	require.NoError(t, err)
	cmd := exec.Command("/archive-user", "prepare")
	cmd.Env = []string{"GOMAXPROCS=2"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000, Groups: []uint32{}}}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	t.Logf("actual joined private-user helper: %s", out)
	o, err := kernel.ObserveNoUserDescendants(context.Background())
	require.NoError(t, err)
	t.Logf("actual PID1 bootstrap + original census/ECHILD: %+v", kernel.Snapshot())
	f := archiveNativeEmptySetup(t, o)
	admissionCtx, admissionCancel := context.WithCancel(context.Background())
	a, err := f.j.AcceptWorkspaceArchive(admissionCtx, f.archive, f.q, f.quiescedReceipt)
	require.NoError(t, err)
	admissionCancel()
	f.clock.now = f.archive.NotAfter().Add(time.Second)
	deadline, err := a.Deadline()
	require.NoError(t, err)
	source, err := kernel.OpenWorkspaceArchiveAsUser(context.Background(), o, f.j.birth.UID, f.j.birth.GID)
	require.NoError(t, err)
	defer source.Close()
	var raw bytes.Buffer
	wire, err := f.j.StreamWorkspaceArchive(context.Background(), a, source, archiveStreamWriter(func(b []byte) (int, error) {
		if err := kernel.ValidateCurrent(); err != nil {
			return 0, err
		}
		if _, err := f.j.LookupWorkspaceArchive(context.Background(), f.archive.Context(), f.archive.Digest()); err != nil {
			return 0, err
		}
		return raw.Write(b)
	}), f.runtimeKey)
	require.NoError(t, err, "original accepted pending and actual private-user source must stream TAR + signed receipt")
	e, err := f.o.Verifier.VerifyTaskWorkspaceArchiveReceipt(wire, f.activation.RuntimeCertificate(), f.archive.Context(), f.archive.Digest(), f.archive.NotBefore(), f.archive.NotAfter(), f.archive.SessionNotAfter(), f.clock.now)
	require.NoError(t, err)
	require.Equal(t, uint64(raw.Len()), e.ArchiveBytes())
	require.Equal(t, fmt.Sprintf("%x", sha256.Sum256(raw.Bytes())), e.SHA256())
	tr := tar.NewReader(bytes.NewReader(raw.Bytes()))
	entries, found := uint64(0), false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		entries++
		if h.Name == "workspace/private/content" {
			b, err := io.ReadAll(tr)
			require.NoError(t, err)
			require.Equal(t, "user private physical content\n", string(b))
			found = true
		}
	}
	require.True(t, found)
	require.Equal(t, entries, e.EntryCount())
	require.NoError(t, kernel.ValidateCurrent())
	require.Equal(t, uint64(3), e.EntryCount())
	require.Equal(t, deadline, a.deadline)
	t.Logf("producer TAR base64: %s", base64.StdEncoding.EncodeToString(raw.Bytes()))
	t.Logf("producer receipt base64: %s", base64.StdEncoding.EncodeToString(wire))
	t.Logf("actual raw TAR bytes=%d entries=%d sha256=%s signedReceiptBytes=%d restored all-thread identity", e.ArchiveBytes(), e.EntryCount(), e.SHA256(), len(wire))
	f.clock.now = f.archive.SessionNotAfter().Add(time.Second)
	a.deadline = time.Now().Add(-time.Second)
	resigned, err := f.j.SignWorkspaceArchiveReceipt(context.Background(), a, o, f.runtimeKey)
	require.NoError(t, err)
	require.Equal(t, wire, resigned)
	t.Log("original successful historical re-sign after session and monotonic expiry with fresh census/current certificates")
	copied := *a
	_, err = f.j.SignWorkspaceArchiveReceipt(context.Background(), &copied, o, f.runtimeKey)
	require.Error(t, err)
	_, err = f.j.SignWorkspaceArchiveReceipt(context.Background(), a, nil, f.runtimeKey)
	require.Error(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = f.j.SignWorkspaceArchiveReceipt(ctx, a, o, f.runtimeKey)
	require.Error(t, err)
	_, err = f.j.StreamWorkspaceArchive(context.Background(), a, source, io.Discard, f.runtimeKey)
	require.Error(t, err)
	require.NoError(t, f.j.Close())
	cold, err := OpenClosedJournal(context.Background(), f.o)
	require.NoError(t, err)
	defer cold.Close()
	r, err := cold.LookupWorkspaceArchive(context.Background(), f.archive.Context(), f.archive.Digest())
	require.NoError(t, err)
	require.Equal(t, "archive_complete", r.State)
	_, err = cold.SignWorkspaceArchiveReceipt(context.Background(), a, o, f.runtimeKey)
	require.Error(t, err)
	_, err = cold.StreamWorkspaceArchive(context.Background(), a, source, io.Discard, f.runtimeKey)
	require.Error(t, err)

	setup := func(t *testing.T) (archiveFixture, *AcceptedWorkspaceArchive, *launcher.WorkspaceArchiveSource) {
		f := archiveNativeEmptySetup(t, o)
		a, err := f.j.AcceptWorkspaceArchive(context.Background(), f.archive, f.q, f.quiescedReceipt)
		require.NoError(t, err)
		s, err := kernel.OpenWorkspaceArchiveAsUser(context.Background(), o, f.j.birth.UID, f.j.birth.GID)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, s.Close()) })
		return f, a, s
	}
	for _, mode := range []string{"nil", "typed-nil", "error", "short", "negative-count", "large-count", "panic", "canceled", "copy-source", "used-source", "late-context", "late-session", "late-deadline", "late-certificate", "late-retained"} {
		t.Run("writer/"+mode, func(t *testing.T) {
			f, a, s := setup(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			first := true
			var dst io.Writer = archiveStreamWriter(func(b []byte) (int, error) {
				if !first && strings.HasPrefix(mode, "late-") {
					return len(b), nil
				}
				first = false
				switch mode {
				case "error":
					return 0, io.ErrClosedPipe
				case "short":
					return len(b) - 1, nil
				case "negative-count":
					return -1, nil
				case "large-count":
					return len(b) + 1, nil
				case "panic":
					panic("writer failure")
				case "late-context":
					cancel()
				case "late-session":
					f.clock.now = f.archive.SessionNotAfter()
				case "late-deadline":
					a.deadline = time.Now().Add(-time.Second)
				case "late-certificate":
					pub, _, err := ed25519.GenerateKey(rand.Reader)
					require.NoError(t, err)
					v, err := p.NewManagementVerifier(f.activation.Binding(), []ed25519.PublicKey{pub})
					require.NoError(t, err)
					f.j.verifier = v
				case "late-retained":
					require.NoError(t, os.Remove(f.o.Directory+"/users-quiesce.json"))
				}
				return len(b), nil
			})
			switch mode {
			case "nil":
				dst = nil
			case "typed-nil":
				var pointer *bytes.Buffer
				dst = pointer
			case "canceled":
				cancel()
			case "copy-source":
				copy := reflect.New(reflect.TypeOf(*s))
				copy.Elem().Set(reflect.ValueOf(s).Elem())
				s = copy.Interface().(*launcher.WorkspaceArchiveSource)
			case "used-source":
				require.NoError(t, s.Close())
			}
			wire, err := f.j.StreamWorkspaceArchive(ctx, a, s, dst, f.runtimeKey)
			require.Error(t, err)
			require.Empty(t, wire)
			require.True(t, f.j.Status().Poisoned)
			require.True(t, a.consumed)
			wire, err = f.j.StreamWorkspaceArchive(context.Background(), a, s, io.Discard, f.runtimeKey)
			require.Error(t, err)
			require.Empty(t, wire)
			r, err := f.j.LookupWorkspaceArchive(context.Background(), f.archive.Context(), f.archive.Digest())
			require.NoError(t, err)
			require.Equal(t, "pending", r.State)
			wire, err = f.j.SignWorkspaceArchiveReceipt(context.Background(), a, o, f.runtimeKey)
			require.Error(t, err)
			require.Empty(t, wire)
			require.NoError(t, kernel.ValidateCurrent())
		})
	}
	for _, op := range []string{"open-temp", "write", "file-sync", "rename", "dir-sync"} {
		for _, after := range []bool{false, true} {
			t.Run(fmt.Sprintf("terminal/%s/%t", op, after), func(t *testing.T) {
				f, a, s := setup(t)
				hit := false
				f.j.files.hook = func(operation, name string, isAfter bool) error {
					if operation == op && isAfter == after && (op == "dir-sync" || name == "workspace-archive.json" || strings.HasPrefix(name, ".workspace-archive.")) {
						hit = true
						return errors.New("terminal persistence boundary")
					}
					return nil
				}
				wire, err := f.j.StreamWorkspaceArchive(context.Background(), a, s, io.Discard, f.runtimeKey)
				require.True(t, hit)
				require.Error(t, err)
				require.Empty(t, wire)
				require.True(t, f.j.Status().Poisoned)
				f.j.files.hook = nil
				r, err := f.j.LookupWorkspaceArchive(context.Background(), f.archive.Context(), f.archive.Digest())
				require.NoError(t, err)
				require.NotNil(t, r)
				require.Nil(t, a.terminal)
			})
		}
	}
	for _, mode := range []string{"clock", "session", "certificate", "context", "before-return"} {
		t.Run("post-terminal/"+mode, func(t *testing.T) {
			f, a, s := setup(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hit := false
			f.j.files.hook = func(op, name string, after bool) error {
				if op == "dir-sync" && after {
					hit = true
					switch mode {
					case "clock":
						f.clock.err = errors.New("post-terminal clock lost")
					case "session":
						f.clock.now = f.archive.SessionNotAfter()
					case "certificate":
						pub, _, err := ed25519.GenerateKey(rand.Reader)
						require.NoError(t, err)
						f.j.verifier, err = p.NewManagementVerifier(f.activation.Binding(), []ed25519.PublicKey{pub})
						require.NoError(t, err)
					case "context":
						cancel()
					case "before-return":
						calls := 0
						f.j.clock = receiptClock(func(ctx context.Context) (p.ClockObservation, error) {
							calls++
							if calls == 2 {
								return p.ClockObservation{}, errors.New("after signing clock lost")
							}
							return f.clock.Observe(ctx)
						})
					}
				}
				return nil
			}
			wire, err := f.j.StreamWorkspaceArchive(ctx, a, s, io.Discard, f.runtimeKey)
			require.True(t, hit)
			require.Error(t, err)
			require.Empty(t, wire)
			require.Nil(t, a.terminal)
			require.True(t, f.j.Status().Poisoned)
			f.j.files.hook = nil
			f.clock.err = nil
			r, err := f.j.LookupWorkspaceArchive(context.Background(), f.archive.Context(), f.archive.Digest())
			require.NoError(t, err)
			require.Equal(t, "archive_complete", r.State)
		})
	}
	t.Run("blocked-owner-close", func(t *testing.T) {
		f, a, s := setup(t)
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		defer once.Do(func() { close(release) })
		streamDone := make(chan error, 1)
		go func() {
			wire, err := f.j.StreamWorkspaceArchive(context.Background(), a, s, archiveStreamWriter(func(b []byte) (int, error) { close(entered); <-release; return 0, io.ErrClosedPipe }), f.runtimeKey)
			if len(wire) > 0 {
				err = errors.New("unexpected blocked receipt")
			}
			streamDone <- err
		}()
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("writer did not enter")
		}
		fd := int(f.j.root.Fd())
		closed1, closed2, sourceClosed := make(chan error, 1), make(chan error, 1), make(chan error, 1)
		go func() { closed1 <- f.j.Close() }()
		go func() { closed2 <- f.j.Close() }()
		go func() { sourceClosed <- s.Close() }()
		for i := 0; i < 100 && !f.j.Status().Closed; i++ {
			time.Sleep(time.Millisecond)
		}
		require.True(t, f.j.Status().Closed)
		for _, ch := range []chan error{closed1, closed2, sourceClosed} {
			select {
			case err := <-ch:
				t.Fatalf("Close released physical owner early: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
		}
		var st unix.Stat_t
		require.NoError(t, unix.Fstat(fd, &st))
		require.Equal(t, a, f.j.workspaceArchive)
		once.Do(func() { close(release) })
		select {
		case err := <-streamDone:
			require.Error(t, err)
		case <-time.After(3 * time.Second):
			t.Fatal("stream not joined")
		}
		for _, ch := range []chan error{closed1, closed2, sourceClosed} {
			select {
			case err := <-ch:
				require.NoError(t, err)
			case <-time.After(3 * time.Second):
				t.Fatal("Close not joined")
			}
		}
		require.ErrorIs(t, unix.Fstat(fd, &st), unix.EBADF)
		require.Nil(t, a.done)
		require.Nil(t, a.terminal)
		t.Log("blocked writer physically joined before journal descriptor/source close and both Close callers returned")
	})
}

// No execution is accepted in this fixture: the physical original census and
// CompleteUserQuiescence empty registration set therefore omit no accepted exec.
func archiveNativeEmptySetup(t *testing.T, o *launcher.UserNamespaceQuiescenceObservation) archiveFixture {
	t.Helper()
	f, q := archiveEmptyQuiesceSetup(t, liveSetup(t))
	j := f.j
	ctx := context.Background()
	completed, err := j.CompleteUserQuiescence(ctx, q, o, nil, f.runtimeKey)
	require.NoError(t, err)
	require.NotEmpty(t, completed)
	require.Zero(t, q.terminal.RegisteredCount)
	require.Zero(t, j.Status().Records)
	return archiveEvidenceSetup(t, f, q, completed)
}

func TestTaskArchiveAdmissionNative(t *testing.T) {
	if os.Getenv("SANDBOX_TASK_ARCHIVE_NATIVE") != "1" {
		t.Skip("requires restricted original Linux PID1 fixture")
	}
	require.Equal(t, 1, os.Getpid())
	require.Equal(t, "/journal", os.Getenv("TMPDIR"))
	info, err := os.Stat("/journal")
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0700), info.Mode().Perm())
	kernel, err := launcher.BootstrapPID1()
	require.NoError(t, err)
	o, err := kernel.ObserveNoUserDescendants(context.Background())
	require.NoError(t, err)
	t.Logf("actual PID1 bootstrap + original census/ECHILD: %+v", kernel.Snapshot())
	for _, fault := range []string{"none", "file-sync", "dir-sync", "post-durable-clock"} {
		t.Run(fault, func(t *testing.T) {
			f := archiveNativeEmptySetup(t, o)
			hit := false
			if fault != "none" {
				f.j.files.hook = func(op, name string, after bool) error {
					if fault == "post-durable-clock" && op == "dir-sync" && after {
						hit = true
						f.clock.err = errors.New("lost trusted clock after actual archive pending persistence")
						return nil
					}
					if fault == op && !after {
						hit = true
						return errors.New("actual archive admission persistence fault")
					}
					return nil
				}
			}
			a, err := f.j.AcceptWorkspaceArchive(context.Background(), f.archive, f.q, f.quiescedReceipt)
			f.j.files.hook = nil
			f.clock.err = nil
			if fault != "none" {
				require.True(t, hit)
				require.Error(t, err)
				require.Nil(t, a)
				require.True(t, f.j.Status().Poisoned)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, a)
			r, err := f.j.LookupWorkspaceArchive(context.Background(), f.archive.Context(), f.archive.Digest())
			require.NoError(t, err)
			require.Equal(t, "pending", r.State)
			require.NoError(t, o.RevalidateCurrent(context.Background()))
			require.NoError(t, f.j.Close())
			cold, err := OpenClosedJournal(context.Background(), f.o)
			require.NoError(t, err)
			defer cold.Close()
			_, err = cold.AcceptWorkspaceArchive(context.Background(), f.archive, f.q, f.quiescedReceipt)
			require.Error(t, err)
			t.Log("actual completed original USERS quiescence admitted durable pending; cold authority refused")
		})
	}
}
