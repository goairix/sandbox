package etcd

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// CreationClaim is an opaque capability for one original creation task Lease.
// Its reference records attribution and cannot reconstruct the capability.
type CreationClaim struct {
	origin                    *Backend
	reference                 CreationClaimReference
	claimKey, guardKey, value string
	comparisons               []clientv3.Cmp
	mu                        sync.Mutex
	deadline                  time.Time
	lost                      bool
}

// CreationClaimReference records attribution without granting mutation authority.
type CreationClaimReference struct {
	ClaimID, WorkerID, IntentID, SandboxID, WorkspaceHash, RestoreEpoch string
	Partition                                                           uint8
	Generation, DataGateEpoch, CreateRevision, LeaseID                  int64
}

// Reference returns an attribution copy; it cannot be used to adopt a Lease.
func (c *CreationClaim) Reference() CreationClaimReference {
	if c == nil {
		return CreationClaimReference{}
	}
	return c.reference
}

// ClaimCreation claims only a pending durable creation intent, never runtime execution.
func (b *Backend) ClaimCreation(ctx context.Context, w WorkspaceIdentity, intentID, workerID string, ttl time.Duration) (*CreationClaim, error) {
	if ctx == nil || !validDomainSegment(intentID) || !validDomainSegment(workerID) || ttl <= 0 || ttl > 24*time.Hour {
		return nil, ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := b.validateWorkspaceBinding(w); err != nil {
		return nil, err
	}
	ik, err := b.namespace.intentKey(w.Partition(), intentID)
	if err != nil {
		return nil, err
	}
	claimKey := ik + "/claim"
	bundle, err := b.readCreationBundle(ctx, w, intentID, claimKey)
	if err != nil {
		return nil, err
	}
	record := creationClaimRecord{Version: 1, ClaimID: uuid.NewString(), WorkerID: workerID, IntentID: intentID, SandboxID: bundle.intent.SandboxID, WorkspaceHash: w.Hash(), RestoreEpoch: b.restoreEpoch, Partition: w.Partition(), Generation: bundle.intent.Generation, DataGateEpoch: bundle.control.DataGateEpoch, LeaseID: 1}
	if bundle.claim != nil {
		existing, err := decodeCreationClaim(bundle.claim)
		if err != nil {
			return nil, err
		}
		if existing.IntentID != intentID || existing.SandboxID != record.SandboxID || existing.WorkspaceHash != record.WorkspaceHash || existing.RestoreEpoch != record.RestoreEpoch || existing.Partition != record.Partition || existing.Generation != record.Generation || existing.DataGateEpoch != record.DataGateEpoch {
			return nil, ErrCorruptRecord
		}
		return nil, ErrConflict
	}
	guardKey := ik + "/guards/" + record.ClaimID
	if !validNamespaceKey(b.namespace, claimKey) || !validNamespaceKey(b.namespace, guardKey) {
		return nil, ErrInvalidRecord
	}
	if _, err := encodeCreationClaim(record); err != nil {
		return nil, err
	}
	permanent := make([]clientv3.Cmp, 0, 18)
	for i, kv := range bundle.records {
		permanent = append(permanent, clientv3.Compare(clientv3.ModRevision(bundle.keys[i]), "=", kv.ModRevision), clientv3.Compare(clientv3.Value(bundle.keys[i]), "=", string(kv.Value)), clientv3.Compare(clientv3.LeaseValue(bundle.keys[i]), "=", 0))
	}
	// Preflight the future 24 comparisons against the existing stage budget.
	preflight := append([]clientv3.Cmp(nil), permanent...)
	for _, key := range []string{claimKey, guardKey} {
		preflight = append(preflight, clientv3.Compare(clientv3.Value(key), "=", ""), clientv3.Compare(clientv3.LeaseValue(key), "=", 1), clientv3.Compare(clientv3.CreateRevision(key), "=", 1))
	}
	if _, _, err := prepareMutation(b.namespace, Mutation{Comparisons: preflight, Writes: []Write{{Key: ik + "/checkpoint", Value: []byte("check")}}}); err != nil {
		return nil, err
	}
	seconds := int64(ttl / time.Second)
	if ttl%time.Second != 0 {
		seconds++
	}
	grantCtx, cancel := b.requestContext(ctx)
	sent := time.Now()
	lease, err := b.client.Grant(grantCtx, seconds)
	cancel()
	known := lease != nil && lease.ResponseHeader != nil && lease.ClusterId == b.clusterID && lease.ID != 0
	cleanup := func(cause error) (*CreationClaim, error) {
		if known {
			return nil, errors.Join(cause, b.revokeStageLease(lease.ID))
		}
		return nil, cause
	}
	if err != nil {
		return cleanup(fmt.Errorf("%w: grant creation lease: %w", ErrOutcomeUnknown, err))
	}
	if !known {
		return cleanup(ErrIdentityMismatch)
	}
	if err := ctx.Err(); err != nil {
		return cleanup(err)
	}
	if lease.ID <= 0 || lease.TTL <= 0 || lease.TTL > 86400 {
		return cleanup(ErrGuardExpired)
	}
	deadline := sent.Add(time.Duration(lease.TTL) * time.Second)
	if !time.Now().Before(deadline) {
		return cleanup(ErrGuardExpired)
	}
	record.LeaseID = int64(lease.ID)
	value, err := encodeCreationClaim(record)
	if err != nil {
		return cleanup(err)
	}
	comparisons := append(b.baseComparisons(), permanent...)
	comparisons = append(comparisons, clientv3.Compare(clientv3.CreateRevision(claimKey), "=", 0), clientv3.Compare(clientv3.CreateRevision(guardKey), "=", 0))
	requestCtx, cancel := b.requestContext(ctx)
	response, err := b.client.Txn(requestCtx).If(comparisons...).Then(clientv3.OpPut(claimKey, value, clientv3.WithLease(lease.ID)), clientv3.OpPut(guardKey, value, clientv3.WithLease(lease.ID))).Else(clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey)).Commit()
	cancel()
	if err != nil {
		return cleanup(fmt.Errorf("%w: create claim: %w", ErrOutcomeUnknown, err))
	}
	if response == nil || response.Header == nil || response.Header.ClusterId != b.clusterID {
		return cleanup(ErrIdentityMismatch)
	}
	if !response.Succeeded {
		if err := b.creationResponseIdentity(response); err != nil {
			return cleanup(err)
		}
		return cleanup(ErrConflict)
	}
	if response.Header.Revision <= 0 || len(response.Responses) != 2 || response.Responses[0] == nil || response.Responses[1] == nil || response.Responses[0].GetResponsePut() == nil || response.Responses[1].GetResponsePut() == nil {
		return cleanup(ErrOutcomeUnknown)
	}
	if !time.Now().Before(deadline) {
		return cleanup(ErrGuardExpired)
	}
	if err := ctx.Err(); err != nil {
		return cleanup(err)
	}
	c := &CreationClaim{origin: b, reference: record.reference(response.Header.Revision), claimKey: claimKey, guardKey: guardKey, value: value, deadline: deadline, comparisons: permanent}
	for _, key := range []string{claimKey, guardKey} {
		c.comparisons = append(c.comparisons, clientv3.Compare(clientv3.Value(key), "=", value), clientv3.Compare(clientv3.LeaseValue(key), "=", int64(lease.ID)), clientv3.Compare(clientv3.CreateRevision(key), "=", response.Header.Revision))
	}
	return c, nil
}
func (b *Backend) creationResponseIdentity(response *clientv3.TxnResponse) error {
	if response == nil || response.Header == nil || response.Header.ClusterId != b.clusterID || len(response.Responses) != 2 {
		return ErrIdentityMismatch
	}
	for i, key := range []string{b.identityKey, b.restoreKey} {
		if response.Responses[i] == nil {
			return ErrIdentityMismatch
		}
		r := response.Responses[i].GetResponseRange()
		if r == nil || r.More || r.Count != 1 || len(r.Kvs) != 1 || r.Kvs[0] == nil || string(r.Kvs[0].Key) != key || r.Kvs[0].Lease != 0 {
			return ErrIdentityMismatch
		}
	}
	return b.validateResponseIdentity(response)
}

// creationClaimComparisons returns private copies of the original server fences.
// Losing the local capability cannot retract copies already attached to a Stage.
func (b *Backend) creationClaimComparisons(c *CreationClaim) ([]clientv3.Cmp, error) {
	if c == nil || c.origin != b {
		return nil, ErrInvalidRecord
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkLive(); err != nil {
		return nil, err
	}
	return c.copyComparisons(), nil
}
func (c *CreationClaim) checkLive() error {
	if c.lost || !time.Now().Before(c.deadline) {
		c.lost = true
		return ErrGuardExpired
	}
	return nil
}
func (c *CreationClaim) copyComparisons() []clientv3.Cmp {
	result := make([]clientv3.Cmp, len(c.comparisons))
	for i, comparison := range c.comparisons {
		result[i] = copyComparison(comparison)
	}
	return result
}

// RenewCreationClaim renews only the original Lease after rechecking every fence.
// An unknown renew permanently stops this local capability without deciding Stage outcomes.
func (b *Backend) RenewCreationClaim(ctx context.Context, c *CreationClaim) error {
	if ctx == nil || c == nil || c.origin != b {
		return ErrInvalidRecord
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.checkLive(); err != nil {
		return err
	}
	fail := func(err error) error { c.lost = true; return err }
	requestCtx, cancel := b.requestContext(ctx)
	compares := append(b.baseComparisons(), c.copyComparisons()...)
	response, err := b.client.Txn(requestCtx).If(compares...).Then().Else(clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey)).Commit()
	cancel()
	if err != nil {
		return fail(fmt.Errorf("%w: validate creation claim: %w", ErrOutcomeUnknown, err))
	}
	if response == nil || response.Header == nil || response.Header.ClusterId != b.clusterID {
		return fail(ErrIdentityMismatch)
	}
	if !response.Succeeded {
		if err := b.creationResponseIdentity(response); err != nil {
			return fail(err)
		}
		return fail(ErrGuardExpired)
	}
	if len(response.Responses) != 0 {
		return fail(ErrOutcomeUnknown)
	}
	if err := c.checkLive(); err != nil {
		return err
	}
	requestCtx, cancel = b.requestContext(ctx)
	sent := time.Now()
	kept, err := b.client.KeepAliveOnce(requestCtx, clientv3.LeaseID(c.reference.LeaseID))
	cancel()
	if err != nil {
		return fail(fmt.Errorf("%w: renew creation lease: %w", ErrOutcomeUnknown, err))
	}
	if kept == nil || kept.ResponseHeader == nil || kept.ClusterId != b.clusterID {
		return fail(ErrIdentityMismatch)
	}
	if kept.ID != clientv3.LeaseID(c.reference.LeaseID) || kept.TTL <= 0 || kept.TTL > 86400 {
		return fail(ErrGuardExpired)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := c.checkLive(); err != nil {
		return err
	}
	deadline := sent.Add(time.Duration(kept.TTL) * time.Second)
	if !time.Now().Before(deadline) {
		return fail(ErrGuardExpired)
	}
	c.deadline = deadline
	return nil
}

// ReleaseCreationClaim stops the local capability and revokes only its original Lease.
// It preserves permanent ownership and permits cleanup after local capability loss.
func (b *Backend) ReleaseCreationClaim(ctx context.Context, c *CreationClaim) error {
	if ctx == nil || c == nil || c.origin != b {
		return ErrInvalidRecord
	}
	c.mu.Lock()
	c.lost = true
	c.mu.Unlock()
	requestCtx, cancel := b.requestContext(ctx)
	defer cancel()
	_, err := b.client.Revoke(requestCtx, clientv3.LeaseID(c.reference.LeaseID))
	if errors.Is(err, rpctypes.ErrLeaseNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: release creation lease: %w", ErrOutcomeUnknown, err)
	}
	return nil
}
