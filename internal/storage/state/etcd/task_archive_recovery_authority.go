package etcd

import (
	"context"
	"errors"
	"sync"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Historical records are values only. No old claim, draft or send capability is
// reconstructed. Every transaction retains the full NEW claim fences.
type taskArchiveRecoveryAuthority struct {
	backend     *Backend
	claim       *TaskClaim
	manifest    taskArchiveRecoveryManifest
	command     taskArchiveCommandRecord
	comparisons []clientv3.Cmp
	prefix      string
	nativeMu    sync.Mutex
}

func (b *Backend) pinTaskArchiveRecovery(ctx context.Context, c *TaskClaim, m taskArchiveRecoveryManifest, q *PreparedTaskQuiescence) (*taskArchiveRecoveryAuthority, error) {
	if !b.validTaskClaim(c) || ctx == nil || m.validate() != nil || m.State != "bound" || q == nil || q.self != q || q.origin != b || q.claim != c || q.claimReference != c.reference || m.Seed.Task != c.reference.Task || m.Seed.TaskBirth != c.birth || c.reference.CreateRevision <= m.Seed.ArchiveRevision || c.reference.CreateRevision <= m.Seed.ClaimRevision {
		return nil, ErrInvalidRecord
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	archiveKey, err := b.namespace.taskWorkspaceArchiveKey(c.reference.Task)
	if err != nil {
		return nil, err
	}
	finalKey, err := b.namespace.taskFinalSyncAttemptKey(c.reference.Task)
	if err != nil {
		return nil, err
	}
	points, err := b.readQuiescencePoints(ctx, c, []string{b.identityKey, b.restoreKey, archiveKey, finalKey})
	if err != nil {
		return nil, err
	}
	var command taskArchiveCommandRecord
	var final TaskFinalSyncAttemptRecord
	if decodeTaskArchiveCommand(points[2], &command) != nil || decodeTaskFinalSyncAttempt(points[3], &final) != nil || command.FinalSync.record() != final || points[2].CreateRevision != m.Seed.ArchiveRevision || points[3].CreateRevision != m.Seed.FinalSyncRevision || workspaceSyncIntentDigest(string(points[2].Value)) != m.Seed.ArchiveDigest || workspaceSyncIntentDigest(string(points[3].Value)) != m.Seed.FinalSyncDigest {
		return nil, ErrCorruptRecord
	}
	seed := m.Seed
	oldClaim, err := encodeTaskClaimRecord(taskClaimRecord{Version: 1, Task: seed.Task, ClaimID: seed.ClaimID, WorkerID: seed.WorkerID, LeaseID: seed.LeaseID})
	if err != nil {
		return nil, err
	}
	task, err := encodeTaskRecord(final.Task)
	if err != nil {
		return nil, err
	}
	oldRef := TaskClaimReference{Task: seed.Task, ClaimID: seed.ClaimID, WorkerID: seed.WorkerID, LeaseID: seed.LeaseID, CreateRevision: seed.ClaimRevision}
	if final.Claim != oldRef || final.Task != q.pin.task || final.Task.Reference != c.reference.Task || final.DestroyingRevision != c.birth || workspaceSyncIntentDigest(task) != seed.TaskDigest || workspaceSyncIntentDigest(oldClaim) != seed.ClaimDigest || final.AttemptID != seed.FinalSyncAttempt || final.AttemptID != seed.ArchiveCommand || final.Quiescence != q.pin.reference || final.QuiescenceRevision != q.pin.intent.mod || final.ReceiptDigest != command.Claims.Context.QuiescenceReceiptDigest || command.IssuerRevision != seed.IssuerRevision || workspaceSyncIntentDigest(string(command.Ticket)) != seed.TicketDigest {
		return nil, ErrCorruptRecord
	}
	plan, err := m.Binding.Plan.plan()
	if err != nil || plan.MatchesAttempt(final) != nil || plan.workspace.provider != "local" {
		return nil, ErrIdentityMismatch
	}
	local, err := encodeOperationMetadata(m.Binding.Local, 16384)
	if err != nil || workspaceSyncIntentDigest(local) != m.Binding.TargetDigest {
		return nil, ErrCorruptRecord
	}
	if err := b.validateTaskFinalSyncHistory(ctx, &final, &q.pin); err != nil {
		return nil, err
	}
	issuerKey, err := b.namespace.commandIssuerKey(command.Claims.Context.Current.IssuerCertificateID)
	if err != nil {
		return nil, err
	}
	issuerPoints, err := b.readQuiescencePoints(ctx, c, []string{b.identityKey, b.restoreKey, issuerKey})
	if err != nil {
		return nil, err
	}
	issuer, err := b.decodeCommandIssuer(issuerPoints[2])
	if err != nil || issuer == nil || issuer.Revision != seed.IssuerRevision || workspaceSyncIntentDigest(string(issuerPoints[2].Value)) != seed.IssuerDigest || string(issuer.Record.Certificate) != string(command.Certificate) {
		return nil, errors.Join(ErrCorruptRecord, err)
	}
	cmps := append(b.baseComparisons(), c.comparisons()...)
	for _, pin := range []struct {
		key string
		mod int64
	}{{q.pin.intent.key, q.pin.intent.mod}, {q.pin.receipt.key, q.pin.receipt.mod}, {q.pin.reservation.key, q.pin.reservation.revision}, {finalKey, seed.FinalSyncRevision}, {issuerKey, seed.IssuerRevision}, {archiveKey, seed.ArchiveRevision}} {
		cmps = append(cmps, clientv3.Compare(clientv3.ModRevision(pin.key), "=", pin.mod))
	}
	if len(cmps) != 50 {
		return nil, ErrInvalidRecord
	}
	prefix, err := b.taskOperationsPrefix(c.reference.Task)
	if err != nil {
		return nil, err
	}
	a := &taskArchiveRecoveryAuthority{backend: b, claim: c, manifest: m, command: command, comparisons: cmps, prefix: prefix}
	return a, c.live(ctx)
}

func (a *taskArchiveRecoveryAuthority) local(ctx context.Context) error {
	if a == nil || !a.backend.validTaskClaim(a.claim) || ctx == nil {
		return ErrInvalidRecord
	}
	a.claim.mu.Lock()
	defer a.claim.mu.Unlock()
	return a.claim.live(ctx)
}

func (a *taskArchiveRecoveryAuthority) guard(ctx context.Context) error {
	a.nativeMu.Lock()
	defer a.nativeMu.Unlock()
	if err := a.local(ctx); err != nil {
		return err
	}
	cmps := append([]clientv3.Cmp(nil), a.comparisons...)
	cmps = append(cmps, clientv3.Compare(clientv3.Version(a.prefix), "=", 0).WithRange(clientv3.GetPrefixRangeEnd(a.prefix)))
	b := a.backend
	keys := []string{b.identityKey}
	request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: taskProtoPoints(keys)}
	if err := taskCheckWire(b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{taskProtoResponse(keys, []string{b.identityValue})}); err != nil {
		return err
	}
	bounded, cancel := b.requestContext(ctx)
	r, err := b.client.Txn(bounded).If(cmps...).Then(clientv3.OpGet(b.identityKey)).Commit()
	requestErr := bounded.Err()
	cancel()
	if err != nil || requestErr != nil {
		return errors.Join(ErrOutcomeUnknown, err, requestErr)
	}
	if err := b.operationResponseHeader(r); err != nil {
		return err
	}
	if err := taskCheckWire(b.namespace, nil, []*pb.TxnResponse{(*pb.TxnResponse)(r)}); err != nil {
		return err
	}
	if !r.Succeeded || len(r.Responses) != 1 {
		return ErrConflict
	}
	if _, err := workspaceSyncPointResponse(b, r, keys); err != nil {
		return err
	}
	return a.local(ctx)
}

// Keep the historical context private to the registered recovery owner.
func (a *taskArchiveRecoveryAuthority) archiveContext() p.TaskWorkspaceArchiveContext {
	return a.command.Claims.Context
}
