package etcd

import (
	"context"
	"encoding/json"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// controllerRecord stores the requested TTL in nanoseconds alongside the
// public lease. Old records omit it and retain the grant-and-swap path; a
// changed TTL also needs a new server lease because KeepAlive preserves the
// original lease duration.
type controllerRecord struct {
	state.ActiveSandboxControllerLease
	RequestedTTL time.Duration `json:"requested_ttl_ns,omitempty"`
}

func decodeControllerRecord(raw []byte, id string) (controllerRecord, error) {
	var record controllerRecord
	if json.Unmarshal(raw, &record) != nil || record.SandboxID != id || record.Token == "" ||
		record.InstanceID == "" || record.Generation <= 0 || record.RequestedTTL < 0 {
		return controllerRecord{}, state.ErrActiveSandboxCorrupt
	}
	return record, nil
}

func decodeController(raw []byte, id string) (state.ActiveSandboxControllerLease, error) {
	record, err := decodeControllerRecord(raw, id)
	return record.ActiveSandboxControllerLease, err
}

// observeControllerStage reports one completed RPC. The observer receives no
// record keys or lease data and cannot change the transaction outcome.
func (s *Store) observeControllerStage(ctx context.Context, operation, stage, result string, started time.Time) {
	if s.controllerStageObserver != nil {
		s.controllerStageObserver(ctx, operation, stage, result, time.Since(started))
	}
}

func controllerStageResult(err error) string {
	if err != nil {
		return "error"
	}
	return "success"
}

// AcquireController claims the lifecycle controller only while no leased
// controller key exists. Its record generation is compared in the same
// transaction so an old controller cannot attach to a replacement sandbox.
func (r *ActiveSandboxRepository) AcquireController(ctx context.Context, lease state.ActiveSandboxControllerLease, ttl time.Duration) (*state.ActiveSandboxControllerLease, bool, error) {
	if !validActiveID(lease.SandboxID) || lease.Token == "" || lease.InstanceID == "" || lease.Generation <= 0 || state.ValidateActiveSandboxTTL(ttl) != nil {
		return nil, false, state.ErrActiveSandboxCorrupt
	}
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	recordKey := r.recordKey(lease.SandboxID)
	started := time.Now()
	recordRead, err := r.store.client.Get(requestCtx, recordKey)
	r.store.observeControllerStage(requestCtx, "acquire", "read", controllerStageResult(err), started)
	if err != nil {
		return nil, false, err
	}
	if len(recordRead.Kvs) == 0 {
		return nil, false, state.ErrActiveSandboxStaleToken
	}
	record, err := decodeActiveEnvelope(recordRead.Kvs[0].Value, lease.SandboxID)
	if err != nil {
		return nil, false, err
	}
	if record.Record.Generation != lease.Generation {
		return nil, false, state.ErrActiveSandboxStaleToken
	}
	grantedAt := time.Now()
	leaseID, err := r.store.leaseFor(requestCtx, ttl)
	r.store.observeControllerStage(requestCtx, "acquire", "grant", controllerStageResult(err), grantedAt)
	if err != nil {
		return nil, false, err
	}
	lease.ExpiresAt = grantedAt.Add(ttl)
	raw, err := json.Marshal(controllerRecord{ActiveSandboxControllerLease: lease, RequestedTTL: ttl})
	if err != nil {
		return nil, false, err
	}
	key := r.controllerKey(lease.SandboxID)
	started = time.Now()
	response, err := r.store.client.Txn(requestCtx).
		If(clientv3.Compare(clientv3.ModRevision(recordKey), "=", recordRead.Kvs[0].ModRevision),
			clientv3.Compare(clientv3.Version(key), "=", 0)).
		Then(clientv3.OpPut(key, string(raw), clientv3.WithLease(leaseID))).
		Else(clientv3.OpGet(recordKey), clientv3.OpGet(key)).Commit()
	result := controllerStageResult(err)
	if err == nil && !response.Succeeded {
		result = "conflict"
	}
	r.store.observeControllerStage(requestCtx, "acquire", "commit", result, started)
	if err != nil {
		return nil, false, err
	}
	if response.Succeeded {
		return &lease, true, nil
	}
	started = time.Now()
	_, revokeErr := r.store.client.Revoke(requestCtx, leaseID)
	r.store.observeControllerStage(requestCtx, "acquire", "revoke", controllerStageResult(revokeErr), started)
	if len(response.Responses) != 2 || response.Responses[0].GetResponseRange() == nil || response.Responses[1].GetResponseRange() == nil {
		return nil, false, state.ErrActiveSandboxCorrupt
	}
	current := response.Responses[0].GetResponseRange().Kvs
	if len(current) == 0 {
		return nil, false, state.ErrActiveSandboxStaleToken
	}
	currentRecord, err := decodeActiveEnvelope(current[0].Value, lease.SandboxID)
	if err != nil {
		return nil, false, err
	}
	if currentRecord.Record.Generation != lease.Generation {
		return nil, false, state.ErrActiveSandboxStaleToken
	}
	if len(response.Responses[1].GetResponseRange().Kvs) != 0 {
		return nil, false, nil
	}
	return nil, false, state.ErrActiveSandboxConflict
}

// RenewController compares the exact record and holder at one revision before
// updating the controller key. An unchanged TTL reuses its server lease; old
// records and changed TTLs grant a replacement lease. An unknown write or
// keepalive result never triggers a second renewal.
func (r *ActiveSandboxRepository) RenewController(ctx context.Context, lease state.ActiveSandboxControllerLease, ttl time.Duration) (*state.ActiveSandboxControllerLease, error) {
	if !validActiveID(lease.SandboxID) || lease.Token == "" || lease.InstanceID == "" || lease.Generation <= 0 || state.ValidateActiveSandboxTTL(ttl) != nil {
		return nil, state.ErrActiveSandboxCorrupt
	}
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	recordKey := r.recordKey(lease.SandboxID)
	key := r.controllerKey(lease.SandboxID)
	started := time.Now()
	read, err := r.store.client.Txn(requestCtx).
		Then(clientv3.OpGet(recordKey), clientv3.OpGet(key)).Commit()
	r.store.observeControllerStage(requestCtx, "renew", "read", controllerStageResult(err), started)
	if err != nil {
		return nil, err
	}
	if len(read.Responses) != 2 || read.Responses[0].GetResponseRange() == nil || read.Responses[1].GetResponseRange() == nil {
		return nil, state.ErrActiveSandboxCorrupt
	}
	records := read.Responses[0].GetResponseRange().Kvs
	controllers := read.Responses[1].GetResponseRange().Kvs
	if len(records) == 0 {
		return nil, state.ErrActiveSandboxStaleToken
	}
	record, err := decodeActiveEnvelope(records[0].Value, lease.SandboxID)
	if err != nil {
		return nil, err
	}
	if record.Record.Generation != lease.Generation {
		return nil, state.ErrActiveSandboxStaleToken
	}
	if len(controllers) == 0 {
		return nil, state.ErrActiveSandboxStaleToken
	}
	oldRecord, err := decodeControllerRecord(controllers[0].Value, lease.SandboxID)
	if err != nil {
		return nil, err
	}
	old := oldRecord.ActiveSandboxControllerLease
	if old.Token != lease.Token || old.Generation != lease.Generation || old.InstanceID != lease.InstanceID || old.PodUID != lease.PodUID {
		return nil, state.ErrActiveSandboxStaleToken
	}
	oldLeaseID := clientv3.LeaseID(controllers[0].Lease)
	reuseLease := oldLeaseID != 0 && oldRecord.RequestedTTL == ttl
	grantedAt := time.Now()
	leaseID := oldLeaseID
	if !reuseLease {
		leaseID, err = r.store.leaseFor(requestCtx, ttl)
		r.store.observeControllerStage(requestCtx, "renew", "grant", controllerStageResult(err), grantedAt)
		if err != nil {
			return nil, err
		}
	}
	lease.ExpiresAt = grantedAt.Add(ttl)
	raw, err := json.Marshal(controllerRecord{ActiveSandboxControllerLease: lease, RequestedTTL: ttl})
	if err != nil {
		return nil, err
	}
	started = time.Now()
	response, err := r.store.client.Txn(requestCtx).
		If(clientv3.Compare(clientv3.ModRevision(recordKey), "=", records[0].ModRevision),
			clientv3.Compare(clientv3.ModRevision(key), "=", controllers[0].ModRevision)).
		Then(clientv3.OpPut(key, string(raw), clientv3.WithLease(leaseID))).Commit()
	result := controllerStageResult(err)
	if err == nil && !response.Succeeded {
		result = "conflict"
	}
	r.store.observeControllerStage(requestCtx, "renew", "commit", result, started)
	if err != nil {
		return nil, err
	}
	if !response.Succeeded {
		if !reuseLease {
			started = time.Now()
			_, revokeErr := r.store.client.Revoke(requestCtx, leaseID)
			r.store.observeControllerStage(requestCtx, "renew", "revoke", controllerStageResult(revokeErr), started)
		}
		return nil, state.ErrActiveSandboxStaleToken
	}
	if reuseLease {
		// The CAS establishes the holder before renewal. If keepalive fails or
		// its reply is unknown, the caller fails closed; persisted ExpiresAt is
		// advisory and the server-side leased key remains authoritative.
		started = time.Now()
		kept, keepErr := r.store.client.KeepAliveOnce(requestCtx, oldLeaseID)
		r.store.observeControllerStage(requestCtx, "renew", "keepalive", controllerStageResult(keepErr), started)
		if keepErr != nil {
			return nil, keepErr
		}
		if kept == nil || kept.TTL <= 0 {
			return nil, state.ErrActiveSandboxLeaseExpired
		}
		// The local start precedes the server's renewal, so this advisory
		// deadline never assumes time gained during the RPC round trip.
		lease.ExpiresAt = started.Add(ttl)
		return &lease, nil
	}
	if oldLeaseID != 0 {
		started = time.Now()
		_, revokeErr := r.store.client.Revoke(requestCtx, oldLeaseID)
		r.store.observeControllerStage(requestCtx, "renew", "revoke", controllerStageResult(revokeErr), started)
	}
	return &lease, nil
}

// ReleaseController removes only the current holder. Historical absence is
// idempotent; a different live token is never released by an old caller.
func (r *ActiveSandboxRepository) ReleaseController(ctx context.Context, lease state.ActiveSandboxControllerLease) error {
	if !validActiveID(lease.SandboxID) || lease.Token == "" || lease.Generation <= 0 {
		return state.ErrActiveSandboxCorrupt
	}
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	key := r.controllerKey(lease.SandboxID)
	read, err := r.store.client.Get(requestCtx, key)
	if err != nil || len(read.Kvs) == 0 {
		return err
	}
	current, err := decodeController(read.Kvs[0].Value, lease.SandboxID)
	if err != nil {
		return err
	}
	if current.Token != lease.Token || current.Generation != lease.Generation {
		return state.ErrActiveSandboxStaleToken
	}
	response, err := r.store.client.Txn(requestCtx).
		If(clientv3.Compare(clientv3.ModRevision(key), "=", read.Kvs[0].ModRevision)).
		Then(clientv3.OpDelete(key)).Commit()
	if err != nil {
		return err
	}
	if !response.Succeeded {
		return state.ErrActiveSandboxStaleToken
	}
	return nil
}

// CheckpointController advances cleanup only while the exact controller key
// remains live. The record and leased holder are read in one etcd snapshot,
// then both revisions are compared in the write transaction. ExpiresAt is
// advisory metadata; the server-side lease key determines authority.
func (r *ActiveSandboxRepository) CheckpointController(ctx context.Context, lease state.ActiveSandboxControllerLease, expectedRevision uint64, checkpoint string) (*state.ActiveSandboxRecord, error) {
	if !validActiveID(lease.SandboxID) || lease.Token == "" || lease.InstanceID == "" || lease.Generation <= 0 ||
		expectedRevision == 0 || checkpoint == "" || len(checkpoint) > 1<<20 {
		return nil, state.ErrActiveSandboxCorrupt
	}

	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	recordKey := r.recordKey(lease.SandboxID)
	controllerKey := r.controllerKey(lease.SandboxID)
	read, err := r.store.client.Txn(requestCtx).
		Then(clientv3.OpGet(recordKey), clientv3.OpGet(controllerKey)).Commit()
	if err != nil {
		return nil, err
	}
	if len(read.Responses) != 2 || read.Responses[0].GetResponseRange() == nil || read.Responses[1].GetResponseRange() == nil {
		return nil, state.ErrActiveSandboxCorrupt
	}
	records := read.Responses[0].GetResponseRange().Kvs
	controllers := read.Responses[1].GetResponseRange().Kvs
	if len(controllers) == 0 {
		return nil, state.ErrActiveSandboxStaleToken
	}
	current, err := decodeController(controllers[0].Value, lease.SandboxID)
	if err != nil {
		return nil, err
	}
	if current.Token != lease.Token || current.InstanceID != lease.InstanceID || current.PodUID != lease.PodUID || current.Generation != lease.Generation {
		return nil, state.ErrActiveSandboxStaleToken
	}
	if len(records) == 0 {
		return nil, state.ErrActiveSandboxConflict
	}
	envelope, err := decodeActiveEnvelope(records[0].Value, lease.SandboxID)
	if err != nil {
		return nil, err
	}
	if envelope.Record.Generation != lease.Generation {
		return nil, state.ErrActiveSandboxStaleToken
	}
	if envelope.Record.Revision != expectedRevision ||
		(envelope.Record.Phase != state.ActiveSandboxDestroying && envelope.Record.Phase != state.ActiveSandboxCleanupPending) {
		return nil, state.ErrActiveSandboxConflict
	}

	previous := envelope
	envelope.Record.Phase = state.ActiveSandboxCleanupPending
	envelope.Record.Revision++
	envelope.Record.CleanupCheckpoint = checkpoint
	envelope.Record.UpdatedAt = time.Now().UTC()
	indexCompares, indexWrites, err := r.lifecycleIndexMutation(&previous, &envelope)
	if err != nil {
		return nil, err
	}
	next, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	compares := append([]clientv3.Cmp{
		clientv3.Compare(clientv3.ModRevision(recordKey), "=", records[0].ModRevision),
		clientv3.Compare(clientv3.ModRevision(controllerKey), "=", controllers[0].ModRevision),
	}, indexCompares...)
	writes := append([]clientv3.Op{clientv3.OpPut(recordKey, string(next))}, indexWrites...)
	written, err := r.store.client.Txn(requestCtx).
		If(compares...).Then(writes...).
		Else(clientv3.OpGet(controllerKey)).Commit()
	if err != nil {
		// The commit may have reached etcd. The caller must read back the
		// record and must not send a second checkpoint blindly.
		return nil, err
	}
	if !written.Succeeded {
		if len(written.Responses) != 1 || written.Responses[0].GetResponseRange() == nil {
			return nil, state.ErrActiveSandboxCorrupt
		}
		currentControllers := written.Responses[0].GetResponseRange().Kvs
		if len(currentControllers) == 0 || currentControllers[0].ModRevision != controllers[0].ModRevision {
			return nil, state.ErrActiveSandboxStaleToken
		}
		return nil, state.ErrActiveSandboxConflict
	}
	return &envelope.Record, nil
}
