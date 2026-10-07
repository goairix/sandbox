package main

import (
	"context"
	"net"
	"os"
	"os/signal"
	"syscall"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/controlrunner"
	t "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/goairix/sandbox/internal/runtime/launcher"
)

func serve() error {
	if os.Getpid() != 1 || os.Geteuid() != 0 {
		return controlrunner.ErrUnavailable
	}
	c, err := readProtectedConfig(configPath)
	if err != nil {
		return err
	}
	if err = t.CheckProtectedPath(t.ControlDirectory, os.ModeDir|0700); err != nil {
		return err
	}
	// A stale control socket is never removed or adopted on a new birth.
	if _, err = os.Lstat(t.ControlSocket); !os.IsNotExist(err) {
		return controlrunner.ErrInvalidConfiguration
	}
	if err = t.CheckProtectedPath(t.ClockSocket, os.ModeSocket|0600); err != nil {
		return err
	}
	verifier, err := p.NewManagementVerifier(c.trustBinding(), c.Roots)
	if err != nil {
		return err
	}
	clock, err := p.NewSignedClockClient(p.SignedClockClientOptions{Binding: c.trustBinding(), Audience: c.ClockAudience, PublicKey: c.ClockPublicKey, Dial: func(ctx context.Context) (net.Conn, error) {
		if err := t.CheckProtectedPath(t.ClockSocket, os.ModeSocket|0600); err != nil {
			return nil, err
		}
		return (&net.Dialer{}).DialContext(ctx, "unix", t.ClockSocket)
	}})
	if err != nil {
		return err
	}
	boundary, err := launcher.BootstrapPID1()
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	supervisor, err := controlrunner.NewSupervisor(controlrunner.SupervisorOptions{Kernel: boundary, UID: c.UID, GID: c.GID, NetworkAllowed: c.NetworkAllowed, ContractDigest: c.ContractDigest, Verifier: verifier, Clock: clock, JournalDirectory: t.ControlDirectory + "/journal", Executable: executable, MaxActive: c.MaxActive})
	if err != nil {
		return err
	}
	defer supervisor.Close()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: t.ControlSocket, Net: "unix"})
	if err != nil {
		return err
	}
	defer listener.Close()
	if err = os.Chmod(t.ControlSocket, 0600); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	return supervisor.Serve(ctx, listener)
}
