package etcd

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// RuntimeDispatchInput contains the external operation's immutable input.
type RuntimeDispatchInput struct {
	Kind    RuntimeDispatchKind
	Target  string
	Payload json.RawMessage
}

type RuntimeDispatchDisposition string

const (
	DispatchDeclared RuntimeDispatchDisposition = "declared"
	DispatchReplay   RuntimeDispatchDisposition = "replay"
)

// RuntimeDispatchResult reports metadata evidence, never runtime execution authority.
type RuntimeDispatchResult struct {
	Outcome           Outcome
	Disposition       RuntimeDispatchDisposition
	Reference         StageReference
	Entry             *RuntimeDispatchEntry
	GuardCleanupError error
}

// DeclareRuntimeDispatch atomically records an operation under the original
// creation fences. Committed and replay results prove metadata only. An unknown
// result retains its exact attempt reference and never grants runtime authority.
func (b *Backend) DeclareRuntimeDispatch(ctx context.Context, c *CreationClaim, input RuntimeDispatchInput) (result RuntimeDispatchResult, err error) {
	result.Outcome = OutcomeUnknown
	if ctx == nil {
		return result, ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	comparisons, err := b.creationClaimComparisons(c)
	if err != nil {
		return result, err
	}
	if (input.Kind != RuntimeDispatchCreate && input.Kind != RuntimeDispatchPrepare) || !validOpaque(input.Target, 128) {
		return result, ErrInvalidRecord
	}
	// Compact into private storage before the first RPC can yield to the caller.
	payload, err := compactSnapshot(input.Payload)
	if err != nil {
		return result, err
	}
	digest, err := snapshotDigest(payload)
	if err != nil {
		return result, err
	}
	ref := c.reference
	body := RuntimeDispatchInputRecord{Version: 1, IntentID: ref.IntentID, SandboxID: ref.SandboxID, WorkspaceHash: ref.WorkspaceHash, RestoreEpoch: ref.RestoreEpoch, Generation: ref.Generation, OperationID: "00000000-0000-0000-0000-000000000000", PayloadDigest: digest, Payload: payload}
	// Include the complete envelope in the payload limit, even on replay.
	if _, err := encodeDomainRecord(body); err != nil {
		return result, err
	}
	existing, err := b.LoadRuntimeDispatch(ctx, c.workspace, ref.IntentID)
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if _, err := b.creationClaimComparisons(c); err != nil {
		return result, err
	}
	if existing != nil {
		if !c.matchesRuntimeDispatch(existing.Record) {
			return result, ErrCorruptRecord
		}
		if existing.Record.Kind != input.Kind || existing.Record.Target != input.Target || existing.Record.PayloadDigest != digest {
			return result, ErrIdempotencyConflict
		}
		result.Outcome, result.Disposition, result.Reference, result.Entry = OutcomeCommitted, DispatchReplay, existing.Reference, existing
		return result, nil
	}
	dk, ik, err := b.namespace.runtimeDispatchKeys(ref.Partition, ref.IntentID)
	if err != nil {
		return result, err
	}
	operationID := uuid.NewString()
	body.OperationID = operationID
	record := RuntimeDispatchRecord{Version: 1, IntentID: ref.IntentID, SandboxID: ref.SandboxID, WorkspaceHash: ref.WorkspaceHash, RequestHash: c.request.RequestHash, ConfigurationDigest: c.request.ConfigurationDigest, RestoreEpoch: ref.RestoreEpoch, Generation: ref.Generation, DataGateEpoch: c.control.DataGateEpoch, Snapshot: c.control.Snapshot, ExpiresAt: c.control.ExpiresAt, OperationID: operationID, Kind: input.Kind, Target: input.Target, PayloadDigest: digest, Claim: DispatchClaimReference{ClaimID: ref.ClaimID, WorkerID: ref.WorkerID, CreateRevision: ref.CreateRevision, LeaseID: ref.LeaseID}}
	stage, err := b.beginStageWithBuilder(ctx, ref.Partition, c.request.RequestID, "runtime_dispatch", 30*time.Second, func(locator StageAttemptLocator) (Mutation, error) {
		record.Attempt = locator
		declaration, err := encodeDomainRecord(record)
		if err != nil {
			return Mutation{}, err
		}
		encodedInput, err := encodeDomainRecord(body)
		if err != nil {
			return Mutation{}, err
		}
		comparisons = append(comparisons, clientv3.Compare(clientv3.CreateRevision(dk), "=", 0), clientv3.Compare(clientv3.CreateRevision(ik), "=", 0))
		mutation := Mutation{Comparisons: comparisons, Writes: []Write{{Key: dk, Value: []byte(declaration)}, {Key: ik, Value: []byte(encodedInput)}}}
		// Preserve the exact reference even if guard allocation loses its reply.
		// The builder and BeginStage use the same closed mutation encoding.
		_, mutationDigest, err := prepareMutation(b.namespace, mutation)
		if err != nil {
			return Mutation{}, err
		}
		result.Reference = locator.reference(mutationDigest)
		return mutation, nil
	})
	if err != nil {
		return result, err
	}
	defer func() { result.GuardCleanupError = b.ReleaseStage(context.Background(), stage) }()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if _, err := b.creationClaimComparisons(c); err != nil {
		return result, err
	}
	result.Outcome, err = b.CommitStage(ctx, stage)
	if result.Outcome == OutcomeCommitted {
		result.Disposition = DispatchDeclared
		result.Entry = &RuntimeDispatchEntry{Record: record, Payload: payload, Reference: result.Reference}
	}
	return result, err
}

// Creator attribution can differ on recovery. The durable ownership, request,
// gate, snapshot and business deadline must still match the validated claim.
func (c *CreationClaim) matchesRuntimeDispatch(r RuntimeDispatchRecord) bool {
	return r.IntentID == c.reference.IntentID && r.SandboxID == c.reference.SandboxID &&
		r.WorkspaceHash == c.workspace.Hash() && r.RestoreEpoch == c.reference.RestoreEpoch &&
		r.Generation == c.reference.Generation && r.DataGateEpoch == c.control.DataGateEpoch &&
		r.RequestHash == c.request.RequestHash && r.ConfigurationDigest == c.request.ConfigurationDigest &&
		r.Attempt.RequestID == c.request.RequestID && r.Snapshot == c.control.Snapshot && r.ExpiresAt.Equal(c.control.ExpiresAt)
}
