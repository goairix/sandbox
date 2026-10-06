package etcd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const operationLeaseTTL int64 = 30

// OperationCapability authorizes one admitted operation under its original
// request context and Lease. Public metadata cannot reconstruct this capability.
type OperationCapability struct {
	origin                                      *Backend
	parentCtx                                   context.Context
	record                                      OperationRecord
	control                                     SandboxControlRecord
	fences                                      []operationAdmissionFence
	tokenKey, guardKey, receiptKey, mutationKey string
	value                                       string
	guardRevision                               int64
	admitRevision                               int64
	mu                                          sync.Mutex
	deadline                                    time.Time
	lost                                        bool
	execDraft                                   *execEffectDraft
}

func (c *OperationCapability) Reference() OperationReference {
	if c == nil {
		return OperationReference{}
	}
	return c.record.Reference
}
func (c *OperationCapability) Control() SandboxControlRecord {
	if c == nil {
		return SandboxControlRecord{}
	}
	result := c.control
	if result.Runtime != nil {
		runtime := *result.Runtime
		result.Runtime = &runtime
	}
	return result
}

type BeginOperationInput struct {
	SandboxID, RequestID string
	Kind                 OperationKind
}
type BeginOperationResult struct {
	Outcome           OperationOutcome
	Reference         OperationReference
	Capability        *OperationCapability
	GuardCleanupError error
}

type operationAdmissionFence struct {
	Key         string `json:"key"`
	ModRevision int64  `json:"mod_revision"`
	Value       []byte `json:"value"`
}

// Explicit typed fields exclude LeaseID and Digest rather than zeroing them in
// a serialized public reference. Field and fence order are part of version 1.
type operationAdmissionDescriptor struct {
	Version         uint32                    `json:"version"`
	Namespace       string                    `json:"namespace"`
	RestoreEpoch    string                    `json:"restore_epoch"`
	RequestID       string                    `json:"request_id"`
	SandboxID       string                    `json:"sandbox_id"`
	OperationID     string                    `json:"operation_id"`
	Partition       uint8                     `json:"partition"`
	Kind            OperationKind             `json:"kind"`
	WorkspaceHash   string                    `json:"workspace_hash"`
	IntentID        string                    `json:"intent_id"`
	Generation      int64                     `json:"generation"`
	DataGateEpoch   int64                     `json:"data_gate_epoch"`
	ControlRevision int64                     `json:"control_revision"`
	Runtime         RuntimeReference          `json:"runtime"`
	Snapshot        SnapshotReference         `json:"snapshot"`
	ExpiresAt       time.Time                 `json:"expires_at"`
	Fences          []operationAdmissionFence `json:"fences"`
}

// BeginOperation admits data or mutation metadata atomically; it never starts a
// target call or drains already admitted data operations. A mutation only closes
// new admissions until its original Lease releases the mutation key.
func (b *Backend) BeginOperation(ctx context.Context, in BeginOperationInput) (result BeginOperationResult, err error) {
	result.Outcome = OperationUnknown
	if ctx == nil || !validDomainSegment(in.SandboxID) || !validDomainSegment(in.RequestID) || (in.Kind != OperationData && in.Kind != OperationMutation) {
		return result, ErrInvalidRecord
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	bundle, err := b.loadOperationControl(ctx, in.SandboxID)
	if err != nil {
		return result, err
	}
	if err = b.operationAdmissionTime(ctx, bundle.Control.ExpiresAt); err != nil {
		return result, err
	}
	control := bundle.Control
	record := OperationRecord{Version: 1, Reference: OperationReference{Namespace: b.namespace.Root(), RestoreEpoch: b.restoreEpoch, RequestID: in.RequestID, SandboxID: in.SandboxID, OperationID: uuid.NewString(), Partition: bundle.Partition, Kind: in.Kind, LeaseID: math.MaxInt64}, WorkspaceHash: control.WorkspaceHash, IntentID: control.IntentID, Generation: control.Generation, DataGateEpoch: control.DataGateEpoch, ControlRevision: bundle.KVs[1].ModRevision, Runtime: *control.Runtime, Snapshot: control.Snapshot, ExpiresAt: control.ExpiresAt}
	fences := make([]operationAdmissionFence, len(bundle.Keys))
	for i, key := range bundle.Keys {
		fences[i] = operationAdmissionFence{Key: key, ModRevision: bundle.KVs[i].ModRevision, Value: bytes.Clone(bundle.KVs[i].Value)}
	}
	r := record.Reference
	descriptor := operationAdmissionDescriptor{1, r.Namespace, r.RestoreEpoch, r.RequestID, r.SandboxID, r.OperationID, r.Partition, r.Kind, record.WorkspaceHash, record.IntentID, record.Generation, record.DataGateEpoch, record.ControlRevision, record.Runtime, record.Snapshot, record.ExpiresAt, fences}
	wire, err := json.Marshal(descriptor)
	if err != nil {
		return result, ErrInvalidRecord
	}
	digest := sha256.Sum256(append([]byte("sandbox-operation-admission:v1\x00"), wire...))
	record.Reference.Digest = hex.EncodeToString(digest[:])
	token, guard, receipt, mutation, err := b.namespace.operationKeys(record.Reference)
	if err != nil {
		return result, err
	}
	c := &OperationCapability{origin: b, parentCtx: ctx, record: record, control: control, fences: fences, tokenKey: token, guardKey: guard, receiptKey: receipt, mutationKey: mutation, guardRevision: math.MaxInt64}
	// Own the Runtime pointer separately from the loader and every public copy.
	c.control = c.Control()
	if err = b.preflightOperation(c, wire); err != nil {
		return result, err
	}
	result.Reference = record.Reference
	result.Reference.LeaseID = 0 // diagnostic only; never attach the placeholder to RPCs
	requestCtx, cancel := b.requestContext(ctx)
	sent := time.Now()
	granted, grantErr := b.client.Grant(requestCtx, operationLeaseTTL)
	cancel()
	known := granted != nil && granted.ResponseHeader != nil && granted.ClusterId == b.clusterID && granted.ID > 0
	if known {
		result.Reference.LeaseID = int64(granted.ID)
	}
	// Only undelivered operations are cleaned up here, independently of the
	// caller's cancellation. Cleanup errors cannot overwrite historical evidence.
	defer func() {
		if known && result.Capability == nil {
			result.GuardCleanupError = b.revokeStageLease(granted.ID)
		}
	}()
	if grantErr != nil {
		return result, fmt.Errorf("%w: grant operation Lease: %w", ErrOutcomeUnknown, grantErr)
	}
	if !known {
		return result, ErrIdentityMismatch
	}
	if granted.TTL <= 0 || granted.TTL > operationLeaseTTL {
		return result, ErrGuardExpired
	}
	c.deadline = sent.Add(time.Duration(granted.TTL) * time.Second)
	c.record.Reference = result.Reference
	c.value, err = encodeOperationRecord(c.record)
	if err != nil {
		return result, err
	}
	receiptValue, err := encodeOperationReceipt(operationReceipt{Version: 1, Reference: result.Reference, Outcome: OperationCommitted})
	if err != nil {
		return result, err
	}
	if err = c.admissionLive(); err != nil {
		return result, err
	}
	failureReads := c.evidenceReads(b)
	guardCmp := b.baseComparisons()
	for _, key := range []string{guard, receipt, token} {
		guardCmp = append(guardCmp, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	}
	requestCtx, cancel = b.requestContext(ctx)
	response, err := b.client.Txn(requestCtx).If(guardCmp...).Then(clientv3.OpPut(guard, c.value, clientv3.WithLease(granted.ID))).Else(failureReads...).Commit()
	cancel()
	if err != nil {
		return result, fmt.Errorf("%w: initialize operation guard: %w", ErrOutcomeUnknown, err)
	}
	if err = b.operationResponseHeader(response); err != nil {
		return result, err
	}
	if !response.Succeeded {
		return result, b.operationAdmissionFailure(response, c, &result)
	}
	if !operationPutResponses(response, 1) {
		return result, ErrOutcomeUnknown
	}
	c.guardRevision = response.Header.Revision
	if err = c.admissionLive(); err != nil {
		return result, err
	}
	if err = b.operationAdmissionTime(ctx, c.record.ExpiresAt); err != nil {
		return result, err
	}
	if err = c.admissionLive(); err != nil {
		return result, err
	}
	writes := []clientv3.Op{clientv3.OpPut(token, c.value, clientv3.WithLease(granted.ID)), clientv3.OpPut(receipt, receiptValue, clientv3.WithLease(granted.ID))}
	if in.Kind == OperationMutation {
		writes = append(writes, clientv3.OpPut(mutation, c.value, clientv3.WithLease(granted.ID)))
	}
	requestCtx, cancel = b.requestContext(ctx)
	response, err = b.client.Txn(requestCtx).If(c.admissionComparisons(b)...).Then(writes...).Else(failureReads...).Commit()
	cancel()
	if err != nil {
		return result, fmt.Errorf("%w: admit operation: %w", ErrOutcomeUnknown, err)
	}
	if err = b.operationResponseHeader(response); err != nil {
		return result, err
	}
	if !response.Succeeded {
		return result, b.operationAdmissionFailure(response, c, &result)
	}
	if !operationPutResponses(response, len(writes)) {
		return result, ErrOutcomeUnknown
	}
	result.Outcome = OperationCommitted
	c.admitRevision = response.Header.Revision
	if err = c.admissionLive(); err != nil {
		return result, err
	}
	// Check the original records remain live after the commit response. This is
	// metadata evidence only; every eventual target call still needs its gate.
	requestCtx, cancel = b.requestContext(ctx)
	liveResponse, err := b.client.Txn(requestCtx).If(b.baseComparisons()...).Then(failureReads...).Else(failureReads...).Commit()
	cancel()
	if err != nil {
		return result, fmt.Errorf("%w: verify admitted operation: %w", ErrOutcomeUnknown, err)
	}
	evidence, err := b.operationEvidence(liveResponse, c)
	if err != nil {
		return result, err
	}
	if err = c.validateLiveEvidence(evidence, c.admitRevision); err != nil {
		return result, err
	}
	if err = c.admissionLive(); err != nil {
		return result, err
	}
	result.Capability = c
	return result, nil
}

func (b *Backend) operationAdmissionTime(ctx context.Context, expires time.Time) error {
	now, err := b.observePublicationClock(ctx)
	if err != nil {
		return err
	}
	if !now.Add(time.Second).Before(expires) {
		return ErrOperationAdmissionClosed
	}
	return nil
}
func (c *OperationCapability) admissionLive() error {
	if err := c.parentCtx.Err(); err != nil {
		c.lost = true
		return err
	}
	if c.lost || !time.Now().Before(c.deadline) {
		c.lost = true
		return ErrGuardExpired
	}
	return nil
}
func (c *OperationCapability) admissionComparisons(b *Backend) []clientv3.Cmp {
	comparisons := b.baseComparisons()
	for _, f := range c.fences {
		comparisons = append(comparisons, clientv3.Compare(clientv3.ModRevision(f.Key), "=", f.ModRevision), clientv3.Compare(clientv3.LeaseValue(f.Key), "=", 0))
	}
	comparisons = append(comparisons, clientv3.Compare(clientv3.Value(c.guardKey), "=", c.value), clientv3.Compare(clientv3.LeaseValue(c.guardKey), "=", c.record.Reference.LeaseID), clientv3.Compare(clientv3.CreateRevision(c.guardKey), "=", c.guardRevision))
	for _, key := range []string{c.receiptKey, c.tokenKey, c.mutationKey} {
		comparisons = append(comparisons, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	}
	return comparisons
}
func (c *OperationCapability) evidenceReads(b *Backend) []clientv3.Op {
	return []clientv3.Op{clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey), clientv3.OpGet(c.receiptKey), clientv3.OpGet(c.guardKey), clientv3.OpGet(c.tokenKey)}
}
func (b *Backend) preflightOperation(c *OperationCapability, descriptor []byte) error {
	value, err := encodeOperationRecord(c.record)
	if err != nil {
		return err
	}
	receipt, err := encodeOperationReceipt(operationReceipt{Version: 1, Reference: c.record.Reference, Outcome: OperationCommitted})
	if err != nil {
		return err
	}
	c.value = value
	comparisons := c.admissionComparisons(b)
	writes := []Write{{Key: c.tokenKey, Value: []byte(value)}, {Key: c.receiptKey, Value: []byte(receipt)}}
	if c.record.Reference.Kind == OperationMutation {
		writes = append(writes, Write{Key: c.mutationKey, Value: []byte(value)})
	}
	reads := []string{b.identityKey, b.restoreKey, c.receiptKey, c.guardKey, c.tokenKey}
	if len(comparisons)+len(writes)+len(reads) > maxStageOperations {
		return ErrInvalidMutation
	}
	for _, key := range append(append([]string{}, reads...), c.mutationKey) {
		if !validNamespaceKey(b.namespace, key) {
			return ErrInvalidMutation
		}
	}
	for _, f := range c.fences {
		if !validNamespaceKey(b.namespace, f.Key) || len(f.Value) > maxRecordBytes {
			return ErrInvalidMutation
		}
	}
	// JSON with byte slices conservatively includes base64 expansion; reserve all
	// RPC keys/Lease varints and the smaller guard-init transaction as well.
	accounted, err := json.Marshal(struct {
		Comparisons []clientv3.Cmp
		Writes      []Write
		Reads       []string
	}{comparisons, writes, reads})
	if err != nil || len(accounted)+len(descriptor)+16*maxKeyBytes > maxMutationBytes {
		return ErrInvalidMutation
	}
	return nil
}
func (b *Backend) operationResponseHeader(response *clientv3.TxnResponse) error {
	if response == nil || response.Header == nil || response.Header.ClusterId != b.clusterID {
		return ErrIdentityMismatch
	}
	if response.Header.Revision <= 0 {
		return ErrOutcomeUnknown
	}
	return nil
}
func operationPutResponses(response *clientv3.TxnResponse, count int) bool {
	if len(response.Responses) != count {
		return false
	}
	for _, op := range response.Responses {
		if op == nil || op.GetResponsePut() == nil || op.GetResponsePut().PrevKv != nil || !operationNestedHeader(op.GetResponsePut().Header, response.Header) {
			return false
		}
	}
	return true
}

// Native etcd may omit per-operation headers inside a Txn. If present, they
// may omit cluster identity (zero), but any identity must match and the
// revision must describe this transaction.
func operationNestedHeader(nested, outer *pb.ResponseHeader) bool {
	return nested == nil || (nested.ClusterId == 0 || nested.ClusterId == outer.ClusterId) && nested.Revision == outer.Revision
}

// All evidence uses the fixed identity/restore/receipt/guard/token order. Absence
// in a read alone never decides an unknown dispatch outcome.
func (b *Backend) operationEvidence(response *clientv3.TxnResponse, c *OperationCapability) ([]*mvccpb.KeyValue, error) {
	if err := b.operationResponseHeader(response); err != nil {
		return nil, err
	}
	keys := []string{b.identityKey, b.restoreKey, c.receiptKey, c.guardKey, c.tokenKey}
	if len(response.Responses) != len(keys) {
		return nil, ErrCorruptRecord
	}
	values := make([]*mvccpb.KeyValue, len(keys))
	for i, key := range keys {
		if response.Responses[i] == nil {
			return nil, ErrCorruptRecord
		}
		point := response.Responses[i].GetResponseRange()
		if point == nil || point.More || point.Count != int64(len(point.Kvs)) || len(point.Kvs) > 1 || !operationNestedHeader(point.Header, response.Header) {
			return nil, ErrCorruptRecord
		}
		if len(point.Kvs) == 0 {
			continue
		}
		kv := point.Kvs[0]
		if kv == nil || string(kv.Key) != key || kv.CreateRevision <= 0 || kv.ModRevision < kv.CreateRevision || kv.ModRevision > response.Header.Revision {
			return nil, ErrCorruptRecord
		}
		values[i] = kv
	}
	if !operationPermanentKV(values[0]) || !operationPermanentKV(values[1]) || string(values[0].Value) != b.identityValue || string(values[1].Value) != b.restoreEpoch {
		return nil, ErrIdentityMismatch
	}
	return values[2:], nil
}
func (b *Backend) operationAdmissionFailure(response *clientv3.TxnResponse, c *OperationCapability, result *BeginOperationResult) error {
	values, err := b.operationEvidence(response, c)
	if err != nil {
		return err
	}
	receiptKV, guard, token := values[0], values[1], values[2]
	for _, kv := range []*mvccpb.KeyValue{guard, token} {
		if kv == nil {
			continue
		}
		var record OperationRecord
		if err = decodeOperationRecord(kv, &record); err != nil {
			return err
		}
		if record != c.record || string(kv.Value) != c.value {
			return ErrCorruptRecord
		}
	}
	if receiptKV == nil {
		if token != nil {
			return ErrCorruptReceipt
		}
		// Neither completion record exists, and this Txn is known not to have
		// written. This also covers guard-init rejection before guardRevision
		// is known and ordinary admission rejection after an original fence loss.
		result.Outcome = OperationAborted
		return ErrConflict
	}
	var receipt operationReceipt
	if err = decodeOperationReceipt(receiptKV, &receipt); err != nil {
		return err
	}
	if receipt.Reference != c.record.Reference {
		return ErrCorruptReceipt
	}
	// A receipt-backed historical result needs this attempt's original guard,
	// not merely a matching body in a missing or reconstructed envelope.
	if guard == nil || c.guardRevision <= 0 || guard.CreateRevision != c.guardRevision {
		return ErrCorruptRecord
	}
	if token != nil && token.CreateRevision <= guard.CreateRevision {
		return ErrCorruptRecord
	}
	if receiptKV.CreateRevision <= guard.CreateRevision {
		return ErrCorruptReceipt
	}
	if receipt.Outcome == OperationAborted {
		if token != nil {
			return ErrCorruptRecord
		}
	} else if err = validateOperationCompletion(token, receiptKV); err != nil {
		return err
	}
	// Publish a historical outcome only after its entire evidence shape passes.
	result.Outcome = receipt.Outcome
	return ErrConflict
}
func (c *OperationCapability) validateLiveEvidence(values []*mvccpb.KeyValue, admitRevision int64) error {
	for _, kv := range values {
		if kv == nil {
			return ErrGuardExpired
		}
	}
	if err := validateOperationCompletion(values[2], values[0]); err != nil {
		return err
	}
	for _, kv := range values[1:] {
		var record OperationRecord
		if err := decodeOperationRecord(kv, &record); err != nil {
			return err
		}
		if record != c.record || string(kv.Value) != c.value {
			return ErrCorruptRecord
		}
	}
	if values[1].CreateRevision != c.guardRevision || values[2].CreateRevision != admitRevision {
		return ErrGuardExpired
	}
	return nil
}
