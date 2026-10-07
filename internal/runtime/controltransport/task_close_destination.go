package controltransport

import (
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"time"
)

func (d *Destination) MatchTaskClose(c p.TaskCloseDataContext, issuer []byte) bool {
	return d != nil && d.self == d && d.identity == (p.RuntimeIdentityContext{SandboxID: c.SandboxID, WorkspaceHash: c.WorkspaceHash, Generation: c.Generation, Runtime: c.Runtime}) && d.credential.MatchesCertificate(issuer)
}

// VerifyTaskCloseRuntime uses the backend's trust, never destination-selected
// roots, and constrains the original ticket interval to the pinned certificate.
func (d *Destination) VerifyTaskCloseRuntime(v *p.ManagementVerifier, c p.TaskCloseDataContext, before, after, now time.Time) error {
	if d == nil || d.self != d || v == nil || d.identity != (p.RuntimeIdentityContext{SandboxID: c.SandboxID, WorkspaceHash: c.WorkspaceHash, Generation: c.Generation, Runtime: c.Runtime}) {
		return ErrDestination
	}
	cert, err := v.VerifyRuntimeIdentityCertificate(d.certificate, d.identity, now)
	if err != nil {
		return err
	}
	if before.Before(cert.NotBefore()) || after.After(cert.NotAfter()) {
		return ErrDestination
	}
	return nil
}
func (d *Destination) VerifyTaskCloseReceipt(v *p.ManagementVerifier, wire []byte, c p.TaskCloseDataContext, digest string, before, after, now time.Time) (p.TaskDataClosedReceiptEvidence, error) {
	if err := d.VerifyTaskCloseRuntime(v, c, before, after, now); err != nil {
		return p.TaskDataClosedReceiptEvidence{}, err
	}
	return v.VerifyTaskDataClosedReceipt(wire, d.certificate, c, digest, before, after, now)
}
