package etcd

import (
	"bytes"
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	k "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/goairix/sandbox/internal/runtime/launcher"
)

// A target is constructed only for one admitted Task, from its authenticated
// snapshot descriptor and enrolled history. No opaque business payload or
// caller-selected runtime address participates in this derivation.
type cleanupLocalTarget struct {
	snapshot    SandboxSnapshotRecord
	publication *RuntimeEnrolledPublicationEntry
	issuer      CommandIssuerEntry
	plan        TaskWorkspaceSyncPlan
	// Detached targets retain their authentic ordinary identity separately;
	// no local/archive plan is manufactured for that profile.
	detachedWorkspace WorkspaceIdentity
	root              string
	// This diagnostic reference supplies only prospective wire dimensions.
	// Grant revalidates the genuine Kernel and the actual persisted Run.
	worker      launcher.WorkspaceSyncWorkerReference
	destination *ct.Destination
	termination *ct.TerminationDestination
}

func (b *Backend) cleanupPhysicalProfile() error {
	c := b.localCleanup
	if c == nil || c.Worker == nil || c.Bridge == nil || c.Credential == nil || c.ObserverCredential == nil || len(c.Nodes) == 0 || b.archiveStore == nil || b.archiveStore.recovery == nil || b.localRuntimeRelease == nil || b.enrolledVerifier == nil {
		return ErrInvalidConfiguration
	}
	if _, err := c.Worker.WorkspaceSyncWorkerReference(); err != nil {
		return ErrInvalidConfiguration
	}
	return nil
}

func (b *Backend) cleanupSnapshot(ctx context.Context, task TaskRecord) (*cleanupLocalTarget, error) {
	// The sole actor owns only native metadata here. Kernel validation and
	// filesystem opens run in the registered job after its owner is released.
	if b.localCleanup == nil {
		return nil, ErrInvalidConfiguration
	}
	snapshot, err := b.LoadSnapshot(ctx, task.Reference.Partition, task.Reference.SandboxID, task.Snapshot)
	if err != nil || snapshot == nil || snapshot.Version != 2 || snapshot.LocalCleanup == nil {
		return nil, errors.Join(ErrInvalidConfiguration, err)
	}
	d := snapshot.LocalCleanup
	c := b.localCleanup
	workspace, err := NewWorkspaceIdentity("local", c.Storage.Identity, c.Storage.Bucket, d.Prefix)
	if err != nil || workspace.hash != task.WorkspaceHash || workspace.partition != task.Reference.Partition || workspace.storageIdentityHash != d.StorageIdentityHash || workspace.bucket != d.Bucket || !strings.HasPrefix(d.Prefix, c.Storage.PrefixBase) {
		return nil, ErrInvalidConfiguration
	}
	plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspace, Exclude: d.Exclude, DeleteExtraneous: d.DeleteExtraneous})
	if err != nil {
		return nil, err
	}
	root := filepath.Join(c.Storage.Directory, filepath.FromSlash(strings.TrimSuffix(d.Prefix, "/")))
	rel, err := filepath.Rel(c.Storage.Directory, root)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || !validWorkspaceSyncLocalRoot(root) {
		return nil, ErrInvalidConfiguration
	}
	return &cleanupLocalTarget{snapshot: *snapshot, plan: plan, root: root}, nil
}

// prepareCleanupLocalTarget belongs to the same bounded registry job as the
// eventual physical chain. Filesystem calls may outlive context cancellation:
// Stop/Close must await this goroutine, while the actor remains free to renew.
// A copied job or an active native owner cannot enter the target check.
func (b *Backend) prepareCleanupLocalTarget(j *cleanupLocalJob) error {
	if b == nil || b.archiveLife == nil || j == nil || j.worker == nil || j.worker.backend != b || j.worker.self != j.worker || j.ctx == nil || j.target == nil || j.bundle == nil || j.bundle.task.Reference != j.task {
		return ErrInvalidRecord
	}
	b.archiveLife.mu.Lock()
	registered := false
	for _, job := range j.worker.cleanupJobs {
		if job == j {
			registered = true
		}
	}
	valid := registered && j.started && j.owner == nil && j.request == nil && j.step == "target"
	b.archiveLife.mu.Unlock()
	if !valid {
		return ErrInvalidRecord
	}
	if err := j.ctx.Err(); err != nil {
		return err
	}
	if err := b.cleanupPhysicalProfile(); err != nil {
		return err
	}
	worker, err := b.localCleanup.Worker.WorkspaceSyncWorkerReference()
	if err != nil {
		return err
	}

	// This read-only open validates actual uid/gid, filesystem, mount and
	// no-symlink resolution. No slot or Run exists merely because it succeeds.
	// Bind and finalization still repeat their original authority checks.
	target, err := openTaskWorkspaceSyncLocalTarget(j.ctx, b.localCleanup.Worker, j.target.plan, j.target.root)
	if err != nil {
		return err
	}
	if err = target.Close(); err != nil {
		return err
	}
	j.target.worker = worker
	return nil
}

func (b *Backend) cleanupPublication(ctx context.Context, task TaskRecord, target *cleanupLocalTarget) error {
	read, err := b.loadRuntimePublicationEnrolledAt(ctx, historyLocator(target.workspaceIdentity()), task.CreationIntentID)
	if err != nil || read == nil || read.Publication == nil || read.Dispatch == nil {
		return errors.Join(ErrInvalidConfiguration, err)
	}
	entry := read.Publication
	r := entry.Record
	if r.SandboxID != task.Reference.SandboxID || r.WorkspaceHash != task.WorkspaceHash || r.Runtime != task.Runtime || r.Generation != task.Generation || r.DataGateEpoch != task.DataGateEpoch || r.Snapshot != task.Snapshot || r.MountAttempt != 0 {
		return ErrInvalidConfiguration
	}
	// Reuse the original verifier to obtain its sealed activation projection;
	// decoded activation JSON is never promoted into an independent authority.
	verified, err := b.publicationVerifier.VerifyEnrolledPublicationHistorical(entry.Proof, p.PublicationContext{
		Certificate: b.preparationCertificateContext(read.Dispatch.Record), Runtime: p.RuntimeReference(r.Runtime),
		CertificateDigest: r.CertificateDigest, Claim: p.ClaimReference{ClaimID: r.Claim.ClaimID, CreateRevision: r.Claim.CreateRevision, LeaseID: r.Claim.LeaseID}, DataGateEpoch: r.DataGateEpoch,
	}, entry.Enrollment, b.enrolledPublication.ContractDigest)
	if err != nil {
		return err
	}
	c := b.localCleanup
	var node *LocalCleanupNode
	for i := range c.Nodes {
		if c.Nodes[i].UID == r.NodeUID && c.Nodes[i].BootID == r.NodeBootID {
			node = &c.Nodes[i]
		}
	}
	if node == nil {
		return ErrInvalidConfiguration
	}
	activation := verified.Activation()
	birth := activation.Birth()
	bridgeTarget := k.EnrolledBridgeTarget{PodName: r.Runtime.ID, PodUID: r.Runtime.UID, NodeName: node.Name, NodeUID: node.UID, NodeBootID: node.BootID, LogicalID: r.SandboxID, CreationIntentID: r.IntentID, Generation: r.Generation, Authority: activation.Binding(), ContractDigest: birth.ContractDigest, UID: birth.UID, GID: birth.GID, NetworkAllowed: birth.NetworkAllowed}
	// Keep original enrollment/termination history immutable. Current TLS may
	// have changed before Destroy, or a fixed renewal may have installed its
	// candidate while its metadata outcome remains unknown. Neither observation
	// authorizes execution or reopens the gate; cleanup still uses its original
	// task tickets, actual signed responses and owner fences.
	view, err := b.ReadSandboxView(ctx, task.Reference.SandboxID)
	if err != nil {
		return err
	}
	if view != nil {
		control := view.Records().Control
		if control.Runtime == nil || *control.Runtime != task.Runtime || control.Generation != task.Generation || control.DataGateEpoch != task.DataGateEpoch || control.IntentID != task.CreationIntentID || control.WorkspaceHash != task.WorkspaceHash {
			return ErrConflict
		}
		current, err := b.ReadSandboxTTLCurrent(ctx, view)
		if err != nil {
			return err
		}
		if current != nil {
			activation = current.Activation
		}
		pending, err := b.ReadSandboxTTLPending(ctx, view)
		if err != nil {
			return err
		}
		if pending != nil {
			activation = pending.Activation
		}
	}
	destination, err := ct.NewDestination(ct.DestinationOptions{Identity: activation.Identity(), RuntimeCertificate: activation.RuntimeCertificate(), Credential: c.Credential, Verifier: b.enrolledVerifier, Clock: b.authorityClock, Dial: func(ctx context.Context) (net.Conn, error) {
		return c.Bridge.Dial(ctx, bridgeTarget)
	}})
	if err != nil {
		return err
	}
	termination, err := ct.NewTerminationDestination(ct.TerminationDestinationOptions{Address: node.ObserverAddress, Binding: c.binding, Roots: c.roots, Clock: b.authorityClock, Credential: c.ObserverCredential, ObserverCertificate: entry.Enrollment.ObserverCertificate, NodeUID: node.UID, NodeBootID: node.BootID, EnrollmentDigest: r.EnrollmentDigest})
	if err != nil {
		return err
	}
	target.publication, target.destination, target.termination = entry, destination, termination
	return nil
}

func (t *cleanupLocalTarget) workspaceIdentity() WorkspaceIdentity {
	if t.detachedWorkspace.Kind() == WorkspaceIdentityDetached {
		return t.detachedWorkspace
	}
	return t.plan.workspace
}

// All four fixed signing roles must use the configured credential. This
// bounded registration is at most two native RPCs under the original owner.
func (b *Backend) cleanupIssuer(ctx context.Context, target *cleanupLocalTarget) error {
	bounded, cancel := b.requestContext(ctx)
	defer cancel()
	var certificate []byte
	issuers := []interface {
		Certificate(context.Context) ([]byte, error)
	}{b.taskIssuer, b.taskQuiescenceIssuer, b.taskRuntimeTerminationIssuer}
	if target.detachedWorkspace.Kind() != WorkspaceIdentityDetached {
		issuers = append(issuers, b.taskWorkspaceArchiveIssuer)
	}
	for _, issuer := range issuers {
		wire, err := issuer.Certificate(bounded)
		if err != nil || !b.localCleanup.Credential.MatchesCertificate(wire) || certificate != nil && !bytes.Equal(certificate, wire) {
			return errors.Join(ErrInvalidConfiguration, err)
		}
		certificate = wire
	}
	entry, err := b.registerCommandIssuerCertificate(bounded, certificate, b.taskVerifier)
	if err != nil {
		return err
	}
	target.issuer = *entry
	return nil
}
