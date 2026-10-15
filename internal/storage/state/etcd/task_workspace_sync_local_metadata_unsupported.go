//go:build !linux

package etcd

import "context"

func (s *taskWorkspaceSyncRecoverySource) checkFinalizationRoot(context.Context) error {
	return ErrInvalidRecord
}

func (f *taskWorkspaceSyncLocalFinalizer) executeMetadata(context.Context) error {
	return ErrInvalidRecord
}
