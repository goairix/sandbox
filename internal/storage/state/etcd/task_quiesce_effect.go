package etcd

import (
	"context"
	"errors"
	tr "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"
	"time"
)

// PrepareTaskQuiescence commits metadata only. It never closes a runtime or
// derives live authority from a diagnostic reference or historical intent.
func (b *Backend) PrepareTaskQuiescence(ctx context.Context, c *TaskClaim, destination *tr.Destination) (result PrepareTaskQuiescenceResult, err error) {
	result.Outcome = OutcomeUnknown
	if !b.validTaskClaim(c) {
		return result, ErrInvalidRecord
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	d := c.quiescenceDraft
	if d != nil {
		result.Reference, result.Outcome, result.AttemptReference = d.reference, d.outcome, d.attemptReference
	}
	if err = c.live(ctx); err != nil {
		return result, err
	}
	if b.taskQuiescenceIssuer == nil || b.taskQuiescenceVerifier == nil || b.authorityClock == nil || b.publicationVerifier == nil {
		return result, ErrInvalidConfiguration
	}
	if d == nil {
		task, e := taskCloseOriginalTask(c)
		if e != nil {
			return result, e
		}
		d = &taskQuiescenceDraft{task: task, deadline: c.deadline, commandID: uuid.NewString(), outcome: OutcomeUnknown}
		c.quiescenceDraft = d
	}
	defer func() {
		result.Reference, result.Outcome, result.AttemptReference = d.reference, d.outcome, d.attemptReference
	}()
	bounded, cancel := b.taskQuiescenceContext(ctx, c, d)
	defer cancel()
	// Provider errors cannot hide irreversible cancellation of this attempt.
	defer func() {
		if bounded.Err() != nil || c.parentCtx.Err() != nil {
			c.lost = true
		}
	}()
	if err = taskQuiescenceLive(bounded, c, d); err != nil {
		return result, err
	}
	if err = b.reserveTaskQuiescenceAttempt(bounded, c, d); err != nil {
		return result, err
	}
	if d.outcome == OutcomeUnknown {
		if d.reference.Stage.AttemptID != "" && d.stage == nil {
			d.outcome, err = b.ResolveStage(bounded, d.reference.Stage)
			if err != nil {
				return result, err
			}
		} else {
			if d.stage == nil {
				existing, e := b.loadTaskQuiescence(bounded, c.reference.Task, c)
				if e != nil {
					return result, e
				}
				if existing != nil {
					return result, ErrConflict
				}
				if err = b.acquireTaskQuiescencePrerequisite(bounded, c, d, destination); err != nil {
					return result, err
				}
				if err = b.signTaskQuiescence(bounded, c, d); err != nil {
					return result, err
				}
				if err = b.verifyTaskQuiescenceRuntime(bounded, c, d, destination); err != nil {
					return result, err
				}
				d.stage, err = b.beginReservedTaskQuiescenceStage(bounded, d.attemptReference.Attempt, d.reservation, time.Until(d.deadline), func(l StageAttemptLocator) (Mutation, error) { return b.buildTaskQuiescence(c, d, l) })
				if err != nil {
					var fail *stageBeginFailure
					if errors.As(err, &fail) {
						result.GuardCleanupError = fail.cleanup
						err = fail.cause
					}
					return result, err
				}
			}
			if err = b.checkTaskQuiescenceFences(bounded, c, d, nil); err != nil {
				return result, err
			}
			if err = b.verifyTaskQuiescenceTicket(bounded, c, d); err != nil {
				return result, err
			}
			if err = b.verifyTaskQuiescenceRuntime(bounded, c, d, destination); err != nil {
				return result, err
			}
			d.outcome, err = b.CommitStage(bounded, d.stage)
			if err != nil {
				return result, err
			}
		}
	}
	if d.outcome != OutcomeUnknown && d.stage != nil && !d.cleaned {
		result.GuardCleanupError = taskQuiescenceLive(bounded, c, d)
		if result.GuardCleanupError == nil {
			result.GuardCleanupError = b.ReleaseStage(bounded, d.stage)
		}
		d.cleaned = result.GuardCleanupError == nil
	}
	if d.outcome != OutcomeCommitted {
		return result, taskQuiescenceLive(bounded, c, d)
	}
	if err = b.authorizePreparedTaskQuiescence(bounded, c, d); err != nil {
		return result, err
	}
	if err = b.verifyTaskQuiescenceRuntime(bounded, c, d, destination); err != nil {
		return result, err
	}
	result.Prepared = &PreparedTaskUserQuiescence{origin: b, claim: c, draft: d}
	result.Prepared.self = result.Prepared
	return result, nil
}
func (b *Backend) buildTaskQuiescence(c *TaskClaim, d *taskQuiescenceDraft, l StageAttemptLocator) (Mutation, error) {
	if d.prior == nil || d.prior.receiptRevision <= 0 {
		return Mutation{}, ErrInvalidRecord
	}
	record := TaskQuiescenceRecord{Version: 1, Task: d.task, Claim: c.reference, Context: d.claims.Context, TicketDigest: d.ticket.Digest(), IssuerCertificateID: d.issuer.Record.CertificateID, IssuerCertificateDigest: d.issuer.Record.CertificateDigest, IssuerRevision: d.issuer.Revision, Ticket: d.ticket.Wire(), CloseDataReceipt: append([]byte(nil), d.prior.proof...), Attempt: l}
	value, err := encodeTaskQuiescenceRecord(record)
	if err != nil {
		return Mutation{}, err
	}
	key, err := b.namespace.taskQuiescenceKey(c.reference.Task)
	if err != nil {
		return Mutation{}, err
	}
	cmps := append(b.taskQuiescenceComparisons(c, d), clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	mutation, digest, err := prepareMutation(b.namespace, Mutation{Comparisons: cmps, Writes: []Write{{Key: key, Value: []byte(value)}}})
	if err != nil {
		return Mutation{}, err
	}
	d.recordValue = value
	d.reference = TaskQuiescenceReference{Task: c.reference.Task, CommandID: d.commandID, Stage: l.reference(digest)}
	return mutation, nil
}
func (b *Backend) authorizePreparedTaskQuiescence(ctx context.Context, c *TaskClaim, d *taskQuiescenceDraft) error {
	if err := taskQuiescenceLive(ctx, c, d); err != nil {
		return err
	}
	entry, err := b.loadTaskQuiescence(ctx, c.reference.Task, c)
	if err != nil {
		return err
	}
	if entry == nil || entry.Record == nil || entry.Outcome != OutcomeCommitted || entry.Reference != d.reference {
		return ErrCorruptRecord
	}
	value, err := encodeTaskQuiescenceRecord(*entry.Record)
	if err != nil || value != d.recordValue {
		return ErrCorruptRecord
	}
	if err = b.verifyTaskQuiescenceTicket(ctx, c, d); err != nil {
		return err
	}
	key, err := b.namespace.taskQuiescenceKey(c.reference.Task)
	if err != nil {
		return err
	}
	fence := taskFence{key: key, value: value, create: entry.Revision, mod: entry.Revision}
	if err = b.checkTaskQuiescenceFences(ctx, c, d, &fence); err != nil {
		return err
	}
	return b.verifyTaskQuiescenceTicket(ctx, c, d)
}

func (b *Backend) acquireTaskQuiescencePrerequisite(ctx context.Context, c *TaskClaim, d *taskQuiescenceDraft, destination *tr.Destination) error {
	if d.prior != nil {
		return b.checkTaskQuiescenceFences(ctx, c, d, nil)
	}
	if err := b.checkTaskQuiescenceFences(ctx, c, d, nil); err != nil {
		return err
	}
	prior, err := b.loadQuiescencePrerequisite(ctx, c.reference.Task, c)
	if err != nil {
		return err
	}
	if err = taskQuiescenceLive(ctx, c, d); err != nil {
		return err
	}
	if prior.entry.Record.Task != d.task || prior.entry.Record.Context.ControlRevision != c.birth {
		return ErrConflict
	}
	if err = b.checkTaskQuiescenceFences(ctx, c, d, nil); err != nil {
		return err
	}
	proof, err := b.QueryTaskCloseData(ctx, prior.entry.Reference, destination)
	if liveErr := taskQuiescenceLive(ctx, c, d); liveErr != nil {
		return liveErr
	}
	if err != nil {
		return err
	}
	if err = b.checkTaskQuiescenceFences(ctx, c, d, nil); err != nil {
		return err
	}
	if proof.Receipt == nil {
		return ErrCorruptReceipt
	}
	checked, err := b.loadQuiescencePrerequisite(ctx, c.reference.Task, c)
	if err != nil {
		return err
	}
	if err = taskQuiescenceLive(ctx, c, d); err != nil {
		return err
	}
	if checked.intent != prior.intent || checked.receiptKey != prior.receiptKey || checked.receiptRevision != prior.receiptRevision || checked.entry.Reference != prior.entry.Reference {
		return ErrConflict
	}
	// Only consume the authenticated historical receipt after current claim and
	// coherent permanent prerequisite checks; it never reconstructs the old claim.
	checked.proof = proof.Receipt.Wire()
	d.prior = checked
	return b.checkTaskQuiescenceFences(ctx, c, d, nil)
}

func (b *Backend) verifyTaskQuiescenceRuntime(ctx context.Context, c *TaskClaim, d *taskQuiescenceDraft, destination *tr.Destination) error {
	if err := taskQuiescenceLive(ctx, c, d); err != nil {
		return err
	}
	now, err := b.observePublicationClock(ctx)
	if err != nil {
		return err
	}
	if err = taskQuiescenceLive(ctx, c, d); err != nil {
		return err
	}
	if err = destination.VerifyTaskCloseRuntime(b.taskQuiescenceVerifier, d.claims.Context.Current, d.claims.NotBefore, d.claims.NotAfter, now); err != nil {
		return err
	}
	return taskQuiescenceLive(ctx, c, d)
}
