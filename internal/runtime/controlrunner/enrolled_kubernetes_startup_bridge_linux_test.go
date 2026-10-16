//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/goairix/sandbox/internal/kubecontract"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// No program/path/PID is supplied by the exec request. FD3 is the original
// launcher's retained mount namespace, passed only by this fixed fixture.
func TestEnrolledKubernetesStartupBridgeExec(t *testing.T) {
	if os.Getenv("ENROLLED_STARTUP_BRIDGE_EXEC") != "1" {
		return
	}
	if err := startupExecBridge(); err != nil {
		fmt.Fprintln(os.Stderr, "fixed startup bridge helper:", err)
		os.Exit(125)
	}
	os.Exit(125) // Successful exec never returns; never print a Go PASS on stdout.
}

func startupExecBridge() error {
	runtime.LockOSThread()
	if os.Geteuid() != 0 {
		return errors.New("root helper required")
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(3, &fs); err != nil {
		return err
	}
	if fs.Type != unix.NSFS_MAGIC {
		return errors.New("original namespace FD required")
	}
	if err := unix.Unshare(unix.CLONE_FS); err != nil {
		return err
	}
	if err := unix.Setns(3, unix.CLONE_NEWNS); err != nil {
		return err
	}
	if err := unix.Close(3); err != nil {
		return err
	}
	if err := startupDropSetupCapabilities(); err != nil {
		return err
	}
	return syscall.Exec(kubecontract.EnrolledLauncherPath, []string{kubecontract.EnrolledLauncherPath, "bridge"}, []string{"GOMAXPROCS=1"})
}

// Only the startup fixture receives SYS_CHROOT for mount setns. Both formal
// exec paths erase it alongside the other setup privileges on every Go thread.
func startupDropSetupCapabilities() error {
	for _, capability := range []uintptr{unix.CAP_SYS_ADMIN, unix.CAP_SYS_PTRACE, unix.CAP_SYS_CHROOT} {
		_, _, errno := syscall.AllThreadsSyscall6(unix.SYS_PRCTL, unix.PR_CAPBSET_DROP, capability, 0, 0, 0, 0)
		if errno != 0 {
			return errno
		}
	}
	header := &unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := &[2]unix.CapUserData{{Permitted: 0x1e0, Effective: 0x1e0}}
	_, _, errno := syscall.AllThreadsSyscall(unix.SYS_CAPSET, uintptr(unsafe.Pointer(header)), uintptr(unsafe.Pointer(data)), 0)
	runtime.KeepAlive(header)
	runtime.KeepAlive(data)
	if errno != 0 {
		return errno
	}
	_, _, errno = syscall.AllThreadsSyscall6(unix.SYS_PRCTL, unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0, 0)
	if errno != 0 {
		return errno
	}
	if err := unix.Capget(header, &data[0]); err != nil {
		return err
	}
	if data[0].Effective != 0x1e0 || data[0].Permitted != 0x1e0 || data[0].Inheritable != 0 || data[1] != (unix.CapUserData{}) {
		return errors.New("unexpected formal exec capability boundary")
	}
	for capability := 0; capability < 64; capability++ {
		bounding, err := unix.PrctlRetInt(unix.PR_CAPBSET_READ, uintptr(capability), 0, 0, 0)
		if errors.Is(err, unix.EINVAL) {
			break
		}
		if err != nil {
			return err
		}
		want := 0
		if capability >= unix.CAP_KILL && capability <= unix.CAP_SETPCAP {
			want = 1
		}
		ambient, err := unix.PrctlRetInt(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_IS_SET, uintptr(capability), 0, 0)
		if err != nil || bounding != want || ambient != 0 {
			return fmt.Errorf("unexpected capability %d bounding=%d ambient=%d error=%v", capability, bounding, ambient, err)
		}
	}
	if value, err := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0); err != nil || value != 1 {
		return fmt.Errorf("no-new-privileges missing: value=%d error=%v", value, err)
	}
	return nil
}

type startupBridgeFixture struct {
	mu        sync.Mutex
	namespace *os.File
	closed    bool
	attempted bool
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
	done      chan struct{}
	result    error
	stdinEOF  bool
	stdout    int64
	waited    bool
	waitErr   error
	// A cold-create fixture may lose the actual activation response after the
	// formal bridge consumes it. The original startup fixture leaves this false.
	discardStdout bool
}

func newStartupBridgeFixture() *startupBridgeFixture {
	return &startupBridgeFixture{done: make(chan struct{})}
}

func (f *startupBridgeFixture) bindOriginalMountNamespace(pid int) error {
	namespace, err := os.Open(fmt.Sprintf("/proc/%d/ns/mnt", pid))
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || f.namespace != nil {
		namespace.Close()
		return errors.New("namespace already bound or closed")
	}
	f.namespace = namespace
	return nil
}

func (f *startupBridgeFixture) Close() {
	f.closeOnce.Do(func() {
		f.mu.Lock()
		f.closed = true
		cancel := f.cancel
		f.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		f.wg.Wait()
		if f.namespace != nil {
			f.namespace.Close()
		}
	})
}

func (f *startupBridgeFixture) requireJoined(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	attempted := f.attempted
	f.mu.Unlock()
	require.True(t, attempted, "production bridge client must issue real exec")
	<-f.done
	f.wg.Wait()
	require.NoError(t, f.result)
	require.True(t, f.stdinEOF, "v5 FIN must become actual child stdin EOF")
	require.Positive(t, f.stdout, "actual bridge stdout must carry TLS response")
	t.Log("startup actual bridge Cmd.Wait=exit0; all IO joined; real v5 stdin EOF and stdout preserved")
}

func (f *startupBridgeFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	expected := url.Values{"container": {"sandbox"}, "command": {"/usr/local/bin/sandbox-launcher", "bridge"}, "stdin": {"true"}, "stdout": {"true"}, "stderr": {"true"}, "tty": {"false"}}
	protocols := websocket.Subprotocols(r)
	if r.Method != http.MethodGet || r.URL.Query().Encode() != expected.Encode() || len(protocols) != 1 || protocols[0] != "v5.channel.k8s.io" {
		http.Error(w, "fixed bridge exec required", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	if f.closed || f.attempted || f.namespace == nil {
		f.mu.Unlock()
		http.Error(w, "bridge not available", http.StatusConflict)
		return
	}
	f.attempted = true
	f.wg.Add(1)
	f.mu.Unlock()
	defer f.wg.Done()
	defer close(f.done)
	upgrader := websocket.Upgrader{Subprotocols: []string{"v5.channel.k8s.io"}, ReadBufferSize: 4096, WriteBufferSize: 32769}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		f.result = err
		return
	}
	defer ws.Close()
	f.result = f.relay(ws)
}

type startupChannelWriter struct {
	ws      *websocket.Conn
	mu      *sync.Mutex
	channel byte
}

func (w startupChannelWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	frame := append([]byte{w.channel}, data...)
	if err := w.ws.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		return 0, err
	}
	return len(data), nil
}

func (f *startupBridgeFixture) relay(ws *websocket.Conn) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f.mu.Lock()
	f.cancel = cancel
	if f.closed {
		cancel()
	}
	f.mu.Unlock()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestEnrolledKubernetesStartupBridgeExec$", "-test.timeout=15s")
	cmd.Env = []string{"ENROLLED_STARTUP_BRIDGE_EXEC=1", "GOMAXPROCS=1"}
	cmd.ExtraFiles = []*os.File{f.namespace}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	defer stdout.Close()
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	defer stderr.Close()
	if err := cmd.Start(); err != nil {
		return err
	}
	// Cancellation owns all physical streams. It is itself joined below;
	// Cmd.Wait occurs exactly once, after the original pipe readers finish.
	canceled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(canceled)
		ws.Close()
		stdin.Close()
		stdout.Close()
		stderr.Close()
	})
	defer func() {
		if !stop() {
			<-canceled
		}
	}()
	ws.SetReadLimit(32769)
	inputDone := make(chan error, 1)
	go func() {
		err := startupBridgeInput(ws, stdin)
		if err != nil {
			cancel()
		}
		inputDone <- err
	}()
	type copied struct {
		n   int64
		err error
	}
	var writeMu sync.Mutex
	outputDone, errorDone := make(chan copied, 1), make(chan copied, 1)
	copyStream := func(source io.Reader, channel byte, done chan copied) {
		var destination io.Writer = startupChannelWriter{ws, &writeMu, channel}
		if channel == 1 && f.discardStdout {
			destination = io.Discard
		}
		n, err := io.CopyBuffer(destination, source, make([]byte, 32768))
		if err != nil {
			cancel()
		}
		done <- copied{n, err}
	}
	go copyStream(stdout, 1, outputDone)
	go copyStream(stderr, 2, errorDone)
	inputErr, output, diagnostic := <-inputDone, <-outputDone, <-errorDone
	waitErr := cmd.Wait()
	f.waited, f.waitErr = true, waitErr
	f.stdinEOF, f.stdout = inputErr == nil, output.n
	if err := errors.Join(inputErr, output.err, diagnostic.err, waitErr, ctx.Err()); err != nil {
		return fmt.Errorf("actual bridge/IO join: %w", err)
	}
	if diagnostic.n != 0 {
		return fmt.Errorf("formal bridge emitted %d stderr bytes", diagnostic.n)
	}
	// Success comes solely from the real child exit after all pipe owners join.
	status := append([]byte{3}, []byte(`{"status":"Success"}`)...)
	if err := ws.WriteMessage(websocket.BinaryMessage, status); err != nil {
		return fmt.Errorf("bridge success status: %w", err)
	}
	return ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
}

func startupBridgeInput(ws *websocket.Conn, stdin io.WriteCloser) error {
	for {
		kind, frame, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		if kind != websocket.BinaryMessage || len(frame) == 0 {
			return errors.New("invalid exec input frame")
		}
		if len(frame) == 2 && frame[0] == 255 && frame[1] == 0 {
			return stdin.Close()
		}
		if frame[0] != 0 || len(frame) < 2 {
			return errors.New("only stdin bytes and stdin FIN are supported")
		}
		if n, err := stdin.Write(frame[1:]); err != nil {
			return err
		} else if n != len(frame)-1 {
			return io.ErrShortWrite
		}
	}
}
