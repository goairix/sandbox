//go:build !linux

package etcd

import (
	"context"

	"github.com/goairix/sandbox/internal/runtime/launcher"
)

func openTaskWorkspaceSyncLocalTarget(context.Context, *launcher.KernelBoundary, TaskWorkspaceSyncPlan, string) (*taskWorkspaceSyncLocalTarget, error) {
	return nil, launcher.ErrUnsupported
}
func (*taskWorkspaceSyncLocalTarget) check(context.Context) error { return launcher.ErrUnsupported }
func closeWorkspaceSyncLocalFD(int) error                         { return launcher.ErrUnsupported }
