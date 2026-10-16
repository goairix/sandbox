package etcd

import (
	"context"
	"fmt"
	"sync"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// ColdCreatePermit exists only in the original successful commit's process.
// A copied record, replay, or unknown result cannot reconstruct this handle.
type ColdCreatePermit struct {
	self   *ColdCreatePermit
	origin *Backend
	claim  *CreationClaim
	mu     sync.Mutex
	used   bool
}

type ColdCreateAttemptRecord struct {
	Version       int                    `json:"version"`
	RestoreEpoch  string                 `json:"restore_epoch"`
	ProfileDigest string                 `json:"profile_digest"`
	Owner         ColdSlotOwner          `json:"owner"`
	Quota         ColdSlotReference      `json:"quota"`
	Preparation   ColdSlotReference      `json:"preparation"`
	OperationID   string                 `json:"operation_id"`
	PayloadDigest string                 `json:"payload_digest"`
	Claim         DispatchClaimReference `json:"claim"`
}

type ColdCreateObservation struct {
	Version       int           `json:"version"`
	RestoreEpoch  string        `json:"restore_epoch"`
	ProfileDigest string        `json:"profile_digest"`
	Owner         ColdSlotOwner `json:"owner"`
	OperationID   string        `json:"operation_id"`
	AttemptDigest string        `json:"attempt_digest"`
	PodUID        string        `json:"pod_uid"`
}

type ColdCreateAttemptResult struct {
	Outcome Outcome
	Replay  bool
	Permit  *ColdCreatePermit
	Record  *ColdCreateAttemptRecord
}

func (b *Backend) BeginColdCreateAttempt(ctx context.Context, c *CreationClaim) (ColdCreateAttemptResult, error) {
	result := ColdCreateAttemptResult{Outcome: OutcomeUnknown}
	claim, err := b.creationClaimComparisons(c)
	if err != nil {
		return result, err
	}
	s, err := b.coldPendingSnapshot(ctx, c.workspace, c.reference.IntentID)
	if err != nil {
		return result, err
	}
	if s.record.Owner != coldClaimOwner(c) {
		return result, ErrCorruptRecord
	}
	dispatch, err := b.LoadRuntimeDispatch(ctx, c.workspace, c.reference.IntentID)
	if err != nil {
		return result, err
	}
	if dispatch == nil || dispatch.Record.Kind != RuntimeDispatchCreate || !c.matchesRuntimeDispatch(dispatch.Record) {
		return result, ErrConflict
	}
	dk, ik, _ := b.namespace.runtimeDispatchKeys(c.reference.Partition, c.reference.IntentID)
	rk, _ := b.runtimeDispatchReceiptKey(dispatch.Record.Attempt)
	points, err := b.readRuntimeDomain(ctx, []string{dk, ik, rk}, map[int]bool{2: true})
	if err != nil {
		return result, err
	}
	dispatch, err = b.decodeRuntimeDispatchEntry(points, c.workspace, c.reference.IntentID, dk, ik, rk, dispatch.Record.Attempt)
	if err != nil || dispatch == nil {
		return result, ErrCorruptRecord
	}
	for _, point := range points {
		if !enrolledImmutableKV(point) {
			return result, ErrCorruptRecord
		}
	}
	existing, _, err := b.coldReadAttempt(ctx, s)
	if err != nil {
		return result, err
	}
	if existing != nil {
		if existing.OperationID != dispatch.Record.OperationID || existing.PayloadDigest != dispatch.Record.PayloadDigest {
			return result, ErrCorruptRecord
		}
		return ColdCreateAttemptResult{Outcome: OutcomeCommitted, Replay: true, Record: existing}, nil
	}
	_, ready, err := b.coldReady(ctx)
	if err != nil {
		return result, err
	}
	r := ColdCreateAttemptRecord{Version: 1, RestoreEpoch: b.restoreEpoch, ProfileDigest: b.preparationDigest, Owner: s.record.Owner, Quota: s.record.Quota, Preparation: s.record.Preparation, OperationID: dispatch.Record.OperationID, PayloadDigest: dispatch.Record.PayloadDigest, Claim: preparationClaim(c)}
	wire, err := coldEncode(r)
	if err != nil {
		return result, err
	}
	cmps := append(b.baseComparisons(), claim...)
	cmps = append(cmps, s.comparisons()...)
	for _, kv := range points {
		cmps = append(cmps, coldComparisons(kv)...)
	}
	cmps = append(cmps, coldComparisons(ready)...)
	cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(s.attemptKey), "=", 0))
	writes := []Write{{Key: s.attemptKey, Value: []byte(wire)}, {Key: string(s.work.Key), Value: s.work.Value}}
	if len(cmps) != 53 || len(writes) != 2 {
		return result, ErrInvalidMutation
	}
	if _, err := b.coldCommit(ctx, cmps, writes, s.attemptKey); err != nil {
		return result, err
	}
	result.Outcome, result.Record = OutcomeCommitted, &r
	if b.archiveLife == nil {
		return result, ErrGuardExpired
	}
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if b.archiveLife.closed {
		return result, ErrGuardExpired
	}
	if _, err := b.creationClaimComparisons(c); err != nil {
		return result, err
	}
	permit := &ColdCreatePermit{origin: b, claim: c}
	permit.self = permit
	result.Permit = permit
	return result, nil
}

func (b *Backend) ConsumeColdCreatePermit(c *CreationClaim, permit *ColdCreatePermit) error {
	if permit == nil || permit.self != permit || permit.origin != b || permit.claim != c {
		return ErrInvalidRecord
	}
	if b.archiveLife == nil {
		return ErrGuardExpired
	}
	// Share the original Backend's admission seal with Close. This lock covers
	// only local capability consumption, never the subsequent native I/O.
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if b.archiveLife.closed {
		return ErrGuardExpired
	}
	permit.mu.Lock()
	defer permit.mu.Unlock()
	if permit.used {
		return ErrConflict
	}
	permit.used = true
	_, err := b.creationClaimComparisons(c)
	return err
}

func (b *Backend) LoadColdCreateAttempt(ctx context.Context, w WorkspaceIdentity, intent string) (*ColdCreateAttemptRecord, error) {
	s, err := b.coldPendingSnapshot(ctx, w, intent)
	if err != nil {
		return nil, err
	}
	r, _, err := b.coldReadAttempt(ctx, s)
	return r, err
}

func (b *Backend) RecordColdCreateObservation(ctx context.Context, c *CreationClaim, uid string) (*ColdCreateObservation, error) {
	claim, err := b.creationClaimComparisons(c)
	if err != nil {
		return nil, err
	}
	if !validPreparationUUID(uid) {
		return nil, ErrInvalidRecord
	}
	s, err := b.coldPendingSnapshot(ctx, c.workspace, c.reference.IntentID)
	if err != nil {
		return nil, err
	}
	if s.record.Owner != coldClaimOwner(c) {
		return nil, ErrCorruptRecord
	}
	attempt, kv, err := b.coldReadAttempt(ctx, s)
	if err != nil {
		return nil, err
	}
	if attempt == nil {
		return nil, ErrConflict
	}
	r := ColdCreateObservation{Version: 1, RestoreEpoch: b.restoreEpoch, ProfileDigest: b.preparationDigest, Owner: s.record.Owner, OperationID: attempt.OperationID, AttemptDigest: workspaceSyncIntentDigest(string(kv.Value)), PodUID: uid}
	existing, _, err := b.coldReadObservation(ctx, s, attempt, kv)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if *existing != r {
			return nil, ErrIdempotencyConflict
		}
		return existing, nil
	}
	wire, err := coldEncode(r)
	if err != nil {
		return nil, err
	}
	cmps := append(b.baseComparisons(), claim...)
	cmps = append(cmps, s.comparisons()...)
	cmps = append(cmps, coldComparisons(kv)...)
	cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(s.observationKey), "=", 0))
	writes := []Write{{Key: s.observationKey, Value: []byte(wire)}, {Key: string(s.work.Key), Value: s.work.Value}}
	if len(cmps) != 44 || len(writes) != 2 {
		return nil, ErrInvalidMutation
	}
	if _, err := b.coldCommit(ctx, cmps, writes, s.observationKey); err != nil {
		return nil, err
	}
	return &r, nil
}

func (b *Backend) LoadColdCreateObservation(ctx context.Context, w WorkspaceIdentity, intent string) (*ColdCreateObservation, error) {
	s, err := b.coldPendingSnapshot(ctx, w, intent)
	if err != nil {
		return nil, err
	}
	attempt, kv, err := b.coldReadAttempt(ctx, s)
	if err != nil {
		return nil, err
	}
	r, _, err := b.coldReadObservation(ctx, s, attempt, kv)
	return r, err
}

type coldPendingState struct {
	record                     ColdPreparationRecord
	reservation, work          *mvccpb.KeyValue
	slots                      [2]*mvccpb.KeyValue
	attemptKey, observationKey string
}

func (s *coldPendingState) comparisons() []clientv3.Cmp {
	var cmps []clientv3.Cmp
	for _, kv := range []*mvccpb.KeyValue{s.reservation, s.work, s.slots[0], s.slots[1]} {
		cmps = append(cmps, coldComparisons(kv)...)
	}
	return cmps
}

func (b *Backend) coldPendingSnapshot(ctx context.Context, w WorkspaceIdentity, intent string) (*coldPendingState, error) {
	r, err := b.LoadColdPreparation(ctx, w, intent)
	if err != nil {
		return nil, err
	}
	if r == nil || r.Phase != "reserved" {
		return nil, ErrConflict
	}
	key, work, _ := b.namespace.coldPreparationKeys(w.Partition(), intent)
	q, qfree, _ := b.namespace.coldSlotKeys(r.Quota)
	p, pfree, _ := b.namespace.coldSlotKeys(r.Preparation)
	values, err := b.readDomain(ctx, key, work, q, qfree, p, pfree)
	if err != nil {
		return nil, err
	}
	var current ColdPreparationRecord
	var marker coldPreparationWork
	if coldDecode(values[0], &current) != nil || current != *r || coldDecode(values[1], &marker) != nil || marker != (coldPreparationWork{Version: 1, RestoreEpoch: b.restoreEpoch, ProfileDigest: b.preparationDigest, IntentID: intent, Phase: "reserved"}) {
		return nil, ErrConflict
	}
	s := &coldPendingState{record: current, reservation: values[0], work: values[1]}
	for i, ref := range []ColdSlotReference{r.Quota, r.Preparation} {
		var slot coldSlotRecord
		kv := values[2+i*2]
		if coldDecode(kv, &slot) != nil || !b.validColdSlot(slot) || !slot.Occupied || slot.Owner != r.Owner || slot.Slot != ref || values[3+i*2] != nil {
			return nil, ErrCorruptRecord
		}
		s.slots[i] = kv
	}
	s.attemptKey, _ = b.namespace.Key("p", fmt.Sprintf("%02x", w.Partition()), "intents", intent, "cold-create-attempt")
	s.observationKey, _ = b.namespace.Key("p", fmt.Sprintf("%02x", w.Partition()), "intents", intent, "cold-create-observation")
	return s, nil
}

func (b *Backend) coldReadAttempt(ctx context.Context, s *coldPendingState) (*ColdCreateAttemptRecord, *mvccpb.KeyValue, error) {
	values, err := b.readDomain(ctx, s.attemptKey)
	if err != nil {
		return nil, nil, err
	}
	if values[0] == nil {
		return nil, nil, nil
	}
	var r ColdCreateAttemptRecord
	kv := values[0]
	if !enrolledImmutableKV(kv) || coldDecode(kv, &r) != nil || r.Version != 1 || r.RestoreEpoch != b.restoreEpoch || r.ProfileDigest != b.preparationDigest || r.Owner != s.record.Owner || r.Quota != s.record.Quota || r.Preparation != s.record.Preparation || !validPreparationUUID(r.OperationID) || !validHexDigest(r.PayloadDigest) || !validPreparationUUID(r.Claim.ClaimID) || !validDomainSegment(r.Claim.WorkerID) || r.Claim.LeaseID <= 0 || r.Claim.CreateRevision <= 0 || r.Claim.CreateRevision >= kv.CreateRevision {
		return nil, nil, ErrCorruptRecord
	}
	return &r, kv, nil
}

func (b *Backend) coldReadObservation(ctx context.Context, s *coldPendingState, attempt *ColdCreateAttemptRecord, kv *mvccpb.KeyValue) (*ColdCreateObservation, *mvccpb.KeyValue, error) {
	values, err := b.readDomain(ctx, s.observationKey)
	if err != nil {
		return nil, nil, err
	}
	if values[0] == nil {
		return nil, nil, nil
	}
	var r ColdCreateObservation
	if attempt == nil || kv == nil || !enrolledImmutableKV(values[0]) || coldDecode(values[0], &r) != nil || r.Version != 1 || r.RestoreEpoch != b.restoreEpoch || r.ProfileDigest != b.preparationDigest || r.Owner != s.record.Owner || r.OperationID != attempt.OperationID || r.AttemptDigest != workspaceSyncIntentDigest(string(kv.Value)) || !validPreparationUUID(r.PodUID) || values[0].CreateRevision <= kv.CreateRevision {
		return nil, nil, ErrCorruptRecord
	}
	return &r, values[0], nil
}
