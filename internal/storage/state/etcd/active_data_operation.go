package etcd

import (
	"context"
	"encoding/json"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

var _ state.ActiveDataOperationRepository = (*ActiveSandboxRepository)(nil)

// OpenDataOperation uses the existing admission transaction and record format.
// Its final read binds the local capability to the server's actual LeaseID.
func (r *ActiveSandboxRepository) OpenDataOperation(ctx context.Context, id, token string, ttl time.Duration) (*state.ActiveDataView, error) {
	_, operation, err := r.BeginOperation(ctx, id, token, state.ActiveOperationData, ttl)
	if err != nil {
		return nil, err
	}
	if operation == nil {
		return nil, state.ErrActiveSandboxNotFound
	}
	return r.readDataOperation(ctx, state.ActiveDataLease{Operation: *operation, RequestedTTL: ttl}, true, true, "open")
}

// ReadDataOperation reads lifecycle and participant at a single authoritative
// revision. requireActive=false is for draining existing users, not admission.
func (r *ActiveSandboxRepository) ReadDataOperation(ctx context.Context, lease state.ActiveDataLease, requireActive bool) (*state.ActiveDataView, error) {
	return r.readDataOperation(ctx, lease, requireActive, false, "read")
}

func (r *ActiveSandboxRepository) readDataOperation(ctx context.Context, lease state.ActiveDataLease, requireActive, bind bool, purpose string) (*state.ActiveDataView, error) {
	op := lease.Operation
	if !validActiveID(op.SandboxID) || op.Token == "" || op.Generation <= 0 || op.Kind != state.ActiveOperationData || (!bind && lease.LeaseID == 0) {
		return nil, state.ErrActiveSandboxStaleToken
	}
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	started := time.Now()
	read, err := r.store.client.Txn(requestCtx).Then(
		clientv3.OpGet(r.recordKey(op.SandboxID)),
		clientv3.OpGet(r.operationKey(op.SandboxID, op.Token)),
	).Commit()
	r.store.observeCoordinationStage(requestCtx, purpose, "read", err, started)
	if err != nil {
		return nil, err
	}
	if len(read.Responses) != 2 || read.Responses[0].GetResponseRange() == nil || read.Responses[1].GetResponseRange() == nil {
		return nil, state.ErrActiveSandboxCorrupt
	}
	records, operations := read.Responses[0].GetResponseRange().Kvs, read.Responses[1].GetResponseRange().Kvs
	if len(records) == 0 {
		return nil, state.ErrActiveSandboxNotFound
	}
	if len(records) != 1 || len(operations) != 1 {
		return nil, state.ErrActiveSandboxStaleToken
	}
	record, err := decodeActiveEnvelope(records[0].Value, op.SandboxID)
	if err != nil {
		return nil, err
	}
	stored := state.ActiveSandboxOperation{}
	if json.Unmarshal(operations[0].Value, &stored) != nil || stored.SandboxID != op.SandboxID || stored.Token != op.Token || stored.Generation != op.Generation || stored.Kind != state.ActiveOperationData || record.Record.Generation != op.Generation || operations[0].Lease == 0 || (!bind && operations[0].Lease != lease.LeaseID) {
		return nil, state.ErrActiveSandboxStaleToken
	}
	switch record.Record.Phase {
	case state.ActiveSandboxActive:
	case state.ActiveSandboxExclusive, state.ActiveSandboxDestroying, state.ActiveSandboxCleanupPending:
		if requireActive {
			return nil, state.ErrActiveSandboxAdmissionClosed
		}
	default:
		return nil, state.ErrActiveSandboxAdmissionClosed
	}
	transition, err := workspaceTransition(record.Record.Snapshot)
	if err != nil {
		return nil, err
	}
	if requireActive && transition {
		return nil, state.ErrActiveSandboxAdmissionClosed
	}
	lease.Operation = stored // ExpiresAt is advisory; the live leased key is authority.
	lease.LeaseID = operations[0].Lease
	return &state.ActiveDataView{Record: &record.Record, Lease: lease}, nil
}

// KeepAliveDataOperation never creates a replacement lease or rewrites the key.
// An unknown result is returned once; the caller must stop admitting work.
func (r *ActiveSandboxRepository) KeepAliveDataOperation(ctx context.Context, lease state.ActiveDataLease) (*state.ActiveDataView, time.Duration, error) {
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	view, err := r.readDataOperation(requestCtx, lease, false, false, "renew")
	if err != nil {
		return nil, 0, err
	}
	started := time.Now()
	kept, err := r.store.client.KeepAliveOnce(requestCtx, clientv3.LeaseID(lease.LeaseID))
	if err != nil {
		r.store.observeCoordinationStage(requestCtx, "renew", "keepalive", err, started)
		return nil, 0, err
	}
	if kept == nil || kept.ID != clientv3.LeaseID(lease.LeaseID) || kept.TTL <= 0 || kept.TTL > int64((1<<63-1)/time.Second) {
		r.store.observeCoordinationStage(requestCtx, "renew", "keepalive", state.ErrActiveSandboxStaleToken, started)
		return nil, 0, state.ErrActiveSandboxStaleToken
	}
	r.store.observeCoordinationStage(requestCtx, "renew", "keepalive", nil, started)
	return view, time.Duration(kept.TTL) * time.Second, nil
}
