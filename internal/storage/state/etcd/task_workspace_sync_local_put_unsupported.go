//go:build !linux

package etcd

import "context"

func (s *PreparedTaskWorkspaceSyncSource) putLocal(ctx context.Context, body *taskWorkspaceSyncBody, target *taskWorkspaceSyncLocalTarget) (*taskWorkspaceSyncLocalWitness, error) {
	return nil, ErrInvalidRecord
}
