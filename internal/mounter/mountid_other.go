//go:build !linux

package mounter

import "fmt"

func kernelMountID(string) (uint64, error) {
	return 0, fmt.Errorf("kernel mount ID is unavailable on this platform")
}
