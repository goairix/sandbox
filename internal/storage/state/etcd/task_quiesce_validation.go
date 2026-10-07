package etcd

import (
	"context"
	"errors"
	"fmt"
	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	clientv3 "go.etcd.io/etcd/client/v3"
	"time"
)

type taskQuiescenceDraft struct {
	task                   TaskRecord
	deadline               time.Time
	commandID              string
	certificate            []byte
	issuer                 *CommandIssuerEntry
	issuerKey, issuerValue string
	claims                 controlprotocol.TaskUserQuiescenceTicketClaims
	ticket                 controlprotocol.TaskUserQuiescenceEvidence
	reference              TaskQuiescenceReference
	recordValue            string
	stage                  *Stage
	outcome                Outcome
	cleaned, lost          bool
	deliveryAttempted      bool
	prior                  *taskQuiescencePrerequisite
}

func (b *Backend) taskQuiescenceContext(ctx context.Context, c *TaskClaim, d *taskQuiescenceDraft) (context.Context, context.CancelFunc) {
	bounded, cancel := context.WithDeadline(ctx, d.deadline)
	stop := context.AfterFunc(c.parentCtx, cancel)
	request, finish := b.requestContext(bounded)
	return request, func() { stop(); finish(); cancel() }
}
func taskQuiescenceLive(ctx context.Context, c *TaskClaim, d *taskQuiescenceDraft) error {
	if err := c.live(ctx); err != nil {
		return err
	}
	if d.lost || !time.Now().Before(d.deadline) {
		d.lost = true
		return ErrGuardExpired
	}
	return nil
}
func (b *Backend) taskQuiescenceComparisons(c *TaskClaim, d *taskQuiescenceDraft) []clientv3.Cmp {
	cmps := c.comparisons()
	if d.issuer != nil {
		cmps = append(cmps, clientv3.Compare(clientv3.Value(d.issuerKey), "=", d.issuerValue), clientv3.Compare(clientv3.ModRevision(d.issuerKey), "=", d.issuer.Revision), clientv3.Compare(clientv3.LeaseValue(d.issuerKey), "=", 0))
	}
	if d.prior != nil {
		cmps = append(cmps, d.prior.intent.comparisons()...)
		cmps = append(cmps, clientv3.Compare(clientv3.ModRevision(d.prior.receiptKey), "=", d.prior.receiptRevision))
	}
	return cmps
}
func (b *Backend) checkTaskQuiescenceFences(ctx context.Context, c *TaskClaim, d *taskQuiescenceDraft, intent *taskFence) error {
	if err := taskQuiescenceLive(ctx, c, d); err != nil {
		return err
	}
	cmps := append(b.baseComparisons(), b.taskQuiescenceComparisons(c, d)...)
	if intent != nil {
		cmps = append(cmps, intent.comparisons()...)
	}
	request, cancel := b.requestContext(ctx)
	response, err := b.client.Txn(request).If(cmps...).Then(clientv3.OpGet(b.identityKey)).Else(clientv3.OpGet(b.identityKey)).Commit()
	requestErr := request.Err()
	cancel()
	if err != nil || requestErr != nil {
		c.lost = true
		return fmt.Errorf("%w: task quiescence fences: %w", ErrOutcomeUnknown, errors.Join(err, requestErr))
	}
	if err = b.validateFenceCheckResponse(response); err != nil {
		c.lost = true
		return err
	}
	if !response.Succeeded {
		c.lost = true
		return ErrConflict
	}
	return taskQuiescenceLive(ctx, c, d)
}
func (b *Backend) signTaskQuiescence(ctx context.Context, c *TaskClaim, d *taskQuiescenceDraft) error {
	if err := b.checkTaskQuiescenceFences(ctx, c, d, nil); err != nil {
		return err
	}
	if d.certificate == nil {
		wire, err := b.taskQuiescenceIssuer.Certificate(ctx)
		wire = append([]byte(nil), wire...)
		if liveErr := taskQuiescenceLive(ctx, c, d); liveErr != nil {
			return liveErr
		}
		if err != nil {
			return fmt.Errorf("task issuer certificate: %w", err)
		}
		if err = taskQuiescenceLive(ctx, c, d); err != nil {
			return err
		}
		now, err := b.observePublicationClock(ctx)
		if err != nil {
			return err
		}
		identity, err := b.taskQuiescenceVerifier.VerifyCommandIssuerCertificate(wire, now)
		if err != nil {
			return fmt.Errorf("%w: task issuer: %v", ErrInvalidRecord, err)
		}
		d.certificate = identity.Wire()
	}
	if d.issuer == nil {
		entry, err := b.registerCommandIssuerCertificate(ctx, d.certificate, b.taskQuiescenceVerifier)
		if err != nil {
			return err
		}
		if err = taskQuiescenceLive(ctx, c, d); err != nil {
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
	if err := b.checkTaskQuiescenceFences(ctx, c, d, nil); err != nil {
		return err
	}
	now, err := b.observePublicationClock(ctx)
	anchor := time.Now()
	if err != nil {
		return err
	}
	if err = taskQuiescenceLive(ctx, c, d); err != nil {
		return err
	}
	issuer, err := b.taskQuiescenceVerifier.VerifyCommandIssuerCertificate(d.certificate, now)
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
		d.claims = controlprotocol.TaskUserQuiescenceTicketClaims{Version: 1, Purpose: "task_quiesce_users", Context: controlprotocol.TaskUserQuiescenceContext{Current: expected, CloseDataContext: d.prior.entry.Record.Context, CloseDataTicketDigest: d.prior.entry.Record.TicketDigest, CloseDataReceiptDigest: digestTaskQuiescenceProof(d.prior.proof), CloseDataIntentRevision: d.prior.entry.Revision}, NotBefore: before, NotAfter: after}
	}
	if d.ticket.Digest() != "" {
		return b.verifyTaskQuiescenceTicket(ctx, c, d)
	}
	if err = b.checkTaskQuiescenceFences(ctx, c, d, nil); err != nil {
		return err
	}
	wire, err := b.taskQuiescenceIssuer.SignQuiesceUsers(ctx, issuer.Digest(), d.claims)
	wire = append([]byte(nil), wire...)
	if liveErr := taskQuiescenceLive(ctx, c, d); liveErr != nil {
		return liveErr
	}
	if err != nil {
		return fmt.Errorf("task quiescence signer: %w", err)
	}
	if err = taskQuiescenceLive(ctx, c, d); err != nil {
		return err
	}
	now, err = b.observePublicationClock(ctx)
	if err != nil {
		return err
	}
	if err = taskQuiescenceLive(ctx, c, d); err != nil {
		return err
	}
	evidence, err := b.taskQuiescenceVerifier.VerifyTaskUserQuiescenceTicket(wire, d.certificate, d.claims.Context, now)
	if err != nil {
		return fmt.Errorf("%w: task quiescence ticket: %v", ErrInvalidRecord, err)
	}
	if !evidence.NotBefore().Equal(d.claims.NotBefore) || !evidence.NotAfter().Equal(d.claims.NotAfter) {
		return ErrInvalidRecord
	}
	if err = b.checkTaskQuiescenceFences(ctx, c, d, nil); err != nil {
		return err
	}
	d.ticket = evidence
	return nil
}
func (b *Backend) verifyTaskQuiescenceTicket(ctx context.Context, c *TaskClaim, d *taskQuiescenceDraft) error {
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
	_, err = b.taskQuiescenceVerifier.VerifyTaskUserQuiescenceTicket(d.ticket.Wire(), d.certificate, d.claims.Context, now)
	if err != nil {
		d.lost = true
		return fmt.Errorf("%w: task quiescence ticket: %v", ErrInvalidRecord, err)
	}
	return nil
}

func digestTaskQuiescenceProof(w []byte) string { d, _ := snapshotDigest(w); return d }
