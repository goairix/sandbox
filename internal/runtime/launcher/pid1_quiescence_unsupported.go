//go:build !linux

package launcher

import "context"

func (b *KernelBoundary) ObserveNoUserDescendants(context.Context) (*UserNamespaceQuiescenceObservation, error) {
	return nil, ErrUnsupported
}
