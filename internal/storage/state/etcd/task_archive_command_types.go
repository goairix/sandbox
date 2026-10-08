package etcd

import (
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	t "github.com/goairix/sandbox/internal/runtime/controltransport"
	"time"
)

// TaskWorkspaceArchiveReference is immutable metadata attribution, not a sender.
type TaskWorkspaceArchiveReference struct {
	Task              TaskReference
	CommandID         string
	FinalSyncRevision int64
	Revision          int64
}

type TaskWorkspaceArchivePreparationResult struct {
	Reference TaskWorkspaceArchiveReference
	Outcome   Outcome
	Prepared  *PreparedTaskWorkspaceArchive
}

// PreparedTaskWorkspaceArchive is sealed to the original Backend, claim and
// final-sync draft. It exposes no ticket, guard, stage or provider authority.
type PreparedTaskWorkspaceArchive struct {
	self      *PreparedTaskWorkspaceArchive
	origin    *Backend
	claim     *TaskClaim
	finalSync *taskFinalSyncDraft
	draft     *taskArchiveDraft
}

type taskArchiveDraft struct {
	finalSync                              *taskFinalSyncDraft
	prepared                               *PreparedTaskQuiescence
	destination                            *t.Destination
	duration                               time.Duration
	deadline                               time.Time
	windowDeadline                         time.Time
	certificate                            []byte
	certificateAttempted, signingAttempted bool
	signedWire                             []byte
	issuerKey, issuerValue                 string
	issuerRevision                         int64
	claims                                 p.TaskWorkspaceArchiveTicketClaims
	ticket                                 p.TaskWorkspaceArchiveEvidence
	value                                  string
	revision                               int64
	writeAttempted, lost                   bool
	preparedArchive                        *PreparedTaskWorkspaceArchive
}

func (d *taskArchiveDraft) reference() TaskWorkspaceArchiveReference {
	return TaskWorkspaceArchiveReference{Task: d.finalSync.record.Task.Reference, CommandID: d.finalSync.record.AttemptID, FinalSyncRevision: d.finalSync.revision, Revision: d.revision}
}

func (b *Backend) validPreparedTaskWorkspaceArchive(p *PreparedTaskWorkspaceArchive) bool {
	return b != nil && p != nil && p.self == p && p.origin == b && b.validTaskClaim(p.claim) && p.finalSync != nil && p.claim.finalSyncDraft == p.finalSync && p.draft != nil && p.finalSync.archive == p.draft && p.draft.preparedArchive == p
}
