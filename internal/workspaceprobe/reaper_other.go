//go:build !linux

package workspaceprobe

import "fmt"

func requirePID1Reaper() error { return fmt.Errorf("quiesce broker is supported only on linux") }
