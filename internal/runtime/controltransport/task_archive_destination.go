package controltransport

import (
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

func (d *Destination) MatchTaskWorkspaceArchive(c p.TaskWorkspaceArchiveContext, issuer []byte) bool {
	return d.MatchTaskClose(c.Current, issuer)
}

// VerifyTaskWorkspaceArchiveRuntime uses original backend trust and checks
// certificate coverage through the retained original session deadline.
func (d *Destination) VerifyTaskWorkspaceArchiveRuntime(v *p.ManagementVerifier, c p.TaskWorkspaceArchiveContext, before, after, sessionAfter, now time.Time) error {
	if before.IsZero() || after.IsZero() || sessionAfter.IsZero() || !before.Before(after) || !after.Before(sessionAfter) {
		return ErrDestination
	}
	return d.VerifyTaskCloseRuntime(v, c.Current, before, sessionAfter, now)
}

// VerifyTaskWorkspaceArchiveReceipt authenticates historical attribution only;
// it does not prove received bytes, stream EOF, ownership, or safe release.
func (d *Destination) VerifyTaskWorkspaceArchiveReceipt(v *p.ManagementVerifier, wire []byte, c p.TaskWorkspaceArchiveContext, digest string, before, after, sessionAfter, now time.Time) (p.TaskWorkspaceArchiveReceiptEvidence, error) {
	if err := d.VerifyTaskWorkspaceArchiveRuntime(v, c, before, after, sessionAfter, now); err != nil {
		return p.TaskWorkspaceArchiveReceiptEvidence{}, err
	}
	return v.VerifyTaskWorkspaceArchiveReceipt(wire, d.certificate, c, digest, before, after, sessionAfter, now)
}
