//go:build windows

package workspaceprobe

import (
	"fmt"
	"os"
)

func newOSIOChild(*os.Process) ioChild { return unsupportedIOChild{} }

type unsupportedIOChild struct{}

func (unsupportedIOChild) poll() (bool, error) {
	return true, fmt.Errorf("workspace probe I/O helper is unsupported")
}
func (unsupportedIOChild) kill() error    { return nil }
func (unsupportedIOChild) release() error { return nil }
