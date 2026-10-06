package etcd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type PreparationResult struct {
	Outcome           Outcome
	Reference         StageReference
	Binding           *RuntimeBindingEntry
	Mount             *RuntimeMountIntentEntry
	Replay            bool
	GuardCleanupError error
}

// BindRuntime reserves an authenticated exact runtime forever. Its result is
// metadata evidence only and never grants permission to invoke that runtime.
func (b *Backend) BindRuntime(ctx context.Context, c *CreationClaim, wire []byte) (result PreparationResult, err error) {
	result.Outcome = OutcomeUnknown
	if ctx == nil || len(wire) > 4096 {
		return result, ErrInvalidRecord
	}
	owned := append([]byte(nil), wire...)
	comparisons, err := b.creationClaimComparisons(c)
	if err != nil {
		return result, err
	}
	if _, err = b.observePublicationClock(ctx); err != nil {
		return result, err
	}
	initial, err := b.loadRuntimePreparation(ctx, c.workspace, c.reference.IntentID, "")
	if err != nil {
		return result, err
	}
	evidence, err := b.authenticatePreparation(ctx, c, initial, owned)
	if err != nil {
		return result, err
	}
	bundle := initial
	if initial.Binding == nil {
		bundle, err = b.loadRuntimePreparation(ctx, c.workspace, c.reference.IntentID, evidence.Runtime().UID)
		if err != nil {
			return result, err
		}
		evidence, err = b.authenticatePreparation(ctx, c, bundle, owned)
		if err != nil {
			return result, err
		}
	}
	if bundle.Binding != nil {
		if !bytes.Equal(bundle.Binding.Certificate, evidence.Wire()) || bundle.Binding.Record.CertificateDigest != evidence.Digest() || bundle.Binding.Record.Runtime != RuntimeReference(evidence.Runtime()) || bundle.Binding.Record.WorkspaceMode != evidence.WorkspaceMode() {
			return result, ErrIdempotencyConflict
		}
		if err = b.checkPreparationReplay(ctx, c); err != nil {
			return result, err
		}
		result.Outcome, result.Reference, result.Binding, result.Replay = OutcomeCommitted, bundle.Binding.Reference, bundle.Binding, true
		return result, nil
	}
	if bundle.IndexKV != nil {
		var reservation RuntimeIndexRecord
		if !immutableDispatchKV(bundle.IndexKV) || decodeDomainRecord(bundle.IndexKV, &reservation) != nil || reservation.Runtime.UID != evidence.Runtime().UID {
			return result, ErrCorruptRecord
		}
		if reservation.IntentID == c.reference.IntentID && reservation.WorkspaceHash == c.workspace.Hash() {
			return result, ErrCorruptRecord
		}
		return result, ErrConflict
	}
	d := bundle.Dispatch.Record
	r := RuntimeBindingRecord{Version: 1, IntentID: d.IntentID, SandboxID: d.SandboxID, WorkspaceHash: d.WorkspaceHash, RestoreEpoch: d.RestoreEpoch, Generation: d.Generation, Snapshot: d.Snapshot, ExpiresAt: d.ExpiresAt, OperationID: d.OperationID, PayloadDigest: d.PayloadDigest, CertificateDigest: evidence.Digest(), WorkspaceMode: evidence.WorkspaceMode(), Runtime: RuntimeReference(evidence.Runtime()), Claim: preparationClaim(c)}
	cert := RuntimeBindingCertificateRecord{Version: 1, IntentID: r.IntentID, SandboxID: r.SandboxID, WorkspaceHash: r.WorkspaceHash, RestoreEpoch: r.RestoreEpoch, Generation: r.Generation, CertificateDigest: r.CertificateDigest, Payload: evidence.Wire()}
	index := RuntimeIndexRecord{Version: 1, IntentID: r.IntentID, SandboxID: r.SandboxID, WorkspaceHash: r.WorkspaceHash, RestoreEpoch: r.RestoreEpoch, Generation: r.Generation, Runtime: r.Runtime}
	comparisons = append(comparisons, immutablePreparationComparisons(bundle.DispatchKVs)...)
	for _, key := range []string{bundle.BindingKey, bundle.CertificateKey, bundle.IndexKey} {
		comparisons = append(comparisons, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	}
	build := func(locator StageAttemptLocator) (Mutation, error) {
		r.Attempt = locator
		bindingWire, e := encodeDomainRecord(r)
		if e != nil {
			return Mutation{}, e
		}
		certificateWire, e := encodeDomainRecord(cert)
		if e != nil {
			return Mutation{}, e
		}
		indexWire, e := encodeDomainRecord(index)
		if e != nil {
			return Mutation{}, e
		}
		return Mutation{Comparisons: comparisons, Writes: []Write{{Key: bundle.BindingKey, Value: []byte(bindingWire)}, {Key: bundle.CertificateKey, Value: []byte(certificateWire)}, {Key: bundle.IndexKey, Value: []byte(indexWire)}}}, nil
	}
	err = b.commitPreparation(ctx, c, "runtime_bind", &result, build, func() error { _, e := b.authenticatePreparation(ctx, c, bundle, owned); return e })
	if result.Outcome == OutcomeCommitted {
		result.Binding = &RuntimeBindingEntry{Record: r, Certificate: append(json.RawMessage(nil), cert.Payload...), Reference: result.Reference}
	}
	return result, err
}

// ConsumeRuntimeMount records the sole mount attempt (or explicit plain-mode
// no-mount decision). Callers still need separate authority for external effects.
func (b *Backend) ConsumeRuntimeMount(ctx context.Context, c *CreationClaim) (result PreparationResult, err error) {
	result.Outcome = OutcomeUnknown
	if ctx == nil {
		return result, ErrInvalidRecord
	}
	comparisons, err := b.creationClaimComparisons(c)
	if err != nil {
		return result, err
	}
	if _, err = b.observePublicationClock(ctx); err != nil {
		return result, err
	}
	bundle, err := b.loadRuntimePreparation(ctx, c.workspace, c.reference.IntentID, "")
	if err != nil {
		return result, err
	}
	if bundle.Binding == nil {
		return result, ErrConflict
	}
	if _, err = b.authenticatePreparation(ctx, c, bundle, bundle.Binding.Certificate); err != nil {
		return result, err
	}
	if bundle.Mount != nil {
		if err = b.checkPreparationReplay(ctx, c); err != nil {
			return result, err
		}
		result.Outcome, result.Reference, result.Binding, result.Mount, result.Replay = OutcomeCommitted, bundle.Mount.Reference, bundle.Binding, bundle.Mount, true
		return result, nil
	}
	r := bundle.Binding.Record
	mount := RuntimeMountIntentRecord{Version: 1, IntentID: r.IntentID, SandboxID: r.SandboxID, WorkspaceHash: r.WorkspaceHash, RestoreEpoch: r.RestoreEpoch, Generation: r.Generation, Runtime: r.Runtime, CertificateDigest: r.CertificateDigest, DispatchOperationID: r.OperationID, OperationID: uuid.NewString(), WorkspaceMode: r.WorkspaceMode, Claim: preparationClaim(c)}
	if r.WorkspaceMode == "fuse" {
		mount.MountAttempt = 1
	}
	comparisons = append(comparisons, immutablePreparationComparisons(bundle.DispatchKVs)...)
	comparisons = append(comparisons, immutablePreparationComparisons(bundle.BindingKVs)...)
	comparisons = append(comparisons, clientv3.Compare(clientv3.CreateRevision(bundle.MountKey), "=", 0))
	build := func(locator StageAttemptLocator) (Mutation, error) {
		mount.Attempt = locator
		wire, e := encodeDomainRecord(mount)
		if e != nil {
			return Mutation{}, e
		}
		return Mutation{Comparisons: comparisons, Writes: []Write{{Key: bundle.MountKey, Value: []byte(wire)}}}, nil
	}
	err = b.commitPreparation(ctx, c, "runtime_mount", &result, build, func() error { _, e := b.authenticatePreparation(ctx, c, bundle, bundle.Binding.Certificate); return e })
	if result.Outcome == OutcomeCommitted {
		result.Binding = bundle.Binding
		result.Mount = &RuntimeMountIntentEntry{Record: mount, Reference: result.Reference}
	}
	return result, err
}

func preparationClaim(c *CreationClaim) DispatchClaimReference {
	r := c.reference
	return DispatchClaimReference{ClaimID: r.ClaimID, WorkerID: r.WorkerID, CreateRevision: r.CreateRevision, LeaseID: r.LeaseID}
}
func immutablePreparationComparisons(values []*mvccpb.KeyValue) []clientv3.Cmp {
	comparisons := make([]clientv3.Cmp, 0, len(values)*2)
	for _, kv := range values {
		key := string(kv.Key)
		comparisons = append(comparisons, clientv3.Compare(clientv3.ModRevision(key), "=", kv.ModRevision), clientv3.Compare(clientv3.LeaseValue(key), "=", 0))
	}
	return comparisons
}
func (b *Backend) preparationCertificateContext(d RuntimeDispatchRecord) controlprotocol.CertificateContext {
	return controlprotocol.CertificateContext{Namespace: b.namespace.Root(), AuthorityID: b.publicationAuthorityID, Target: d.Target, RestoreEpoch: d.RestoreEpoch, IntentID: d.IntentID, SandboxID: d.SandboxID, WorkspaceHash: d.WorkspaceHash, Generation: d.Generation, OperationID: d.OperationID, PayloadDigest: d.PayloadDigest, Snapshot: controlprotocol.SnapshotReference(d.Snapshot), ExpiresAt: d.ExpiresAt}
}
func (b *Backend) authenticatePreparation(ctx context.Context, c *CreationClaim, bundle *runtimePreparationBundle, wire []byte) (controlprotocol.CertificateEvidence, error) {
	if _, err := b.creationClaimComparisons(c); err != nil {
		return controlprotocol.CertificateEvidence{}, err
	}
	if bundle.Dispatch == nil {
		return controlprotocol.CertificateEvidence{}, ErrConflict
	}
	if !c.matchesRuntimeDispatch(bundle.Dispatch.Record) {
		return controlprotocol.CertificateEvidence{}, ErrCorruptRecord
	}
	now, err := b.observePublicationClock(ctx)
	if err != nil {
		return controlprotocol.CertificateEvidence{}, err
	}
	evidence, err := b.publicationVerifier.VerifyCertificate(wire, b.preparationCertificateContext(bundle.Dispatch.Record), now)
	if err != nil {
		return controlprotocol.CertificateEvidence{}, err
	}
	// The durable binding must agree with authenticated certificate contents;
	// structural public loading by itself can never supply these assertions.
	if bundle.Binding != nil && bytes.Equal(wire, bundle.Binding.Certificate) {
		r := bundle.Binding.Record
		if r.Runtime != RuntimeReference(evidence.Runtime()) || r.WorkspaceMode != evidence.WorkspaceMode() || r.CertificateDigest != evidence.Digest() {
			return controlprotocol.CertificateEvidence{}, ErrCorruptRecord
		}
	}
	return evidence, nil
}
func (b *Backend) commitPreparation(ctx context.Context, c *CreationClaim, stageID string, result *PreparationResult, build func(StageAttemptLocator) (Mutation, error), recheck func() error) error {
	stage, err := b.beginStageWithBuilder(ctx, c.reference.Partition, c.request.RequestID, stageID, 30*time.Second, func(locator StageAttemptLocator) (Mutation, error) {
		mutation, e := build(locator)
		if e != nil {
			return Mutation{}, e
		}
		_, digest, e := prepareMutation(b.namespace, mutation)
		if e != nil {
			return Mutation{}, e
		}
		result.Reference = locator.reference(digest)
		return mutation, nil
	})
	if err != nil {
		var failure *stageBeginFailure
		if errors.As(err, &failure) {
			result.GuardCleanupError = failure.cleanup
			return failure.cause
		}
		return err
	}
	defer func() { result.GuardCleanupError = b.ReleaseStage(context.Background(), stage) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := recheck(); err != nil {
		return err
	}
	if _, err := b.creationClaimComparisons(c); err != nil {
		return err
	}
	result.Outcome, err = b.CommitStage(ctx, stage)
	return err
}

// Readonly replays still reject changed server claim/control fences. This does
// not renew a Lease and grants no mutation or runtime capability.
func (b *Backend) checkPreparationReplay(ctx context.Context, c *CreationClaim) error {
	comparisons, err := b.creationClaimComparisons(c)
	if err != nil {
		return err
	}
	bounded, cancel := b.requestContext(ctx)
	defer cancel()
	response, err := b.client.Txn(bounded).If(append(b.baseComparisons(), comparisons...)...).Then().Commit()
	if err != nil {
		return err
	}
	if response == nil || response.Header == nil || response.Header.ClusterId != b.clusterID {
		return ErrIdentityMismatch
	}
	if !response.Succeeded {
		return ErrConflict
	}
	return ctx.Err()
}
