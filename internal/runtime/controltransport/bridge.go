package controltransport

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

const ControlDirectory = "/run/sandbox-control"
const ControlSocket = ControlDirectory + "/control.sock"
const ClockSocket = "/run/sandbox-clock/clock.sock"

// CheckProtectedPath checks every original path component without following
// symlinks. Privileged operator replacement remains an external trust boundary.
func CheckProtectedPath(path string, mode os.FileMode) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrDestination
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 0 || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("unprotected path %s", current)
		}
		if current == path {
			if info.Mode() != mode || (info.Mode().IsRegular() && st.Nlink != 1) {
				return ErrDestination
			}
		} else if !info.IsDir() {
			return ErrDestination
		}
		if current == "/" {
			break
		}
	}
	return nil
}

// Bridge is the immutable root-only byte relay. It accepts no endpoint, key or
// program selection; the caller supplies only owned stdin/stdout byte streams.
func Bridge(ctx context.Context, input io.ReadCloser, output io.WriteCloser) error {
	if os.Geteuid() != 0 || isNil(ctx) || isNil(input) || isNil(output) {
		return ErrDestination
	}
	if err := CheckProtectedPath(ControlSocket, os.ModeSocket|0600); err != nil {
		return err
	}
	raw, err := (&net.Dialer{}).DialContext(ctx, "unix", ControlSocket)
	if err != nil {
		return err
	}
	conn, ok := raw.(*net.UnixConn)
	if !ok {
		raw.Close()
		return ErrDestination
	}
	defer conn.Close()
	active, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	stop := context.AfterFunc(active, func() { defer close(done); conn.Close(); input.Close(); output.Close() })
	defer func() {
		cancel()
		if !stop() {
			<-done
		}
	}()
	sent := make(chan error, 1)
	go func() {
		_, err := io.CopyBuffer(conn, input, make([]byte, 32768))
		if err == nil {
			err = conn.CloseWrite()
		}
		sent <- err
		if err != nil {
			cancel()
		}
	}()
	_, receiveErr := io.CopyBuffer(output, conn, make([]byte, 32768))
	input.Close()
	sendErr := <-sent
	if receiveErr != nil {
		return receiveErr
	}
	return sendErr
}
