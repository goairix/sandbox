//go:build !linux

package workspaceprobe

import "fmt"

func disableProcessDump() error {
	return fmt.Errorf("quiesce broker is supported only on linux")
}
