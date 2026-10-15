package etcd

import (
	"context"
	"os"
	"sync"
)

// One fixed slot owner holds its actual file and all physical-operation joins.
// It is independent of every original received/source/draft/witness seal.
type taskWorkspaceSyncRecoverySource struct {
	self         *taskWorkspaceSyncRecoverySource
	store        *taskArchiveStore
	slot         uint32
	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}
	mu           sync.Mutex
	sealed, busy bool
	ops          sync.WaitGroup
	err          error
	file         *os.File
	identity     taskArchiveIdentity
	index        []byte
	manifest     taskArchiveRecoveryManifest
	authority    *taskArchiveRecoveryAuthority
	plan         TaskWorkspaceSyncPlan
	target       *taskWorkspaceSyncLocalTarget
}
