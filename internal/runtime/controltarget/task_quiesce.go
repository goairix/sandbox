package controltarget

import (
	"bytes"
	"context"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"strings"
)

// AcceptedUserQuiescence belongs only to the original clean live journal.
// It is pending barrier ownership, not a coordinator, drain or execution grant.
// Only a future trusted Supervisor that owns the finite worker may expose ACK.
type AcceptedUserQuiescence struct {
	self    *AcceptedUserQuiescence
	journal *Journal
	record  TaskUserQuiescenceRecord
}

// AcceptUserQuiescence installs the immutable pending USERS barrier. It never
// starts, stops or joins executions. Uncertain persistence returns no handle.
func (j *Journal) AcceptUserQuiescence(ctx context.Context, e p.TaskUserQuiescenceEvidence, closeReceipt []byte) (*AcceptedUserQuiescence, error) {
	if j == nil || j.self != j {
		return nil, ErrJournalUnavailable
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.checkLocked(ctx, true); err != nil {
		return nil, err
	}
	r := TaskUserQuiescenceRecord{Version: 1, State: "pending", Context: e.Context(), TicketDigest: e.Digest(), NotBefore: e.NotBefore(), NotAfter: e.NotAfter()}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if err := j.taskCloseBinding(r.Context.Current); err != nil {
		return nil, err
	}
	if !j.accountingKnown || j.gate.GateState != "closed" {
		return nil, ErrJournalUnavailable
	}
	if err := j.taskCloseInstalledLocked(); err != nil {
		return nil, err
	}
	previous, err := j.readTaskQuiescenceLocked(ctx)
	if err != nil {
		return nil, j.poison(err)
	}
	if (previous == nil) != (j.taskQuiescenceBytes == 0) {
		return nil, j.poison(ErrConflict)
	}
	if previous != nil && (!sameTaskQuiescence(*previous, r) || previous.State != "pending") {
		return nil, ErrConflict
	}
	// Retain only owned bounded signature bytes throughout both authentications.
	if len(closeReceipt) > 8192 {
		return nil, ErrInvalidRecord
	}
	receipt := bytes.Clone(closeReceipt)
	if err = j.authenticateTaskQuiescenceLocked(ctx, e, receipt); err != nil {
		return nil, err
	}
	if previous != nil {
		if j.userQuiescence == nil || j.userQuiescence.self != j.userQuiescence || j.userQuiescence.journal != j || !sameTaskQuiescence(j.userQuiescence.record, r) {
			return nil, ErrJournalUnavailable
		}
		return j.userQuiescence, nil
	}
	if j.usersClosed || j.userQuiescence != nil {
		return nil, ErrJournalUnavailable
	}
	pending, err := encodeTaskQuiescence(r)
	if err != nil {
		return nil, err
	}
	// Reserve the eventual largest valid bounded terminal replacement, without
	// producing it. Pending remains retained while its replacement temp exists.
	terminal := r
	terminal.State = "users_quiesced"
	terminal.ExecutionSetDigest = strings.Repeat("f", 64)
	terminal.RegisteredCount = 64
	terminal.NeverSpawnedCount = 32
	terminal.LocalTerminalCount = 32
	maximum, err := encodeTaskQuiescence(terminal)
	if err != nil {
		return nil, err
	}
	peak := j.logicalBytes + int64(len(pending)) + int64(len(maximum))
	if peak > j.maxBytes || peak*100 >= j.maxBytes*85 || j.contentFilesLocked()+2 > maxJournalContentFiles {
		return nil, ErrCapacity
	}
	j.usersClosed = true
	if err = j.persistTaskQuiescencePendingLocked(ctx, pending); err != nil {
		return nil, j.poison(err)
	}
	if err = j.authenticateTaskQuiescenceLocked(ctx, e, receipt); err != nil {
		return nil, j.poison(err)
	}
	a := &AcceptedUserQuiescence{journal: j, record: r}
	a.self = a
	j.userQuiescence = a
	return a, nil
}
func (j *Journal) authenticateTaskQuiescenceLocked(ctx context.Context, e p.TaskUserQuiescenceEvidence, receipt []byte) error {
	if err := j.taskCloseInstalledLocked(); err != nil {
		return err
	}
	if err := j.taskCloseIssuerLocked(e.Context().Current); err != nil {
		return err
	}
	old, err := j.readTaskCloseLocked(ctx)
	if err != nil {
		return err
	}
	if old == nil || old.State != "data_closed" || old.Context != e.Context().CloseDataContext || old.TicketDigest != e.Context().CloseDataTicketDigest {
		return ErrConflict
	}
	now, err := j.authorityNow(ctx)
	if err != nil {
		return err
	}
	verified, err := j.verifier.VerifyTaskUserQuiescenceTicket(e.Wire(), j.activation.IssuerCertificate(), e.Context(), now)
	if err != nil {
		return err
	}
	if verified.Digest() != e.Digest() || !verified.NotBefore().Equal(e.NotBefore()) || !verified.NotAfter().Equal(e.NotAfter()) {
		return ErrInvalidRecord
	}
	historical, err := j.verifier.VerifyTaskDataClosedReceipt(receipt, j.activation.RuntimeCertificate(), old.Context, old.TicketDigest, old.NotBefore, old.NotAfter, now)
	if err != nil {
		return err
	}
	if digestJournalBytes(historical.Wire()) != e.Context().CloseDataReceiptDigest {
		return ErrConflict
	}
	return j.authenticateTaskQuiescenceReceiptLocked(ctx, TaskUserQuiescenceRecord{NotBefore: e.NotBefore(), NotAfter: e.NotAfter()})
}

// LookupUserQuiescence returns copied structural history, including cold or
// poisoned history. It cannot mint an accepted handle or authorize completion.
func (j *Journal) LookupUserQuiescence(ctx context.Context, expected p.TaskUserQuiescenceContext, digest string) (*TaskUserQuiescenceRecord, error) {
	if j == nil || j.self != j {
		return nil, ErrJournalUnavailable
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.checkLocked(ctx, false); err != nil {
		return nil, err
	}
	if err := validateJournalQuiescenceContext(expected); err != nil {
		return nil, err
	}
	if !journalHash(digest) {
		return nil, ErrInvalidRecord
	}
	if err := j.taskCloseBinding(expected.Current); err != nil {
		return nil, err
	}
	r, err := j.readTaskQuiescenceLocked(ctx)
	if err != nil || r == nil {
		return r, err
	}
	if r.Context != expected || r.TicketDigest != digest {
		return nil, ErrConflict
	}
	return r, nil
}
