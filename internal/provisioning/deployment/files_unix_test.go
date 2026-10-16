//go:build darwin || linux

package deployment

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestBoundedFileRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := boundedFile(path, 16, "credential"); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("accepted FIFO")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked opening FIFO")
	}
}
