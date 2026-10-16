package etcd

import (
	"context"
	"errors"
	"fmt"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	t "github.com/goairix/sandbox/internal/runtime/controltransport"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"time"
)

// PrepareTaskWorkspaceArchive commits one original immutable command. It sends
// no archive command bytes and performs no archive/provider IO. Its finite
// callbacks share the original prerequisite deadline while holding c.mu.
// The single Destination must currently use the same still-valid management
// certificate as historical quiescence queries. Certificate rotation/query TLS
// semantics are a separate integration requirement; mismatches fail closed.
func (b *Backend) PrepareTaskWorkspaceArchive(ctx context.Context, c *TaskClaim, prepared *PreparedTaskQuiescence, destination *t.Destination, sessionDuration time.Duration) (TaskWorkspaceArchivePreparationResult, error) {
	result := TaskWorkspaceArchivePreparationResult{Outcome: OutcomeUnknown}
	if ctx == nil || !b.validTaskClaim(c) || prepared == nil || prepared.self != prepared || prepared.origin != b || prepared.claim != c || prepared.claimReference != c.reference || destination == nil || sessionDuration <= 30*time.Second || sessionDuration > time.Hour {
		return result, ErrInvalidRecord
	}
	if b.taskWorkspaceArchiveIssuer == nil || b.taskWorkspaceArchiveVerifier == nil || b.authorityClock == nil {
		return result, ErrInvalidConfiguration
	}
	c.mu.Lock()
	f := c.finalSyncDraft
	if f == nil || f.prepared != prepared || f.revision <= 0 || !f.writeAttempted || f.lost || f.record.Claim != c.reference {
		c.mu.Unlock()
		return result, ErrConflict
	}
	d := f.archive
	if d != nil {
		result.Reference = d.reference()
		if d.prepared != prepared || d.destination != destination || d.duration != sessionDuration {
			c.mu.Unlock()
			return result, ErrConflict
		}
		if d.writeAttempted {
			defer c.mu.Unlock()
			return b.arbitrateTaskArchive(ctx, c, d, result)
		}
	}
	if !time.Now().Before(prepared.deadline) {
		c.mu.Unlock()
		return result, ErrGuardExpired
	}
	if err := c.live(ctx); err != nil {
		c.mu.Unlock()
		return result, err
	}
	if d == nil {
		d = &taskArchiveDraft{finalSync: f, prepared: prepared, destination: destination, duration: sessionDuration, deadline: prepared.deadline}
		f.archive = d
	}
	if err := taskArchiveLive(ctx, c, d); err != nil {
		c.mu.Unlock()
		return result, err
	}
	result.Reference = d.reference()
	c.mu.Unlock()
	if err := b.RevalidateTaskQuiescence(ctx, c, prepared, destination); err != nil {
		return result, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if d.writeAttempted {
		return b.arbitrateTaskArchive(ctx, c, d, result)
	}
	bounded, cancel := context.WithDeadline(ctx, d.deadline)
	defer cancel()
	request, finish := b.taskObservationContext(bounded, c)
	defer finish()
	if err := b.refreshTaskArchivePins(request, c, d); err != nil {
		return result, err
	}
	if err := b.signTaskArchive(request, c, d); err != nil {
		return result, err
	}
	key, err := b.namespace.taskWorkspaceArchiveKey(c.reference.Task)
	if err != nil {
		return result, err
	}
	points, succeeded, err := b.readTaskArchivePoints(request, c, d, key)
	if err != nil {
		return result, err
	}
	if !succeeded || points[2] != nil {
		d.lost = true
		return result, ErrConflict
	}
	cmps := b.taskArchiveComparisons(c, d)
	if err = b.preflightTaskArchive(cmps, key, d.value); err != nil {
		return result, err
	}
	// Callbacks/point discovery cannot replace this final whole-prefix Range.
	if err = b.observeTaskArchiveEmpty(request, c, d); err != nil {
		return result, err
	}
	if err = taskArchiveLive(request, c, d); err != nil {
		return result, err
	}
	d.writeAttempted = true // consumed before the first native Put can be sent
	writeCmps := append(cmps, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	response, err := b.client.Txn(request).If(writeCmps...).Then(clientv3.OpPut(key, d.value)).Else(b.commandIssuerPoints(key)...).Commit()
	if err != nil {
		return result, errors.Join(ErrOutcomeUnknown, err, request.Err())
	}
	if err = b.operationResponseHeader(response); err != nil {
		return result, errors.Join(ErrOutcomeUnknown, err)
	}
	if !response.Succeeded {
		if _, err = b.stageEvidencePoints(response, []string{b.identityKey, b.restoreKey, key}); err != nil {
			return result, err
		}
		d.lost = true
		return result, ErrConflict
	}
	if !operationPutResponses(response, 1) || response.Header.Revision <= f.revision || response.Header.Revision <= d.issuerRevision {
		return result, ErrOutcomeUnknown
	}
	d.revision = response.Header.Revision
	result.Reference, result.Outcome = d.reference(), OutcomeCommitted
	if err = taskArchiveLive(request, c, d); err != nil {
		return result, err
	}
	if err = b.checkTaskArchiveFences(request, c, d); err != nil {
		return result, err
	}
	// A successful write lends no authority to this later linearizable read.
	points, succeeded, err = b.readTaskArchivePoints(request, c, d, key)
	if err != nil {
		return result, err
	}
	if !succeeded || b.exactTaskArchive(points[2], d) != nil {
		d.lost = true
		return result, ErrConflict
	}
	if err = b.verifyTaskArchive(request, c, d); err != nil {
		return result, err
	}
	if err = taskArchiveLive(request, c, d); err != nil {
		return result, err
	}
	h := &PreparedTaskWorkspaceArchive{origin: b, claim: c, finalSync: f, draft: d}
	h.self = h
	d.preparedArchive = h
	result.Prepared = h
	return result, nil
}

func taskArchiveLive(ctx context.Context, c *TaskClaim, d *taskArchiveDraft) error {
	if err := c.live(ctx); err != nil {
		return err
	}
	if d.lost || d.finalSync.lost || c.finalSyncDraft != d.finalSync || d.finalSync.archive != d || !time.Now().Before(d.deadline) || (!d.windowDeadline.IsZero() && !time.Now().Before(d.windowDeadline)) {
		return ErrGuardExpired
	}
	return nil
}

// Original full claim fences plus immutable MOD pins keep both native branches
// below64 nodes. These pins are installed only after strict fresh history reads;
// delete/recreate advances MOD even when bytes are identical.
func (b *Backend) taskArchiveComparisons(c *TaskClaim, d *taskArchiveDraft) []clientv3.Cmp {
	cmps := append(b.baseComparisons(), c.comparisons()...)
	pin := d.prepared.pin
	for _, item := range []struct {
		key      string
		revision int64
	}{{pin.intent.key, pin.intent.mod}, {pin.receipt.key, pin.receipt.mod}, {pin.reservation.key, pin.reservation.revision}} {
		cmps = append(cmps, clientv3.Compare(clientv3.ModRevision(item.key), "=", item.revision))
	}
	key, _ := b.namespace.taskFinalSyncAttemptKey(c.reference.Task)
	cmps = append(cmps, clientv3.Compare(clientv3.ModRevision(key), "=", d.finalSync.revision))
	if d.issuerRevision > 0 {
		cmps = append(cmps, clientv3.Compare(clientv3.ModRevision(d.issuerKey), "=", d.issuerRevision))
	}
	if d.revision > 0 {
		key, _ := b.namespace.taskWorkspaceArchiveKey(c.reference.Task)
		cmps = append(cmps, clientv3.Compare(clientv3.ModRevision(key), "=", d.revision))
	}
	return cmps
}

func (b *Backend) refreshTaskArchivePins(ctx context.Context, c *TaskClaim, d *taskArchiveDraft) error {
	if err := taskArchiveLive(ctx, c, d); err != nil {
		return err
	}
	if _, _, err := b.pinTaskDualDrain(ctx, c, &d.prepared.pin); err != nil {
		return err
	}
	key, err := b.namespace.taskFinalSyncAttemptKey(c.reference.Task)
	if err != nil {
		return err
	}
	pin := d.prepared.pin
	keys := []string{b.identityKey, b.restoreKey, key, pin.intent.key, pin.receipt.key, pin.reservation.key}
	values := []string{b.identityValue, b.restoreEpoch, d.finalSync.value, pin.intent.value, pin.receipt.value, string(make([]byte, 8192))}
	if d.issuerRevision > 0 {
		keys = append(keys, d.issuerKey)
	}
	if d.issuerRevision > 0 {
		values = append(values, d.issuerValue)
	}
	freshCmps := append(b.baseComparisons(), c.comparisons()...)
	if q := c.quiescenceDraft; q != nil && q.reservation.revision > 0 {
		freshCmps = append(freshCmps, q.reservation.comparison())
	}
	reads := taskProtoPoints(keys)
	if err = taskCheckWire(b.namespace, []*pb.TxnRequest{{Compare: taskProtoComparisons(freshCmps), Success: reads, Failure: reads}}, []*pb.TxnResponse{taskProtoResponse(keys, values)}); err != nil {
		return err
	}
	points, err := b.readQuiescencePoints(ctx, c, keys)
	if live := taskArchiveLive(ctx, c, d); live != nil {
		return live
	}
	if err != nil {
		return err
	}
	var f TaskFinalSyncAttemptRecord
	if decodeTaskFinalSyncAttempt(points[2], &f) != nil || points[2].ModRevision != d.finalSync.revision || string(points[2].Value) != d.finalSync.value || f != d.finalSync.record || f.ReceiptDigest != digestTaskQuiescenceProof(d.prepared.receipt.Wire()) {
		return ErrConflict
	}
	for i, expected := range []struct {
		revision int64
		value    string
	}{{pin.intent.mod, pin.intent.value}, {pin.receipt.mod, pin.receipt.value}, {pin.reservation.revision, ""}} {
		kv := points[i+3]
		if !immutableDispatchKV(kv) || kv.Version != 1 || kv.ModRevision != expected.revision || (expected.value != "" && string(kv.Value) != expected.value) {
			return ErrConflict
		}
	}
	var reservation TaskQuiescenceAttemptRecord
	if decodeTaskQuiescenceAttempt(points[5], &reservation) != nil {
		return ErrCorruptRecord
	}
	canonical, err := encodeTaskQuiescenceAttempt(reservation)
	if err != nil || string(points[5].Value) != canonical || reservation.Task != f.Task || reservation.CommandID != pin.reference.CommandID {
		return ErrConflict
	}
	if d.issuerRevision > 0 {
		entry, err := b.decodeCommandIssuer(points[6])
		if err != nil {
			return err
		}
		if entry == nil || !immutableDispatchKV(points[6]) || points[6].Version != 1 || entry.Revision != d.issuerRevision || string(points[6].Value) != d.issuerValue {
			return ErrConflict
		}
	}
	return b.checkTaskArchiveFences(ctx, c, d)
}

func (b *Backend) checkTaskArchiveFences(ctx context.Context, c *TaskClaim, d *taskArchiveDraft) error {
	if err := taskArchiveLive(ctx, c, d); err != nil {
		return err
	}
	cmps := b.taskArchiveComparisons(c, d)
	reads := taskProtoPoints([]string{b.identityKey})
	if err := taskCheckWire(b.namespace, []*pb.TxnRequest{{Compare: taskProtoComparisons(cmps), Success: reads, Failure: reads}}, []*pb.TxnResponse{taskProtoResponse([]string{b.identityKey}, []string{b.identityValue})}); err != nil {
		return err
	}
	response, err := b.client.Txn(ctx).If(cmps...).Then(clientv3.OpGet(b.identityKey)).Else(clientv3.OpGet(b.identityKey)).Commit()
	if live := taskArchiveLive(ctx, c, d); live != nil {
		return live
	}
	if err != nil {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	if err = b.validateFenceCheckResponse(response); err != nil {
		return err
	}
	if !response.Succeeded {
		d.lost = true
		return ErrConflict
	}
	return nil
}

func (b *Backend) archiveClock(ctx context.Context, c *TaskClaim, d *taskArchiveDraft) (time.Time, error) {
	if err := b.checkTaskArchiveFences(ctx, c, d); err != nil {
		return time.Time{}, err
	}
	now, err := b.observePublicationClock(ctx)
	if live := taskArchiveLive(ctx, c, d); live != nil {
		return time.Time{}, live
	}
	if err != nil {
		return time.Time{}, err
	}
	if err = b.checkTaskArchiveFences(ctx, c, d); err != nil {
		return time.Time{}, err
	}
	return now, nil
}

func (b *Backend) signTaskArchive(ctx context.Context, c *TaskClaim, d *taskArchiveDraft) error {
	if d.claims.Version == 0 {
		now, err := b.archiveClock(ctx, c, d)
		if err != nil {
			return err
		}
		// Freeze the first trusted observation before any subsequent callback.
		d.claims = p.TaskWorkspaceArchiveTicketClaims{Version: 1, Purpose: "task_workspace_archive", NotBefore: now.Add(-time.Second), NotAfter: now.Add(20 * time.Second), SessionNotAfter: now.Add(-time.Second).Add(d.duration)}
		d.windowDeadline = time.Now().Add(19 * time.Second)
	}
	if !d.certificateAttempted {
		if err := b.checkTaskArchiveFences(ctx, c, d); err != nil {
			return err
		}
		d.certificateAttempted = true
		wire, err := b.taskWorkspaceArchiveIssuer.Certificate(ctx)
		wire = append([]byte(nil), wire...)
		d.certificate = wire
		if live := taskArchiveLive(ctx, c, d); live != nil {
			return live
		}
		if err != nil {
			d.lost = true
			return err
		}
		if err = b.checkTaskArchiveFences(ctx, c, d); err != nil {
			return err
		}
		// Pin even a rejected callback result: retry cannot select a replacement.
	}
	now, err := b.archiveClock(ctx, c, d)
	if err != nil {
		return err
	}
	issuer, err := b.taskWorkspaceArchiveVerifier.VerifyCommandIssuerCertificate(d.certificate, now)
	if err != nil {
		return fmt.Errorf("%w: archive issuer: %v", ErrInvalidRecord, err)
	}
	if issuer.NotBefore().After(d.claims.NotBefore) || issuer.NotAfter().Before(d.claims.SessionNotAfter) {
		return ErrGuardExpired
	}
	if d.claims.Context.Current.CommandID == "" {
		f := d.finalSync.record
		task, err := encodeTaskRecord(f.Task)
		if err != nil {
			return err
		}
		q := d.prepared.receipt.Context()
		current := p.TaskCloseDataContext{Namespace: b.namespace.Root(), AuthorityID: b.publicationAuthorityID, Target: b.publicationTarget, RestoreEpoch: b.restoreEpoch, IssuerCertificateID: issuer.CertificateID(), IssuerCertificateDigest: issuer.Digest(), CommandID: f.AttemptID, TaskID: f.Task.Reference.TaskID, TaskDigest: digestTaskQuiescenceProof([]byte(task)), ClaimID: c.reference.ClaimID, WorkerID: c.reference.WorkerID, SandboxID: c.reference.Task.SandboxID, WorkspaceHash: f.Task.WorkspaceHash, Generation: f.Task.Generation, DataGateEpoch: f.Task.DataGateEpoch, ControlRevision: c.birth, ClaimCreateRevision: c.reference.CreateRevision, LeaseID: c.reference.LeaseID, Runtime: p.RuntimeReference(f.Task.Runtime)}
		d.claims.Context = p.TaskWorkspaceArchiveContext{Current: current, QuiescenceContext: q, QuiescenceTicketDigest: d.prepared.receipt.TicketDigest(), QuiescenceReceiptDigest: f.ReceiptDigest, PlanDigest: f.Plan.Digest, QuiescenceIntentRevision: f.QuiescenceRevision, FinalSyncRevision: d.finalSync.revision}
	}
	if err = d.claims.Validate(); err != nil {
		return ErrInvalidRecord
	}
	if !d.destination.MatchTaskWorkspaceArchive(d.claims.Context, d.certificate) {
		return t.ErrDestination
	}
	if err = d.destination.VerifyTaskWorkspaceArchiveRuntime(b.taskWorkspaceArchiveVerifier, d.claims.Context, d.claims.NotBefore, d.claims.NotAfter, d.claims.SessionNotAfter, now); err != nil {
		return err
	}
	if d.issuerRevision == 0 {
		if err = b.registerTaskArchiveIssuer(ctx, c, d, issuer); err != nil {
			return err
		}
	}
	if d.ticket.Digest() == "" {
		if !d.signingAttempted {
			if err = b.checkTaskArchiveFences(ctx, c, d); err != nil {
				return err
			}
			d.signingAttempted = true
			wire, signErr := b.taskWorkspaceArchiveIssuer.SignWorkspaceArchive(ctx, issuer.Digest(), d.claims)
			d.signedWire = append([]byte(nil), wire...)
			if live := taskArchiveLive(ctx, c, d); live != nil {
				return live
			}
			if signErr != nil {
				d.lost = true
				return signErr
			}
			if err = b.checkTaskArchiveFences(ctx, c, d); err != nil {
				return err
			}
		}
		now, err = b.archiveClock(ctx, c, d)
		if err != nil {
			return err
		}
		evidence, err := b.taskWorkspaceArchiveVerifier.VerifyTaskWorkspaceArchiveTicket(d.signedWire, d.certificate, d.claims.Context, now)
		if err != nil {
			return fmt.Errorf("%w: archive ticket: %v", ErrInvalidRecord, err)
		}
		if !evidence.NotBefore().Equal(d.claims.NotBefore) || !evidence.NotAfter().Equal(d.claims.NotAfter) || !evidence.SessionNotAfter().Equal(d.claims.SessionNotAfter) {
			return ErrInvalidRecord
		}
		d.ticket = evidence
	}
	if err = b.verifyTaskArchive(ctx, c, d); err != nil {
		return err
	}
	d.value, err = encodeTaskArchiveCommand(taskArchiveCommandRecord{Version: 1, FinalSync: taskFinalSyncWire(d.finalSync.record), FinalSyncRevision: d.finalSync.revision, Claims: d.claims, IssuerRevision: d.issuerRevision, Certificate: d.certificate, Ticket: d.ticket.Wire(), QuiescenceReceipt: d.prepared.receipt.Wire()})
	return err
}

func (b *Backend) verifyTaskArchive(ctx context.Context, c *TaskClaim, d *taskArchiveDraft) error {
	now, err := b.archiveClock(ctx, c, d)
	if err != nil {
		return err
	}
	e, err := b.taskWorkspaceArchiveVerifier.VerifyTaskWorkspaceArchiveTicket(d.ticket.Wire(), d.certificate, d.claims.Context, now)
	if err != nil {
		return fmt.Errorf("%w: archive ticket: %v", ErrInvalidRecord, err)
	}
	if !e.NotBefore().Equal(d.claims.NotBefore) || !e.NotAfter().Equal(d.claims.NotAfter) || !e.SessionNotAfter().Equal(d.claims.SessionNotAfter) || !d.destination.MatchTaskWorkspaceArchive(d.claims.Context, d.certificate) {
		return ErrInvalidRecord
	}
	return d.destination.VerifyTaskWorkspaceArchiveRuntime(b.taskWorkspaceArchiveVerifier, d.claims.Context, d.claims.NotBefore, d.claims.NotAfter, d.claims.SessionNotAfter, now)
}

func (b *Backend) registerTaskArchiveIssuer(ctx context.Context, c *TaskClaim, d *taskArchiveDraft, issuer p.CommandIssuerIdentity) error {
	r := CommandIssuerRecord{Version: 1, Namespace: b.namespace.Root(), RestoreEpoch: b.restoreEpoch, CertificateID: issuer.CertificateID(), CertificateDigest: issuer.Digest(), Certificate: issuer.Wire()}
	value, err := encodeCommandIssuerRecord(r)
	if err != nil {
		return err
	}
	key, err := b.namespace.commandIssuerKey(r.CertificateID)
	if err != nil {
		return err
	}
	if err = b.observeTaskArchiveEmpty(ctx, c, d); err != nil {
		return err
	}
	cmps := b.taskArchiveComparisons(c, d)
	if err = b.preflightTaskArchive(cmps, key, value); err != nil {
		return err
	}
	response, err := b.client.Txn(ctx).If(append(cmps, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))...).Then(clientv3.OpPut(key, value)).Else(b.commandIssuerPoints(key)...).Commit()
	if live := taskArchiveLive(ctx, c, d); live != nil {
		return live
	}
	if err != nil {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	if err = b.operationResponseHeader(response); err != nil {
		return err
	}
	if response.Succeeded {
		if !operationPutResponses(response, 1) {
			return ErrOutcomeUnknown
		}
		d.issuerKey, d.issuerValue, d.issuerRevision = key, value, response.Header.Revision
	} else {
		kv, err := b.commandIssuerResponse(response, key)
		if err != nil {
			return err
		}
		entry, err := b.decodeCommandIssuer(kv)
		if err != nil {
			return err
		}
		if entry == nil || !immutableDispatchKV(kv) || kv.Version != 1 || string(kv.Value) != value {
			return ErrConflict
		}
		d.issuerKey, d.issuerValue, d.issuerRevision = key, value, entry.Revision
	}
	points, succeeded, err := b.readTaskArchivePoints(ctx, c, d, key)
	if err != nil {
		return err
	}
	if !succeeded {
		d.lost = true
		return ErrConflict
	}
	entry, err := b.decodeCommandIssuer(points[2])
	if err != nil {
		return err
	}
	if entry == nil || !immutableDispatchKV(points[2]) || points[2].Version != 1 || string(points[2].Value) != value {
		return ErrConflict
	}
	d.issuerKey, d.issuerValue, d.issuerRevision = key, value, entry.Revision
	return b.checkTaskArchiveFences(ctx, c, d)
}

func (b *Backend) observeTaskArchiveEmpty(ctx context.Context, c *TaskClaim, d *taskArchiveDraft) error {
	if err := b.checkTaskArchiveFences(ctx, c, d); err != nil {
		return err
	}
	prefix, err := b.taskOperationsPrefix(c.reference.Task)
	if err != nil {
		return err
	}
	cmps := b.taskArchiveComparisons(c, d)
	op := taskProtoRange(prefix)
	op.GetRequestRange().RangeEnd = []byte(clientv3.GetPrefixRangeEnd(prefix))
	op.GetRequestRange().Limit = 1
	op.GetRequestRange().SortOrder = pb.RangeRequest_ASCEND
	op.GetRequestRange().SortTarget = pb.RangeRequest_KEY
	if err = taskCheckWire(b.namespace, []*pb.TxnRequest{{Compare: taskProtoComparisons(cmps), Success: []*pb.RequestOp{op}, Failure: []*pb.RequestOp{op}}}, []*pb.TxnResponse{taskProtoResponse([]string{prefix + "bounded"}, []string{string(make([]byte, maxRecordBytes))})}); err != nil {
		return err
	}
	page, err := b.taskOperationsRange(ctx, c, d.finalSync.record.Task, prefix, prefix, 1, 0, cmps)
	if err != nil {
		return err
	}
	if len(page.Entries) != 0 || page.Count != 0 {
		return ErrConflict
	}
	return taskArchiveLive(ctx, c, d)
}

func (b *Backend) preflightTaskArchive(cmps []clientv3.Cmp, key, value string) error {
	projections, err := b.taskArchiveWireProjections(cmps, key, value)
	if err != nil {
		return err
	}
	return checkTaskWireProjections(b.namespace, projections)
}

// The archive write and readback retain their three diagnostic failure points.
// Sharing this pure envelope prevents cleanup admission from approximating the
// full command body or silently dropping one of those native branches.
func (b *Backend) taskArchiveWireProjections(cmps []clientv3.Cmp, key, value string) ([]cleanupWireProjection, error) {
	keys := []string{b.identityKey, b.restoreKey, key}
	values := []string{b.identityValue, b.restoreEpoch, value}
	for i, k := range keys {
		if !validNamespaceKey(b.namespace, k) || len(values[i]) > maxRecordBytes {
			return nil, ErrInvalidMutation
		}
	}
	reads := taskProtoPoints(keys)
	writeCmps := append(append([]clientv3.Cmp(nil), cmps...), clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	write := &pb.TxnRequest{Compare: taskProtoComparisons(writeCmps), Success: []*pb.RequestOp{taskProtoPut(key, value, 0)}, Failure: reads}
	read := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: reads, Failure: reads}
	response := taskProtoResponse(keys, values)
	return []cleanupWireProjection{{"write", write, taskProtoPutResponse(1), response}, {"read", read, response, response}}, nil
}

func (b *Backend) readTaskArchivePoints(ctx context.Context, c *TaskClaim, d *taskArchiveDraft, key string) ([]*mvccpb.KeyValue, bool, error) {
	if err := taskArchiveLive(ctx, c, d); err != nil {
		return nil, false, err
	}
	cmps := b.taskArchiveComparisons(c, d)
	// Bound any diagnostic point to the largest command body before native IO.
	if err := b.preflightTaskArchive(cmps, key, string(make([]byte, maxRecordBytes))); err != nil {
		return nil, false, err
	}
	ops := b.commandIssuerPoints(key)
	response, err := b.client.Txn(ctx).If(cmps...).Then(ops...).Else(ops...).Commit()
	if live := taskArchiveLive(ctx, c, d); live != nil {
		return nil, false, live
	}
	if err != nil {
		return nil, false, errors.Join(ErrOutcomeUnknown, err)
	}
	points, err := b.stageEvidencePoints(response, []string{b.identityKey, b.restoreKey, key})
	if err != nil {
		return nil, false, err
	}
	if (*pb.TxnResponse)(response).Size() > maxMutationBytes {
		return nil, false, ErrCorruptRecord
	}
	return points, response.Succeeded, nil
}

func (b *Backend) exactTaskArchive(kv *mvccpb.KeyValue, d *taskArchiveDraft) error {
	var r taskArchiveCommandRecord
	if decodeTaskArchiveCommand(kv, &r) != nil || string(kv.Value) != d.value || (d.revision > 0 && kv.ModRevision != d.revision) {
		return ErrConflict
	}
	return nil
}

// Even a dead original can recover committed diagnostics. If no original Put
// revision was known, an identical first-read ABA cannot be distinguished: pin
// that revision for subsequent diagnostics, but never manufacture Prepared.
func (b *Backend) arbitrateTaskArchive(ctx context.Context, c *TaskClaim, d *taskArchiveDraft, result TaskWorkspaceArchivePreparationResult) (TaskWorkspaceArchivePreparationResult, error) {
	key, err := b.namespace.taskWorkspaceArchiveKey(c.reference.Task)
	if err != nil {
		return result, err
	}
	cmps := b.taskArchiveComparisons(c, d)
	if err = b.preflightTaskArchive(cmps, key, d.value); err != nil {
		return result, err
	}
	request, cancel := b.requestContext(ctx)
	defer cancel()
	ops := b.commandIssuerPoints(key)
	response, err := b.client.Txn(request).If(cmps...).Then(ops...).Else(ops...).Commit()
	if err != nil || request.Err() != nil {
		return result, errors.Join(ErrOutcomeUnknown, err, request.Err())
	}
	points, err := b.stageEvidencePoints(response, []string{b.identityKey, b.restoreKey, key})
	if err != nil {
		return result, err
	}
	if points[2] == nil {
		if d.revision > 0 {
			d.lost = true
			return result, ErrConflict
		}
		return result, ErrOutcomeUnknown
	}
	if err = b.exactTaskArchive(points[2], d); err != nil {
		d.lost = true
		return result, err
	}
	d.revision = points[2].ModRevision
	result.Reference, result.Outcome = d.reference(), OutcomeCommitted
	if !response.Succeeded {
		return result, ErrConflict
	}
	if err = taskArchiveLive(ctx, c, d); err != nil {
		return result, err
	}
	if d.preparedArchive == nil {
		return result, ErrOutcomeUnknown
	}
	// Reuse only the same already issued handle, with fresh original pins/time.
	bounded, finish := context.WithDeadline(ctx, d.deadline)
	defer finish()
	current, done := b.taskObservationContext(bounded, c)
	defer done()
	if err = b.refreshTaskArchivePins(current, c, d); err != nil {
		return result, err
	}
	if err = b.verifyTaskArchive(current, c, d); err != nil {
		return result, err
	}
	if err = b.observeTaskArchiveEmpty(current, c, d); err != nil {
		return result, err
	}
	result.Prepared = d.preparedArchive
	return result, nil
}

// LoadTaskWorkspaceArchive returns structural diagnostics only. No history
// loader can manufacture the original instance-sealed preparation.
func (b *Backend) LoadTaskWorkspaceArchive(ctx context.Context, task TaskReference) (*TaskWorkspaceArchiveReference, error) {
	if b == nil || ctx == nil || b.validateTaskReference(task) != nil {
		return nil, ErrInvalidRecord
	}
	key, err := b.namespace.taskWorkspaceArchiveKey(task)
	if err != nil {
		return nil, err
	}
	request, cancel := b.requestContext(ctx)
	defer cancel()
	if err = b.preflightTaskArchive(b.baseComparisons(), key, string(make([]byte, maxRecordBytes))); err != nil {
		return nil, err
	}
	points, err := b.readQuiescencePoints(request, nil, []string{b.identityKey, b.restoreKey, key})
	if err != nil {
		return nil, err
	}
	if points[2] == nil {
		return nil, nil
	}
	var r taskArchiveCommandRecord
	if err = decodeTaskArchiveCommand(points[2], &r); err != nil {
		return nil, err
	}
	if r.FinalSync.Task.Reference != task {
		return nil, ErrCorruptRecord
	}
	return &TaskWorkspaceArchiveReference{Task: r.FinalSync.record().Task.Reference, CommandID: r.Claims.Context.Current.CommandID, FinalSyncRevision: r.FinalSyncRevision, Revision: points[2].ModRevision}, nil
}
