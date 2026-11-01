package state

import (
	"context"
	"time"
)

// ActiveDataLease identifies one server-leased Data participant. LeaseID is
// checked together with its exact operation, never used as authority alone.
type ActiveDataLease struct {
	Operation    ActiveSandboxOperation
	LeaseID      int64
	RequestedTTL time.Duration
}

type ActiveDataView struct {
	Record *ActiveSandboxRecord
	Lease  ActiveDataLease
}

// ActiveDataOperationRepository is optional. It does not change the independent
// mutation/exclusive operation protocol or make cached snapshots authoritative.
type ActiveDataOperationRepository interface {
	OpenDataOperation(context.Context, string, string, time.Duration) (*ActiveDataView, error)
	ReadDataOperation(context.Context, ActiveDataLease, bool) (*ActiveDataView, error)
	KeepAliveDataOperation(context.Context, ActiveDataLease) (*ActiveDataView, time.Duration, error)
}
