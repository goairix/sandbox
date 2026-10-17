package etcd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync/atomic"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// coldQuotaWorkerCommitPermit records only trusted original Stage completion
// and joined original-lease cleanup. It is not a verified quota-return entry:
// separate paid return/receipt reads must still authenticate permanent state.
// Retain the original journal/preparation/owner, not a reconstructible Stage or
// native response pointer; the enrolled owner remains registered for those reads.
type coldQuotaWorkerCommitPermit struct {
	self     *coldQuotaWorkerCommitPermit
	backend  *Backend
	prepared *coldQuotaReturnPrepared
	owner    *runtimeEnrolledPublicationOwner
	journal  *coldQuotaWorkerJournalPermit

	// Proof reads stay on the exact cohort's original scheduling members.
	// Their diagnostic references cannot reconstruct this continuation.
	session   *WorkerSession
	partition *WorkerPartitionClaim
	ranges    *WorkerBudgetShare

	reference StageReference
	commitACK pb.ResponseHeader
}

// continueColdRuntimeQuotaReturnWorker admits one complete Stage cohort after
// its original journal ACK. Credit refusal leaves the original permit usable;
// after admission only the consume-CAS winner may Grant. Every subsequent
// failure closes this attempt and preserves its original journal/reference for
// arbitration, never another Grant, another snapshot, or refunded credit.
func (b *Backend) continueColdRuntimeQuotaReturnWorker(o *runtimeEnrolledPublicationOwner, journal *coldQuotaWorkerJournalPermit, session *WorkerSession, partition *WorkerPartitionClaim, ranges, claims *WorkerBudgetShare) (permit *coldQuotaWorkerCommitPermit, err error) {
	if err := b.validateColdQuotaWorkerContinuation(o, journal, partition); err != nil {
		return nil, err
	}
	prepared := journal.prepared
	call, worker, err := b.beginColdQuotaWorkerCall(o, session, partition, ranges, claims)
	if err != nil {
		return nil, err
	}
	// Defers are ordered deliberately: identified cleanup below must actually
	// return before finish publishes call.done to Release/Close. Neither defer
	// completes the enrolled owner, whose later proof reads remain outstanding.
	defer call.finish()
	if !atomic.CompareAndSwapUint32(&prepared.consumed, 0, 1) {
		return nil, ErrConflict
	}
	if err := b.preflightColdQuotaWorkerCohort(prepared, worker); err != nil {
		return nil, err
	}

	grant, grantErr := b.coldQuotaWorkerGrantRPC(call)
	identified := grant != nil && b.coldQuotaWorkerRPCHeader(grant.Header) && grant.ID > 0
	if identified {
		// The scalar is fixed before any cleanup, including a canceled or bad-
		// TTL Grant. Revoke uses the same paid call and its own bounded context.
		leaseID := grant.ID
		defer func() {
			_, cleanupErr := b.coldQuotaWorkerRevokeRPC(call, &pb.LeaseRevokeRequest{ID: leaseID})
			if errors.Is(cleanupErr, rpctypes.ErrLeaseNotFound) {
				cleanupErr = nil
			}
			if cleanupErr != nil {
				err = joinStageBeginCleanup(err, cleanupErr)
			}
			if err == nil {
				if postErr := call.postcheck(); postErr != nil {
					err = errors.Join(ErrOutcomeUnknown, postErr)
				}
			}
			if err != nil {
				permit = nil
			}
		}()
	}
	if grantErr != nil {
		return nil, grantErr
	}
	if !identified || grant.TTL <= 0 || grant.Error != "" {
		return nil, ErrOutcomeUnknown
	}

	guard, _, err := b.coldQuotaWorkerCohortRequests(prepared, worker, grant.ID, 1)
	if err != nil {
		return nil, err
	}
	guardACK, err := b.coldQuotaWorkerGuardRPC(call, guard)
	if err != nil {
		return nil, err
	}
	guardResponse := (*clientv3.TxnResponse)(guardACK)
	if err := b.operationResponseHeader(guardResponse); err != nil || !enrolledResponseBound(guardResponse) {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	if !guardResponse.Succeeded {
		if _, err := b.stageEvidencePoints(guardResponse, []string{b.identityKey, b.restoreKey}); err != nil {
			return nil, err
		}
		return nil, ErrConflict
	}
	if !operationPutResponses(guardResponse, 1) {
		return nil, ErrOutcomeUnknown
	}

	_, commit, err := b.coldQuotaWorkerCohortRequests(prepared, worker, grant.ID, guardACK.Header.Revision)
	if err != nil {
		return nil, err
	}
	commitACK, err := b.coldQuotaWorkerCommitRPC(call, commit)
	if err != nil {
		return nil, err
	}
	if err := b.coldQuotaWorkerCommitEvidence(prepared, grant.ID, guardACK.Header.Revision, guard.Success[0].GetRequestPut().Value, commitACK); err != nil {
		return nil, err
	}

	// This value cannot escape until the deferred original Revoke has joined.
	// Copy only scalar ACK fields, retaining no protobuf graph or Stage authority.
	header := commitACK.Header
	permit = &coldQuotaWorkerCommitPermit{
		backend: b, prepared: prepared, owner: o, journal: journal,
		session: session, partition: partition, ranges: ranges,
		reference: prepared.reference,
		commitACK: pb.ResponseHeader{ClusterId: header.ClusterId, MemberId: header.MemberId, Revision: header.Revision, RaftTerm: header.RaftTerm},
	}
	permit.self = permit
	return permit, nil
}

// validateColdQuotaWorkerContinuation authenticates only original in-memory
// bindings. Hashing the already-copied mutation detects changes without cloning
// it or reading a replacement history. ACK revision must cover every source
// history point; an ACK stamp alone never grants capability ownership.
func (b *Backend) validateColdQuotaWorkerContinuation(o *runtimeEnrolledPublicationOwner, journal *coldQuotaWorkerJournalPermit, partition *WorkerPartitionClaim) error {
	if !b.validWorkerBackend() || journal == nil || journal.self != journal || journal.backend != b || journal.owner != o || journal.prepared == nil {
		return ErrInvalidRecord
	}
	prepared := journal.prepared
	if atomic.LoadUint32(&prepared.consumed) != 0 {
		return ErrConflict
	}
	if prepared.self != prepared || prepared.backend != b || prepared.owner != o || prepared.reference != journal.reference || prepared.history == nil || prepared.returnKey == "" || atomic.LoadUint32(&prepared.journalAttempted) != 1 {
		return ErrInvalidRecord
	}
	if _, err := b.coldQuotaReturnOwnerContext(o); err != nil {
		return err
	}
	history := prepared.history
	if history.publication == nil || history.release == nil || partition == nil || partition.partition != prepared.reference.Partition || partition.partition != history.reservation.Owner.Partition || prepared.reference.StageID != "cold_runtime_quota_return" {
		return ErrInvalidRecord
	}
	if _, _, err := b.stageKeys(prepared.reference); err != nil {
		return err
	}
	locator := StageAttemptLocator{Namespace: prepared.reference.Namespace, Partition: prepared.reference.Partition, RequestID: prepared.reference.RequestID, StageID: prepared.reference.StageID, AttemptID: prepared.reference.AttemptID, RestoreEpoch: prepared.reference.RestoreEpoch}
	if err := locator.Validate(); err != nil {
		return err
	}
	if !b.coldQuotaWorkerRPCHeader(&journal.journalACK) {
		return ErrInvalidRecord
	}
	for _, point := range []*mvccpb.KeyValue{history.publication.journal, history.publication.receipt, history.reservedKV, history.workKV, history.settledKV, history.release.kv} {
		if point == nil || point.ModRevision <= 0 || journal.journalACK.Revision < point.ModRevision {
			return ErrInvalidRecord
		}
	}
	// Ready and occupied-slot revisions live only in the frozen mutation.
	// Their original snapshot fences must also precede the journal ACK.
	for _, comparison := range prepared.mutation.Comparisons {
		native := pb.Compare(comparison)
		if native.Target == pb.Compare_CREATE && native.GetCreateRevision() > journal.journalACK.Revision || native.Target == pb.Compare_MOD && native.GetModRevision() > journal.journalACK.Revision {
			return ErrInvalidRecord
		}
	}
	encoded, err := json.Marshal(prepared.mutation)
	if err != nil || len(encoded)+16*maxKeyBytes > maxMutationBytes {
		return ErrInvalidMutation
	}
	digest := sha256.Sum256(encoded)
	if hex.EncodeToString(digest[:]) != prepared.reference.Digest {
		return ErrInvalidMutation
	}
	return nil
}

// coldQuotaWorkerCommitEvidence preserves CommitStage's original Else ordering:
// the original immutable receipt arbitrates first, then the complete original
// guard distinguishes expiration from a live-guard conflict. An aborted receipt
// cannot create a commit permit and does not authorize replacing this attempt.
func (b *Backend) coldQuotaWorkerCommitEvidence(prepared *coldQuotaReturnPrepared, leaseID, guardRevision int64, guardValue []byte, native *pb.TxnResponse) error {
	response := (*clientv3.TxnResponse)(native)
	if err := b.operationResponseHeader(response); err != nil || !enrolledResponseBound(response) {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	if response.Succeeded {
		if !stageCommitResponses(response, prepared.mutation.Writes) {
			return ErrOutcomeUnknown
		}
		return nil
	}
	guardKey, receiptKey, err := b.stageKeys(prepared.reference)
	if err != nil {
		return err
	}
	points, err := b.stageEvidencePoints(response, []string{b.identityKey, b.restoreKey, receiptKey, guardKey})
	if err != nil {
		return err
	}
	if points[2] != nil {
		outcome, err := decodeReceipt(points[2], prepared.reference)
		if err != nil {
			return err
		}
		if outcome == OutcomeCommitted {
			return nil
		}
		return ErrConflict
	}
	guard := points[3]
	if guard == nil || string(guard.Value) != string(guardValue) || guard.Lease != leaseID || guard.CreateRevision != guardRevision || guard.ModRevision != guard.CreateRevision {
		return ErrGuardExpired
	}
	return ErrConflict
}
