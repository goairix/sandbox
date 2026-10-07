package controltarget

import (
	"context"
	"fmt"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

// AcceptedExecution is an origin-bound, one-use registration. Neither a copied
// value nor any diagnostic lookup can reconstruct it. Fields are journal-locked.
type AcceptedExecution struct {
	self       *AcceptedExecution
	journal    *Journal
	record     ExecJournalRecord
	consumed   bool
	startReady bool
}

// Snapshot copies diagnostic state, never a reusable start registration.
func (a *AcceptedExecution) Snapshot() (ExecJournalRecord, error) {
	if a == nil || a.journal == nil {
		return ExecJournalRecord{}, ErrInvalidRecord
	}
	j := a.journal
	j.mu.Lock()
	defer j.mu.Unlock()
	if a.self != a {
		return ExecJournalRecord{}, ErrInvalidRecord
	}
	return a.record, nil
}
func (j *Journal) handleLocked(a *AcceptedExecution) error {
	if a == nil || a.self != a || a.journal != j || a.record.Version != 2 || a.record.State != "accepted" {
		return ErrInvalidRecord
	}
	return nil
}
func (j *Journal) checkStartAtLocked(r ExecJournalRecord, now time.Time) error {
	if err := j.recordBinding(r); err != nil {
		return err
	}
	issuer, err := j.verifier.VerifyCommandIssuerCertificate(j.activation.IssuerCertificate(), now)
	if err != nil {
		return err
	}
	if issuer.CertificateID() != r.Context.IssuerCertificateID || issuer.Digest() != r.Context.IssuerCertificateDigest || r.NotBefore.Before(issuer.NotBefore()) || r.NotAfter.After(issuer.NotAfter()) || r.NotAfter.After(j.activation.NotAfter()) || r.NotBefore.Before(j.activation.NotBefore()) {
		return ErrIdentityMismatch
	}
	if now.Add(-time.Second).Before(r.NotBefore) || !now.Add(time.Second).Before(r.NotAfter) {
		return fmt.Errorf("start outside current authority window")
	}
	return nil
}
func (j *Journal) AcceptExecution(ctx context.Context, e controlprotocol.ExecStartEvidence) (*AcceptedExecution, error) {
	if j == nil || j.self != j {
		return nil, ErrJournalUnavailable
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.checkLocked(ctx, true); err != nil {
		return nil, err
	}
	r, err := recordFromEvidence(e)
	if err != nil {
		return nil, err
	}
	if err = j.recordBinding(r); err != nil {
		return nil, err
	}
	existing, err := j.readCommandLocked(ctx, r.Context.CommandID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if sameAcceptance(*existing, r) {
			return nil, ErrExecutionExists
		}
		return nil, ErrConflict
	}
	now, err := j.liveNowLocked(ctx)
	if err != nil {
		return nil, err
	}
	if err = j.checkStartAtLocked(r, now); err != nil {
		return nil, err
	}
	r.Version = 2
	r.State = "accepted"
	r.AuthorityDeadline = r.NotAfter
	if err = j.persistNewCommandLocked(ctx, r); err != nil {
		return nil, err
	}
	now, err = j.liveNowLocked(ctx)
	if err == nil {
		err = j.checkStartAtLocked(r, now)
	}
	if err != nil {
		return nil, j.poison(err)
	}
	a := &AcceptedExecution{journal: j, record: r}
	a.self = a
	return a, nil
}

// ConsumeStart burns registration before the supervisor can create a monitor.
// A failed late check cannot restore it or make renewal rescue a missed start.
func (j *Journal) ConsumeStart(ctx context.Context, a *AcceptedExecution) error {
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
	if a.consumed {
		return ErrStartConsumed
	}
	a.consumed = true
	now, err := j.liveNowLocked(ctx)
	if err != nil {
		return err
	}
	if err = j.checkStartAtLocked(a.record, now); err != nil {
		return err
	}
	a.startReady = true
	return nil
}
