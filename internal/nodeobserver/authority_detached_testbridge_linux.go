//go:build linux && sandbox_detached_authority_test

package nodeobserver

import (
	"bytes"
	"context"
	"fmt"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// ValidateDetachedTerminationNativeForTest exercises the same private native
// reader and final guard using an explicit test-only build. The operator trust,
// concrete client and signed enrollment must come from the actual native writer
// fixture. It returns no snapshot or capability and never signals a process.
// Passing proves a reader component, not managed admission or a physical chain.
func ValidateDetachedTerminationNativeForTest(ctx context.Context, client *clientv3.Client, options Options, enrollment p.EnrolledRuntimeEvidence, ticket, issuer []byte) error {
	if ctx == nil || client == nil || options.Clock == nil || options.EtcdClusterID == 0 || len(options.EtcdIdentity) == 0 || len(options.EtcdIdentity) > 4096 || options.RequestTimeout <= 0 || options.RequestTimeout > 30*time.Second {
		return fmt.Errorf("native observer test bridge requires original bounded operator inputs")
	}
	verifier, err := p.NewManagementVerifier(options.Binding, options.Roots)
	if err != nil {
		return err
	}
	publication, err := p.NewPublicationVerifier(options.Binding, options.Roots)
	if err != nil {
		return err
	}
	receipt, err := p.DecodeNodeEnrollmentReceipt(enrollment.Receipt)
	if err != nil {
		return err
	}
	activation, err := verifier.VerifyNodeEnrollmentRequest(enrollment.Request, receipt.Claims.ObservedAt)
	if err != nil {
		return err
	}
	verifiedReceipt, err := verifier.VerifyNodeEnrollmentReceipt(enrollment.Receipt, enrollment.ObserverCertificate, receipt.Claims, receipt.Claims.ObservedAt)
	if err != nil {
		return err
	}
	i, rc := activation.Identity(), verifiedReceipt.Claims
	if i.Runtime != rc.Runtime || i.SandboxID != rc.SandboxID || i.WorkspaceHash != rc.WorkspaceHash || i.Generation != rc.Generation || activation.Digest() != rc.ActivationDigest || digest(enrollment.ObserverCertificate) != rc.ObserverCertificateDigest || !bytes.Equal(activation.RuntimeCertificate(), enrollment.Request.RuntimeCertificate) || !bytes.Equal(activation.IssuerCertificate(), enrollment.Request.IssuerCertificate) {
		return fmt.Errorf("native observer test bridge enrollment graph mismatch")
	}
	reader := &authorityReader{client: client, verifier: verifier, publication: publication, clock: options.Clock, binding: options.Binding, identity: bytes.Clone(options.EtcdIdentity), clusterID: options.EtcdClusterID, timeout: options.RequestTimeout}
	registered := authorityEnrollment{Identity: i, CreationIntentID: rc.CreationIntentID, EnrollmentDigest: rc.EnrollmentDigest, ObserverCertificateDigest: rc.ObserverCertificateDigest, NodeUID: rc.NodeUID, NodeBootID: rc.NodeBootID, ContractDigest: activation.Birth().ContractDigest}
	cutoff := time.Now().Add(30 * time.Second)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(cutoff) {
		cutoff = deadline
	}
	snapshot, err := reader.load(ctx, ticket, issuer, registered, cutoff)
	if err != nil {
		return err
	}
	if snapshot.record.Version != 2 || snapshot.detached == nil {
		return fmt.Errorf("native observer test bridge requires authenticated detached profile")
	}
	return snapshot.revalidate(ctx)
}
