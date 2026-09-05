//go:build linux

package workspaceprobe

import (
	"os"
	"strconv"
	"strings"
)

func requirePID1Reaper() error {
	status, err := os.ReadFile("/proc/1/status")
	if err != nil {
		return err
	}
	var ignored, caught, blocked uint64
	for _, line := range strings.Split(string(status), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		value, parseErr := strconv.ParseUint(fields[1], 16, 64)
		if parseErr != nil {
			continue
		}
		switch fields[0] {
		case "SigIgn:":
			ignored = value
		case "SigCgt:":
			caught = value
		case "SigBlk:":
			blocked = value
		}
	}
	return validateReaperSignals(ignored, caught, blocked)
}
