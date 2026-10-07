package etcd

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
)

// LoadTaskCloseData discovers structural committed history by original task,
// including after worker restart. Neither absence nor history grants permission.
func (b *Backend) LoadTaskCloseData(ctx context.Context, task TaskReference) (*TaskCloseDataEntry, error) {
	if ctx == nil {
		return nil, ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := b.validateTaskReference(task); err != nil {
		return nil, err
	}
	key, err := b.namespace.taskCloseDataKey(task)
	if err != nil {
		return nil, err
	}
	bounded, cancel := b.requestContext(ctx)
	defer cancel()
	keys := []string{b.identityKey, b.restoreKey, key}
	initial, err := b.readExecEffectPoints(bounded, keys)
	if err != nil {
		return nil, err
	}
	if err = bounded.Err(); err != nil {
		return nil, err
	}
	if initial[2] == nil {
		return nil, nil
	}
	var located TaskCloseDataRecord
	if err = decodeTaskCloseDataRecord(initial[2], &located); err != nil {
		return nil, err
	}
	if located.Task.Reference != task {
		return nil, ErrCorruptRecord
	}
	original := bytes.Clone(initial[2].Value)
	birth := initial[2].CreateRevision
	// The placeholder is used only to derive a fixed key; it is never evidence or
	// returned authority. The committed receipt supplies its own checked digest.
	_, receiptKey, err := b.stageKeys(located.Attempt.reference(strings.Repeat("0", 64)))
	if err != nil {
		return nil, err
	}
	keys = append(keys, receiptKey)
	points, err := b.readExecEffectPoints(bounded, keys)
	if err != nil {
		return nil, err
	}
	if err = bounded.Err(); err != nil {
		return nil, err
	}
	kv := points[2]
	if kv == nil || kv.CreateRevision != birth || !bytes.Equal(kv.Value, original) {
		return nil, ErrCorruptRecord
	}
	var record TaskCloseDataRecord
	if err = decodeTaskCloseDataRecord(kv, &record); err != nil {
		return nil, err
	}
	receipt := points[3]
	if !immutableDispatchKV(receipt) || string(receipt.Key) != receiptKey || receipt.CreateRevision != birth || len(receipt.Value) > maxRecordBytes || !taskCloseJSON(receipt.Value) || strictPreparationMetadata(receipt.Value, reflect.TypeOf(stageReceipt{})) != nil {
		return nil, ErrCorruptReceipt
	}
	var decoded stageReceipt
	if json.Unmarshal(receipt.Value, &decoded) != nil || !validHexDigest(decoded.Digest) || decoded.StageReference != record.Attempt.reference(decoded.Digest) {
		return nil, ErrCorruptReceipt
	}
	outcome, err := decodeReceipt(receipt, decoded.StageReference)
	if err != nil {
		return nil, err
	}
	if outcome != OutcomeCommitted {
		return nil, ErrCorruptReceipt
	}
	ref := TaskCloseDataReference{Task: task, CommandID: record.Context.CommandID, Stage: decoded.StageReference}
	if ref.Validate() != nil {
		return nil, ErrCorruptRecord
	}
	return &TaskCloseDataEntry{Reference: ref, Record: &record, Revision: birth, Outcome: outcome}, nil
}
