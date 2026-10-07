package controltarget

import (
	"context"
	"fmt"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

func (j *Journal) verifyRenewLocked(ctx context.Context, a *AcceptedExecution, e controlprotocol.ExecRenewEvidence) (controlprotocol.ExecRenewEvidence, error) {
	now, err := j.liveNowLocked(ctx)
	if err != nil {
		return controlprotocol.ExecRenewEvidence{}, err
	}
	if !now.Add(time.Second).Before(a.record.AuthorityDeadline) {
		return controlprotocol.ExecRenewEvidence{}, fmt.Errorf("accepted authority already expired")
	}
	fresh, err := j.verifier.VerifyExecRenewTicket(e.Wire(), j.activation.IssuerCertificate(), a.record.Context, a.record.DescriptorDigest, now)
	if err != nil {
		return fresh, err
	}
	if fresh.NotAfter().After(j.activation.NotAfter()) || fresh.NotBefore().Before(j.activation.NotBefore()) {
		return controlprotocol.ExecRenewEvidence{}, ErrIdentityMismatch
	}
	return fresh, nil
}

// RenewAccepted persists only a live consumed registration's increasing maximum.
// Replays cannot extend beyond the signed interval or reopen terminal history.
func (j *Journal) RenewAccepted(ctx context.Context, a *AcceptedExecution, e controlprotocol.ExecRenewEvidence) error {
	if j == nil || j.self != j {
		return ErrJournalUnavailable
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.checkLocked(ctx, true); err != nil {
		return err
	}
	if err := j.handleLocked(a); err != nil {
		return err
	}
	if !a.startReady {
		return ErrInvalidRecord
	}
	fresh, err := j.verifyRenewLocked(ctx, a, e)
	if err != nil {
		return err
	}
	next := a.record
	if fresh.NotAfter().After(next.AuthorityDeadline) {
		next.AuthorityDeadline = fresh.NotAfter()
	}
	// Compare the exact expected disk record even for non-widening retries.
	if err = j.replaceCommandLocked(ctx, a.record, next, true); err != nil {
		return err
	}
	if _, err = j.verifyRenewLocked(ctx, a, e); err != nil {
		return j.poison(err)
	}
	a.record = next
	return nil
}
