//go:build !linux

package etcd

import (
	"context"
	"os"
)

func workspaceSyncRecoveryArchiveStamp(*os.File) ([5]int64, error) {
	return [5]int64{}, ErrInvalidConfiguration
}

func (s *taskWorkspaceSyncRecoverySource) reconcileLocal(context.Context, *taskWorkspaceSyncRecoveryBody, *taskWorkspaceSyncRecoveryCharge) (*taskWorkspaceSyncRecoveryCompletion, error) {
	return nil, ErrInvalidConfiguration
}
