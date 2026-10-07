//go:build !linux || (!amd64 && !arm64)

package launcher

import "context"

func ConfineMonitor(*KernelBoundary, uint32, uint32) (*MonitorBoundary, error) {
	return nil, ErrUnsupported
}
func (*MonitorBoundary) ValidateCurrent() error { return ErrUnsupported }

func (*MonitorBoundary) Drain(context.Context) (LocalDrainObservation, error) {
	return LocalDrainObservation{}, ErrUnsupported
}

func (*MonitorBoundary) SuperviseRoot(context.Context, int) (RootExitObservation, error) {
	return RootExitObservation{}, ErrUnsupported
}
func (*MonitorBoundary) SuperviseRootWithLifecycle(context.Context, int, *RootLifecycle) (RootExitObservation, error) {
	return RootExitObservation{}, ErrUnsupported
}
func NewRootLifecycle(int64) (*RootLifecycle, error) { return nil, ErrUnsupported }
