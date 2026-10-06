//go:build !linux || (!amd64 && !arm64)

package launcher

func ConfineMonitor(*KernelBoundary, uint32, uint32) (*MonitorBoundary, error) {
	return nil, ErrUnsupported
}
func (*MonitorBoundary) ValidateCurrent() error { return ErrUnsupported }
