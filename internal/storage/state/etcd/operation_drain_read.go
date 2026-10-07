package etcd

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const taskOperationsPageSize = 16
const taskOperationsPageBytes = 80 * 1024

// TaskOperationEntry is copied diagnostic metadata, never an operation capability.
type TaskOperationEntry struct {
	Key, Value                           []byte
	Record                               OperationRecord
	CreateRevision, ModRevision, LeaseID int64
}

// Count describes the remaining requested native range, not a fleet count or
// an empty-prefix proof. Pages at different current revisions are not a snapshot.
type TaskOperationsPage struct {
	Entries         []TaskOperationEntry
	Next            *TaskOperationsCursor
	Revision, Count int64
	More            bool
}

// TaskOperationsCursor cannot be serialized or reconstructed from diagnostics.
// Its immutable position belongs only to the original in-memory claim.
type TaskOperationsCursor struct {
	self            *TaskOperationsCursor
	origin          *Backend
	claim           *TaskClaim
	task            TaskRecord
	birth, revision int64
	lastKey         string
}

func (b *Backend) taskOperationsPrefix(ref TaskReference) (string, error) {
	if err := b.validateTaskReference(ref); err != nil {
		return "", err
	}
	k, err := b.namespace.Key("p", fmt.Sprintf("%02x", ref.Partition), "sandboxes", ref.SandboxID, "operations")
	return k + "/", err
}
func (b *Backend) taskObservationContext(ctx context.Context, c *TaskClaim) (context.Context, context.CancelFunc) {
	bounded, cancel := context.WithDeadline(ctx, c.deadline)
	stop := context.AfterFunc(c.parentCtx, cancel)
	request, finish := b.requestContext(bounded)
	return request, func() { stop(); finish(); cancel() }
}
func (b *Backend) ObserveTaskOperationsPage(ctx context.Context, c *TaskClaim, cursor *TaskOperationsCursor) (TaskOperationsPage, error) {
	if !b.validTaskClaim(c) {
		return TaskOperationsPage{}, ErrInvalidRecord
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.live(ctx); err != nil {
		return TaskOperationsPage{}, err
	}
	task, err := taskCloseOriginalTask(c)
	if err != nil {
		return TaskOperationsPage{}, err
	}
	prefix, err := b.taskOperationsPrefix(c.reference.Task)
	if err != nil {
		return TaskOperationsPage{}, err
	}
	start, min := prefix, int64(0)
	if cursor != nil {
		if cursor.self != cursor || cursor.origin != b || cursor.claim != c || cursor.task != task || cursor.birth != c.birth || cursor.revision <= 0 || !strings.HasPrefix(cursor.lastKey, prefix) {
			return TaskOperationsPage{}, ErrInvalidRecord
		}
		start, min = cursor.lastKey+"\x00", cursor.revision
	}
	bounded, cancel := b.taskObservationContext(ctx, c)
	defer cancel()
	cmps := append(b.baseComparisons(), c.comparisons()...)
	page, err := b.taskOperationsRange(bounded, c, task, prefix, start, taskOperationsPageSize, min, cmps)
	if err != nil {
		return TaskOperationsPage{}, err
	}
	if page.More {
		next := &TaskOperationsCursor{origin: b, claim: c, task: task, birth: c.birth, revision: page.Revision, lastKey: string(page.Entries[len(page.Entries)-1].Key)}
		next.self = next
		page.Next = next
	}
	return page, nil
}
func (b *Backend) taskOperationsRange(ctx context.Context, c *TaskClaim, task TaskRecord, prefix, start string, limit int64, min int64, cmps []clientv3.Cmp) (TaskOperationsPage, error) {
	if err := c.live(ctx); err != nil {
		return TaskOperationsPage{}, err
	}
	op := clientv3.OpGet(start, clientv3.WithRange(clientv3.GetPrefixRangeEnd(prefix)), clientv3.WithLimit(limit), clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend))
	response, err := b.client.Txn(ctx).If(cmps...).Then(op).Else(op).Commit()
	if live := c.live(ctx); live != nil {
		return TaskOperationsPage{}, live
	}
	if err != nil {
		return TaskOperationsPage{}, fmt.Errorf("%w: operation observation: %w", ErrOutcomeUnknown, err)
	}
	page, err := b.decodeTaskOperationsPage(response, task, c.birth, prefix, start, limit, min)
	if err != nil {
		return TaskOperationsPage{}, err
	}
	if !response.Succeeded {
		c.lost = true
		return TaskOperationsPage{}, ErrConflict
	}
	return page, nil
}
func (b *Backend) decodeTaskOperationsPage(response *clientv3.TxnResponse, task TaskRecord, birth int64, prefix, start string, limit int64, min int64) (TaskOperationsPage, error) {
	fail := func() (TaskOperationsPage, error) { return TaskOperationsPage{}, ErrCorruptRecord }
	if limit < 1 || limit > 16 || b.operationResponseHeader(response) != nil || response.Header.Revision < min || len(response.Responses) != 1 || response.Responses[0] == nil {
		return fail()
	}
	r := response.Responses[0].GetResponseRange()
	if r == nil || !operationNestedHeader(r.Header, response.Header) || len(r.Kvs) > int(limit) || r.Count < int64(len(r.Kvs)) || r.More != (r.Count > int64(len(r.Kvs))) || (r.More && len(r.Kvs) != int(limit)) || (*pb.TxnResponse)(response).Size() > taskOperationsPageBytes {
		return fail()
	}
	result := TaskOperationsPage{Revision: response.Header.Revision, Count: r.Count, More: r.More}
	// Charge the native envelope plus the additional owned key/value copies.
	// Decoded bounded records cannot allocate in proportion to native Count.
	used, last := (*pb.TxnResponse)(response).Size(), ""
	for _, kv := range r.Kvs {
		if kv == nil {
			return fail()
		}
		key := string(kv.Key)
		used += len(kv.Key) + len(kv.Value)
		if used > taskOperationsPageBytes || !strings.HasPrefix(key, prefix) || key < start || (last != "" && key <= last) || kv.ModRevision > response.Header.Revision || kv.CreateRevision >= birth {
			return fail()
		}
		var record OperationRecord
		if decodeOperationRecord(kv, &record) != nil {
			return fail()
		}
		expected, _, _, _, err := b.namespace.operationKeys(record.Reference)
		ref := record.Reference
		if err != nil || expected != key || ref.Namespace != task.Reference.Namespace || ref.RestoreEpoch != task.Reference.RestoreEpoch || ref.Partition != task.Reference.Partition || ref.SandboxID != task.Reference.SandboxID || record.WorkspaceHash != task.WorkspaceHash || record.IntentID != task.CreationIntentID || record.Generation != task.Generation || record.DataGateEpoch != task.DataGateEpoch || record.Runtime != task.Runtime || record.ControlRevision > task.ControlRevision || kv.CreateRevision <= record.ControlRevision {
			return fail()
		}
		if record.ControlRevision == task.ControlRevision && (record.Snapshot != task.Snapshot || !record.ExpiresAt.Equal(task.ExpiresAt)) {
			return fail()
		}
		result.Entries = append(result.Entries, TaskOperationEntry{Key: bytes.Clone(kv.Key), Value: bytes.Clone(kv.Value), Record: record, CreateRevision: kv.CreateRevision, ModRevision: kv.ModRevision, LeaseID: kv.Lease})
		last = key
	}
	return result, nil
}

// observeTaskOperationsEmpty is only the read-only metadata half of Task4's
// future prerequisite. The caller must freshly authenticate terminal target
// evidence around this call. A true result itself grants no further authority.
func (b *Backend) observeTaskOperationsEmpty(ctx context.Context, c *TaskClaim, entry *TaskQuiescenceEntry) (revision int64, empty bool, err error) {
	if !b.validTaskClaim(c) || entry == nil || entry.Record == nil {
		return 0, false, ErrInvalidRecord
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err = c.live(ctx); err != nil {
		return 0, false, err
	}
	task, err := taskCloseOriginalTask(c)
	if err != nil {
		return 0, false, err
	}
	bounded, cancel := b.taskObservationContext(ctx, c)
	defer cancel()
	fresh, err := b.loadTaskQuiescence(bounded, c.reference.Task, c)
	if live := c.live(bounded); live != nil {
		return 0, false, live
	}
	if err != nil {
		return 0, false, err
	}
	if fresh == nil || fresh.Record == nil || fresh.Outcome != OutcomeCommitted || fresh.Reference != entry.Reference || fresh.Revision != entry.Revision || fresh.Record.Task != task || fresh.Record.Context.Current.ControlRevision != c.birth {
		return 0, false, ErrConflict
	}
	value, err := encodeTaskQuiescenceRecord(*fresh.Record)
	if err != nil {
		return 0, false, err
	}
	original, err := encodeTaskQuiescenceRecord(*entry.Record)
	if err != nil || original != value {
		return 0, false, ErrConflict
	}
	old := fresh.Record.Claim
	if c.reference.CreateRevision < old.CreateRevision || (c.reference.CreateRevision == old.CreateRevision && c.reference != old) {
		return 0, false, ErrConflict
	}
	key, _ := b.namespace.taskQuiescenceKey(c.reference.Task)
	_, receiptKey, err := b.stageKeys(fresh.Reference.Stage)
	if err != nil {
		return 0, false, err
	}
	if err = c.live(bounded); err != nil {
		return 0, false, err
	}
	points, err := b.readQuiescencePoints(bounded, c, []string{b.identityKey, b.restoreKey, key, receiptKey})
	if live := c.live(bounded); live != nil {
		return 0, false, live
	}
	if err != nil {
		return 0, false, err
	}
	var record TaskQuiescenceRecord
	if decodeTaskQuiescenceRecord(points[2], &record) != nil || points[2].CreateRevision != fresh.Revision || string(points[2].Value) != value {
		return 0, false, ErrCorruptRecord
	}
	stage, err := quiescenceCommittedReceipt(points[3], receiptKey, fresh.Revision, record.Attempt)
	if err != nil {
		return 0, false, err
	}
	if stage != fresh.Reference.Stage {
		return 0, false, ErrCorruptReceipt
	}
	intent := taskFence{key: key, value: value, create: fresh.Revision, mod: fresh.Revision}
	receipt := taskFence{key: receiptKey, value: string(points[3].Value), create: fresh.Revision, mod: fresh.Revision}
	cmps := taskEmptyComparisons(b, c, intent, receipt)
	prefix, err := b.taskOperationsPrefix(c.reference.Task)
	if err != nil {
		return 0, false, err
	}
	page, err := b.taskOperationsRange(bounded, c, task, prefix, prefix, 1, 0, cmps)
	if err != nil {
		return 0, false, err
	}
	return page.Revision, len(page.Entries) == 0, nil
}
func taskEmptyComparisons(b *Backend, c *TaskClaim, intent, receipt taskFence) []clientv3.Cmp {
	cmps := append(b.baseComparisons(), c.comparisons()...)
	cmps = append(cmps, intent.comparisons()...)
	return append(cmps, receipt.comparisons()...)
}
