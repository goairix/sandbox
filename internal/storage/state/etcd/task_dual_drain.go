package etcd

import (
	"context"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	t "github.com/goairix/sandbox/internal/runtime/controltransport"
	clientv3 "go.etcd.io/etcd/client/v3"
	"time"
)

// PreparedTaskQuiescence is a local prerequisite bound to its original current
// claim, immutable history and fixed observation context. A future flush/sync
// consumer MUST freshly revalidate those pins and observe the whole operations
// prefix again; neither this object nor its revision is a reusable zero permit.
type PreparedTaskQuiescence struct {
	self           *PreparedTaskQuiescence
	origin         *Backend
	claim          *TaskClaim
	claimReference TaskClaimReference
	deadline       time.Time
	pin            taskDualDrainPin
	receipt        p.TaskUserQuiescenceReceiptEvidence
	emptyRevision  int64
}

type taskDualDrainPin struct {
	reference       TaskQuiescenceReference
	task            TaskRecord
	intent, receipt taskFence
	reservation     taskQuiescenceReservationPin
}

// Caller owns c.mu; historical terminal evidence may support a later claimant's
// NEW local prerequisite, while never reconstructing old effect-send authority.
func validateTaskDualDrainHistory(c *TaskClaim, entry *TaskQuiescenceEntry) error {
	if c == nil || entry == nil || entry.Record == nil || entry.Outcome != OutcomeCommitted {
		return ErrConflict
	}
	task, err := taskCloseOriginalTask(c)
	if err != nil {
		return err
	}
	r := entry.Record
	if r.Task != task || r.Context.Current.ControlRevision != c.birth || c.reference.CreateRevision < r.Claim.CreateRevision || (c.reference.CreateRevision == r.Claim.CreateRevision && c.reference != r.Claim) {
		return ErrConflict
	}
	return nil
}

func (b *Backend) PrepareAfterDualDrain(ctx context.Context, c *TaskClaim, destination *t.Destination) (*PreparedTaskQuiescence, error) {
	return b.prepareAfterDualDrain(ctx, c, destination, nil)
}

// A nil expected preserves the original preparation path. A saved original
// instead constrains both terminal responses, all pins, and the whole call's
// deadline; it never substitutes its old emptyRevision for a new native Range.
func (b *Backend) prepareAfterDualDrain(ctx context.Context, c *TaskClaim, destination *t.Destination, expected *PreparedTaskQuiescence) (*PreparedTaskQuiescence, error) {
	if !b.validTaskClaim(c) || ctx == nil || b.taskQuiescenceVerifier == nil || b.authorityClock == nil {
		return nil, ErrInvalidRecord
	}
	// Merely presenting expired saved evidence must not mark a renewed claim
	// lost. Expiry/cancellation during the call retains c.live's existing
	// conservative lost-on-cancellation policy.
	if expected != nil && !time.Now().Before(expected.deadline) {
		return nil, ErrGuardExpired
	}
	c.mu.Lock()
	if err := c.live(ctx); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	deadline := c.deadline
	var expectedPin *taskDualDrainPin
	if expected != nil {
		deadline = expected.deadline
		expectedPin = &expected.pin
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	bounded, cancel := b.taskObservationContext(ctx, c)
	defer cancel()
	entry, pin, err := b.pinTaskDualDrain(bounded, c, expectedPin)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	first, err := b.queryTaskDualDrainLocked(bounded, c, destination, pin)
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if expected != nil && digestTaskQuiescenceProof(first.Wire()) != digestTaskQuiescenceProof(expected.receipt.Wire()) {
		return nil, ErrConflict
	}
	// The accepted helper owns c.mu and performs the NEW linearizable full-prefix
	// limit1 Range with 53 comparisons and two read branches (55 literal nodes).
	revision, empty, err := b.observeTaskOperationsEmpty(bounded, c, entry)
	if err != nil {
		return nil, err
	}
	if !empty {
		return nil, ErrConflict
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	second, err := b.queryTaskDualDrainLocked(bounded, c, destination, pin)
	if err != nil {
		return nil, err
	}
	if digestTaskQuiescenceProof(first.Wire()) != digestTaskQuiescenceProof(second.Wire()) {
		return nil, ErrConflict
	}
	if expected != nil && digestTaskQuiescenceProof(second.Wire()) != digestTaskQuiescenceProof(expected.receipt.Wire()) {
		return nil, ErrConflict
	}
	if _, _, err = b.pinTaskDualDrain(bounded, c, &pin); err != nil {
		return nil, err
	}
	if err = b.checkTaskDualDrainPins(bounded, c, pin); err != nil {
		return nil, err
	}
	if err = c.live(bounded); err != nil {
		return nil, err
	}
	if expected != nil {
		return nil, nil // Revalidation manufactures no replacement prerequisite.
	}
	// Queries and final checks do not manufacture a newer emptiness revision.
	prepared := &PreparedTaskQuiescence{origin: b, claim: c, claimReference: c.reference, deadline: deadline, pin: pin, receipt: second, emptyRevision: revision}
	prepared.self = prepared
	return prepared, nil
}

// Caller holds c.mu. Strict native common-birth history is freshly loaded;
// comparisons retain exact immutable bytes/revisions and original reservation.
func (b *Backend) pinTaskDualDrain(ctx context.Context, c *TaskClaim, expected *taskDualDrainPin) (*TaskQuiescenceEntry, taskDualDrainPin, error) {
	fail := func(err error) (*TaskQuiescenceEntry, taskDualDrainPin, error) { return nil, taskDualDrainPin{}, err }
	if err := c.live(ctx); err != nil {
		return fail(err)
	}
	entry, err := b.loadTaskQuiescence(ctx, c.reference.Task, c)
	if err != nil {
		return fail(err)
	}
	if err = validateTaskDualDrainHistory(c, entry); err != nil {
		return fail(err)
	}
	value, err := encodeTaskQuiescenceRecord(*entry.Record)
	if err != nil {
		return fail(err)
	}
	key, err := b.namespace.taskQuiescenceKey(c.reference.Task)
	if err != nil {
		return fail(err)
	}
	_, receiptKey, err := b.stageKeys(entry.Reference.Stage)
	if err != nil {
		return fail(err)
	}
	points, err := b.readQuiescencePoints(ctx, c, []string{b.identityKey, b.restoreKey, key, receiptKey})
	if err != nil {
		return fail(err)
	}
	var record TaskQuiescenceRecord
	if decodeTaskQuiescenceRecord(points[2], &record) != nil || points[2].CreateRevision != entry.Revision || string(points[2].Value) != value {
		return fail(ErrCorruptRecord)
	}
	stage, err := quiescenceCommittedReceipt(points[3], receiptKey, entry.Revision, record.Attempt)
	if err != nil {
		return fail(err)
	}
	if stage != entry.Reference.Stage || entry.reservation == nil {
		return fail(ErrCorruptReceipt)
	}
	reservationKey, err := b.namespace.taskQuiescenceAttemptKey(c.reference.Task)
	if err != nil {
		return fail(err)
	}
	pin := taskDualDrainPin{reference: entry.Reference, task: record.Task, intent: taskFence{key: key, value: value, create: entry.Revision, mod: entry.Revision}, receipt: taskFence{key: receiptKey, value: string(points[3].Value), create: entry.Revision, mod: entry.Revision}, reservation: taskQuiescenceReservationPin{key: reservationKey, revision: entry.reservation.Revision}}
	if expected != nil && pin != *expected {
		return fail(ErrConflict)
	}
	if err = c.live(ctx); err != nil {
		return fail(err)
	}
	return entry, pin, nil
}

// Fixed read-only original pins: 53 comparisons plus both bounded point reads.
// No effect, lease grant, checkpoint update, deletion or command replacement.
func (b *Backend) checkTaskDualDrainPins(ctx context.Context, c *TaskClaim, pin taskDualDrainPin) error {
	if err := c.live(ctx); err != nil {
		return err
	}
	response, err := b.client.Txn(ctx).If(taskEmptyComparisons(b, c, pin.intent, pin.receipt, pin.reservation)...).Then(clientv3.OpGet(b.identityKey)).Else(clientv3.OpGet(b.identityKey)).Commit()
	if live := c.live(ctx); live != nil {
		return live
	}
	if err != nil {
		return err
	}
	if err = b.validateFenceCheckResponse(response); err != nil {
		return err
	}
	if !response.Succeeded {
		c.lost = true
		return ErrConflict
	}
	return c.live(ctx)
}

func (b *Backend) queryTaskDualDrainLocked(ctx context.Context, c *TaskClaim, destination *t.Destination, pin taskDualDrainPin) (p.TaskUserQuiescenceReceiptEvidence, error) {
	empty := p.TaskUserQuiescenceReceiptEvidence{}
	if err := b.checkTaskDualDrainPins(ctx, c, pin); err != nil {
		return empty, err
	}
	result, err := b.QueryTaskQuiescence(ctx, pin.reference.Task, destination)
	if live := c.live(ctx); live != nil {
		return empty, live
	}
	if err != nil {
		return empty, err
	}
	if err = b.checkTaskDualDrainPins(ctx, c, pin); err != nil {
		return empty, err
	}
	if result.Reference != pin.reference || result.Receipt == nil || result.Accepted != nil {
		return empty, ErrDeliveryUnknown
	}
	return *result.Receipt, nil
}
