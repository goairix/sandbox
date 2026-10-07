package etcd

import (
	"context"
	"errors"
	"fmt"
	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"time"
)

type taskCloseDraft struct {
	task                   TaskRecord
	deadline               time.Time
	commandID              string
	certificate            []byte
	issuer                 *CommandIssuerEntry
	issuerKey, issuerValue string
	claims                 controlprotocol.TaskCloseDataTicketClaims
	ticket                 controlprotocol.TaskCloseDataEvidence
	reference              TaskCloseDataReference
	recordValue            string
	stage                  *Stage
	outcome                Outcome
	cleaned, lost          bool
}

func (b *Backend) taskCloseContext(ctx context.Context, c *TaskClaim, d *taskCloseDraft) (context.Context, context.CancelFunc) {
	bounded, cancel := context.WithDeadline(ctx, d.deadline)
	stop := context.AfterFunc(c.parentCtx, cancel)
	request, finish := b.requestContext(bounded)
	return request, func() { stop(); finish(); cancel() }
}
func taskCloseLive(ctx context.Context, c *TaskClaim, d *taskCloseDraft) error {
	if err := c.live(ctx); err != nil {
		return err
	}
	if d.lost || !time.Now().Before(d.deadline) {
		d.lost = true
		return ErrGuardExpired
	}
	return nil
}
func taskCloseOriginalTask(c *TaskClaim) (TaskRecord, error) {
	var r TaskRecord
	if len(c.fences) != 8 {
		return r, ErrInvalidRecord
	}
	f := c.fences[0]
	kv := &mvccpb.KeyValue{Key: []byte(f.key), Value: []byte(f.value), CreateRevision: f.create, ModRevision: f.mod, Lease: f.lease}
	if decodeTaskRecord(kv, &r) != nil || r.Reference != c.reference.Task || f.create != c.birth {
		return TaskRecord{}, ErrInvalidRecord
	}
	return r, nil
}
func (b *Backend) taskCloseComparisons(c *TaskClaim, d *taskCloseDraft) []clientv3.Cmp {
	cmps := c.comparisons()
	if d.issuer != nil {
		cmps = append(cmps, clientv3.Compare(clientv3.Value(d.issuerKey), "=", d.issuerValue), clientv3.Compare(clientv3.ModRevision(d.issuerKey), "=", d.issuer.Revision), clientv3.Compare(clientv3.LeaseValue(d.issuerKey), "=", 0))
	}
	return cmps
}
func (b *Backend) checkTaskCloseFences(ctx context.Context, c *TaskClaim, d *taskCloseDraft, intent *taskFence) error {
	if err := taskCloseLive(ctx, c, d); err != nil {
		return err
	}
	cmps := append(b.baseComparisons(), b.taskCloseComparisons(c, d)...)
	if intent != nil {
		cmps = append(cmps, intent.comparisons()...)
	}
	request, cancel := b.requestContext(ctx)
	response, err := b.client.Txn(request).If(cmps...).Then(clientv3.OpGet(b.identityKey)).Else(clientv3.OpGet(b.identityKey)).Commit()
	requestErr := request.Err()
	cancel()
	if err != nil || requestErr != nil {
		c.lost = true
		return fmt.Errorf("%w: task close fences: %w", ErrOutcomeUnknown, errors.Join(err, requestErr))
	}
	if err = b.validateFenceCheckResponse(response); err != nil {
		c.lost = true
		return err
	}
	if !response.Succeeded {
		c.lost = true
		return ErrConflict
	}
	return taskCloseLive(ctx, c, d)
}
func (b *Backend) signTaskClose(ctx context.Context, c *TaskClaim, d *taskCloseDraft) error {
	if err := b.checkTaskCloseFences(ctx, c, d, nil); err != nil {
		return err
	}
	if d.certificate == nil {
		wire, err := b.taskIssuer.Certificate(ctx)
		wire = append([]byte(nil), wire...)
		if err != nil {
			return fmt.Errorf("task issuer certificate: %w", err)
		}
		if err = taskCloseLive(ctx, c, d); err != nil {
			return err
		}
		now, err := b.observePublicationClock(ctx)
		if err != nil {
			return err
		}
		identity, err := b.taskVerifier.VerifyCommandIssuerCertificate(wire, now)
		if err != nil {
			return fmt.Errorf("%w: task issuer: %v", ErrInvalidRecord, err)
		}
		d.certificate = identity.Wire()
	}
	if d.issuer == nil {
		entry, err := b.registerCommandIssuerCertificate(ctx, d.certificate, b.taskVerifier)
		if err != nil {
			return err
		}
		d.issuer = entry
		d.issuerKey, err = b.namespace.commandIssuerKey(entry.Record.CertificateID)
		if err != nil {
			return err
		}
		d.issuerValue, err = encodeCommandIssuerRecord(entry.Record)
		if err != nil {
			return err
		}
	}
	if err := b.checkTaskCloseFences(ctx, c, d, nil); err != nil {
		return err
	}
	now, err := b.observePublicationClock(ctx)
	anchor := time.Now()
	if err != nil {
		return err
	}
	if err = taskCloseLive(ctx, c, d); err != nil {
		return err
	}
	issuer, err := b.taskVerifier.VerifyCommandIssuerCertificate(d.certificate, now)
	if err != nil {
		return fmt.Errorf("%w: task issuer: %v", ErrInvalidRecord, err)
	}
	if d.claims.Version == 0 {
		before := now.Add(-2 * time.Second)
		if issuer.NotBefore().After(before) {
			before = issuer.NotBefore()
		}
		after := now.Add(-time.Second).Add(d.deadline.Sub(anchor))
		for _, limit := range []time.Time{before.Add(30 * time.Second), issuer.NotAfter()} {
			if limit.Before(after) {
				after = limit
			}
		}
		if now.Add(-time.Second).Before(before) || !now.Add(time.Second).Before(after) {
			return ErrGuardExpired
		}
		task, err := encodeTaskRecord(d.task)
		if err != nil {
			return err
		}
		digest, err := snapshotDigest([]byte(task))
		if err != nil {
			return err
		}
		r := d.task
		ref := c.reference
		expected := controlprotocol.TaskCloseDataContext{Namespace: b.namespace.Root(), AuthorityID: b.publicationAuthorityID, Target: b.publicationTarget, RestoreEpoch: b.restoreEpoch, IssuerCertificateID: issuer.CertificateID(), IssuerCertificateDigest: issuer.Digest(), CommandID: d.commandID, TaskID: ref.Task.TaskID, TaskDigest: digest, ClaimID: ref.ClaimID, WorkerID: ref.WorkerID, SandboxID: ref.Task.SandboxID, WorkspaceHash: r.WorkspaceHash, Generation: r.Generation, DataGateEpoch: r.DataGateEpoch, ControlRevision: c.birth, ClaimCreateRevision: ref.CreateRevision, LeaseID: ref.LeaseID, Runtime: controlprotocol.RuntimeReference(r.Runtime)}
		d.claims = controlprotocol.TaskCloseDataTicketClaims{Version: 1, Purpose: "task_close_data", Context: expected, NotBefore: before, NotAfter: after}
	}
	if d.ticket.Digest() != "" {
		return b.verifyTaskCloseTicket(ctx, c, d)
	}
	if err = b.checkTaskCloseFences(ctx, c, d, nil); err != nil {
		return err
	}
	wire, err := b.taskIssuer.SignCloseData(ctx, issuer.Digest(), d.claims)
	wire = append([]byte(nil), wire...)
	if err != nil {
		return fmt.Errorf("task close signer: %w", err)
	}
	if err = taskCloseLive(ctx, c, d); err != nil {
		return err
	}
	now, err = b.observePublicationClock(ctx)
	if err != nil {
		return err
	}
	if err = taskCloseLive(ctx, c, d); err != nil {
		return err
	}
	evidence, err := b.taskVerifier.VerifyTaskCloseDataTicket(wire, d.certificate, d.claims.Context, now)
	if err != nil {
		return fmt.Errorf("%w: task close ticket: %v", ErrInvalidRecord, err)
	}
	if !evidence.NotBefore().Equal(d.claims.NotBefore) || !evidence.NotAfter().Equal(d.claims.NotAfter) {
		return ErrInvalidRecord
	}
	if err = b.checkTaskCloseFences(ctx, c, d, nil); err != nil {
		return err
	}
	d.ticket = evidence
	return nil
}
func (b *Backend) verifyTaskCloseTicket(ctx context.Context, c *TaskClaim, d *taskCloseDraft) error {
	if err := taskCloseLive(ctx, c, d); err != nil {
		return err
	}
	now, err := b.observePublicationClock(ctx)
	if err != nil {
		return err
	}
	if err = taskCloseLive(ctx, c, d); err != nil {
		return err
	}
	_, err = b.taskVerifier.VerifyTaskCloseDataTicket(d.ticket.Wire(), d.certificate, d.claims.Context, now)
	if err != nil {
		d.lost = true
		return fmt.Errorf("%w: task close ticket: %v", ErrInvalidRecord, err)
	}
	return nil
}
