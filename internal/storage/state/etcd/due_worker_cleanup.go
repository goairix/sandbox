package etcd

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// A job has one metadata actor and one preparation/physical goroutine.
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

	step           string
	state          *cleanupSlotState
	bundle         *taskClaimBundle
	evidence       *cleanupSlotEvidence
	target         *cleanupLocalTarget
	inventory      *cleanupLocalWireInventory
	claim          *TaskClaim
	runID          string
	rotations      uint32
	proof          *localOwnerReleaseProof
	owner          *workerDueOwner
	terminal       *cleanupTerminalEvidence
	detachedTarget *cleanupDetachedTarget
	// The explicit public local API cannot opt into the private mixed actor
	// profile. This immutable routing restriction is installed before enqueue.
	localOnly bool

	// At most one unresolved progress submission is retained. Its owned bytes
	// and fences let this same registered job resolve a lost ACK before joining;
	// they cannot authorize a cold job or another progress attempt.
	pendingProgress *cleanupLocalProgress

	// The original physical owner retains one detached metadata submission.
	// Unknown never replaces its Claim/Run/attempt. Its compact pins and wire
	// are cleared at the actual finish tail, rather than retaining source graphs.
	detachedNoEffect *cleanupDetachedNoEffectDraft
	// A Joined native Run permits only this original job's single replacement
	// candidate. Unknown retains it until the actor replies and is reconciled.
	detachedRecovery *cleanupDetachedNoEffectRecovery

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

	// started/request and registry membership use archiveLife.mu. The same
	// goroutine owns target preparation before admission and physical work after
	// its accepted admit continuation returns. Preparation never owns a native
	// call and remains registered until actual return, even during cancellation.
	// The physical owner sets effectsJoined after all transports/files/renewal/claims
	// return, before requesting post-work metadata settlement.
	started       bool
	request       *cleanupLocalContinuation
	effectsJoined bool
	// Only compact, original-job quota attribution survives actor yields.
	// All heavy cold history and prepared/permit graphs belong to worker.coldQuota.
	coldQuota cleanupColdQuotaJob
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
	job, err := w.enqueueCleanupLocalProfile(ctx, task, true)
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
	return w.enqueueCleanupLocalProfile(ctx, task, false)
}

func (w *DueWorker) enqueueCleanupLocalProfile(ctx context.Context, task TaskReference, localOnly bool) (*cleanupLocalJob, error) {
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
	job := &cleanupLocalJob{worker: w, task: task, partition: partition, ctx: owned, cancel: cancel, done: make(chan struct{}), step: "observe", result: CleanupLocalResult{Task: task, Outcome: OutcomeUnknown}, localOnly: localOnly}
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

// Ordinary cleanup admission spends one original Claim credit per turn. Cold
// metadata instead pays Range only, and cold mutations pay both original
// shares. Original reads/writes remain split at explicit state boundaries,
// never 32 slots in one call. Every native call joins before due or renewal.
func (w *DueWorker) cleanupTurn() (time.Time, error) {
	b := w.backend
	b.archiveLife.mu.Lock()
	var job *cleanupLocalJob
	for n := 0; n < len(w.cleanupJobs); n++ {
		i := (w.cleanupNext + n) % len(w.cleanupJobs)
		candidate := w.cleanupJobs[i]
		// A cold waiter cannot advance while another job owns the workspace.
		// Skip it in this bounded original scan, preserving round robin among
		// the workspace owner and ordinary physical metadata continuations.
		if candidate != nil && w.coldQuota != nil && candidate != w.coldQuota.job && cleanupColdQuotaPhase(candidate.step) {
			continue
		}
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
	ready, sourceErr := w.acquireCleanupManagedSource(job)
	if sourceErr != nil {
		w.cleanupTurnFailed(job, sourceErr)
		return time.Time{}, nil
	}
	if !ready {
		return time.Now().Add(100 * time.Millisecond), nil
	}
	if b.cleanupColdQuotaEnabled() {
		if cleanupColdQuotaPhase(job.step) {
			return w.cleanupColdQuotaTurn(job)
		}
		// The original paid publication phase must seal cold provenance before
		// wire preflight or any execution occupation/Run mutation can begin.
		switch job.step {
		case "wire-preflight", "initialize", "reserve", "run", "recovery-run", "early-run":
			if err := w.validateCleanupColdQuotaOrigin(job); err != nil {
				w.cleanupTurnFailed(job, err)
				return time.Time{}, nil
			}
		}
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
	if err != nil || job.step != "detached-target" {
		w.releaseCleanupManagedSource(job)
	}
	if err != nil {
		w.cleanupTurnFailed(job, err)
	} else {
		w.cleanupTurnCompleted(job)
	}
	return time.Now(), nil
}

func (w *DueWorker) cleanupTurnFailed(job *cleanupLocalJob, err error) {
	job.inventory = nil
	// The serial caller has already joined its actual native call. A physical
	// continuation is still owed its reply; releasing cold metadata never joins
	// or detaches that separate original goroutine.
	w.releaseCleanupColdQuota(job)
	w.releaseCleanupManagedSource(job)
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
	if job.step == "detached-target" {
		if err := w.beginCleanupDetachedPreparation(job); err != nil {
			w.cleanupTurnFailed(job, err)
			return
		}
		go w.backend.runCleanupDetachedPreparation(job)
		return
	}
	if job.step == "done" {
		w.backend.finishCleanupJob(job, nil)
		return
	}
	if job.step != "target" && job.step != "physical" && job.step != "replied" {
		return
	}
	b := w.backend
	if job.step == "replied" && job.detachedTarget != nil && job.effectsJoined {
		// The terminal CAS and refund have returned. Copy the bounded result
		// while this actor still owns its opaque cold proof, then retire the
		// heavy history before handing control back to the physical tail.
		job.result = b.cleanupResult(job)
		w.releaseCleanupColdQuota(job)
	}
	b.archiveLife.mu.Lock()
	request := job.request
	job.request = nil
	// Only target starts the job, after cleanupTurn released the real owner.
	// Later physical/replied transitions hand the same goroutine its reply.
	start := job.step == "target" && !job.started
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
	case "admit":
		job.step = "publication"
	case "claim":
		job.step = "rollover"
	case "claim-detached":
		if job.detachedTarget == nil || job.state == nil || job.state.record.Run == nil || job.state.record.Run.Joined || job.state.record.Run.ID != job.runID || job.state.record.Pending == nil || job.state.record.Pending.Purpose != "claim" || job.claim != nil {
			job.request = nil
			b.archiveLife.mu.Unlock()
			return cleanupLocalReply{}, ErrInvalidRecord
		}
		job.step = "claim-bundle"
	case "resume-bound":
		job.step = "recovery-run"
	case "resume-early":
		job.step = "early-run"
	case "resume-detached-noeffect":
		job.step = "detached-recovery-run"
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
	if request.kind == "resume-detached-noeffect" && reply.err == nil {
		reply.err = b.acceptCleanupDetachedRecoveryReply(job)
	}
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
		if handled, routeErr := j.worker.routeCleanupDetachedQuotaOccupation(j); handled {
			return routeErr
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
		if b.detachedCleanup != nil {
			var detached bool
			detached, err = b.cleanupManagedSource(ctx, j)
			if err != nil || detached {
				return err
			}
		}
		j.target, err = b.cleanupSnapshot(ctx, j.bundle.task)
		j.step = "target"
	case "publication":
		err = b.cleanupPublication(ctx, j.bundle.task, j.target)
		j.step = "issuer"
		if err == nil && b.cleanupColdQuotaEnabled() {
			j.step = "cold-origin"
		}
	case "issuer":
		err = b.cleanupIssuer(ctx, j.target)
		j.step = "wire-preflight"
	case "wire-preflight":
		// This complete pure plan is discarded before any permanent E/S/Run
		// mutation. Actual Grant admission later rebuilds and binds it to its
		// genuine Kernel and exact current execution/slot/Claim coordinates.
		if j.detachedTarget != nil {
			_, err = b.visitCleanupDetachedWire(b.cleanupDetachedInput(j), cleanupLocalWireDimensions{runID: "11111111-1111-4111-8111-111111111111", claimID: "22222222-2222-4222-8222-222222222222", worker: j.target.worker}, func(cleanupDetachedWireEmission) error { return nil })
		} else {
			err = b.preflightCleanupLocalWire(cleanupLocalWireInput{bundle: j.bundle, snapshot: j.target.snapshot, publication: j.target.publication, issuer: j.target.issuer, plan: j.target.plan, workerID: j.worker.options.WorkerID, claimTTL: b.localCleanup.ClaimTTL}, j.target.worker)
		}
		if j.boundRecovery || j.earlyRecovery {
			// Reply to the same owned goroutine for authentic recovery IO;
			// its next continuation admits the one new Run and managed Claim.
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
	case "detached-recovery-run":
		// This finite private gate only creates/reconciles the metadata Run.
		// Actual managed Claim admission remains outside this Task3 component.
		err = b.beginCleanupDetachedNoEffectRecoveryRun(owner, j)
		j.step = "physical"
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
			if j.detachedTarget != nil {
				j.inventory, err = b.prepareCleanupDetachedInventory(j)
			} else {
				j.inventory, err = b.prepareCleanupLocalWireInventory(cleanupLocalWireInput{bundle: j.bundle, state: j.state, snapshot: j.target.snapshot, publication: j.target.publication, issuer: j.target.issuer, plan: j.target.plan, workerID: j.worker.options.WorkerID, claimTTL: b.localCleanup.ClaimTTL}, b.localCleanup.Worker)
			}
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
		if j.detachedTarget != nil {
			// This live physical owner has joined its Claim and Run. The shared
			// profile reader authenticates the permanent v2 seal without a local
			// archive DTO; refund and settlement still use the original paid
			// history cursor and the same registered job below.
			var proof *taskOwnerReleaseProof
			proof, err = b.readTaskOwnerReleaseProof(ctx, j.task, nil)
			if err != nil {
				return err
			}
			if proof == nil || proof.detached == nil || proof.local != nil {
				return ErrOutcomeUnknown
			}
			j.step = "terminal-history"
			return nil
		}
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
			if b.cleanupColdQuotaEnabled() {
				j.step = "cold-quota-history"
			}
		} else if j.proof == nil {
			if j.started {
				return ErrConflict
			}
			if b.detachedCleanup != nil && j.state.record.Archive == nil && (j.state.record.Confirmed == "reserved" || j.state.record.Confirmed == "drained" || j.state.record.Confirmed == "terminated" || j.state.record.Confirmed == "owner-released") {
				// This only routes to the real snapshot/source classifier. A local
				// snapshot retains its old stricter early recovery checks.
				j.earlyRecovery = true
				j.step = "bundle"
			} else if j.state.record.Confirmed == "archive-bound" && j.state.record.Archive != nil {
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
		if err == nil && b.cleanupColdQuotaEnabled() {
			if s := j.worker.coldQuota; j.coldQuota.quotaOnly && s != nil && s.job == j && s.history != nil && s.history.profileRelease != nil {
				// This same job already authenticated its refund. Retain its
				// opaque proof for settlement instead of looping through refund.
				_, err = b.detachedCleanupOccupationProof(j)
			} else {
				j.step = "cold-quota-history"
			}
		}
	case "settle":
		if j.started {
			err = b.settleCleanupRun(ctx, j, j.proof)
			j.step = "replied"
		} else {
			err = b.settleCleanupOccupation(owner, j, j.proof, j.terminal)
			j.step = "done"
			if err == nil && j.coldQuota.quotaOnly && j.worker.coldQuota != nil && j.worker.coldQuota.job == j && j.worker.coldQuota.history != nil && j.worker.coldQuota.history.profileRelease != nil {
				// Keep the original registry identity through fresh birth/proof
				// and P deletion. Settlement alone cannot finish discovery work.
				j.step = "cold-quota-pending-birth"
			}
		}
	default:
		return ErrInvalidRecord
	}
	return err
}
