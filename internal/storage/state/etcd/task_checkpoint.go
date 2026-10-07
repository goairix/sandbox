package etcd

import (
	"context"
	"errors"
	"time"
)

type TaskCheckpointInput struct {
	StageID          string
	ExpectedRevision int64
	State            TaskCheckpointState
	DetailDigest     string
}

func (c *TaskClaim) checkpointMutation(before taskFence, in TaskCheckpointInput, attempt StageAttemptLocator) (Mutation, error) {
	record := TaskCheckpointRecord{Version: 1, Reference: c.reference.Task, ClaimID: c.reference.ClaimID, DetailDigest: in.DetailDigest, State: in.State, Attempt: attempt}
	value, err := encodeTaskCheckpointRecord(record)
	if err != nil {
		return Mutation{}, err
	}
	return Mutation{Comparisons: append(c.comparisons(), before.comparisons()...), Writes: []Write{{Key: c.checkpointKey, Value: []byte(value)}}}, nil
}

// PrepareTaskCheckpoint records conservative metadata only. Every returned
// Stage still requires its original task claim/guard at native commit time;
// cancellation alone cannot retract an already prepared metadata transaction.
func (b *Backend) PrepareTaskCheckpoint(ctx context.Context, c *TaskClaim, in TaskCheckpointInput, ttl time.Duration) (*Stage, error) {
	if !b.validTaskClaim(c) {
		return nil, ErrInvalidRecord
	}
	if !validDomainSegment(in.StageID) || in.ExpectedRevision <= 0 || !validHexDigest(in.DetailDigest) || (in.State != TaskCheckpointPending && in.State != TaskCheckpointNeedsReconciliation) {
		return nil, ErrInvalidRecord
	}
	if ttl <= 0 || ttl > 24*time.Hour {
		return nil, ErrInvalidMutation
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.live(ctx); err != nil {
		return nil, err
	}
	if err := b.checkTaskClaimFences(ctx, c); err != nil {
		c.lost = true
		return nil, err
	}
	values, err := b.readDomain(ctx, c.checkpointKey)
	if err != nil {
		c.lost = true
		return nil, err
	}
	if _, err = decodeTaskCheckpointAt(values[0], c.reference.Task, c.birth, c.initial); err != nil {
		c.lost = true
		return nil, err
	}
	if values[0].ModRevision != in.ExpectedRevision {
		return nil, ErrConflict
	}
	before := ownTaskFence(values[0])
	if err = c.live(ctx); err != nil {
		return nil, err
	}
	stage, err := b.beginStageWithBuilder(ctx, c.reference.Task.Partition, c.reference.ClaimID, in.StageID, ttl, func(attempt StageAttemptLocator) (Mutation, error) { return c.checkpointMutation(before, in, attempt) })
	if err != nil {
		c.lost = true
		return nil, err
	}
	if err = c.live(ctx); err == nil {
		err = b.checkTaskClaimFences(ctx, c)
	}
	if err == nil {
		err = c.live(ctx)
	}
	if err != nil {
		c.lost = true
		cleanup, cancel := b.requestContext(context.Background())
		defer cancel()
		return nil, errors.Join(err, b.ReleaseStage(cleanup, stage))
	}
	return stage, nil
}
