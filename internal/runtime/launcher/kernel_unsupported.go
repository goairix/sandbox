//go:build !linux

package launcher

func BootstrapPID1() (*KernelBoundary, error)  { return nil, ErrUnsupported }
func PrepareMonitor() (*KernelBoundary, error) { return nil, ErrUnsupported }
func (b *KernelBoundary) ValidateCurrent() error {
	if b == nil || b.pid == 0 || b.role == 0 {
		return ErrKernelUnavailable
	}
	return ErrUnsupported
}
