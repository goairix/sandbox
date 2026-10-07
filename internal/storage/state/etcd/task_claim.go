package etcd

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// TaskClaim owns one original native Lease. Neither a copied struct nor a
// diagnostic reference can reconstruct it. It grants metadata authority only.
type TaskClaim struct {
	origin                                   *Backend
	self                                     *TaskClaim
	parentCtx                                context.Context
	reference                                TaskClaimReference
	fences                                   []taskFence
	claimKey, guardKey, checkpointKey, value string
	birth                                    int64
	initial                                  StageAttemptLocator
	ttlSeconds                               int64
	mu                                       sync.Mutex
	deadline                                 time.Time
	lost                                     bool
	closeDraft                               *taskCloseDraft
}

func (c *TaskClaim) Reference() TaskClaimReference {
	if c == nil {
		return TaskClaimReference{}
	}
	return c.reference
}
func (b *Backend) validTaskClaim(c *TaskClaim) bool {
	return b != nil && c != nil && c.self == c && c.origin == b && c.parentCtx != nil && c.reference.LeaseID > 0 && c.reference.CreateRevision > 0
}
func (c *TaskClaim) live(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidRecord
	}
	if err := c.parentCtx.Err(); err != nil {
		c.lost = true
		return err
	}
	if err := ctx.Err(); err != nil {
		c.lost = true
		return err
	}
	if c.lost || !time.Now().Before(c.deadline) {
		c.lost = true
		return ErrGuardExpired
	}
	return nil
}
func (c *TaskClaim) permanentComparisons() []clientv3.Cmp {
	var result []clientv3.Cmp
	for _, f := range c.fences {
		result = append(result, f.comparisons()...)
	}
	return result
}
func (c *TaskClaim) comparisons() []clientv3.Cmp {
	result := c.permanentComparisons()
	for _, key := range []string{c.claimKey, c.guardKey} {
		f := taskFence{key: key, value: c.value, lease: c.reference.LeaseID, create: c.reference.CreateRevision, mod: c.reference.CreateRevision}
		result = append(result, f.comparisons()...)
	}
	return result
}
func (c *TaskClaim) creationMutation(checkpoint taskFence) Mutation {
	cmps := append(c.permanentComparisons(), checkpoint.comparisons()...)
	for _, key := range []string{c.claimKey, c.guardKey} {
		cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	}
	return Mutation{Comparisons: cmps, Writes: []Write{{Key: c.claimKey, Value: []byte(c.value)}, {Key: c.guardKey, Value: []byte(c.value)}}}
}
func (b *Backend) preflightTaskClaim(c *TaskClaim, checkpoint taskFence) error {
	if _, _, err := prepareMutation(b.namespace, c.creationMutation(checkpoint)); err != nil {
		return err
	}
	// Cover the largest valid typed checkpoint before this claim's first Grant.
	in := TaskCheckpointInput{StageID: strings.Repeat("s", 128), ExpectedRevision: checkpoint.mod, State: TaskCheckpointNeedsReconciliation, DetailDigest: strings.Repeat("f", 64)}
	attempt := StageAttemptLocator{Namespace: c.reference.Task.Namespace, RestoreEpoch: c.reference.Task.RestoreEpoch, Partition: c.reference.Task.Partition, RequestID: c.reference.ClaimID, StageID: in.StageID, AttemptID: c.reference.ClaimID}
	mutation, err := c.checkpointMutation(checkpoint, in, attempt)
	if err != nil {
		return err
	}
	_, _, err = prepareMutation(b.namespace, mutation)
	return err
}
func (b *Backend) ClaimTask(ctx context.Context, ref TaskReference, workerID string, ttl time.Duration) (claim *TaskClaim, err error) {
	if ctx == nil || !validDomainSegment(workerID) {
		return nil, ErrInvalidRecord
	}
	if ttl <= 0 || ttl > 24*time.Hour {
		return nil, ErrInvalidMutation
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	bundle, err := b.loadTaskClaimBundle(ctx, ref)
	if err != nil {
		return nil, err
	}
	seconds := int64(ttl / time.Second)
	if ttl%time.Second != 0 {
		seconds++
	}
	// Maximal integers here are byte-budget placeholders, never native revisions
	// or published authority. Checked server values replace both before return.
	c := &TaskClaim{origin: b, parentCtx: ctx, reference: TaskClaimReference{Task: ref, ClaimID: uuid.NewString(), WorkerID: workerID, LeaseID: math.MaxInt64, CreateRevision: math.MaxInt64}, fences: bundle.fences, birth: bundle.birth, initial: bundle.initial, ttlSeconds: seconds}
	c.self = c
	c.claimKey, err = b.namespace.taskClaimKey(ref)
	if err != nil {
		return nil, err
	}
	c.guardKey, err = b.namespace.taskGuardKey(ref, c.reference.ClaimID)
	if err != nil {
		return nil, err
	}
	c.checkpointKey, err = b.namespace.taskCheckpointKey(ref)
	if err != nil {
		return nil, err
	}
	record := taskClaimRecord{Version: 1, Task: ref, ClaimID: c.reference.ClaimID, WorkerID: workerID, LeaseID: math.MaxInt64}
	c.value, err = encodeTaskClaimRecord(record)
	if err != nil {
		return nil, err
	}
	checkpoint := ownTaskFence(bundle.checkpoint)
	if err = b.preflightTaskClaim(c, checkpoint); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	request, cancel := b.requestContext(ctx)
	sent := time.Now()
	grant, grantErr := b.client.Grant(request, seconds)
	requestErr := request.Err()
	cancel()
	known := grant != nil && grant.ResponseHeader != nil && grant.ClusterId == b.clusterID && grant.Revision > 0 && grant.ID > 0
	if known {
		defer func() {
			if claim == nil {
				cleanup, cancel := b.requestContext(context.Background())
				defer cancel()
				err = errors.Join(err, b.revokeTaskLease(cleanup, grant.ID))
			}
		}()
	}
	if grantErr != nil || requestErr != nil {
		return nil, errors.Join(ErrOutcomeUnknown, grantErr, requestErr)
	}
	// Native etcd may raise a short request to its minimum Lease TTL.
	// Bound the actual grant, and retain it for original-Lease renewal checks.
	if !known || grant.Error != "" || grant.TTL <= 0 || grant.TTL > 86400 {
		return nil, ErrOutcomeUnknown
	}
	c.deadline = sent.Add(time.Duration(grant.TTL) * time.Second)
	c.ttlSeconds = grant.TTL
	c.reference.LeaseID = int64(grant.ID)
	if err = c.live(ctx); err != nil {
		return nil, err
	}
	record.LeaseID = int64(grant.ID)
	c.value, err = encodeTaskClaimRecord(record)
	if err != nil {
		return nil, err
	}
	mutation := c.creationMutation(checkpoint)
	if _, _, err = prepareMutation(b.namespace, mutation); err != nil {
		return nil, err
	}
	request, cancel = b.requestContext(ctx)
	response, err := b.client.Txn(request).If(append(b.baseComparisons(), mutation.Comparisons...)...).Then(clientv3.OpPut(c.claimKey, c.value, clientv3.WithLease(grant.ID)), clientv3.OpPut(c.guardKey, c.value, clientv3.WithLease(grant.ID))).Else(clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey)).Commit()
	requestErr = request.Err()
	cancel()
	if err != nil || requestErr != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err, requestErr)
	}
	if err = b.operationResponseHeader(response); err != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	if !response.Succeeded {
		if _, err = b.stageEvidencePoints(response, []string{b.identityKey, b.restoreKey}); err != nil {
			return nil, err
		}
		return nil, ErrConflict
	}
	if !operationPutResponses(response, 2) || response.Header.Revision <= c.birth {
		return nil, ErrOutcomeUnknown
	}
	c.reference.CreateRevision = response.Header.Revision
	if err = c.live(ctx); err != nil {
		return nil, err
	}
	if err = b.checkTaskClaimFences(ctx, c); err != nil {
		return nil, err
	}
	if err = c.live(ctx); err != nil {
		return nil, err
	}
	return c, nil
}
func (b *Backend) checkTaskClaimFences(ctx context.Context, c *TaskClaim) error {
	request, cancel := b.requestContext(ctx)
	// Both branches contain a linearizable Range; comparison-only transactions
	// with empty branches are not sufficient freshness evidence in native etcd.
	response, err := b.client.Txn(request).If(append(b.baseComparisons(), c.comparisons()...)...).Then(clientv3.OpGet(b.identityKey)).Else(clientv3.OpGet(b.identityKey)).Commit()
	requestErr := request.Err()
	cancel()
	if err != nil || requestErr != nil {
		return errors.Join(ErrOutcomeUnknown, err, requestErr)
	}
	if err = b.validateFenceCheckResponse(response); err != nil {
		return err
	}
	if !response.Succeeded {
		return ErrConflict
	}
	return nil
}
func (b *Backend) RenewTaskClaim(ctx context.Context, c *TaskClaim) (err error) {
	if !b.validTaskClaim(c) {
		return ErrInvalidRecord
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	defer func() {
		if err != nil {
			c.lost = true
		}
	}()
	if err = c.live(ctx); err != nil {
		return err
	}
	if err = b.checkTaskClaimFences(ctx, c); err != nil {
		return err
	}
	if err = c.live(ctx); err != nil {
		return err
	}
	request, cancel := b.requestContext(ctx)
	sent := time.Now()
	renewed, err := b.client.KeepAliveOnce(request, clientv3.LeaseID(c.reference.LeaseID))
	requestErr := request.Err()
	cancel()
	if err != nil || requestErr != nil {
		return errors.Join(ErrOutcomeUnknown, err, requestErr)
	}
	if renewed == nil || renewed.ResponseHeader == nil || renewed.ClusterId != b.clusterID || renewed.Revision <= 0 || int64(renewed.ID) != c.reference.LeaseID || renewed.TTL <= 0 || renewed.TTL > c.ttlSeconds {
		return ErrOutcomeUnknown
	}
	// The old deadline remains authoritative through post-renewal fence checks.
	if err = c.live(ctx); err != nil {
		return err
	}
	if err = b.checkTaskClaimFences(ctx, c); err != nil {
		return err
	}
	if err = c.live(ctx); err != nil {
		return err
	}
	deadline := sent.Add(time.Duration(renewed.TTL) * time.Second)
	if !time.Now().Before(deadline) {
		return ErrGuardExpired
	}
	c.deadline = deadline
	return nil
}
func (b *Backend) ReleaseTaskClaim(ctx context.Context, c *TaskClaim) error {
	if !b.validTaskClaim(c) {
		return ErrInvalidRecord
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lost = true
	if ctx == nil {
		return ErrInvalidRecord
	}
	request, cancel := b.requestContext(ctx)
	defer cancel()
	return b.revokeTaskLease(request, clientv3.LeaseID(c.reference.LeaseID))
}
func (b *Backend) revokeTaskLease(ctx context.Context, id clientv3.LeaseID) error {
	response, err := b.client.Revoke(ctx, id)
	if errors.Is(err, rpctypes.ErrLeaseNotFound) {
		return nil
	}
	if err != nil || ctx.Err() != nil {
		return errors.Join(ErrOutcomeUnknown, err, ctx.Err())
	}
	if response == nil || response.Header == nil || response.Header.ClusterId != b.clusterID || response.Header.Revision <= 0 {
		return ErrOutcomeUnknown
	}
	return nil
}
