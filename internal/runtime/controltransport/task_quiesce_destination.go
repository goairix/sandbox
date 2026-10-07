package controltransport

import (
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"time"
)

func (d *Destination) MatchTaskQuiesce(c p.TaskUserQuiescenceContext, issuer []byte) bool {
	return d.MatchTaskClose(c.Current, issuer)
}

// Backend roots, not destination-provided roots, authenticate all returned
// attribution. The runtime certificate must still be current after ticket expiry.
func (d *Destination) VerifyTaskQuiesceRuntime(v *p.ManagementVerifier, c p.TaskUserQuiescenceContext, before, after, now time.Time) error {
	return d.VerifyTaskCloseRuntime(v, c.Current, before, after, now)
}
func (d *Destination) VerifyTaskQuiesceAccepted(v *p.ManagementVerifier, wire []byte, c p.TaskUserQuiescenceContext, digest string, before, after, now time.Time) (p.TaskUserQuiescenceAcceptedEvidence, error) {
	if err := d.VerifyTaskQuiesceRuntime(v, c, before, after, now); err != nil {
		return p.TaskUserQuiescenceAcceptedEvidence{}, err
	}
	return v.VerifyTaskUserQuiescenceAccepted(wire, d.certificate, c, digest, before, after, now)
}
func (d *Destination) VerifyTaskQuiesceReceipt(v *p.ManagementVerifier, wire []byte, c p.TaskUserQuiescenceContext, digest string, before, after, now time.Time) (p.TaskUserQuiescenceReceiptEvidence, error) {
	if err := d.VerifyTaskQuiesceRuntime(v, c, before, after, now); err != nil {
		return p.TaskUserQuiescenceReceiptEvidence{}, err
	}
	return v.VerifyTaskUserQuiescenceReceipt(wire, d.certificate, c, digest, before, after, now)
}
