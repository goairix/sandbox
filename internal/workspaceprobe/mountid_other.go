//go:build !linux

package workspaceprobe

import "fmt"

func effectiveMountID(string) (uint64, error) {
	return 0, fmt.Errorf("effective mount identity is supported only on linux")
}
