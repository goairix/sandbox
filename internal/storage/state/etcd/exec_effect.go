package etcd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// PrepareExecEffect records one signed metadata intent under the original
// operation capability. It does not deliver a command or start physical work.
func (b *Backend) PrepareExecEffect(ctx context.Context, c *OperationCapability, request controlprotocol.ExecutionRequest) (result PrepareExecEffectResult, err error) {
	result.Outcome = OutcomeUnknown
	if ctx == nil || c == nil || c.origin != b || c.parentCtx == nil {
		return result, ErrInvalidRecord
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	d := c.execDraft
	if d != nil {
		result.Outcome, result.Reference = d.outcome, d.reference
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if c.record.Reference.Kind != OperationData {
		return result, ErrInvalidRecord
	}
	if err = c.admissionLive(); err != nil {
		return result, err
	}
	if b.execIssuer == nil || b.execVerifier == nil || b.authorityClock == nil || b.publicationVerifier == nil {
		return result, ErrInvalidConfiguration
	}
	descriptor, e := controlprotocol.NewExecutionDescriptor(request)
	if e != nil {
		return result, fmt.Errorf("%w: execution descriptor: %v", ErrInvalidRecord, e)
	}
	if d == nil {
		d = &execEffectDraft{descriptor: descriptor, deadline: c.deadline, commandID: uuid.NewString(), outcome: OutcomeUnknown}
		c.execDraft = d
	} else if d.descriptor.Digest() != descriptor.Digest() {
		return result, ErrConflict
	}
	// Never widen this deadline after RenewOperation extends the underlying Lease.
	bounded, cancel := b.execEffectContext(ctx, c, d)
	defer cancel()
	if err = execEffectLive(bounded, c, d); err != nil {
		return result, err
	}
	defer func() { result.Reference = d.reference; result.Outcome = d.outcome }()
	if d.outcome == OutcomeUnknown {
		if d.reference.Stage.AttemptID != "" && d.stage == nil {
			// A Begin with an uncertain result has no private Stage capability. Only
			// arbitration of that exact saved attempt can advance its outcome.
			d.outcome, err = b.ResolveStage(bounded, d.reference.Stage)
			if err != nil {
				return result, err
			}
		} else {
			if err = b.signExecEffect(bounded, c, d); err != nil {
				return result, err
			}
			if d.stage == nil {
				d.stage, err = b.beginStageWithBuilder(bounded, c.record.Reference.Partition, c.record.Reference.RequestID, "operation_exec_start", time.Until(d.deadline), func(locator StageAttemptLocator) (Mutation, error) { return b.buildExecEffect(c, d, locator) })
				if err != nil {
					var failure *stageBeginFailure
					if errors.As(err, &failure) {
						result.GuardCleanupError = failure.cleanup
						err = failure.cause
					}
					return result, err
				}
			}
			if err = b.verifyExecEffectTicket(bounded, c, d); err != nil {
				return result, err
			}
			d.outcome, err = b.CommitStage(bounded, d.stage)
			if err != nil {
				return result, err
			}
		}
	}
	if d.outcome != OutcomeUnknown && d.stage != nil && !d.cleaned {
		result.GuardCleanupError = execEffectLive(bounded, c, d)
		if result.GuardCleanupError == nil {
			result.GuardCleanupError = b.ReleaseStage(bounded, d.stage)
		}
		d.cleaned = result.GuardCleanupError == nil
	}
	if d.outcome != OutcomeCommitted {
		return result, nil
	}
	// An actual commit is retained even when any postcommit check fails.
	if err = b.authorizePreparedExecEffect(bounded, c, d); err != nil {
		return result, err
	}
	result.Prepared = &PreparedExecEffect{origin: b, capability: c, draft: d}
	return result, nil
}

func (b *Backend) execEffectComparisons(c *OperationCapability, d *execEffectDraft) ([]clientv3.Cmp, error) {
	receipt, err := encodeOperationReceipt(operationReceipt{Version: 1, Reference: c.record.Reference, Outcome: OperationCommitted})
	if err != nil {
		return nil, err
	}
	comparisons := make([]clientv3.Cmp, 0, 22)
	for _, f := range c.fences {
		comparisons = append(comparisons, clientv3.Compare(clientv3.ModRevision(f.Key), "=", f.ModRevision), clientv3.Compare(clientv3.LeaseValue(f.Key), "=", 0))
	}
	comparisons = append(comparisons, execEffectEnvelopeComparisons(c.guardKey, c.value, c.record.Reference.LeaseID, c.guardRevision)...)
	comparisons = append(comparisons, execEffectEnvelopeComparisons(c.tokenKey, c.value, c.record.Reference.LeaseID, c.admitRevision)...)
	comparisons = append(comparisons, execEffectEnvelopeComparisons(c.receiptKey, receipt, c.record.Reference.LeaseID, c.admitRevision)...)
	comparisons = append(comparisons, clientv3.Compare(clientv3.Value(d.issuerKey), "=", d.issuerValue), clientv3.Compare(clientv3.ModRevision(d.issuerKey), "=", d.issuer.Revision), clientv3.Compare(clientv3.LeaseValue(d.issuerKey), "=", 0))
	return comparisons, nil
}

// Exec starts pin the original operation body, Lease and first revision. The
// renewal helper additionally compares ModRevision and has a different budget.
func execEffectEnvelopeComparisons(key, value string, lease, revision int64) []clientv3.Cmp {
	return []clientv3.Cmp{clientv3.Compare(clientv3.Value(key), "=", value), clientv3.Compare(clientv3.LeaseValue(key), "=", lease), clientv3.Compare(clientv3.CreateRevision(key), "=", revision)}
}

func (b *Backend) buildExecEffect(c *OperationCapability, d *execEffectDraft, locator StageAttemptLocator) (Mutation, error) {
	record := ExecEffectRecord{Version: 1, Operation: c.record, AdmissionRevision: c.admitRevision, CommandID: d.commandID, DescriptorDigest: d.descriptor.Digest(), TicketDigest: d.ticket.Digest(), IssuerCertificateID: d.issuer.Record.CertificateID, IssuerCertificateDigest: d.issuer.Record.CertificateDigest, IssuerRevision: d.issuer.Revision, Ticket: d.ticket.Wire(), Attempt: locator}
	value, err := encodeExecEffectRecord(record)
	if err != nil {
		return Mutation{}, err
	}
	key, err := b.namespace.execEffectKey(c.record.Reference)
	if err != nil {
		return Mutation{}, err
	}
	comparisons, err := b.execEffectComparisons(c, d)
	if err != nil {
		return Mutation{}, err
	}
	comparisons = append(comparisons, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	mutation, digest, err := prepareMutation(b.namespace, Mutation{Comparisons: comparisons, Writes: []Write{{Key: key, Value: []byte(value)}}})
	if err != nil {
		return Mutation{}, err
	}
	// The record stores only the locator: including its mutation digest would
	// make the digest self-referential. Save both before Begin can issue any RPC.
	d.recordValue = value
	d.reference = ExecEffectReference{Operation: c.record.Reference, CommandID: d.commandID, Stage: locator.reference(digest)}
	return mutation, nil
}

func (b *Backend) authorizePreparedExecEffect(ctx context.Context, c *OperationCapability, d *execEffectDraft) error {
	if err := execEffectLive(ctx, c, d); err != nil {
		return err
	}
	entry, err := b.LoadExecEffect(ctx, d.reference)
	if err != nil {
		return err
	}
	if err := execEffectLive(ctx, c, d); err != nil {
		return err
	}
	if entry == nil || entry.Record == nil || entry.Outcome != OutcomeCommitted {
		return ErrCorruptRecord
	}
	value, err := encodeExecEffectRecord(*entry.Record)
	if err != nil || value != d.recordValue {
		return ErrCorruptRecord
	}
	if err = b.verifyExecEffectTicket(ctx, c, d); err != nil {
		return err
	}
	comparisons, err := b.execEffectComparisons(c, d)
	if err != nil {
		return err
	}
	comparisons = append(b.baseComparisons(), comparisons...)
	response, err := b.client.Txn(ctx).If(comparisons...).Then(clientv3.OpGet(b.identityKey)).Else(clientv3.OpGet(b.identityKey)).Commit()
	if err != nil {
		return fmt.Errorf("%w: fence exec effect: %w", ErrOutcomeUnknown, err)
	}
	if err = b.validateFenceCheckResponse(response); err != nil {
		return err
	}
	if !response.Succeeded {
		return ErrConflict
	}
	// A successful RPC may have consumed the remaining ticket window.
	return b.verifyExecEffectTicket(ctx, c, d)
}
