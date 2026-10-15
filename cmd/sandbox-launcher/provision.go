package main

import (
	"os"

	"github.com/goairix/sandbox/internal/runtime/controlrunner"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
)

func consumeProvisionedConfig() ([]byte, error) {
	wire := []byte(os.Getenv(launcherconfig.Environment))
	if _, err := launcherconfig.Decode(wire); err != nil {
		return nil, err
	}
	if err := os.Unsetenv(launcherconfig.Environment); err != nil {
		return nil, err
	}
	return wire, nil
}

func serveProvisioned() error {
	if os.Getpid() != 1 || os.Geteuid() != 0 {
		return controlrunner.ErrUnavailable
	}
	wire, err := consumeProvisionedConfig()
	if err != nil {
		return err
	}
	if err := provisionProtectedConfig(wire); err != nil {
		return err
	}
	return serve()
}
