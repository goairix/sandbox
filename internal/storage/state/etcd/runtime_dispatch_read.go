package etcd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"unicode/utf8"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

// RuntimeDispatchEntry is durable metadata evidence. Its reference allows exact
// attempt arbitration, but grants no capability to commit or invoke a runtime.
type RuntimeDispatchEntry struct {
	Record    RuntimeDispatchRecord
	Payload   json.RawMessage
	Reference StageReference
}

// LoadRuntimeDispatch uses two bounded, identity-fenced linearizable snapshots.
// The first locates the exact receipt; the second validates all three permanent
// records together. Absence carries no evidence about an external runtime.
func (b *Backend) LoadRuntimeDispatch(ctx context.Context, w WorkspaceIdentity, intentID string) (*RuntimeDispatchEntry, error) {
	if err := b.validateWorkspaceBinding(w); err != nil {
		return nil, err
	}
	dk, ik, err := b.namespace.runtimeDispatchKeys(w.Partition(), intentID)
	if err != nil {
		return nil, err
	}
	initial, err := b.readDomain(ctx, dk, ik)
	if err != nil {
		return nil, err
	}
	record, _, err := b.decodeRuntimeDispatchPair(initial[0], initial[1], w, intentID)
	if err != nil ||
		record == nil {
		return nil, err
	}
	rk, err := b.runtimeDispatchReceiptKey(record.Attempt)
	if err != nil {
		return nil, err
	}
	values, err := b.readRuntimeDomain(ctx, []string{dk, ik, rk}, map[int]bool{2: true})
	if err != nil {
		return nil, err
	}
	return b.decodeRuntimeDispatchEntry(values, w, intentID, dk, ik, rk, record.Attempt)
}

func (b *Backend) runtimeDispatchReceiptKey(l StageAttemptLocator) (string, error) {
	return b.namespace.Key("p", fmt.Sprintf("%02x", l.Partition), "stages", l.RequestID, l.StageID, l.AttemptID, "receipt")
}
func immutableDispatchKV(kv *mvccpb.KeyValue) bool {
	return kv != nil && kv.Lease == 0 && kv.CreateRevision > 0 && kv.CreateRevision == kv.ModRevision
}
func (b *Backend) decodeRuntimeDispatchPair(declaration, input *mvccpb.KeyValue, w WorkspaceIdentity, intentID string) (*RuntimeDispatchRecord, *RuntimeDispatchInputRecord, error) {
	if declaration == nil && input == nil {
		return nil, nil, nil
	}
	if !immutableDispatchKV(declaration) ||
		!immutableDispatchKV(input) ||
		declaration.CreateRevision != input.CreateRevision {
		return nil, nil, ErrCorruptRecord
	}
	var r RuntimeDispatchRecord
	if err := decodeDomainRecord(declaration, &r); err != nil {
		return nil, nil, err
	}
	if err := b.domainEpoch(r.RestoreEpoch); err != nil {
		return nil, nil, err
	}
	var i RuntimeDispatchInputRecord
	if err := decodeDomainRecord(input, &i); err != nil {
		return nil, nil, err
	}
	if err := b.domainEpoch(i.RestoreEpoch); err != nil {
		return nil, nil, err
	}
	if r.IntentID != intentID ||
		r.WorkspaceHash != w.Hash() ||
		r.Attempt.Namespace != b.namespace.Root() ||
		r.Attempt.Partition != w.Partition() ||
		i.IntentID != r.IntentID ||
		i.SandboxID != r.SandboxID ||
		i.WorkspaceHash != r.WorkspaceHash ||
		i.Generation != r.Generation ||
		i.RestoreEpoch != r.RestoreEpoch ||
		i.OperationID != r.OperationID ||
		i.PayloadDigest != r.PayloadDigest {
		return nil, nil, ErrCorruptRecord
	}
	return &r, &i, nil
}
func (b *Backend) decodeRuntimeDispatchEntry(values []*mvccpb.KeyValue, w WorkspaceIdentity, intentID, dk, ik, rk string, located StageAttemptLocator) (*RuntimeDispatchEntry, error) {
	if len(values) != 3 {
		return nil, ErrCorruptRecord
	}
	if values[0] == nil && values[1] == nil && values[2] == nil {
		return nil, nil
	}
	for j, key := range []string{dk, ik} {
		if values[j] != nil && string(values[j].Key) != key {
			return nil, ErrCorruptRecord
		}
	}
	r, i, err := b.decodeRuntimeDispatchPair(values[0], values[1], w, intentID)
	if err != nil {
		return nil, err
	}
	if r == nil ||
		values[2] == nil {
		return nil, ErrCorruptRecord
	}
	// A different declaration cannot borrow the receipt located by the first read.
	if r.Attempt != located {
		return nil, ErrCorruptRecord
	}
	if !immutableDispatchKV(values[2]) ||
		string(values[2].Key) != rk {
		return nil, ErrCorruptReceipt
	}
	if values[2].CreateRevision != values[0].CreateRevision {
		return nil, ErrCorruptRecord
	}
	ref, err := decodeRuntimeDispatchReceipt(values[2], r.Attempt)
	if err != nil {
		return nil, err
	}
	return &RuntimeDispatchEntry{Record: *r, Payload: append(json.RawMessage(nil), i.Payload...), Reference: ref}, nil
}
func decodeRuntimeDispatchReceipt(kv *mvccpb.KeyValue, l StageAttemptLocator) (StageReference, error) {
	if !immutableDispatchKV(kv) ||
		len(kv.Value) > maxRecordBytes ||
		!utf8.Valid(kv.Value) {
		return StageReference{}, ErrCorruptReceipt
	}
	var receipt stageReceipt
	if strictPreparationMetadata(kv.Value, reflect.TypeOf(receipt)) != nil {
		return StageReference{}, ErrCorruptReceipt
	}
	decoder := json.NewDecoder(bytes.NewReader(kv.Value))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&receipt) != nil ||
		decoder.Decode(new(any)) != io.EOF ||
		receipt.Version != 1 ||
		receipt.Outcome != OutcomeCommitted ||
		!validHexDigest(receipt.Digest) ||
		receipt.StageReference != l.reference(receipt.Digest) {
		return StageReference{}, ErrCorruptReceipt
	}
	return receipt.StageReference, nil
}

// readRuntimeDomain preserves exact point attribution for receipt envelopes.
func (b *Backend) readRuntimeDomain(ctx context.Context, keys []string, receipts map[int]bool) ([]*mvccpb.KeyValue, error) {
	values, err := b.readDomain(ctx, keys...)
	if err != nil {
		var point *domainPointReadError
		if errors.As(err, &point) && point.index >= 0 && point.index < len(keys) && receipts[point.index] && point.key == keys[point.index] {
			return nil, ErrCorruptReceipt
		}
	}
	return values, err
}
