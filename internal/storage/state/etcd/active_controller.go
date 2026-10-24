package etcd

import (
	"context"
	"encoding/json"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func decodeController(raw []byte, id string) (state.ActiveSandboxControllerLease, error) {
	var lease state.ActiveSandboxControllerLease
	if json.Unmarshal(raw, &lease) != nil || lease.SandboxID != id || lease.Token == "" || lease.InstanceID == "" || lease.Generation <= 0 {
		return state.ActiveSandboxControllerLease{}, state.ErrActiveSandboxCorrupt
	}
	return lease, nil
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
	recordRead, err := r.store.client.Get(requestCtx, recordKey)
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
	if err != nil {
		return nil, false, err
	}
	lease.ExpiresAt = grantedAt.Add(ttl)
	raw, err := json.Marshal(lease)
	if err != nil {
		return nil, false, err
	}
	key := r.controllerKey(lease.SandboxID)
	response, err := r.store.client.Txn(requestCtx).
		If(clientv3.Compare(clientv3.ModRevision(recordKey), "=", recordRead.Kvs[0].ModRevision),
			clientv3.Compare(clientv3.Version(key), "=", 0)).
		Then(clientv3.OpPut(key, string(raw), clientv3.WithLease(leaseID))).
		Else(clientv3.OpGet(recordKey), clientv3.OpGet(key)).Commit()
	if err != nil {
		return nil, false, err
	}
	if response.Succeeded {
		return &lease, true, nil
	}
	_, _ = r.store.client.Revoke(requestCtx, leaseID)
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

// RenewController atomically moves the exact current holder to a new lease.
// An unknown transaction result never triggers a second renewal.
func (r *ActiveSandboxRepository) RenewController(ctx context.Context, lease state.ActiveSandboxControllerLease, ttl time.Duration) (*state.ActiveSandboxControllerLease, error) {
	if !validActiveID(lease.SandboxID) || lease.Token == "" || lease.InstanceID == "" || lease.Generation <= 0 || state.ValidateActiveSandboxTTL(ttl) != nil {
		return nil, state.ErrActiveSandboxCorrupt
	}
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	recordKey := r.recordKey(lease.SandboxID)
	recordRead, err := r.store.client.Get(requestCtx, recordKey)
	if err != nil {
		return nil, err
	}
	if len(recordRead.Kvs) == 0 {
		return nil, state.ErrActiveSandboxStaleToken
	}
	record, err := decodeActiveEnvelope(recordRead.Kvs[0].Value, lease.SandboxID)
	if err != nil {
		return nil, err
	}
	if record.Record.Generation != lease.Generation {
		return nil, state.ErrActiveSandboxStaleToken
	}
	key := r.controllerKey(lease.SandboxID)
	oldRead, err := r.store.client.Get(requestCtx, key)
	if err != nil {
		return nil, err
	}
	if len(oldRead.Kvs) == 0 {
		return nil, state.ErrActiveSandboxStaleToken
	}
	old, err := decodeController(oldRead.Kvs[0].Value, lease.SandboxID)
	if err != nil {
		return nil, err
	}
	if old.Token != lease.Token || old.Generation != lease.Generation || old.InstanceID != lease.InstanceID || old.PodUID != lease.PodUID {
		return nil, state.ErrActiveSandboxStaleToken
	}
	grantedAt := time.Now()
	leaseID, err := r.store.leaseFor(requestCtx, ttl)
	if err != nil {
		return nil, err
	}
	lease.ExpiresAt = grantedAt.Add(ttl)
	raw, err := json.Marshal(lease)
	if err != nil {
		return nil, err
	}
	response, err := r.store.client.Txn(requestCtx).
		If(clientv3.Compare(clientv3.ModRevision(recordKey), "=", recordRead.Kvs[0].ModRevision),
			clientv3.Compare(clientv3.ModRevision(key), "=", oldRead.Kvs[0].ModRevision)).
		Then(clientv3.OpPut(key, string(raw), clientv3.WithLease(leaseID))).Commit()
	if err != nil {
		return nil, err
	}
	if !response.Succeeded {
		_, _ = r.store.client.Revoke(requestCtx, leaseID)
		return nil, state.ErrActiveSandboxStaleToken
	}
	if oldLease := clientv3.LeaseID(oldRead.Kvs[0].Lease); oldLease != 0 {
		_, _ = r.store.client.Revoke(requestCtx, oldLease)
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
