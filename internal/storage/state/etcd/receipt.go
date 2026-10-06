package etcd

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type stageReceipt struct {
	Version uint32 `json:"version"`
	StageReference
	Outcome Outcome `json:"outcome"`
}

func encodeReceipt(ref StageReference, outcome Outcome) (string, error) {
	encoded, err := json.Marshal(stageReceipt{Version: 1, StageReference: ref, Outcome: outcome})
	if err == nil && len(encoded) > maxRecordBytes {
		return "", fmt.Errorf("%w: receipt byte budget exceeded", ErrInvalidMutation)
	}
	return string(encoded), err
}

func decodeReceipt(kv *mvccpb.KeyValue, ref StageReference) (Outcome, error) {
	expectedKey := fmt.Sprintf("%sp/%02x/stages/%s/%s/%s/receipt", ref.Namespace, ref.Partition, ref.RequestID, ref.StageID, ref.AttemptID)
	if !immutableDispatchKV(kv) || string(kv.Key) != expectedKey || len(kv.Value) > maxRecordBytes || !utf8.Valid(kv.Value) {
		return OutcomeUnknown, ErrCorruptReceipt
	}
	if err := strictPreparationMetadata(kv.Value, reflect.TypeOf(stageReceipt{})); err != nil {
		return OutcomeUnknown, fmt.Errorf("%w: %v", ErrCorruptReceipt, err)
	}
	var receipt stageReceipt
	decoder := json.NewDecoder(bytes.NewReader(kv.Value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return OutcomeUnknown, fmt.Errorf("%w: %v", ErrCorruptReceipt, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF || receipt.Version != 1 || receipt.StageReference != ref || (receipt.Outcome != OutcomeCommitted && receipt.Outcome != OutcomeAborted) {
		return OutcomeUnknown, ErrCorruptReceipt
	}
	return receipt.Outcome, nil
}

func (b *Backend) stageKeys(ref StageReference) (string, string, error) {
	if ref.Namespace != b.namespace.Root() || ref.RestoreEpoch != b.restoreEpoch {
		return "", "", ErrIdentityMismatch
	}
	digest, err := hex.DecodeString(ref.Digest)
	if err != nil || len(digest) != 32 || !validSegment(ref.RequestID) || !validSegment(ref.StageID) || len(ref.RequestID) > 128 || len(ref.StageID) > 128 {
		return "", "", ErrInvalidMutation
	}
	if id, err := uuid.Parse(ref.AttemptID); err != nil || id.String() != ref.AttemptID {
		return "", "", ErrInvalidMutation
	}
	partition := fmt.Sprintf("%02x", ref.Partition)
	guard, err := b.namespace.Key("p", partition, "attempts", ref.AttemptID, "guard")
	if err != nil {
		return "", "", err
	}
	receipt, err := b.namespace.Key("p", partition, "stages", ref.RequestID, ref.StageID, ref.AttemptID, "receipt")
	if err != nil {
		return "", "", err
	}
	if len(guard) > maxKeyBytes || len(receipt) > maxKeyBytes {
		return "", "", ErrInvalidMutation
	}
	return guard, receipt, nil
}

// ResolveStage atomically arbitrates this metadata attempt with a permanent
// aborted marker, or returns its existing durable receipt. It does not undo any
// external effect and never converts an absent read into a failure assumption.
func (b *Backend) ResolveStage(ctx context.Context, ref StageReference) (Outcome, error) {
	if ctx == nil {
		return OutcomeUnknown, ErrInvalidMutation
	}
	_, receiptKey, err := b.stageKeys(ref)
	if err != nil {
		return OutcomeUnknown, err
	}
	aborted, err := encodeReceipt(ref, OutcomeAborted)
	if err != nil {
		return OutcomeUnknown, err
	}
	requestCtx, cancel := b.requestContext(ctx)
	defer cancel()
	compares := append(b.baseComparisons(), clientv3.Compare(clientv3.CreateRevision(receiptKey), "=", 0))
	response, err := b.client.Txn(requestCtx).If(compares...).Then(clientv3.OpPut(receiptKey, aborted)).Else(clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey), clientv3.OpGet(receiptKey)).Commit()
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("%w: resolve stage: %w", ErrOutcomeUnknown, err)
	}
	if err := b.operationResponseHeader(response); err != nil {
		return OutcomeUnknown, errors.Join(ErrOutcomeUnknown, err)
	}
	if response.Succeeded {
		if !operationPutResponses(response, 1) {
			return OutcomeUnknown, ErrOutcomeUnknown
		}
		return OutcomeAborted, nil
	}
	points, err := b.stageEvidencePoints(response, []string{b.identityKey, b.restoreKey, receiptKey})
	if err != nil {
		return OutcomeUnknown, err
	}
	if points[2] == nil {
		return OutcomeUnknown, ErrOutcomeUnknown
	}
	return decodeReceipt(points[2], ref)
}

func (b *Backend) validateResponseIdentity(response *clientv3.TxnResponse) error {
	if response == nil || response.Header == nil || response.Header.ClusterId != b.clusterID || len(response.Responses) < 2 {
		return ErrIdentityMismatch
	}
	identity := response.Responses[0].GetResponseRange()
	restore := response.Responses[1].GetResponseRange()
	if identity == nil || restore == nil || len(identity.Kvs) != 1 || len(restore.Kvs) != 1 || identity.Kvs[0].Lease != 0 || restore.Kvs[0].Lease != 0 || string(identity.Kvs[0].Value) != b.identityValue || string(restore.Kvs[0].Value) != b.restoreEpoch {
		return ErrIdentityMismatch
	}
	return nil
}
