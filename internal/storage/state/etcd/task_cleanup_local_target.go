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
)

// A target is constructed only for one admitted Task, from its authenticated
// snapshot descriptor and enrolled history. No opaque business payload or
// caller-selected runtime address participates in this derivation.
type cleanupLocalTarget struct {
	snapshot    SandboxSnapshotRecord
	publication *RuntimeEnrolledPublicationEntry
	issuer      CommandIssuerEntry
	plan        TaskWorkspaceSyncPlan
	root        string
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
	if err := b.cleanupPhysicalProfile(); err != nil {
		return nil, err
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
	// This read-only open validates actual uid/gid, filesystem, mount and
	// no-symlink resolution. Bind and finalization repeat their original checks.
	target, err := openTaskWorkspaceSyncLocalTarget(ctx, c.Worker, plan, root)
	if err != nil {
		return nil, err
	}
	if err = target.Close(); err != nil {
		return nil, err
	}
	return &cleanupLocalTarget{snapshot: *snapshot, plan: plan, root: root}, nil
}

func (b *Backend) cleanupPublication(ctx context.Context, task TaskRecord, target *cleanupLocalTarget) error {
	read, err := b.loadRuntimePublicationEnrolledAt(ctx, historyLocator(target.plan.workspace), task.CreationIntentID)
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

// All four fixed signing roles must use the configured credential. This
// bounded registration is at most two native RPCs under the original owner.
func (b *Backend) cleanupIssuer(ctx context.Context, target *cleanupLocalTarget) error {
	bounded, cancel := b.requestContext(ctx)
	defer cancel()
	var certificate []byte
	for _, issuer := range []interface {
		Certificate(context.Context) ([]byte, error)
	}{b.taskIssuer, b.taskQuiescenceIssuer, b.taskWorkspaceArchiveIssuer, b.taskRuntimeTerminationIssuer} {
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
