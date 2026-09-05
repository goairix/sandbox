//go:build linux

package mounter

import (
	"errors"
	"os"
	"syscall"
)

// reapOrphanedProcessGroup removes descendants orphaned when a bounded root
// command is killed as a process group. Container PID 1 adopts those zombies;
// waiting only for exec.Cmd's direct child leaves them permanently visible.
// The negative PID scopes wait4 to the command's exact process group and never
// consumes the separately registered UID-1000 broker child.
func reapOrphanedProcessGroup(processGroupID int) {
	if os.Getpid() != 1 || processGroupID <= 1 {
		return
	}
	for {
		var status syscall.WaitStatus
		waited, err := syscall.Wait4(-processGroupID, &status, 0, nil)
		if waited > 0 {
			continue
		}
		if errors.Is(err, syscall.ECHILD) {
			return
		}
		return
	}
}
