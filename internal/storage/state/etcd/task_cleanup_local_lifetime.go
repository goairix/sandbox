package etcd

import (
	"context"
	"errors"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func (w *DueWorker) stopCleanupJobs() {
	b := w.backend
	b.archiveLife.mu.Lock()
	w.cleanupSealed = true
	jobs := w.cleanupJobs
	for _, j := range jobs {
		if j != nil {
			j.cancel()
		}
	}
	b.archiveLife.mu.Unlock()
	// This is the sole actor's stop tail, after its active native turn returned.
	// Release cold metadata before replying to physical owners; their eventual
	// finish must never read or mutate the actor-owned workspace pointer.
	if w.coldQuota != nil {
		w.releaseCleanupColdQuota(w.coldQuota.job)
	}
	if w.managedSource != nil {
		w.releaseCleanupManagedSource(w.managedSource.job)
	}
	// The sole actor has already returned from its active native turn. No
	// accepted continuation remains executing while these replies are refused.
	for _, j := range jobs {
		if j == nil {
			continue
		}
		b.archiveLife.mu.Lock()
		request, started := j.request, j.started
		j.request = nil
		b.archiveLife.mu.Unlock()
		if request != nil {
			request.reply <- cleanupLocalReply{claim: j.claim, err: context.Canceled}
		}
		if !started {
			b.finishCleanupJob(j, context.Canceled)
		}
	}
	for _, j := range jobs {
		if j != nil {
			<-j.done
		}
	}
}

func (b *Backend) cleanupResult(j *cleanupLocalJob) CleanupLocalResult {
	result := j.result
	if j.state == nil {
		return result
	}
	s := j.state
	r := s.record
	result.Phase = CleanupLocalPhase(r.Confirmed)
	result.Retained = r.Phase != "settled"
	result.Slot = &CleanupSlotReference{Task: r.Task, TaskBirth: r.TaskBirth, InitialAttempt: r.InitialAttempt, Incarnation: r.Incarnation, Slot: r.Slot, ExecutionRevision: s.execution.mod}
	if s.slot != nil {
		result.Slot.SlotRevision = s.slot.mod
	}
	if r.Pending != nil {
		p := r.Pending
		result.Pending = &CleanupPendingReference{Purpose: p.Purpose, ClaimID: p.ClaimID, Stage: StageReference{Namespace: r.Task.Namespace, RestoreEpoch: r.Task.RestoreEpoch, Partition: r.Task.Partition, StageID: p.StageID, RequestID: p.RequestID, AttemptID: p.AttemptID, Digest: p.Digest}}
		if r.Archive != nil {
			result.Pending.StoreID, result.Pending.ArchiveSlot = r.Archive.StoreID, r.Archive.Slot
		}
	}
	if r.Phase == "settled" && r.Release != nil {
		var released *mvccpb.KeyValue
		if j.proof != nil && j.proof.record.Task == r.Task {
			released = j.proof.kv
		} else if profile, err := b.detachedCleanupOccupationProof(j); err == nil {
			released = profile.detached.kv
		}
		if released != nil && released.CreateRevision == r.Release.CreateRevision && workspaceSyncIntentDigest(string(released.Value)) == r.Release.Digest {
			result.Outcome = OutcomeCommitted
			result.Release = &TaskOwnerReleaseEntry{Task: r.Task, CreateRevision: r.Release.CreateRevision, SHA256: r.Release.Digest}
			result.Reason = "complete"
		}
	}
	return result
}

// The only publishers are the sole admission actor or the one physical job,
// never both. Publication follows all owned work and callback joins. Keeping
// the registry entry until this point also makes Backend.Close discover it.
func (b *Backend) finishCleanupJob(j *cleanupLocalJob, err error) {
	j.cancel()
	if !j.started && j.claim != nil {
		cleanup, stop := context.WithTimeout(context.Background(), b.requestTimeout)
		err = errors.Join(err, b.ReleaseTaskClaim(cleanup, j.claim))
		stop()
		j.claim = nil
	}
	if j.target != nil && j.target.termination != nil {
		err = errors.Join(err, j.target.termination.Close())
	}
	j.inventory = nil
	j.detachedNoEffect = nil
	j.detachedRecovery = nil
	if !j.started && j.runID != "" {
		j.effectsJoined = true
		cleanup, stop := context.WithTimeout(context.Background(), b.requestTimeout)
		err = errors.Join(err, b.joinCleanupRun(cleanup, j))
		stop()
	}
	j.stopCaller()
	j.result, j.err = b.cleanupResult(j), err

	// A cold metadata job reaches this tail only after cleanupTurn has joined
	// its native owner. Retain detached history through that owner, then retire
	// it on the same actor before removing the original registry identity.
	// Physical jobs keep their separate continuation/join ownership unchanged.
	if !j.started && j.owner == nil {
		j.worker.releaseCleanupColdQuota(j)
	}
	if err != nil && j.result.Reason == "" {
		j.result.Reason = "reconciliation-required"
	}
	// Only the original physical/preparation owner reaches this point after
	// transports, Claims and caller callbacks joined. Snapshot without keeping
	// its error graph; publish history atomically with registry removal/done.
	var failure cleanupJobResult
	if j.worker.options.AutomaticCleanup {
		failure = cleanupResultFailure(err)
	}
	b.archiveLife.mu.Lock()
	if s := j.worker.managedSource; s != nil && s.job == j {
		// This original preparation/physical owner has joined all callbacks,
		// transports, Claim cleanup and accepted continuations. Retire its
		// exclusive source/target graph before refunding the workspace. A
		// copied job or cancellation callback never reaches this ownership tail.
		j.target = nil
		j.bundle, j.evidence, j.early = nil, nil, nil
		j.worker.managedSource = nil
		j.worker.cache.managedSource = false
		j.worker.cache.managedSourceKeys = 0
	}
	j.worker.recordCleanupResultLocked(failure)
	for i, candidate := range j.worker.cleanupJobs {
		if candidate == j {
			j.worker.cleanupJobs[i] = nil
		}
	}
	close(j.done)
	b.archiveLife.mu.Unlock()
}

func (b *Backend) readCleanupKnownState(owner *workerDueOwner, prior *cleanupSlotState) (*cleanupSlotState, error) {
	if prior == nil {
		return nil, ErrInvalidRecord
	}
	key, err := b.namespace.cleanupExecutionSlotKey(prior.record.Slot)
	if err != nil {
		return nil, err
	}
	points, revision, err := b.cleanupReadPoints(owner, prior.execution.key, key)
	if err != nil {
		return nil, err
	}
	state, err := b.cleanupStateFromPoints(prior.record.Task, points[0], points[1], revision)
	if err != nil || state == nil || state.record.Incarnation != prior.record.Incarnation || state.execution.create != prior.execution.create {
		return nil, errors.Join(ErrConflict, err)
	}
	if prior.slot != nil && (state.slot == nil || *prior.slot != *state.slot) {
		return nil, ErrConflict
	}
	return state, nil
}

func (b *Backend) updateCleanupRun(owner *workerDueOwner, j *cleanupLocalJob, next cleanupExecutionRecord) error {
	if j == nil || j.pendingProgress != nil {
		return ErrConflict
	}
	s := j.state
	if s == nil || s.slot == nil || s.record.Phase != "occupied" || next.Phase != "occupied" || cleanupSlotRecordFromExecution(next) != cleanupSlotRecordFromExecution(s.record) || next.Run == nil || next.Run.ID != j.runID || next.Run.Joined || b.validateCleanupState(s) != nil {
		return ErrInvalidRecord
	}
	if s.record.Run != nil && (s.record.Run.ID != j.runID || s.record.Run.Joined || s.record.Run.Worker != next.Run.Worker) {
		return ErrConflict
	}
	cmps, err := b.cleanupOwnerComparisons(owner)
	if err != nil {
		return err
	}
	wire, err := encodeCleanupExecution(next)
	if err != nil {
		return err
	}
	cmps = append(cmps, s.execution.comparisons()...)
	cmps = append(cmps, s.slot.comparisons()...)
	// Capture the sole proposed value before dispatch. Even a successful write
	// followed by an uncertain read must retain this exact attempt, not permit
	// another progress submission or infer the server's new revision.
	j.pendingProgress = &cleanupLocalProgress{execution: s.execution, slot: *s.slot, value: wire}
	if err = b.cleanupWrite(owner, "progress", cmps, []Write{{Key: s.execution.key, Value: []byte(wire)}}); err != nil {
		return err
	}
	observed, err := b.readCleanupKnownState(owner, s)
	if err != nil {
		return err
	}
	if observed.execution.value != wire || observed.execution.mod <= s.execution.mod {
		return ErrConflict
	}
	j.state = observed
	j.pendingProgress = nil
	return nil
}

// Joined is a post-work marker for this exact in-process run. It does not infer
// completion from a dead PID/lease. Marker bookkeeping itself is later joined
// by the original job/Backend lifetime and does not recursively need a marker.
func (b *Backend) joinCleanupRun(ctx context.Context, j *cleanupLocalJob) error {
	if ctx == nil || j == nil || j.worker == nil || j.worker.backend != b || !j.effectsJoined || j.runID == "" || j.state == nil || j.state.slot == nil || j.state.record.Run == nil || j.state.record.Run.ID != j.runID || j.state.record.Run.Joined || b.validateCleanupState(j.state) != nil {
		return ErrInvalidRecord
	}
	if err := b.reconcileCleanupJoinedState(ctx, j); err != nil {
		return err
	}
	s := j.state
	r := s.record
	run := *r.Run
	run.Joined = true
	r.Run = &run
	wire, err := encodeCleanupExecution(r)
	if err != nil {
		return err
	}
	cmps := append(b.baseComparisons(), s.execution.comparisons()...)
	cmps = append(cmps, s.slot.comparisons()...)
	projection := b.cleanupProjection("joined", cmps, taskProtoWrites([]Write{{Key: s.execution.key, Value: []byte(wire)}}), taskProtoPutResponse(1))
	if err = validateCleanupWire(projection); err != nil {
		return err
	}
	rpc, cancel := b.requestContext(ctx)
	response, err := b.client.Txn(rpc).If(cmps...).Then(clientv3.OpPut(s.execution.key, wire)).Else(clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey)).Commit()
	rpcErr := rpc.Err()
	cancel()
	if err != nil || rpcErr != nil || b.operationResponseHeader(response) != nil {
		return errors.Join(ErrOutcomeUnknown, err, rpcErr)
	}
	if !response.Succeeded {
		if _, err = b.stageEvidencePoints(response, []string{b.identityKey, b.restoreKey}); err != nil {
			return err
		}
		return ErrConflict
	}
	if !operationPutResponses(response, 1) || response.Header.Revision <= s.execution.mod || (*pb.TxnResponse)(response).Size() > maxMutationBytes {
		return ErrOutcomeUnknown
	}
	fence := s.execution
	fence.value, fence.mod = wire, response.Header.Revision
	j.state = &cleanupSlotState{record: r, execution: fence, slot: s.slot}
	j.pendingProgress = nil
	return nil
}

func (b *Backend) settleCleanupRun(ctx context.Context, j *cleanupLocalJob, proof *localOwnerReleaseProof) error {
	if ctx == nil || j == nil || j.owner == nil || j.owner.call.ctx != ctx || j.state == nil || j.evidence == nil || j.state.slot == nil || j.state.record.Run == nil || !j.state.record.Run.Joined || j.state.record.Run.ID != j.runID || !j.effectsJoined {
		return ErrInvalidRecord
	}
	if proof == nil {
		if j.detachedTarget == nil {
			return ErrInvalidRecord
		}
		if _, err := b.detachedCleanupOccupationProof(j); err != nil {
			return err
		}
	} else if proof.kv == nil || proof.record.Task != j.task || proof.record.TaskBirth != j.state.record.TaskBirth {
		return ErrInvalidRecord
	}
	return b.settleCleanupOccupation(j.owner, j, proof, j.terminal)
}

// detachedCleanupOccupationProof selects only the opaque history retained by
// this original job's shared cold workspace. A union alone is insufficient:
// the actual returned quota entry, original Task/birth and release stamp must
// all agree. This is also usable for terminal diagnostic publication after
// cancellation; effect admission remains the charged owner's responsibility.
func (b *Backend) detachedCleanupOccupationProof(j *cleanupLocalJob) (*taskOwnerReleaseProof, error) {
	if j == nil || j.worker == nil || j.worker.backend != b || j.proof != nil || j.worker.validateCleanupColdQuotaVerified(j) != nil {
		return nil, ErrInvalidRecord
	}
	s := j.worker.coldQuota
	if s == nil || s.job != j || s.history == nil || s.history.release != nil || !coldQuotaReturnHistoryOriginMatches(s.history) {
		return nil, ErrInvalidRecord
	}
	profile := s.history.profileRelease
	if profile == nil || profile.local != nil || profile.detached == nil {
		return nil, ErrInvalidRecord
	}
	d := profile.detached
	verified := j.coldQuota.verified
	if d.kv == nil || d.record.Task != j.task || d.record.TaskBirth != j.coldQuota.birth || cleanupColdQuotaReleaseStamp(d.kv) != verified.release {
		return nil, ErrInvalidRecord
	}
	return profile, nil
}

// settleCleanupOccupation shares the terminal CAS between a joined live Run
// and cold diagnosis of that exact Run. Cold callers never claim to have joined
// its physical work: the permanent Joined row and authentic release seal are
// mandatory. The original actor owns this request until native return/postcheck
// and only then completes its job; cancellation cannot release S early.
func (b *Backend) settleCleanupOccupation(owner *workerDueOwner, j *cleanupLocalJob, proof *localOwnerReleaseProof, history *cleanupTerminalEvidence) error {
	if b.cleanupColdQuotaEnabled() && (j == nil || j.worker == nil || j.worker.validateCleanupColdQuotaVerified(j) != nil) {
		return ErrInvalidRecord
	}
	if j == nil || j.worker == nil || j.worker.backend != b || j.owner != owner || owner == nil || j.state == nil || j.state.slot == nil || j.state.record.Phase != "occupied" || j.state.record.Run == nil || !j.state.record.Run.Joined || history == nil || history.record != cleanupSlotRecordFromExecution(j.state.record) || j.state.record.Task != j.task {
		return ErrInvalidRecord
	}

	// Keep the original local entry and signature. Only the same job's fully
	// authenticated detached union plus verified refund may select v2; nil
	// local proof on an ordinary caller remains invalid, never default success.
	profile := &taskOwnerReleaseProof{local: proof}
	if proof == nil {
		var err error
		profile, err = b.detachedCleanupOccupationProof(j)
		if err != nil {
			return err
		}
	} else if s := j.worker.coldQuota; s != nil && s.job == j && s.history != nil && s.history.profileRelease != nil {
		return ErrInvalidRecord
	}
	var releaseKV *mvccpb.KeyValue
	if local := profile.local; local != nil && profile.detached == nil {
		if local.kv == nil || local.record.Task != j.task || local.record.TaskBirth != j.state.record.TaskBirth {
			return ErrInvalidRecord
		}
		releaseKV = local.kv
	} else if detached := profile.detached; detached != nil && profile.local == nil {
		if detached.kv == nil || detached.record.Task != j.task || detached.record.TaskBirth != j.state.record.TaskBirth || detached.termination.Authority.Task != localRuntimeFenceStamp(history.task) {
			return ErrInvalidRecord
		}
		releaseKV = detached.kv
	} else {
		return ErrInvalidRecord
	}

	b.archiveLife.mu.Lock()
	registered := false
	for _, candidate := range j.worker.cleanupJobs {
		if candidate == j {
			registered = true
			break
		}
	}
	b.archiveLife.mu.Unlock()
	if !registered {
		return ErrInvalidRecord
	}
	s := j.state
	if err := b.validateCleanupState(s); err != nil {
		return err
	}
	_, _, releaseKey, err := b.namespace.localRuntimeReleaseKeys(j.task)
	if err != nil {
		return err
	}
	if string(releaseKV.Key) != releaseKey {
		return ErrCorruptRecord
	}
	if profile.local != nil {
		release, err := decodeLocalOwnerRelease(releaseKV)
		if err != nil || release != profile.local.record {
			return ErrCorruptRecord
		}
	} else {
		release, err := decodeDetachedOwnerRelease(releaseKV)
		if err != nil || release != profile.detached.record {
			return ErrCorruptRecord
		}
	}
	cmps, err := b.cleanupTerminalComparisons(owner, history)
	if err != nil {
		return err
	}
	r := s.record
	r.Phase, r.Confirmed, r.Pending = "settled", "settled", nil
	r.Release = &cleanupExecutionRelease{CreateRevision: releaseKV.CreateRevision, Digest: workspaceSyncIntentDigest(string(releaseKV.Value))}
	wire, err := encodeCleanupExecution(r)
	if err != nil {
		return err
	}
	cmps = append(cmps, s.execution.comparisons()...)
	cmps = append(cmps, s.slot.comparisons()...)
	cmps = append(cmps, ownTaskFence(releaseKV).comparisons()...)
	success := taskProtoResponse(nil, nil)
	success.Responses = append(success.Responses, &pb.ResponseOp{Response: &pb.ResponseOp_ResponseDeleteRange{ResponseDeleteRange: &pb.DeleteRangeResponse{Header: success.Header, Deleted: 1}}})
	success.Responses = append(success.Responses, taskProtoPutResponse(1).Responses...)
	writes := []Write{{Key: s.slot.key, Delete: true}, {Key: s.execution.key, Value: []byte(wire)}}
	if err = validateCleanupWire(b.cleanupProjection("settle", cmps, taskProtoWrites(writes), success)); err != nil {
		return err
	}
	rpc, cancel := owner.call.requestContext()
	owner.call.started = true
	response, err := b.client.Txn(rpc).If(cmps...).Then(clientv3.OpDelete(s.slot.key), clientv3.OpPut(s.execution.key, wire)).Else(clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey)).Commit()
	rpcErr := rpc.Err()
	cancel()
	if err != nil || rpcErr != nil || b.operationResponseHeader(response) != nil {
		return errors.Join(ErrOutcomeUnknown, err, rpcErr)
	}
	if !response.Succeeded {
		if _, err = b.stageEvidencePoints(response, []string{b.identityKey, b.restoreKey}); err != nil {
			return err
		}
		return ErrConflict
	}
	if len(response.Responses) != 2 || response.Responses[0] == nil || response.Responses[1] == nil || response.Responses[0].GetResponseDeleteRange() == nil || response.Responses[0].GetResponseDeleteRange().Deleted != 1 || response.Responses[1].GetResponsePut() == nil || response.Header.Revision <= s.execution.mod || (*pb.TxnResponse)(response).Size() > maxMutationBytes {
		return ErrOutcomeUnknown
	}
	if err = owner.postcheck(); err != nil {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	fence := s.execution
	fence.value, fence.mod = wire, response.Header.Revision
	j.state = &cleanupSlotState{record: r, execution: fence}
	return nil
}
