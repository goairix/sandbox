package etcd

import (
	"context"
	"sync/atomic"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

type coldQuotaWorkerHistoryPhase uint8

const (
	coldQuotaWorkerHistoryWantLocate coldQuotaWorkerHistoryPhase = iota
	coldQuotaWorkerHistoryWantPublication
	coldQuotaWorkerHistoryWantReservation
	coldQuotaWorkerHistoryWantSettlement
	coldQuotaWorkerHistoryWantRelease
	coldQuotaWorkerHistoryWantHead
	coldQuotaWorkerHistoryWantSignedHistory
	coldQuotaWorkerHistoryDone
)

// coldQuotaWorkerHistoryOrigin is fixed by construction. In particular, the
// registered owner's original context cannot be replaced by another live one
// to extend this cursor's lifetime, and matching diagnostic member fields do
// not replace the original session, partition or Range registry membership.
type coldQuotaWorkerHistoryOrigin struct {
	backend   *Backend
	owner     *runtimeEnrolledPublicationOwner
	context   context.Context
	workspace WorkspaceIdentity
	intent    string
	task      TaskReference
	session   *WorkerSession
	partition *WorkerPartitionClaim
	ranges    *WorkerBudgetShare
}

// coldQuotaWorkerHistory authenticates ordinary historical metadata in seven
// finite paid reads. The returned history retains exactly one authenticated
// release profile for the original quota writer, without conveying worker,
// Stage or physical-cleanup authority by itself.
// The caller must use one shared workspace and keep the returned history
// immutable while this cursor/workspace is retained. Neither this component
// nor its borrowed result permits 32 jobs to retain separate heavy histories;
// original-job origin seals and registry/retention enforcement belong to its
// caller, not to these diagnostic TaskReference inputs.
type coldQuotaWorkerHistory struct {
	self   *coldQuotaWorkerHistory
	origin coldQuotaWorkerHistoryOrigin

	// The nonblocking winner owns all partial state through the actual native
	// return, typed decoding and post-return binding check. Cancellation alone
	// cannot release busy or let another step advance this cursor.
	busy    uint32
	phase   coldQuotaWorkerHistoryPhase
	located RuntimeEnrolledPublicationRecord
	history *coldQuotaReturnHistory

	// The strict native seal selects one profile. Head observations remain
	// temporary until that profile's pure full-history join succeeds.
	releaseProfile taskOwnerReleaseProfile
	releaseKV      *mvccpb.KeyValue
	snapshot       *localRuntimeHistorySnapshot
	detachedHead   *detachedRuntimeHistorySnapshot
	actualTask     *mvccpb.KeyValue

	// This is B's complete private union, never a codec-only seal. Its entire
	// thirteen-point graph remains in the one cursor/shared workspace and is
	// counted by the existing owned-root walker. No v1 Complete is synthesized.
	releaseProof *taskOwnerReleaseProof
}

// newColdQuotaWorkerHistory performs the public authentication entry's local
// configuration and address validation, then seals the original live worker
// members without paying or dispatching. The caller retains the registered
// enrolled owner until all of its cursor reads have actually joined.
func (b *Backend) newColdQuotaWorkerHistory(o *runtimeEnrolledPublicationOwner, w WorkspaceIdentity, intent string, task TaskReference, session *WorkerSession, partition *WorkerPartitionClaim, ranges *WorkerBudgetShare) (*coldQuotaWorkerHistory, error) {
	ctx, err := b.coldQuotaReturnOwnerContext(o)
	if err != nil {
		return nil, err
	}
	origin := coldQuotaWorkerHistoryOrigin{
		backend: b, owner: o, context: ctx, workspace: w, intent: intent,
		task: task, session: session, partition: partition, ranges: ranges,
	}
	if err := b.validateColdQuotaWorkerHistoryOrigin(origin); err != nil {
		return nil, err
	}

	state := &coldQuotaWorkerHistory{origin: origin, history: &coldQuotaReturnHistory{}}
	state.self = state
	return state, nil
}

// stepColdQuotaWorkerHistory executes at most one original Range262144/no-Claim
// W20 read. A successful typed phase publishes partial state and yields nil;
// the seventh read returns the borrowed immutable full history. Errors leave
// phase and partial state unchanged for the next paid turn. Cached done still
// requires the original live binding and the selected detached proof identity.
func (b *Backend) stepColdQuotaWorkerHistory(o *runtimeEnrolledPublicationOwner, state *coldQuotaWorkerHistory, session *WorkerSession, partition *WorkerPartitionClaim, ranges *WorkerBudgetShare) (*coldQuotaReturnHistory, error) {
	if state == nil || state.self != state || state.origin.backend != b {
		return nil, ErrInvalidRecord
	}
	if !atomic.CompareAndSwapUint32(&state.busy, 0, 1) {
		return nil, ErrConflict
	}
	defer atomic.StoreUint32(&state.busy, 0)

	origin := state.origin
	if origin.owner != o || origin.session != session || origin.partition != partition || origin.ranges != ranges {
		return nil, ErrInvalidRecord
	}
	if err := b.validateColdQuotaWorkerHistoryState(state, origin); err != nil {
		return nil, err
	}

	var err error
	switch state.phase {
	case coldQuotaWorkerHistoryWantLocate:
		err = b.readColdQuotaWorkerHistoryLocate(state, origin)
	case coldQuotaWorkerHistoryWantPublication:
		err = b.readColdQuotaWorkerHistoryPublication(state, origin)
	case coldQuotaWorkerHistoryWantReservation:
		err = b.readColdQuotaWorkerHistoryReservation(state, origin)
	case coldQuotaWorkerHistoryWantSettlement:
		err = b.readColdQuotaWorkerHistorySettlement(state, origin)
	case coldQuotaWorkerHistoryWantRelease:
		err = b.readColdQuotaWorkerHistoryRelease(state, origin)
	case coldQuotaWorkerHistoryWantHead:
		err = b.readColdQuotaWorkerHistoryHead(state, origin)
	case coldQuotaWorkerHistoryWantSignedHistory:
		err = b.readColdQuotaWorkerHistorySigned(state, origin)
	case coldQuotaWorkerHistoryDone:
		return state.history, nil
	default:
		return nil, ErrInvalidRecord
	}
	if err != nil {
		return nil, err
	}
	if state.phase == coldQuotaWorkerHistoryDone {
		return state.history, nil
	}
	return nil, nil
}

// Every phase uses its entry copy of origin for the final check, before any
// partial assignment. No native call or validator can replace the constructor's
// owner/context or adopt another otherwise live worker member while yielding.
func (b *Backend) validateColdQuotaWorkerHistoryState(state *coldQuotaWorkerHistory, origin coldQuotaWorkerHistoryOrigin) error {
	if state == nil || state.self != state || state.origin != origin || state.history == nil {
		return ErrInvalidRecord
	}
	if state.phase == coldQuotaWorkerHistoryDone && state.releaseProfile == taskOwnerReleaseDetached {
		if state.releaseProof == nil || state.history.profileRelease != state.releaseProof {
			return ErrInvalidRecord
		}
		if _, err := coldQuotaReturnRelease(state.history); err != nil {
			return err
		}
	}
	return b.validateColdQuotaWorkerHistoryOrigin(origin)
}

func (b *Backend) validateColdQuotaWorkerHistoryOrigin(origin coldQuotaWorkerHistoryOrigin) error {
	if !b.validWorkerBackend() || origin.backend != b {
		return ErrInvalidRecord
	}
	ctx, err := b.coldQuotaReturnOwnerContext(origin.owner)
	if err != nil {
		return err
	}
	if ctx != origin.context {
		return ErrInvalidRecord
	}
	if b.localRuntimeRelease.validate() != nil || b.validateTaskReference(origin.task) != nil || b.validateWorkspaceBinding(origin.workspace) != nil || origin.task.Partition != origin.workspace.Partition() {
		return ErrInvalidRecord
	}
	if _, _, err := b.coldProfile(); err != nil {
		return err
	}
	if b.publicationVerifier == nil || b.enrolledPublication == nil {
		return ErrInvalidConfiguration
	}
	if _, _, err := b.namespace.runtimePublicationKeys(origin.workspace.Partition(), origin.intent); err != nil {
		return err
	}
	if err := b.validateColdQuotaWorkerPostreadMembers(origin.session, origin.partition, origin.ranges); err != nil {
		return err
	}
	if origin.partition.partition != origin.task.Partition {
		return ErrInvalidRecord
	}
	return nil
}
