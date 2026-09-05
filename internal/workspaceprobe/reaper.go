package workspaceprobe

import (
	"fmt"
	"syscall"
)

func validateReaperSignals(ignored, caught, blocked uint64) error {
	mask := uint64(1) << (uint64(syscall.SIGCHLD) - 1)
	// Only kernel SIGCHLD ignore is self-authenticating (children cannot become
	// zombies). A caught or blocked signal does not prove that PID 1 calls wait.
	// Docker's workspace-mounter currently waits only for its known s3fs child;
	// Task 13 must provide a versioned lifecycle handshake before handler-based
	// PID 1 implementations may be trusted here.
	if ignored&mask == 0 {
		return fmt.Errorf("pid 1 does not provide kernel child auto-reaping")
	}
	return nil
}
