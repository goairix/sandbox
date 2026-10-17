package etcd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync/atomic"
	"time"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

type coldQuotaWorkerPostreadPhase uint8

const (
	coldQuotaWorkerWantReturn coldQuotaWorkerPostreadPhase = iota
	coldQuotaWorkerWantReceipt
	coldQuotaWorkerPostreadDone
)

// coldQuotaWorkerPostread keeps one original Commit's two paid proof phases.
// The compact stamp is ordinary decoded data, never physical-cleanup or Stage
// authority. No raw return KV or extra history graph survives the Q1 yield.
type coldQuotaWorkerPostread struct {
	self         *coldQuotaWorkerPostread
	backend      *Backend
	owner        *runtimeEnrolledPublicationOwner
	ownerContext context.Context
	permit       *coldQuotaWorkerCommitPermit
	prepared     *coldQuotaReturnPrepared
	journal      *coldQuotaWorkerJournalPermit
	session      *WorkerSession
	partition    *WorkerPartitionClaim
	ranges       *WorkerBudgetShare
	reference    StageReference
	returnKey    string

	// Only the nonblocking busy winner may inspect or update phase/stamp.
	// It retains ownership through the actual native return and typed decode;
	// cancellation alone never releases ownership to another step.
	busy  uint32
	phase coldQuotaWorkerPostreadPhase
	stamp coldQuotaReturnProofStamp
}

// newColdQuotaWorkerPostread accepts only an original Commit permit after its
// original Revoke has joined. Construction does not charge or perform native
// work; every later step must still validate the same owner and members.
func (b *Backend) newColdQuotaWorkerPostread(o *runtimeEnrolledPublicationOwner, permit *coldQuotaWorkerCommitPermit, session *WorkerSession, partition *WorkerPartitionClaim, ranges *WorkerBudgetShare) (*coldQuotaWorkerPostread, error) {
	ctx, err := b.validateColdQuotaWorkerPostreadPermit(o, permit, session, partition, ranges)
	if err != nil {
		return nil, err
	}
	state := &coldQuotaWorkerPostread{
		backend: b, owner: o, ownerContext: ctx, permit: permit,
		prepared: permit.prepared, journal: permit.journal,
		session: session, partition: partition, ranges: ranges,
		reference: permit.reference, returnKey: permit.prepared.returnKey,
	}
	state.self = state
	return state, nil
}

// stepColdQuotaWorkerPostread executes at most one Range-only W20 read. Q1
// saves a validated compact stamp and yields nil,nil; Q2 additionally binds the
// full receipt reference before returning a copied quota-only entry. A refusal
// or error leaves phase/stamp unchanged, so a later paid turn retries only the
// outstanding point. Done remains subject to original owner/member validation.
func (b *Backend) stepColdQuotaWorkerPostread(o *runtimeEnrolledPublicationOwner, permit *coldQuotaWorkerCommitPermit, state *coldQuotaWorkerPostread, session *WorkerSession, partition *WorkerPartitionClaim, ranges *WorkerBudgetShare) (*ColdRuntimeQuotaReturnEntry, error) {
	if state == nil || state.self != state || state.backend != b {
		return nil, ErrInvalidRecord
	}
	if !atomic.CompareAndSwapUint32(&state.busy, 0, 1) {
		return nil, ErrConflict
	}
	defer atomic.StoreUint32(&state.busy, 0)

	if err := b.validateColdQuotaWorkerPostreadState(o, permit, state, session, partition, ranges); err != nil {
		return nil, err
	}
	switch state.phase {
	case coldQuotaWorkerWantReturn:
		points, err := b.readColdQuotaWorkerPoints(o, session, partition, ranges, []string{state.returnKey}, []int{16384})
		if err != nil {
			return nil, err
		}
		stamp, err := b.decodeColdQuotaReturnProof(state.prepared.history, points[0])
		if err != nil {
			return nil, err
		}
		if err := b.validateColdQuotaWorkerPostreadState(o, permit, state, session, partition, ranges); err != nil {
			return nil, err
		}
		state.stamp = stamp
		state.phase = coldQuotaWorkerWantReceipt
		return nil, nil

	case coldQuotaWorkerWantReceipt:
		key, err := b.runtimeDispatchReceiptKey(state.stamp.attempt)
		if err != nil {
			return nil, err
		}
		points, err := b.readColdQuotaWorkerPoints(o, session, partition, ranges, []string{key}, []int{4096})
		if err != nil {
			return nil, err
		}
		ref, entry, err := b.decodeColdQuotaReturnReceipt(state.stamp, points[0])
		if err != nil {
			return nil, err
		}
		if ref != state.reference {
			return nil, ErrCorruptReceipt
		}
		if err := b.validateColdQuotaWorkerPostreadState(o, permit, state, session, partition, ranges); err != nil {
			return nil, err
		}
		state.phase = coldQuotaWorkerPostreadDone
		return entry, nil

	case coldQuotaWorkerPostreadDone:
		entry := state.stamp.entry
		return &entry, nil

	default:
		return nil, ErrInvalidRecord
	}
}

// validateColdQuotaWorkerPostreadState checks the immutable constructor seals
// even for cached done. The original context pointer must remain the source of
// cancellation; replacing it with another live context cannot extend ownership.
func (b *Backend) validateColdQuotaWorkerPostreadState(o *runtimeEnrolledPublicationOwner, permit *coldQuotaWorkerCommitPermit, state *coldQuotaWorkerPostread, session *WorkerSession, partition *WorkerPartitionClaim, ranges *WorkerBudgetShare) error {
	if state.owner != o || state.permit != permit || state.session != session || state.partition != partition || state.ranges != ranges || permit == nil || state.prepared == nil || state.prepared != permit.prepared || state.journal != permit.journal || state.reference != permit.reference || state.returnKey != state.prepared.returnKey {
		return ErrInvalidRecord
	}
	ctx, err := b.validateColdQuotaWorkerPostreadPermit(o, permit, session, partition, ranges)
	if err != nil {
		return err
	}
	if ctx != state.ownerContext {
		return ErrInvalidRecord
	}
	return nil
}

// validateColdQuotaWorkerPostreadPermit preserves the continuation's original
// binding, history-revision and mutation-digest invariants with consumed==1:
// successful Stage has already consumed that preparation. Reusing the earlier
// continuation validator would wrongly require consumed==0 and reopen Stage.
// No diagnostic reference, header stamp or copied member can grant membership.
func (b *Backend) validateColdQuotaWorkerPostreadPermit(o *runtimeEnrolledPublicationOwner, permit *coldQuotaWorkerCommitPermit, session *WorkerSession, partition *WorkerPartitionClaim, ranges *WorkerBudgetShare) (context.Context, error) {
	if !b.validWorkerBackend() || permit == nil || permit.self != permit || permit.backend != b || permit.owner != o || permit.prepared == nil || permit.journal == nil || permit.session != session || permit.partition != partition || permit.ranges != ranges {
		return nil, ErrInvalidRecord
	}
	prepared, journal := permit.prepared, permit.journal
	if prepared.self != prepared || prepared.backend != b || prepared.owner != o || prepared.reference != permit.reference || prepared.history == nil || prepared.returnKey == "" || atomic.LoadUint32(&prepared.consumed) != 1 || atomic.LoadUint32(&prepared.journalAttempted) != 1 {
		return nil, ErrInvalidRecord
	}
	if journal.self != journal || journal.backend != b || journal.owner != o || journal.prepared != prepared || journal.reference != permit.reference || !b.coldQuotaWorkerRPCHeader(&journal.journalACK) || !b.coldQuotaWorkerRPCHeader(&permit.commitACK) || permit.commitACK.Revision < journal.journalACK.Revision {
		return nil, ErrInvalidRecord
	}
	ctx, err := b.coldQuotaReturnOwnerContext(o)
	if err != nil {
		return nil, err
	}
	if err := b.validateColdQuotaWorkerPostreadMembers(session, partition, ranges); err != nil {
		return nil, err
	}

	history := prepared.history
	if history.publication == nil || history.publication.entry == nil || history.release == nil || partition.partition != prepared.reference.Partition || partition.partition != history.reservation.Owner.Partition || prepared.reference.StageID != "cold_runtime_quota_return" {
		return nil, ErrInvalidRecord
	}
	if _, _, err := b.stageKeys(prepared.reference); err != nil {
		return nil, err
	}
	key, err := b.namespace.coldRuntimeQuotaReturnKey(history.reservation.Owner.Partition, history.reservation.Owner.IntentID)
	if err != nil || key != prepared.returnKey {
		return nil, ErrInvalidRecord
	}
	for _, point := range []*mvccpb.KeyValue{history.publication.journal, history.publication.receipt, history.reservedKV, history.workKV, history.settledKV, history.release.kv} {
		if point == nil || point.ModRevision <= 0 || journal.journalACK.Revision < point.ModRevision {
			return nil, ErrInvalidRecord
		}
	}
	for _, comparison := range prepared.mutation.Comparisons {
		native := pb.Compare(comparison)
		if native.Target == pb.Compare_CREATE && native.GetCreateRevision() > journal.journalACK.Revision || native.Target == pb.Compare_MOD && native.GetModRevision() > journal.journalACK.Revision {
			return nil, ErrInvalidRecord
		}
	}
	encoded, err := json.Marshal(prepared.mutation)
	if err != nil || len(encoded)+16*maxKeyBytes > maxMutationBytes {
		return nil, ErrInvalidMutation
	}
	digest := sha256.Sum256(encoded)
	if hex.EncodeToString(digest[:]) != prepared.reference.Digest {
		return nil, ErrInvalidMutation
	}
	return ctx, nil
}

// validateColdQuotaWorkerPostreadMembers checks original live registry members
// without paying or registering a call. No lock spans hashing or native work;
// readColdQuotaWorkerPoints owns atomic liveness/admission and the actual join.
func (b *Backend) validateColdQuotaWorkerPostreadMembers(session *WorkerSession, partition *WorkerPartitionClaim, ranges *WorkerBudgetShare) error {
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if !b.validWorkerSession(session) || !b.validWorkerPartition(partition) || !b.validWorkerBudget(ranges) || partition.session != session || ranges.session != session || ranges.kind != WorkerBudgetRange {
		return ErrInvalidRecord
	}
	if session.lost || session.released || session.release != nil || partition.lost || partition.released || partition.release != nil || ranges.lost || ranges.released || ranges.release != nil || session.reference.LeaseID <= 0 || !time.Now().Before(session.deadline) {
		return ErrGuardExpired
	}
	return session.ctx.Err()
}
