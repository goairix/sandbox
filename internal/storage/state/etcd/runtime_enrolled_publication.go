package etcd

import (
	"context"
)

type runtimeEnrolledPublicationOwner struct {
	backend *Backend
	context context.Context
	cancel  context.CancelFunc
	done    chan struct{}
}

type runtimeEnrolledFrozenAttempt struct {
	inputDigest string
	reference   StageReference
	owner       *runtimeEnrolledPublicationOwner
}

func (b *Backend) registerEnrolledPublication(ctx context.Context) (*runtimeEnrolledPublicationOwner, error) {
	if ctx == nil || b == nil || b.enrolledPublication == nil || b.archiveLife == nil {
		return nil, ErrInvalidConfiguration
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if b.archiveLife.closed || len(b.archiveLife.enrolled) >= int(b.enrolledPublication.MaxActive) {
		return nil, ErrConflict
	}
	owned, cancel := context.WithCancel(ctx)
	o := &runtimeEnrolledPublicationOwner{backend: b, context: owned, cancel: cancel, done: make(chan struct{})}
	if b.archiveLife.enrolled == nil {
		b.archiveLife.enrolled = make(map[*runtimeEnrolledPublicationOwner]struct{})
	}
	b.archiveLife.enrolled[o] = struct{}{}
	return o, nil
}

func (o *runtimeEnrolledPublicationOwner) finish() {
	o.cancel()
	b := o.backend
	b.archiveLife.mu.Lock()
	delete(b.archiveLife.enrolled, o)
	close(o.done)
	b.archiveLife.mu.Unlock()
}

func (c *CreationClaim) freezeEnrolledPublication(input string, ref StageReference, owner *runtimeEnrolledPublicationOwner) (*runtimeEnrolledFrozenAttempt, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.enrolledAttempt != nil {
		if c.enrolledAttempt.inputDigest != input {
			return nil, false, ErrIdempotencyConflict
		}
		return c.enrolledAttempt, false, nil
	}
	a := &runtimeEnrolledFrozenAttempt{inputDigest: input, reference: ref, owner: owner}
	c.enrolledAttempt = a
	return a, true, nil
}
