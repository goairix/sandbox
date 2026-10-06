package etcd

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

var ErrIdempotencyConflict = errors.New("etcd state: idempotency conflict")
var ErrGenerationOverflow = errors.New("etcd state: generation overflow")

type AcquireIntentInput struct {
	Workspace                                                                  WorkspaceIdentity
	Principal, IdempotencyKey, RequestID, IntentID, SandboxID, SnapshotVersion string
	Payload                                                                    json.RawMessage
	Now                                                                        time.Time
	TTL                                                                        time.Duration
}
type AcquireDisposition string

const (
	AcquireCreated  AcquireDisposition = "created"
	AcquireReplay   AcquireDisposition = "replay"
	AcquireOccupied AcquireDisposition = "occupied"
)

type AcquireIntentResult struct {
	Disposition       AcquireDisposition
	Outcome           Outcome
	Reference         StageReference
	Request           CreationRequestRecord
	Owner             *WorkspaceOwnerRecord
	GuardCleanupError error
}

// AcquireIntent commits only durable creation metadata. A committed outcome
// says nothing about runtime publication. Empty Reference means no business
// transaction was dispatched; occupied and replay results allocate no stage.
func (b *Backend) AcquireIntent(ctx context.Context, input AcquireIntentInput) (AcquireIntentResult, error) {
	result := AcquireIntentResult{Outcome: OutcomeUnknown}
	if ctx == nil {
		return result, fmt.Errorf("%w: context is nil", ErrInvalidRecord)
	}
	if err := b.validateWorkspaceBinding(input.Workspace); err != nil {
		return result, err
	}
	if !validDomainSegment(input.RequestID) || !validDomainSegment(input.IntentID) || !validDomainSegment(input.SandboxID) || !validDomainSegment(input.SnapshotVersion) || input.TTL <= 0 || input.TTL > 365*24*time.Hour || input.Now.IsZero() {
		return result, ErrInvalidRecord
	}
	now := input.Now.UTC()
	expiry := now.Add(input.TTL)
	if !validDomainExpiry(now) || !validDomainExpiry(expiry) {
		return result, ErrInvalidRecord
	}
	requestHash, err := requestKeyHash(input.Principal, input.IdempotencyKey)
	if err != nil {
		return result, err
	}
	// Compacting takes a private copy before any RPC can yield to the caller.
	payload, err := compactSnapshot(input.Payload)
	if err != nil {
		return result, err
	}
	payloadDigest, err := snapshotDigest(payload)
	if err != nil {
		return result, err
	}
	snapshotRef := SnapshotReference{Version: input.SnapshotVersion, Digest: payloadDigest}
	snapshot := SandboxSnapshotRecord{Version: 1, SandboxID: input.SandboxID, Snapshot: snapshotRef, Payload: payload}
	// Account for the complete immutable record envelope before allocating a lease.
	snapshotValue, err := encodeDomainRecord(snapshot)
	if err != nil {
		return result, err
	}
	digest := framedDigest(input.Workspace.Hash(), payloadDigest, strconv.FormatInt(int64(input.TTL), 10))
	configurationDigest := hex.EncodeToString(digest[:])
	ownerKey, fenceKey, err := b.namespace.workspaceKeys(input.Workspace)
	if err != nil {
		return result, err
	}
	controlKey, snapshotKey, err := b.namespace.sandboxKeys(input.Workspace.Partition(), input.SandboxID, input.SnapshotVersion)
	if err != nil {
		return result, err
	}
	intentKey, err := b.namespace.intentKey(input.Workspace.Partition(), input.IntentID)
	if err != nil {
		return result, err
	}
	requestKey, err := b.namespace.requestKey(requestHash)
	if err != nil {
		return result, err
	}
	placementKey, err := b.namespace.placementKey(input.SandboxID)
	if err != nil {
		return result, err
	}
	keys := []string{requestKey, ownerKey, fenceKey, controlKey, snapshotKey, intentKey, placementKey}
	records, err := b.readDomain(ctx, keys...)
	if err != nil {
		return result, err
	}
	request, err := b.decodeRequest(records[0], requestHash)
	if err != nil {
		return result, err
	}
	owner, fence, err := b.decodeOwnerFence(records[1], records[2], input.Workspace)
	if err != nil {
		return result, err
	}
	placement, err := b.decodePlacement(records[6], input.SandboxID)
	if err != nil {
		return result, err
	}
	if _, err := b.decodeControl(records[3], input.Workspace.Partition(), input.SandboxID, placement); err != nil {
		return result, err
	}
	if records[4] != nil {
		// A collision may have a different valid digest at the same version. Decode
		// against its stored reference, while still checking the key and placement.
		var existing SandboxSnapshotRecord
		if err := decodeDomainRecord(records[4], &existing); err != nil {
			return result, err
		}
		if existing.Snapshot.Version != input.SnapshotVersion {
			return result, ErrCorruptRecord
		}
		if _, err := decodeSnapshot(records[4], input.Workspace.Partition(), input.SandboxID, existing.Snapshot, placement); err != nil {
			return result, err
		}
	}
	if records[5] != nil {
		var intent CreationIntentRecord
		if err := decodeDomainRecord(records[5], &intent); err != nil {
			return result, err
		}
		if err := b.domainEpoch(intent.RestoreEpoch); err != nil {
			return result, err
		}
		if intent.IntentID != input.IntentID || hashPartition(intent.WorkspaceHash) != input.Workspace.Partition() {
			return result, ErrCorruptRecord
		}
	}
	if request != nil {
		if request.ConfigurationDigest != configurationDigest {
			return result, ErrIdempotencyConflict
		}
		if request.WorkspaceHash != input.Workspace.Hash() {
			return result, ErrCorruptRecord
		}
		result.Disposition = AcquireReplay
		result.Outcome = OutcomeCommitted
		result.Request = *request
		result.Owner = owner
		return result, nil
	}
	if owner != nil {
		result.Disposition = AcquireOccupied
		result.Owner = owner
		return result, nil
	}
	for _, i := range []int{3, 4, 5, 6} {
		if records[i] != nil {
			return result, ErrConflict
		}
	}
	generation := int64(1)
	if fence != nil {
		if fence.Generation == math.MaxInt64 {
			return result, ErrGenerationOverflow
		}
		generation = fence.Generation + 1
	}
	ownerRecord := WorkspaceOwnerRecord{Version: 1, WorkspaceHash: input.Workspace.Hash(), SandboxID: input.SandboxID, IntentID: input.IntentID, RestoreEpoch: b.restoreEpoch, Generation: generation}
	fenceRecord := WorkspaceFenceRecord{Version: 1, WorkspaceHash: input.Workspace.Hash(), RestoreEpoch: b.restoreEpoch, Generation: generation}
	controlRecord := SandboxControlRecord{Version: 1, SandboxID: input.SandboxID, WorkspaceHash: input.Workspace.Hash(), IntentID: input.IntentID, RestoreEpoch: b.restoreEpoch, Generation: generation, DataGateEpoch: 1, Phase: PhasePublishing, Snapshot: snapshotRef, ExpiresAt: expiry}
	intentRecord := CreationIntentRecord{Version: 1, IntentID: input.IntentID, SandboxID: input.SandboxID, WorkspaceHash: input.Workspace.Hash(), RequestHash: requestHash, ConfigurationDigest: configurationDigest, RestoreEpoch: b.restoreEpoch, Generation: generation, Phase: "pending"}
	requestRecord := CreationRequestRecord{Version: 1, RequestID: input.RequestID, RequestHash: requestHash, ConfigurationDigest: configurationDigest, IntentID: input.IntentID, SandboxID: input.SandboxID, WorkspaceHash: input.Workspace.Hash(), RestoreEpoch: b.restoreEpoch, Generation: generation, Phase: "pending"}
	placementRecord := SandboxPlacementRecord{Version: 1, SandboxID: input.SandboxID, WorkspaceHash: input.Workspace.Hash(), IntentID: input.IntentID, RestoreEpoch: b.restoreEpoch, Partition: input.Workspace.Partition(), Generation: generation}
	values := []interface{ Validate() error }{requestRecord, ownerRecord, fenceRecord, controlRecord, snapshot, intentRecord, placementRecord}
	mutation := Mutation{}
	for i, key := range keys {
		value := snapshotValue
		if i != 4 {
			value, err = encodeDomainRecord(values[i])
			if err != nil {
				return result, err
			}
		}
		mutation.Writes = append(mutation.Writes, Write{Key: key, Value: []byte(value)})
		if i == 2 && fence != nil {
			mutation.Comparisons = append(mutation.Comparisons, clientv3.Compare(clientv3.ModRevision(key), "=", records[i].ModRevision), clientv3.Compare(clientv3.Value(key), "=", string(records[i].Value)), clientv3.Compare(clientv3.LeaseValue(key), "=", 0))
		} else {
			mutation.Comparisons = append(mutation.Comparisons, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
		}
	}
	mutation, _, err = prepareMutation(b.namespace, mutation)
	if err != nil {
		return result, err
	}
	stage, err := b.BeginStage(ctx, input.Workspace.Partition(), input.RequestID, "acquire", mutation, 30*time.Second)
	if err != nil {
		return result, err
	}
	result.Reference = stage.Reference()
	result.Request = requestRecord
	result.Outcome, err = b.CommitStage(ctx, stage)
	// Cleanup has an independent bounded background context even if the caller
	// cancelled or lost the commit reply. It cannot change domain state or outcome.
	result.GuardCleanupError = b.ReleaseStage(context.Background(), stage)
	if result.Outcome == OutcomeCommitted {
		result.Disposition = AcquireCreated
		result.Owner = &ownerRecord
	}
	return result, err
}
