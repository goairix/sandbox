//go:build !linux

package mounter

import "fmt"

func captureNativeOwner() (*nativeOwner, error) {
	return nil, fmt.Errorf("native helper ownership requires Linux PID1")
}

func (owner *nativeOwner) validateCurrent() error {
	return fmt.Errorf("native helper ownership requires Linux PID1")
}
