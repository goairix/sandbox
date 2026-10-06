//go:build !linux

package launcher

import (
	"errors"
	"testing"
)

func TestKernelUnsupported(t *testing.T) {
	for _, f := range []func() (*KernelBoundary, error){BootstrapPID1, PrepareMonitor} {
		b, err := f()
		if b != nil || !errors.Is(err, ErrUnsupported) {
			t.Fatalf("unsupported: %v %v", b, err)
		}
	}
	b := &KernelBoundary{pid: 1, role: rolePID1}
	if !errors.Is(b.ValidateCurrent(), ErrUnsupported) {
		t.Fatal("nonzero handle not unsupported")
	}
}
