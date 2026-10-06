//go:build !linux || (!amd64 && !arm64)

package launcher

import (
	"errors"
	"testing"
)

func TestMonitorUnsupported(t *testing.T) {
	for _, k := range []*KernelBoundary{nil, {}, {role: roleMonitor, pid: 42}} {
		if m, err := ConfineMonitor(k, 1000, 1000); m != nil || !errors.Is(err, ErrUnsupported) {
			t.Fatalf("unsupported: %v %v", m, err)
		}
	}
	for _, m := range []*MonitorBoundary{nil, {}} {
		if err := m.ValidateCurrent(); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("unsupported validation: %v", err)
		}
	}
}

// The older kernel native dispatcher builds on every Linux architecture.
func nativeConfinementMode(string) (bool, error) { return false, ErrUnsupported }
