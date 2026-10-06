package etcd

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type RuntimePublishDisposition string

const (
	PublishDeclared RuntimePublishDisposition = "published"
	PublishReplay   RuntimePublishDisposition = "replay"
)

type RuntimePublishResult struct {
	Outcome           Outcome
	Disposition       RuntimePublishDisposition
	Reference         StageReference
	Entry             *RuntimePublicationEntry
	GuardCleanupError error
}

// PublishRuntime commits authenticated ready metadata atomically under the
// original creation fences. Neither a commit nor a replay authorizes execution.
func (b *Backend) PublishRuntime(ctx context.Context, c *CreationClaim, proof []byte) (result RuntimePublishResult, err error) {
	result.Outcome = OutcomeUnknown
	if ctx == nil {
		return result, ErrInvalidRecord
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	comparisons, err := b.creationClaimComparisons(c)
	if err != nil {
		return result, err
	}
	if len(proof) == 0 || len(proof) > 8192 {
		return result, ErrInvalidRecord
	}
	owned := append([]byte(nil), proof...)
	if _, err = b.observePublicationClock(ctx); err != nil {
		return result, err
	}
	bundle, err := b.loadRuntimePreparation(ctx, c.workspace, c.reference.IntentID, "")
	if err != nil {
		return result, err
	}
	evidence, err := b.authenticatePublication(ctx, c, bundle, owned)
	if err != nil {
		return result, err
	}
	d, m := bundle.Dispatch.Record, bundle.Mount.Record
	mountOperation := ""
	if m.MountAttempt == 1 {
		mountOperation = m.OperationID
	}
	record := RuntimePublicationRecord{Version: 1, IntentID: d.IntentID, SandboxID: d.SandboxID, WorkspaceHash: d.WorkspaceHash, RestoreEpoch: d.RestoreEpoch, Generation: d.Generation, DataGateEpoch: c.control.DataGateEpoch, Snapshot: d.Snapshot, ExpiresAt: d.ExpiresAt, OperationID: d.OperationID, PayloadDigest: d.PayloadDigest, ProofDigest: evidence.Digest(), CertificateDigest: bundle.Binding.Record.CertificateDigest, MountOperationID: mountOperation, Runtime: RuntimeReference(evidence.Runtime()), MountAttempt: evidence.MountAttempt(), Claim: preparationClaim(c)}
	existing, err := b.LoadRuntimePublication(ctx, c.workspace, c.reference.IntentID)
	if err != nil {
		return result, err
	}
	if existing != nil {
		record.Attempt = existing.Record.Attempt
		if existing.Record != record || !bytes.Equal(existing.Proof, evidence.Wire()) {
			return result, ErrIdempotencyConflict
		}
		// The four original publishing values intentionally changed at commit. This
		// readonly report checks fresh proof/time and local capability, not those CAS
		// values, and never refreshes the original publication or grants a new Lease.
		if _, err = b.authenticatePublication(ctx, c, bundle, owned); err != nil {
			return result, err
		}
		result.Outcome, result.Disposition, result.Reference, result.Entry = OutcomeCommitted, PublishReplay, existing.Reference, existing
		return result, nil
	}
	ownerKey, _, err := b.namespace.workspaceKeys(c.workspace)
	if err != nil {
		return result, err
	}
	controlKey, _, err := b.namespace.sandboxKeys(c.reference.Partition, c.reference.SandboxID, c.control.Snapshot.Version)
	if err != nil {
		return result, err
	}
	requestKey, err := b.namespace.requestKey(c.request.RequestHash)
	if err != nil {
		return result, err
	}
	intentKey, err := b.namespace.intentKey(c.reference.Partition, c.reference.IntentID)
	if err != nil {
		return result, err
	}
	journalKey, proofKey, err := b.namespace.runtimePublicationKeys(c.reference.Partition, c.reference.IntentID)
	if err != nil {
		return result, err
	}
	var owner WorkspaceOwnerRecord
	var intent CreationIntentRecord
	if err = decodePublicationOriginal(comparisons, ownerKey, &owner); err != nil {
		return result, err
	}
	if err = decodePublicationOriginal(comparisons, intentKey, &intent); err != nil {
		return result, err
	}
	ownerRuntime, controlRuntime := record.Runtime, record.Runtime
	owner.Runtime, owner.MountAttempt = &ownerRuntime, record.MountAttempt
	control, request := c.control, c.request
	control.Runtime, control.MountAttempt, control.Phase = &controlRuntime, record.MountAttempt, PhaseActive
	request.Phase, intent.Phase = "completed", "published"
	body := RuntimePublicationProofRecord{Version: 1, IntentID: d.IntentID, SandboxID: d.SandboxID, WorkspaceHash: d.WorkspaceHash, RestoreEpoch: d.RestoreEpoch, Generation: d.Generation, ProofDigest: evidence.Digest(), Payload: evidence.Wire()}
	comparisons = append(comparisons, immutablePreparationComparisons(bundle.DispatchKVs)...)
	comparisons = append(comparisons, immutablePreparationComparisons(bundle.BindingKVs)...)
	comparisons = append(comparisons, immutablePreparationComparisons(bundle.MountKVs)...)
	comparisons = append(comparisons, clientv3.Compare(clientv3.CreateRevision(journalKey), "=", 0), clientv3.Compare(clientv3.CreateRevision(proofKey), "=", 0))
	build := func(locator StageAttemptLocator) (Mutation, error) {
		record.Attempt = locator
		values := []interface{ Validate() error }{owner, control, request, intent, record, body}
		keys := []string{ownerKey, controlKey, requestKey, intentKey, journalKey, proofKey}
		mutation := Mutation{Comparisons: comparisons}
		for i, v := range values {
			wire, e := encodeDomainRecord(v)
			if e != nil {
				return Mutation{}, e
			}
			mutation.Writes = append(mutation.Writes, Write{Key: keys[i], Value: []byte(wire)})
		}
		return mutation, nil
	}
	// This shared Stage driver preflights the complete 64-operation budget before
	// Grant, preserves the exact reference on unknown Begin, rechecks after Begin,
	// and cleans up only this Stage with its own bounded background context.
	stageResult := PreparationResult{Outcome: OutcomeUnknown}
	err = b.commitPreparation(ctx, c, "runtime_publish", &stageResult, build, func() error { _, e := b.authenticatePublication(ctx, c, bundle, owned); return e })
	result.Outcome, result.Reference, result.GuardCleanupError = stageResult.Outcome, stageResult.Reference, stageResult.GuardCleanupError
	if result.Outcome == OutcomeCommitted {
		result.Disposition = PublishDeclared
		result.Entry = &RuntimePublicationEntry{Record: record, Proof: append(json.RawMessage(nil), body.Payload...), Reference: result.Reference}
	}
	return result, err
}

func (b *Backend) authenticatePublication(ctx context.Context, c *CreationClaim, bundle *runtimePreparationBundle, wire []byte) (controlprotocol.PublicationEvidence, error) {
	if bundle.Binding == nil || bundle.Mount == nil {
		return controlprotocol.PublicationEvidence{}, ErrConflict
	}
	// Authenticate the original certificate too: public structural records cannot
	// choose a runtime, a workspace mode, or a consumed mount operation for proof.
	if _, err := b.authenticatePreparation(ctx, c, bundle, bundle.Binding.Certificate); err != nil {
		return controlprotocol.PublicationEvidence{}, err
	}
	now, err := b.observePublicationClock(ctx)
	if err != nil {
		return controlprotocol.PublicationEvidence{}, err
	}
	ref, m := c.reference, bundle.Mount.Record
	expected := controlprotocol.PublicationContext{Certificate: b.preparationCertificateContext(bundle.Dispatch.Record), Runtime: controlprotocol.RuntimeReference(bundle.Binding.Record.Runtime), CertificateDigest: bundle.Binding.Record.CertificateDigest, Claim: controlprotocol.ClaimReference{ClaimID: ref.ClaimID, CreateRevision: ref.CreateRevision, LeaseID: ref.LeaseID}, DataGateEpoch: c.control.DataGateEpoch, MountAttempt: m.MountAttempt}
	if m.MountAttempt == 1 {
		expected.MountOperationID = m.OperationID
	}
	return b.publicationVerifier.Verify(wire, expected, now)
}

// Recover permanent original values solely from the opaque claim's exact value
// fences. Closed domain decoding rejects unsupported fields instead of dropping
// them during the publication update. No current owner is borrowed or replaced.
func decodePublicationOriginal(comparisons []clientv3.Cmp, key string, destination interface{ Validate() error }) error {
	for _, cmp := range comparisons {
		if string(cmp.Key) == key && cmp.Target == pb.Compare_VALUE && cmp.Result == pb.Compare_EQUAL {
			value := pb.Compare(cmp)
			return decodeDomainRecord(&mvccpb.KeyValue{Key: []byte(key), Value: append([]byte(nil), value.GetValue()...)}, destination)
		}
	}
	return ErrCorruptRecord
}
