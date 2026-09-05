//go:build !windows

package workspaceprobe

import (
	"fmt"
	"os"
	"sync"
	"syscall"
)

type osIOChild struct {
	mu      sync.Mutex
	process *os.Process
	reaped  bool
}

func newOSIOChild(process *os.Process) ioChild {
	return &osIOChild{process: process}
}

func (c *osIOChild) poll() (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reaped {
		return true, fmt.Errorf("workspace probe I/O helper status already consumed")
	}
	var status syscall.WaitStatus
	pid, err := syscall.Wait4(c.process.Pid, &status, syscall.WNOHANG, nil)
	if err == syscall.EINTR {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if pid == 0 {
		return false, nil
	}
	c.reaped = true
	if status.Exited() && status.ExitStatus() == 0 {
		return true, nil
	}
	return true, fmt.Errorf("workspace probe I/O helper failed")
}

func (c *osIOChild) kill() error {
	return c.process.Signal(os.Kill)
}

func (c *osIOChild) release() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.process.Release()
}
