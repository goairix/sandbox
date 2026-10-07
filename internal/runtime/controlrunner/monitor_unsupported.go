//go:build !linux || (!amd64 && !arm64)

package controlrunner

import (
	"github.com/goairix/sandbox/internal/runtime/launcher"
	"os"
)

func inspectExecutable(string) (os.FileInfo, error) { return nil, launcher.ErrUnsupported }

func RunMonitor() error                { return launcher.ErrUnsupported }
func monitorMonotonic() (int64, error) { return 0, launcher.ErrUnsupported }

func sealExecDescriptors() error { return ErrUnavailable }
