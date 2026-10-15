package etcd

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// ReceivedTaskWorkspaceArchive retains original private quota until Close.
// It grants no provider effect or current native authority.
// Later private consumption must freshly check the original owner's authority.
type ReceivedTaskWorkspaceArchive struct {
	self    *ReceivedTaskWorkspaceArchive
	origin  *Backend
	owner   *taskArchiveDelivery
	witness *taskReceivedArchive
}

func (r *ReceivedTaskWorkspaceArchive) Close() error {
	if r == nil || r.self != r || r.origin == nil || r.owner == nil || r.owner.backend != r.origin || r.owner.spool == nil || r.witness == nil || r.witness.origin != r.owner.spool {
		return errTaskArchive
	}
	r.owner.cancel()
	<-r.owner.done
	return r.owner.spool.close()
}

type taskArchiveDelivery struct {
	backend      *Backend
	claim        *TaskClaim
	prepared     *PreparedTaskWorkspaceArchive
	spool        *taskArchiveSpool
	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}
	wake         chan struct{}
	deadline     atomic.Pointer[time.Time]
	sessionAfter time.Time
	comparisons  []clientv3.Cmp
	prefix       string
	nativeMu     sync.Mutex
	failureMu    sync.Mutex
	failure      error
}

func (o *taskArchiveDelivery) fail(err error) {
	o.failureMu.Lock()
	if o.failure == nil {
		o.failure = err
	}
	o.failureMu.Unlock()
	o.cancel()
}

// Pointer ownership keeps Backend diagnostic copies from copying live locks;
// original Close callers share one admission seal and physical shutdown join.
type taskArchiveBackend struct {
	mu        sync.Mutex
	owners    map[*taskArchiveDelivery]struct{}
	enrolled  map[*runtimeEnrolledPublicationOwner]struct{}
	closed    bool
	closeDone chan struct{}
	closeErr  error
}

func (o *taskArchiveDelivery) failureError() error {
	o.failureMu.Lock()
	defer o.failureMu.Unlock()
	return o.failure
}
