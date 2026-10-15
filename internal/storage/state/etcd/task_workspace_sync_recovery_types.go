package etcd

import (
	"context"
	"hash"
	"io"
	"os"
	"sync"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

// One fixed slot owner holds its actual file and all physical-operation joins.
// It is independent of every original received/source/draft/witness seal.
type taskWorkspaceSyncRecoverySource struct {
	self                           *taskWorkspaceSyncRecoverySource
	store                          *taskArchiveStore
	slot                           uint32
	diagnostic                     taskArchiveRecoveryDiagnostic
	ctx                            context.Context
	cancel                         context.CancelFunc
	parentStop                     func() bool
	done                           chan struct{}
	mu                             sync.Mutex
	sealed, busy                   bool
	ops                            sync.WaitGroup
	err                            error
	file                           *os.File
	identity                       taskArchiveIdentity
	index                          []byte
	manifest                       taskArchiveRecoveryManifest
	authority                      *taskArchiveRecoveryAuthority
	plan                           TaskWorkspaceSyncPlan
	target                         *taskWorkspaceSyncLocalTarget
	stamp                          [5]int64
	cursor                         *taskWorkspaceSyncArchiveCursor
	current                        *workspaceSyncArchiveEntry
	body                           *taskWorkspaceSyncRecoveryBody
	checkpoint                     *TaskWorkspaceSyncCheckpoint
	checkpointKV                   *mvccpb.KeyValue
	completedFiles, completedBytes uint64
	localIOComplete                func(string)
}

type taskWorkspaceSyncRecoveryBody struct {
	self                              *taskWorkspaceSyncRecoveryBody
	source                            *taskWorkspaceSyncRecoverySource
	entry                             *workspaceSyncArchiveEntry
	ctx                               context.Context
	cancel                            context.CancelFunc
	mu                                sync.Mutex
	active, closed, complete, settled bool
	done                              chan struct{}
	reader                            *io.SectionReader
	hash                              hash.Hash
	bytes                             int64
	charge                            *taskWorkspaceSyncRecoveryCharge
	completion                        *taskWorkspaceSyncRecoveryCompletion
}

type taskWorkspaceSyncRecoveryCharge struct {
	source                    *taskWorkspaceSyncRecoverySource
	record                    TaskWorkspaceSyncIntentRecord
	intentKey, slotKey, value string
	birth                     int64
	generation                taskWorkspaceSyncRecyclingGeneration
}

type taskWorkspaceSyncRecoveryCompletion struct {
	self                                                       *taskWorkspaceSyncRecoveryCompletion
	source                                                     *taskWorkspaceSyncRecoverySource
	body                                                       *taskWorkspaceSyncRecoveryBody
	charge                                                     *taskWorkspaceSyncRecoveryCharge
	physical                                                   workspaceSyncLocalIOResult
	value                                                      string
	attempted, denied, known, gcAttempted, gcDenied, completed bool
	terminal                                                   taskWorkspaceSyncRecyclingGeneration
	checkpoint                                                 *taskWorkspaceSyncCheckpointAttempt
	payload                                                    TaskWorkspaceSyncCheckpoint
}
