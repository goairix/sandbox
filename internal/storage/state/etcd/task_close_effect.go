package etcd

import (
	"context"
	"errors"
	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"
	"time"
)

// PrepareTaskCloseData commits metadata only. It never closes a runtime or
// derives live authority from a diagnostic reference or historical intent.
func (b *Backend) PrepareTaskCloseData(ctx context.Context, c *TaskClaim) (result PrepareTaskCloseDataResult, err error) {
	result.Outcome = OutcomeUnknown
	if !b.validTaskClaim(c) {
		return result, ErrInvalidRecord
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	d := c.closeDraft
	if d != nil {
		result.Reference, result.Outcome = d.reference, d.outcome
	}
	if err = c.live(ctx); err != nil {
		return result, err
	}
	if b.taskIssuer == nil || b.taskVerifier == nil || b.authorityClock == nil || b.publicationVerifier == nil {
		return result, ErrInvalidConfiguration
	}
	if d == nil {
		task, e := taskCloseOriginalTask(c)
		if e != nil {
			return result, e
		}
		d = &taskCloseDraft{task: task, deadline: c.deadline, commandID: uuid.NewString(), outcome: OutcomeUnknown}
		c.closeDraft = d
	}
	defer func() { result.Reference, result.Outcome = d.reference, d.outcome }()
	bounded, cancel := b.taskCloseContext(ctx, c, d)
	defer cancel()
	// Provider errors cannot hide irreversible cancellation of this attempt.
	defer func() {
		if bounded.Err() != nil || c.parentCtx.Err() != nil {
			c.lost = true
		}
	}()
	if err = taskCloseLive(bounded, c, d); err != nil {
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
				existing, e := b.LoadTaskCloseData(bounded, c.reference.Task)
				if e != nil {
					return result, e
				}
				if existing != nil {
					return result, ErrConflict
				}
				if err = b.signTaskClose(bounded, c, d); err != nil {
					return result, err
				}
				d.stage, err = b.beginStageWithBuilder(bounded, c.reference.Task.Partition, c.reference.ClaimID, "task_close_data", time.Until(d.deadline), func(l StageAttemptLocator) (Mutation, error) { return b.buildTaskClose(c, d, l) })
				if err != nil {
					var fail *stageBeginFailure
					if errors.As(err, &fail) {
						result.GuardCleanupError = fail.cleanup
						err = fail.cause
					}
					return result, err
				}
			}
			if err = b.checkTaskCloseFences(bounded, c, d, nil); err != nil {
				return result, err
			}
			if err = b.verifyTaskCloseTicket(bounded, c, d); err != nil {
				return result, err
			}
			d.outcome, err = b.CommitStage(bounded, d.stage)
			if err != nil {
				return result, err
			}
		}
	}
	if d.outcome != OutcomeUnknown && d.stage != nil && !d.cleaned {
		result.GuardCleanupError = taskCloseLive(bounded, c, d)
		if result.GuardCleanupError == nil {
			result.GuardCleanupError = b.ReleaseStage(bounded, d.stage)
		}
		d.cleaned = result.GuardCleanupError == nil
	}
	if d.outcome != OutcomeCommitted {
		return result, taskCloseLive(bounded, c, d)
	}
	if err = b.authorizePreparedTaskClose(bounded, c, d); err != nil {
		return result, err
	}
	result.Prepared = &PreparedTaskCloseData{origin: b, claim: c, draft: d}
	result.Prepared.self = result.Prepared
	return result, nil
}
func (b *Backend) buildTaskClose(c *TaskClaim, d *taskCloseDraft, l StageAttemptLocator) (Mutation, error) {
	record := TaskCloseDataRecord{Version: 1, Task: d.task, Claim: c.reference, Context: d.claims.Context, TicketDigest: d.ticket.Digest(), IssuerCertificateID: d.issuer.Record.CertificateID, IssuerCertificateDigest: d.issuer.Record.CertificateDigest, IssuerRevision: d.issuer.Revision, Ticket: d.ticket.Wire(), Attempt: l}
	value, err := encodeTaskCloseDataRecord(record)
	if err != nil {
		return Mutation{}, err
	}
	key, err := b.namespace.taskCloseDataKey(c.reference.Task)
	if err != nil {
		return Mutation{}, err
	}
	cmps := append(b.taskCloseComparisons(c, d), clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	mutation, digest, err := prepareMutation(b.namespace, Mutation{Comparisons: cmps, Writes: []Write{{Key: key, Value: []byte(value)}}})
	if err != nil {
		return Mutation{}, err
	}
	d.recordValue = value
	d.reference = TaskCloseDataReference{Task: c.reference.Task, CommandID: d.commandID, Stage: l.reference(digest)}
	return mutation, nil
}
func (b *Backend) authorizePreparedTaskClose(ctx context.Context, c *TaskClaim, d *taskCloseDraft) error {
	if err := taskCloseLive(ctx, c, d); err != nil {
		return err
	}
	entry, err := b.LoadTaskCloseData(ctx, c.reference.Task)
	if err != nil {
		return err
	}
	if entry == nil || entry.Record == nil || entry.Outcome != OutcomeCommitted || entry.Reference != d.reference {
		return ErrCorruptRecord
	}
	value, err := encodeTaskCloseDataRecord(*entry.Record)
	if err != nil || value != d.recordValue {
		return ErrCorruptRecord
	}
	if err = b.verifyTaskCloseTicket(ctx, c, d); err != nil {
		return err
	}
	key, err := b.namespace.taskCloseDataKey(c.reference.Task)
	if err != nil {
		return err
	}
	fence := taskFence{key: key, value: value, create: entry.Revision, mod: entry.Revision}
	if err = b.checkTaskCloseFences(ctx, c, d, &fence); err != nil {
		return err
	}
	return b.verifyTaskCloseTicket(ctx, c, d)
}
