package etcd

import (
	"context"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

// ConfirmDetachedCleanupTarget checks that this Backend's immutable pinned
// plain profile supports the target of an original current read view. It reads
// and authenticates native snapshot, intent and enrolled history independently
// of caller DTOs. It grants neither a TaskClaim nor permission to send effects;
// an external automatic worker must still obtain its own managed authority.
func (b *Backend) ConfirmDetachedCleanupTarget(ctx context.Context, view *SandboxReadView) error {
	if b == nil || ctx == nil || b.detachedCleanup == nil || b.enrolledPublication == nil || b.detachedCleanup.contractDigest != b.enrolledPublication.ContractDigest {
		return ErrInvalidConfiguration
	}
	if err := b.ConfirmSandboxView(ctx, view); err != nil {
		return err
	}
	c, placement := view.records.Control, view.records.Placement
	snapshot, err := b.LoadSnapshot(ctx, placement.Partition, c.SandboxID, c.Snapshot)
	if err != nil {
		return err
	}
	if snapshot == nil || snapshot.Version != 1 || snapshot.LocalCleanup != nil {
		return ErrInvalidConfiguration
	}
	history, err := b.ReadSandboxRuntime(ctx, view)
	if err != nil {
		return err
	}
	if history == nil || history.Publication == nil || history.Dispatch == nil || history.Publication.Record.MountAttempt != 0 {
		return ErrInvalidConfiguration
	}
	key, err := b.namespace.intentKey(placement.Partition, c.IntentID)
	if err != nil {
		return err
	}
	points, err := b.readEnrolledPoints(ctx, []string{key}, []int{4096})
	if err != nil {
		return err
	}
	var intent CreationIntentRecord
	if points[0] == nil || points[0].Lease != 0 || decodeDomainRecord(points[0], &intent) != nil {
		return ErrCorruptRecord
	}
	identity, err := decodeIdentity(b.identityValue, b.namespace)
	if err != nil {
		return err
	}
	workspace, err := detachedIdentityFromRequestHash(identity.StorageID, intent.RequestHash)
	if err != nil {
		return err
	}
	if _, err = b.decodeCreationIntent(points[0], workspace, c.IntentID); err != nil {
		return err
	}
	r := history.Publication.Record
	if workspace.Hash() != c.WorkspaceHash || workspace.Partition() != placement.Partition || intent.Phase != "published" || intent.SandboxID != c.SandboxID || intent.Generation != c.Generation || intent.WorkspaceHash != c.WorkspaceHash || intent.IntentID != c.IntentID || r.SandboxID != snapshot.SandboxID || r.Snapshot != snapshot.Snapshot {
		return ErrInvalidConfiguration
	}
	if err := b.authenticateDetachedCleanupPublication(history.Publication); err != nil {
		return err
	}
	return b.ConfirmSandboxView(ctx, view)
}

// detachedCleanupSource classifies copied historical metadata and reconstructs
// only an ordinary detached identity. It authenticates the original signed
// publication independently of the supplied DTO's fields. Neither its return
// value nor a valid TaskID proves Task birth, current Claim/Run ownership or
// absence of external effects; durable writers must authenticate and pin the
// original native records under their own authority checks.
func (b *Backend) detachedCleanupSource(task TaskRecord, snapshot SandboxSnapshotRecord, intent CreationIntentRecord, publication *RuntimeEnrolledPublicationEntry) (WorkspaceIdentity, error) {
	workspace, err := b.detachedCleanupSourceRecords(task, snapshot, intent, publication)
	if err != nil {
		return WorkspaceIdentity{}, err
	}
	if !validHexDigest(publication.Reference.Digest) || publication.Reference != publication.Record.Attempt.reference(publication.Reference.Digest) {
		return WorkspaceIdentity{}, ErrInvalidRecord
	}
	return workspace, nil
}

// The surviving-history reader already authenticates actual publication/fact
// stamps and four native companions. It has no publication Stage receipt, so
// this common pure records/signature check must neither invent a Reference nor
// grant authority. The original wrapper retains its full Reference check.
func (b *Backend) detachedCleanupSourceRecords(task TaskRecord, snapshot SandboxSnapshotRecord, intent CreationIntentRecord, publication *RuntimeEnrolledPublicationEntry) (WorkspaceIdentity, error) {
	if b == nil || b.detachedCleanup == nil || b.enrolledPublication == nil || b.publicationVerifier == nil || b.detachedCleanup.contractDigest != b.enrolledPublication.ContractDigest {
		return WorkspaceIdentity{}, ErrInvalidConfiguration
	}
	if publication == nil || task.Validate() != nil || snapshot.Validate() != nil || intent.Validate() != nil || publication.Record.Validate() != nil {
		return WorkspaceIdentity{}, ErrInvalidRecord
	}
	if snapshot.Version != 1 || snapshot.LocalCleanup != nil || intent.Phase != "published" {
		return WorkspaceIdentity{}, ErrInvalidRecord
	}

	identity, err := decodeIdentity(b.identityValue, b.namespace)
	if err != nil {
		return WorkspaceIdentity{}, err
	}
	workspace, err := detachedIdentityFromRequestHash(identity.StorageID, intent.RequestHash)
	if err != nil {
		return WorkspaceIdentity{}, err
	}

	r := publication.Record
	if task.Reference.Namespace != b.namespace.Root() || task.Reference.RestoreEpoch != b.restoreEpoch || task.Reference.Partition != workspace.Partition() || task.WorkspaceHash != workspace.Hash() || intent.WorkspaceHash != workspace.Hash() || r.WorkspaceHash != workspace.Hash() || intent.RestoreEpoch != b.restoreEpoch || r.RestoreEpoch != b.restoreEpoch || r.Attempt.Namespace != b.namespace.Root() || r.Attempt.Partition != workspace.Partition() {
		return WorkspaceIdentity{}, ErrInvalidRecord
	}
	if task.Reference.SandboxID != snapshot.SandboxID || task.Reference.SandboxID != intent.SandboxID || task.Reference.SandboxID != r.SandboxID || task.CreationIntentID != intent.IntentID || task.CreationIntentID != r.IntentID || task.Generation != intent.Generation || task.Generation != r.Generation || task.Runtime != r.Runtime || task.DataGateEpoch != r.DataGateEpoch || task.Snapshot != snapshot.Snapshot || task.Snapshot != r.Snapshot {
		return WorkspaceIdentity{}, ErrInvalidRecord
	}
	// ConfigurationDigest includes the original acquisition TTL, which is not
	// recoverable from these copies. Its codec is validated above; it is not a
	// payload digest. A Task may also have a later expiry after a legal TTL
	// update. Historical certificate authentication retains r.ExpiresAt below.
	if err := b.authenticateDetachedCleanupPublication(publication); err != nil {
		return WorkspaceIdentity{}, err
	}
	return workspace, nil
}

// authenticateDetachedCleanupPublication reuses the original pure verifier:
// proof, activation, runtime/issuer/observer certificates and enrollment receipt
// must all authenticate in the original signed time windows and plain contract.
// This performs no native reads and cannot turn copied record stamps into proof
// of a durable KV; effect writers still need the original historical read path.
func (b *Backend) authenticateDetachedCleanupPublication(entry *RuntimeEnrolledPublicationEntry) error {
	r := entry.Record
	context := p.PublicationContext{
		Certificate: p.CertificateContext{
			Namespace: b.namespace.Root(), AuthorityID: b.publicationAuthorityID, Target: b.publicationTarget, RestoreEpoch: r.RestoreEpoch,
			IntentID: r.IntentID, SandboxID: r.SandboxID, WorkspaceHash: r.WorkspaceHash, Generation: r.Generation,
			OperationID: r.OperationID, PayloadDigest: r.PayloadDigest, Snapshot: p.SnapshotReference(r.Snapshot), ExpiresAt: r.ExpiresAt,
		},
		Runtime: p.RuntimeReference(r.Runtime), CertificateDigest: r.CertificateDigest,
		Claim: p.ClaimReference{ClaimID: r.Claim.ClaimID, CreateRevision: r.Claim.CreateRevision, LeaseID: r.Claim.LeaseID}, DataGateEpoch: r.DataGateEpoch,
	}
	verified, err := b.publicationVerifier.VerifyEnrolledPublicationHistorical(entry.Proof, context, entry.Enrollment, b.detachedCleanup.contractDigest)
	if err != nil {
		return err
	}

	ready, enrollment := verified.Ready(), verified.EnrollmentReceipt()
	if verified.Digest() != r.ProofDigest || ready.EnrollmentDigest != r.EnrollmentDigest || ready.ObserverCertificateDigest != r.ObserverCertificateDigest || ready.ActivationDigest != r.ActivationDigest || enrollment.NodeUID != r.NodeUID || enrollment.NodeBootID != r.NodeBootID {
		return ErrCorruptRecord
	}
	return nil
}
