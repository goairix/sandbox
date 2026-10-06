package launcher

import (
	"errors"
	"testing"
)

func TestMonitorIdentity(t *testing.T) {
	for _, pair := range [][2]uint32{{1, 1}, {1000, 1000}, {2147483647, 2147483647}} {
		if err := validateMonitorIdentity(pair[0], pair[1]); err != nil {
			t.Errorf("valid %v: %v", pair, err)
		}
	}
	for _, pair := range [][2]uint32{{0, 1000}, {1000, 0}, {2147483648, 1}, {1, 2147483648}, {4294967295, 4294967295}} {
		if err := validateMonitorIdentity(pair[0], pair[1]); !errors.Is(err, ErrUnsafeKernel) {
			t.Errorf("invalid %v: %v", pair, err)
		}
	}
}

// A copied mutex must never give a second receiver independent wait ownership.
func TestMonitorReceiverSeal(t *testing.T) {
	original := &MonitorBoundary{pid: 42}
	original.self = original
	if err := original.validateReceiver(42); err != nil {
		t.Fatal(err)
	}
	// Explicit field-copy models the exported struct copy without copying a used lock.
	copied := &MonitorBoundary{self: original.self, pid: original.pid}
	if err := copied.validateReceiver(42); !errors.Is(err, ErrUnsafeKernel) {
		t.Fatalf("copy accepted: %v", err)
	}
	if err := original.validateReceiver(43); !errors.Is(err, ErrUnsafeKernel) {
		t.Fatalf("another PID accepted: %v", err)
	}
	for _, m := range []*MonitorBoundary{nil, {}} {
		if err := m.validateReceiver(42); err == nil {
			t.Fatal("empty seal accepted")
		}
	}
}
