package etcd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"reflect"
	"strings"
)

type taskQuiescencePrerequisite struct {
	entry           *TaskCloseDataEntry
	intent          taskFence
	receiptKey      string
	receiptRevision int64
	proof           []byte
}

// readQuiescencePoints is private to fixed task-derived discovery bundles. A
// claim-aware caller holds the original mutex and fixed parent/deadline context.
func (b *Backend) readQuiescencePoints(ctx context.Context, c *TaskClaim, keys []string) ([]*mvccpb.KeyValue, error) {
	if len(keys) < 3 || len(keys) > 6 || keys[0] != b.identityKey || keys[1] != b.restoreKey {
		return nil, ErrInvalidRecord
	}
	cmps := b.baseComparisons()
	if c != nil {
		if !b.validTaskClaim(c) {
			return nil, ErrInvalidRecord
		}
		if err := c.live(ctx); err != nil {
			return nil, err
		}
		cmps = append(cmps, c.comparisons()...)
	}
	ops := make([]clientv3.Op, len(keys))
	for i, k := range keys {
		ops[i] = clientv3.OpGet(k)
	}
	response, err := b.client.Txn(ctx).If(cmps...).Then(ops...).Else(ops...).Commit()
	if c != nil {
		if live := c.live(ctx); live != nil {
			return nil, live
		}
	} else if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("%w: quiescence history: %w", ErrOutcomeUnknown, err)
	}
	points, err := b.stageEvidencePoints(response, keys)
	if err != nil {
		return nil, err
	}
	if !response.Succeeded {
		if c != nil {
			c.lost = true
			return nil, ErrConflict
		}
		return nil, ErrIdentityMismatch
	}
	return points, nil
}
func (b *Backend) loadQuiescencePrerequisite(ctx context.Context, task TaskReference, c *TaskClaim) (*taskQuiescencePrerequisite, error) {
	key, err := b.namespace.taskCloseDataKey(task)
	if err != nil {
		return nil, err
	}
	initial, err := b.readQuiescencePoints(ctx, c, []string{b.identityKey, b.restoreKey, key})
	if err != nil {
		return nil, err
	}
	if initial[2] == nil {
		return nil, ErrConflict
	}
	var located TaskCloseDataRecord
	if err = decodeTaskCloseDataRecord(initial[2], &located); err != nil {
		return nil, err
	}
	if located.Task.Reference != task {
		return nil, ErrCorruptRecord
	}
	original, birth := bytes.Clone(initial[2].Value), initial[2].CreateRevision
	_, receiptKey, err := b.stageKeys(located.Attempt.reference(strings.Repeat("0", 64)))
	if err != nil {
		return nil, err
	}
	points, err := b.readQuiescencePoints(ctx, c, []string{b.identityKey, b.restoreKey, key, receiptKey})
	if err != nil {
		return nil, err
	}
	var record TaskCloseDataRecord
	if decodeTaskCloseDataRecord(points[2], &record) != nil || points[2].CreateRevision != birth || !bytes.Equal(points[2].Value, original) {
		return nil, ErrCorruptRecord
	}
	stage, err := quiescenceCommittedReceipt(points[3], receiptKey, birth, record.Attempt)
	if err != nil {
		return nil, err
	}
	ref := TaskCloseDataReference{Task: task, CommandID: record.Context.CommandID, Stage: stage}
	if ref.Validate() != nil {
		return nil, ErrCorruptRecord
	}
	entry := &TaskCloseDataEntry{Reference: ref, Record: &record, Revision: birth, Outcome: OutcomeCommitted}
	return &taskQuiescencePrerequisite{entry: entry, intent: taskFence{key: key, value: string(original), create: birth, mod: birth}, receiptKey: receiptKey, receiptRevision: points[3].ModRevision}, nil
}
func quiescenceCommittedReceipt(kv *mvccpb.KeyValue, key string, birth int64, attempt StageAttemptLocator) (StageReference, error) {
	if !immutableDispatchKV(kv) || string(kv.Key) != key || kv.CreateRevision != birth || len(kv.Value) > maxRecordBytes || !taskCloseJSON(kv.Value) || strictPreparationMetadata(kv.Value, reflect.TypeOf(stageReceipt{})) != nil {
		return StageReference{}, ErrCorruptReceipt
	}
	var r stageReceipt
	if json.Unmarshal(kv.Value, &r) != nil || !validHexDigest(r.Digest) || r.StageReference != attempt.reference(r.Digest) {
		return StageReference{}, ErrCorruptReceipt
	}
	outcome, err := decodeReceipt(kv, r.StageReference)
	if err != nil {
		return StageReference{}, err
	}
	if outcome != OutcomeCommitted {
		return StageReference{}, ErrCorruptReceipt
	}
	return r.StageReference, nil
}

// LoadTaskQuiescence returns committed structural history only. Both immutable
// intents and their original common-birth Stage receipts must remain coherent;
// neither stored signatures nor history can reconstruct original send authority.
func (b *Backend) LoadTaskQuiescence(ctx context.Context, task TaskReference) (*TaskQuiescenceEntry, error) {
	return b.loadTaskQuiescence(ctx, task, nil)
}
func (b *Backend) loadTaskQuiescence(ctx context.Context, task TaskReference, c *TaskClaim) (*TaskQuiescenceEntry, error) {
	if b == nil || ctx == nil {
		return nil, ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := b.validateTaskReference(task); err != nil {
		return nil, err
	}
	key, err := b.namespace.taskQuiescenceKey(task)
	if err != nil {
		return nil, err
	}
	bounded, cancel := b.requestContext(ctx)
	defer cancel()
	initial, err := b.readQuiescencePoints(bounded, c, []string{b.identityKey, b.restoreKey, key})
	if err != nil {
		return nil, err
	}
	if err = bounded.Err(); err != nil {
		return nil, err
	}
	if initial[2] == nil {
		return nil, nil
	}
	var located TaskQuiescenceRecord
	if err = decodeTaskQuiescenceRecord(initial[2], &located); err != nil {
		return nil, err
	}
	if located.Task.Reference != task {
		return nil, ErrCorruptRecord
	}
	original, birth := bytes.Clone(initial[2].Value), initial[2].CreateRevision
	_, receiptKey, err := b.stageKeys(located.Attempt.reference(strings.Repeat("0", 64)))
	if err != nil {
		return nil, err
	}
	prior, err := b.loadQuiescencePrerequisite(bounded, task, c)
	if err != nil {
		return nil, err
	}
	points, err := b.readQuiescencePoints(bounded, c, []string{b.identityKey, b.restoreKey, key, receiptKey, prior.intent.key, prior.receiptKey})
	if err != nil {
		return nil, err
	}
	if err = bounded.Err(); err != nil {
		return nil, err
	}
	var record TaskQuiescenceRecord
	if err = decodeTaskQuiescenceRecord(points[2], &record); err != nil {
		return nil, err
	}
	if points[2].CreateRevision != birth || !bytes.Equal(points[2].Value, original) {
		return nil, ErrCorruptRecord
	}
	stage, err := quiescenceCommittedReceipt(points[3], receiptKey, birth, record.Attempt)
	if err != nil {
		return nil, err
	}
	var old TaskCloseDataRecord
	if decodeTaskCloseDataRecord(points[4], &old) != nil || points[4].CreateRevision != record.Context.CloseDataIntentRevision || string(points[4].Value) != prior.intent.value || old.Context != record.Context.CloseDataContext || old.TicketDigest != record.Context.CloseDataTicketDigest {
		return nil, ErrCorruptRecord
	}
	if _, err = quiescenceCommittedReceipt(points[5], prior.receiptKey, points[4].CreateRevision, old.Attempt); err != nil {
		return nil, err
	}
	ref := TaskQuiescenceReference{Task: task, CommandID: record.Context.Current.CommandID, Stage: stage}
	if ref.Validate() != nil {
		return nil, ErrCorruptRecord
	}
	return &TaskQuiescenceEntry{Reference: ref, Record: &record, Revision: birth, Outcome: OutcomeCommitted}, nil
}
