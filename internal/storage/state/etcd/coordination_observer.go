package etcd

import (
	"context"
	clientv3 "go.etcd.io/etcd/client/v3"
	"time"
)

func (s *Store) observeCoordinationStage(ctx context.Context, operation, stage string, err error, started time.Time) {
	if s.coordinationStageObserver == nil {
		return
	}
	result := "success"
	if err != nil {
		result = "error"
	}
	s.coordinationStageObserver(ctx, operation, stage, result, time.Since(started))
}

func (s *Store) observeCoordinationCommit(ctx context.Context, operation string, response *clientv3.TxnResponse, err error, started time.Time) {
	if err == nil && response != nil && !response.Succeeded && s.coordinationStageObserver != nil {
		s.coordinationStageObserver(ctx, operation, "commit", "conflict", time.Since(started))
		return
	}
	s.observeCoordinationStage(ctx, operation, "commit", err, started)
}
func (s *Store) coordinationRevoke(ctx context.Context, operation string, lease clientv3.LeaseID) {
	started := time.Now()
	_, err := s.client.Revoke(ctx, lease)
	s.observeCoordinationStage(ctx, operation, "revoke", err, started)
}
