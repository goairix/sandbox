package etcd

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// A job has one metadata actor and, after admission, one physical goroutine.
// The actor alone changes state/bundle/step. A physical continuation transfers
// ownership through a buffered reply; cancellation never detaches its receiver.
type cleanupLocalJob struct {
	worker     *DueWorker
	task       TaskReference
	partition  *WorkerPartitionClaim
	ctx        context.Context
	cancel     context.CancelFunc
	stopCaller func()
	done       chan struct{}
	result     CleanupLocalResult
	err        error

	step      string
	state     *cleanupSlotState
	bundle    *taskClaimBundle
	evidence  *cleanupSlotEvidence
	target    *cleanupLocalTarget
	inventory *cleanupLocalWireInventory
	claim     *TaskClaim
	runID     string
	rotations uint32
	proof     *localOwnerReleaseProof
	owner     *workerDueOwner
	terminal  *cleanupTerminalEvidence

	// At most one unresolved progress submission is retained. Its owned bytes
	// and fences let this same registered job resolve a lost ACK before joining;
	// they cannot authorize a cold job or another progress attempt.
	pendingProgress *cleanupLocalProgress

	// Recovery before or after local completion reuses the same bound archive,
	// registry job and physical goroutine.
	// Only its completed manifest preflight publishes the finite coordinates;
	// the actor then owns the single new-Run CAS and original Claim pipeline.
	boundRecovery bool
	boundArchive  *cleanupExecutionArchive

	// Early history is loaded and queried by this same job without a native
	// owner. Its exact original E/Pending/S survives until the new-Run CAS.
	earlyRecovery bool
	early         *cleanupEarlyRecovery

	// A completed local sync may resume only the already reserved observer
	// request. This copied fence is checked again by the real termination read;
	// it never grants a new Terminate send or substitutes for its signed proof.
	terminationReservation *taskFence

	// The alternative completed mode comes only from authenticated terminal
	// history. It resumes owner release without observer or provider work.
	terminationComplete *cleanupCompletedTermination

	// started/request and registry membership use archiveLife.mu. The physical
	// owner sets effectsJoined only after all transports/files/renewal/claims
	// return, before requesting post-work metadata settlement.
	started       bool
	request       *cleanupLocalContinuation
	effectsJoined bool
}

type cleanupLocalContinuation struct {
	kind      string
	confirmed string
	pending   *cleanupExecutionPending
	archive   *cleanupExecutionArchive
	reply     chan cleanupLocalReply
}

type cleanupLocalReply struct {
	claim *TaskClaim
	err   error
}

// ExecuteCleanupLocal executes one explicit Task using this actor's original
// partition/session/Claim shares. Caller cancellation cancels and joins the
// original job; it never frees a durable slot or abandons an accepted RPC.
func (w *DueWorker) ExecuteCleanupLocal(ctx context.Context, task TaskReference) (CleanupLocalResult, error) {
	if w == nil || w.self != w || ctx == nil || w.backend.cleanupPhysicalProfile() != nil {
		return CleanupLocalResult{Task: task, Outcome: OutcomeUnknown}, ErrInvalidConfiguration
	}
	job, err := w.enqueueCleanupLocal(ctx, task)
	if err != nil {
		return CleanupLocalResult{Task: task, Outcome: OutcomeUnknown}, err
	}
	<-job.done
	return job.result, job.err
}

func (w *DueWorker) wakeCleanup() {
	select {
	case w.cleanupWake <- struct{}{}:
	default:
	}
}

func (w *DueWorker) enqueueCleanupLocal(ctx context.Context, task TaskReference) (*cleanupLocalJob, error) {
	if w == nil || w.self != w || ctx == nil || w.backend.validateTaskReference(task) != nil || w.backend.localCleanup == nil || w.backend.localCleanup.MaxActive < 1 || w.backend.localCleanup.MaxActive > 32 {
		return nil, ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b := w.backend
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if b.archiveLife.closed || w.ctx.Err() != nil || w.cleanupSealed {
		return nil, context.Canceled
	}
	var partition *WorkerPartitionClaim
	for _, scan := range w.scans {
		if scan.partition.partition == task.Partition {
			partition = scan.partition
		}
	}
	if partition == nil || partition.released || partition.lost || partition.release != nil {
		return nil, ErrGuardExpired
	}
	index := -1
	for i := uint32(0); i < b.localCleanup.MaxActive; i++ {
		if w.cleanupJobs[i] == nil {
			index = int(i)
		} else if w.cleanupJobs[i].task == task {
			return nil, ErrConflict
		}
	}
	if index < 0 {
		return nil, ErrConflict
	}
	timeout := b.localCleanup.JobTimeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	owned, cancel := context.WithTimeout(w.ctx, timeout)
	job := &cleanupLocalJob{worker: w, task: task, partition: partition, ctx: owned, cancel: cancel, done: make(chan struct{}), step: "observe", result: CleanupLocalResult{Task: task, Outcome: OutcomeUnknown}}
	callerDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(callerDone)
		cancel()
		w.wakeCleanup()
	})
	job.stopCaller = func() {
		if !stop() {
			<-callerDone
		}
	}
	w.cleanupJobs[index] = job
	w.wakeCleanup()
	return job, nil
}

// Each actor turn spends one original Claim credit, even for metadata-only
// rejection. A 100-share budget therefore does not mean 100 physical tasks/s.
// Original reads/writes are split at explicit state boundaries, never 32 slots
// in one call. A live owner finishes before the next due or renewal operation.
func (w *DueWorker) cleanupTurn() (time.Time, error) {
	b := w.backend
	b.archiveLife.mu.Lock()
	var job *cleanupLocalJob
	for n := 0; n < len(w.cleanupJobs); n++ {
		i := (w.cleanupNext + n) % len(w.cleanupJobs)
		candidate := w.cleanupJobs[i]
		if candidate != nil && (!candidate.started || candidate.request != nil) {
			job = candidate
			w.cleanupNext = (i + 1) % len(w.cleanupJobs)
			break
		}
	}
	b.archiveLife.mu.Unlock()
	if job == nil {
		return time.Time{}, nil
	}
	if err := job.ctx.Err(); err != nil {
		w.cleanupTurnFailed(job, err)
		return time.Time{}, nil
	}
	share, at := w.readyClaim(time.Now())
	if share == nil {
		return at, nil
	}
	owner, stop, err := b.beginExpiredTaskOwner(job.ctx, job.partition, share)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			return time.Now().Add(100 * time.Millisecond), nil
		}
		w.cleanupTurnFailed(job, err)
		return time.Time{}, nil
	}
	job.owner = owner
	err = b.runCleanupAdmission(owner, job)
	if err == nil {
		err = owner.postcheck()
	}
	if err != nil && job.step == "claimed" && job.claim != nil {
		// A successful Grant pipeline can lose its original owner at the
		// final publication check. Revoke is the fourth bounded native RPC
		// and is joined before this charged call is released.
		err = errors.Join(err, b.ReleaseTaskClaim(owner.call.ctx, job.claim))
		job.claim = nil
	}
	// Actual native cleanup is already complete before releasing this owner.
	job.owner = nil
	owner.finish()
	stop()
	if err != nil {
		w.cleanupTurnFailed(job, err)
	} else {
		w.cleanupTurnCompleted(job)
	}
	return time.Now(), nil
}

func (w *DueWorker) cleanupTurnFailed(job *cleanupLocalJob, err error) {
	job.inventory = nil
	b := w.backend
	b.archiveLife.mu.Lock()
	request, started := job.request, job.started
	job.request = nil
	b.archiveLife.mu.Unlock()
	if started {
		if request != nil {
			// A later metadata failure may still own the already confirmed
			// rollover Claim. Transfer it back even with an error so the job
			// joins its real cleanup instead of overwriting it with nil.
			request.reply <- cleanupLocalReply{claim: job.claim, err: err}
		}
		return
	}
	b.finishCleanupJob(job, err)
}

func (w *DueWorker) cleanupTurnCompleted(job *cleanupLocalJob) {
	if job.step == "done" {
		w.backend.finishCleanupJob(job, nil)
		return
	}
	if job.step != "physical" && job.step != "replied" {
		return
	}
	b := w.backend
	b.archiveLife.mu.Lock()
	request := job.request
	job.request = nil
	start := !job.started
	if start {
		job.started = true
	}
	b.archiveLife.mu.Unlock()
	if request != nil {
		request.reply <- cleanupLocalReply{claim: job.claim}
	}
	if start {
		go b.runCleanupLocal(job)
	}
}

func (w *DueWorker) cleanupContinuation(job *cleanupLocalJob, request *cleanupLocalContinuation) (cleanupLocalReply, error) {
	if job == nil || job.worker != w || request == nil {
		return cleanupLocalReply{}, ErrInvalidRecord
	}
	b := w.backend
	b.archiveLife.mu.Lock()
	if w.cleanupSealed || w.ctx.Err() != nil || job.ctx.Err() != nil || !job.started || job.request != nil {
		b.archiveLife.mu.Unlock()
		return cleanupLocalReply{}, ErrGuardExpired
	}
	request.reply = make(chan cleanupLocalReply, 1)
	job.request = request
	switch request.kind {
	case "claim":
		job.step = "rollover"
	case "resume-bound":
		job.step = "recovery-run"
	case "resume-early":
		job.step = "early-run"
	case "settle":
		job.step = "release-proof"
	default:
		job.step = "progress"
	}
	b.archiveLife.mu.Unlock()
	w.wakeCleanup()
	// No ctx.Done select: once accepted, the actor owes an actual reply after
	// its native call and failed-Grant cleanup finish, including during Stop.
	reply := <-request.reply
	return reply, reply.err
}

func (w *DueWorker) requestCleanupClaim(job *cleanupLocalJob) (*TaskClaim, error) {
	if job == nil || job.worker != w {
		return nil, ErrInvalidRecord
	}
	w.backend.archiveLife.mu.Lock()
	started := job.started
	w.backend.archiveLife.mu.Unlock()
	if !started {
		return nil, ErrInvalidRecord
	}
	reply, err := w.cleanupContinuation(job, &cleanupLocalContinuation{kind: "claim"})
	return reply.claim, err
}

func (b *Backend) runCleanupAdmission(owner *workerDueOwner, j *cleanupLocalJob) (err error) {
	if _, err = b.cleanupOwnerComparisons(owner); err != nil {
		return err
	}
	ctx := owner.call.ctx
	owner.call.started = true
	switch j.step {
	case "observe":
		j.state, err = b.readCleanupSlot(owner, j.task)
		if err != nil {
			return err
		}
		if j.state != nil && j.state.record.Phase == "settled" {
			j.step = "release-proof"
			return nil
		}
		if j.state != nil && j.state.record.Run != nil {
			// A cold Run is never this process's work. Only an already joined
			// Run may query the original release seal; this does not start a
			// new Run or require the three ownership records release deleted.
			if j.state.record.Run.Joined {
				j.step = "release-proof"
			} else {
				j.result.Reason = "retained-run"
				j.step = "done"
			}
			return nil
		}
		j.step = "bundle"
	case "bundle", "claim-bundle":
		j.bundle, err = b.loadCleanupClaimBundle(owner, j.task)
		if j.step == "bundle" {
			j.step = "birth"
		} else {
			j.step = "claim-state"
		}
	case "birth":
		j.evidence, err = b.loadCleanupSlotEvidence(owner, j.bundle)
		j.step = "snapshot"
	case "snapshot":
		j.target, err = b.cleanupSnapshot(ctx, j.bundle.task)
		j.step = "publication"
	case "publication":
		err = b.cleanupPublication(ctx, j.bundle.task, j.target)
		j.step = "issuer"
	case "issuer":
		err = b.cleanupIssuer(ctx, j.target)
		if j.boundRecovery || j.earlyRecovery {
			// Start the same owned goroutine before obtaining a native owner
			// for new-Run admission: manifest/tar validation can perform IO.
			j.step = "physical"
		} else if j.state == nil {
			j.step = "initialize"
		} else if j.state.record.Phase == "selecting" {
			j.step = "reserve"
		} else {
			j.step = "run"
		}
	case "initialize":
		err = b.initializeCleanupSlot(owner, j.evidence)
		if err == nil {
			j.state, err = b.readCleanupSlot(owner, j.task)
		}
		j.step = "reserve"
	case "reserve":
		err = b.reserveCleanupSlot(owner, j.evidence, j.state)
		if errors.Is(err, ErrConflict) && !errors.Is(err, ErrOutcomeUnknown) {
			j.step = "rotate"
			return nil
		}
		if err == nil {
			j.state, err = b.readCleanupKnownState(owner, j.state)
		}
		j.step = "run"
	case "rotate":
		if j.rotations >= 31 {
			return ErrConflict
		}
		err = b.rotateCleanupSlot(owner, j.state)
		if err == nil {
			j.state, err = b.readCleanupSlot(owner, j.task)
		}
		j.rotations++
		j.step = "reserve"
	case "run":
		worker, e := b.localCleanup.Worker.WorkspaceSyncWorkerReference()
		if e != nil {
			return e
		}
		j.runID = uuid.NewString()
		r := j.state.record
		r.Run = &cleanupExecutionRun{ID: j.runID, Worker: worker}
		r.Pending = &cleanupExecutionPending{Purpose: "claim", ClaimID: uuid.NewString()}
		err = b.updateCleanupRun(owner, j, r)
		j.step = "claim-bundle"
	case "rollover":
		if j.state == nil || j.state.record.Confirmed != "archive-bound" || j.state.record.Archive == nil || j.claim != nil {
			return ErrInvalidRecord
		}
		r := j.state.record
		r.Pending = &cleanupExecutionPending{Purpose: "claim", ClaimID: uuid.NewString()}
		err = b.updateCleanupRun(owner, j, r)
		j.step = "claim-bundle"
	case "recovery-run":
		err = b.beginCleanupRecoveryRun(owner, j)
		j.step = "claim-bundle"
	case "early-run":
		err = b.beginCleanupEarlyRun(owner, j)
		j.step = "claim-bundle"
	case "termination-history":
		// The original loader authenticates reserved or completed history and
		// fixed historical points in two RPCs, alone in this charged turn.
		err = b.loadCleanupTerminationRecovery(ctx, j)
		if err == nil {
			j.boundRecovery = true
			j.step = "bundle"
		}
	case "claim-state":
		j.state, err = b.readCleanupClaimState(owner, j.bundle, j.state)
		if err == nil {
			j.inventory, err = b.prepareCleanupLocalWireInventory(cleanupLocalWireInput{bundle: j.bundle, state: j.state, snapshot: j.target.snapshot, publication: j.target.publication, issuer: j.target.issuer, plan: j.target.plan, workerID: j.worker.options.WorkerID, claimTTL: b.localCleanup.ClaimTTL}, b.localCleanup.Worker)
		}
		j.step = "claim"
	case "claim":
		j.claim, err = b.claimCleanupTask(owner, j.ctx, j.bundle, j.state, j.inventory, j.worker.options.WorkerID, b.localCleanup.ClaimTTL)
		// The complete future-cap protobuf plan is admission-only. Its latch
		// cannot be retried, including after an unknown Grant response.
		j.inventory = nil
		j.step = "claimed"
	case "claimed":
		r := j.state.record
		if r.Confirmed == "reserved" {
			r.Confirmed = "claimed"
		}
		r.Pending = nil
		err = b.updateCleanupRun(owner, j, r)
		j.step = "physical"
	case "progress":
		r := j.state.record
		request := j.request
		if request.confirmed != "" {
			r.Confirmed = request.confirmed
		}
		r.Pending = request.pending
		if request.archive != nil {
			r.Archive = request.archive
		}
		err = b.updateCleanupRun(owner, j, r)
		j.step = "replied"
	case "release-proof":
		// The existing authenticating loader has three native reads when a
		// release exists. It is the sole operation in this charged turn.
		j.proof, err = b.readLocalOwnerReleaseProof(ctx, j.task)
		if err != nil {
			return err
		}
		if j.state.record.Phase == "settled" {
			if j.proof == nil {
				return ErrConflict
			}
			if j.state.record.Release == nil || j.proof.kv.CreateRevision != j.state.record.Release.CreateRevision || workspaceSyncIntentDigest(string(j.proof.kv.Value)) != j.state.record.Release.Digest {
				return ErrCorruptRecord
			}
			j.step = "done"
		} else if j.proof == nil {
			if j.started {
				return ErrConflict
			}
			if j.state.record.Confirmed == "archive-bound" && j.state.record.Archive != nil {
				j.boundRecovery = true
				j.step = "bundle"
			} else if (j.state.record.Confirmed == "local-complete" || j.state.record.Confirmed == "terminated") && j.state.record.Archive != nil {
				j.step = "termination-history"
			} else if cleanupEarlyPhase(j.state.record.Confirmed) && j.state.record.Archive == nil {
				j.earlyRecovery = true
				j.step = "bundle"
			} else {
				j.result.Reason = "missing-release-proof"
				j.step = "done"
			}
		} else {
			j.step = "terminal-history"
		}
	case "terminal-history":
		// Four permanent points occupy their own charged turn. Live and cold
		// settlement share this proof without reconstructing a claim bundle.
		j.terminal, err = b.loadCleanupTerminalEvidence(owner, j.state)
		j.step = "settle"
	case "settle":
		if j.started {
			err = b.settleCleanupRun(ctx, j, j.proof)
			j.step = "replied"
		} else {
			err = b.settleCleanupOccupation(owner, j, j.proof, j.terminal)
			j.step = "done"
		}
	default:
		return ErrInvalidRecord
	}
	return err
}
