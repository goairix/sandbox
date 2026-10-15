package main

import (
	"io"
	"os"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	t "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	"golang.org/x/sys/unix"
)

const configPath = t.ControlDirectory + "/config.json"

type configBinding = launcherconfig.Binding
type configEnrollment = launcherconfig.Enrollment
type protectedConfig launcherconfig.Config

func (c protectedConfig) trustBinding() p.TrustBinding {
	return launcherconfig.Config(c).TrustBinding()
}
func decodeProtectedConfig(wire []byte) (protectedConfig, error) {
	c, err := launcherconfig.Decode(wire)
	return protectedConfig(c), err
}
func readProtectedConfig(path string) (protectedConfig, error) {
	var c protectedConfig
	if err := t.CheckProtectedPath(path, 0600); err != nil {
		return c, err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return c, err
	}
	f := os.NewFile(uintptr(fd), "protected-config")
	defer f.Close()
	wire, err := io.ReadAll(io.LimitReader(f, launcherconfig.MaxBytes+1))
	if err != nil {
		return c, err
	}
	return decodeProtectedConfig(wire)
}
