package controltarget

import (
	"bytes"
	"context"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"strings"
	"time"
)

// AcceptedWorkspaceArchive belongs only to the original live journal.
type AcceptedWorkspaceArchive struct {
	self     *AcceptedWorkspaceArchive
	journal  *Journal
	record   TaskWorkspaceArchiveRecord
	deadline time.Time
	prior    *AcceptedUserQuiescence
	consumed bool
	cancel   context.CancelFunc
	done     chan struct{}
	terminal *TaskWorkspaceArchiveRecord
}

// Deadline returns the sealed admission deadline without renewing authority.
func (a *AcceptedWorkspaceArchive) Deadline() (time.Time, error) {
	if a == nil || a.self != a || a.journal == nil || a.journal.self != a.journal {
		return time.Time{}, ErrJournalUnavailable
	}
	j := a.journal
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.workspaceArchive != a {
		return time.Time{}, ErrJournalUnavailable
	}
	return a.deadline, nil
}

func (j *Journal) AcceptWorkspaceArchive(ctx context.Context, e p.TaskWorkspaceArchiveEvidence, q *AcceptedUserQuiescence, receipt []byte) (*AcceptedWorkspaceArchive, error) {
	if j == nil || j.self != j {
		return nil, ErrJournalUnavailable
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.checkLocked(ctx, true); err != nil {
		return nil, err
	}
	r := TaskWorkspaceArchiveRecord{Version: 1, State: "pending", Context: e.Context(), TicketDigest: e.Digest(), NotBefore: e.NotBefore(), NotAfter: e.NotAfter(), SessionNotAfter: e.SessionNotAfter()}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if err := j.taskCloseBinding(r.Context.Current); err != nil {
		return nil, err
	}
	if err := j.taskArchiveOriginalLocked(q, r); err != nil {
		return nil, err
	}
	if len(receipt) > 12288 {
		return nil, ErrInvalidRecord
	}
	w := bytes.Clone(receipt)
	// Exact live duplicate performs no point IO and never renews its deadline.
	if a := j.workspaceArchive; a != nil {
		if a.self != a || a.journal != j || a.prior != q || a.record.State != "pending" || !sameTaskArchive(a.record, r) || digestJournalBytes(w) != r.Context.QuiescenceReceiptDigest {
			return nil, ErrConflict
		}
		if !time.Now().Before(a.deadline) {
			return nil, ErrJournalUnavailable
		}
		if _, err := j.authenticateTaskArchiveLocked(ctx, e, w, q, false); err != nil {
			return nil, j.poison(err)
		}
		return a, nil
	}
	old, err := j.readTaskArchiveLocked(ctx)
	if err != nil {
		return nil, j.poison(err)
	}
	if old != nil || j.taskArchiveBytes != 0 {
		return nil, ErrJournalUnavailable
	}
	monotonicNow := time.Now()
	now, err := j.authenticateTaskArchiveLocked(ctx, e, w, q, true)
	if err != nil {
		return nil, j.poison(err)
	}
	// time.Now carries monotonic time; the authority clock supplies trusted UTC.
	deadline := monotonicNow.Add(r.SessionNotAfter.Sub(now) - time.Second)
	if !time.Now().Before(deadline) {
		return nil, ErrJournalUnavailable
	}
	b, err := encodeTaskArchive(r)
	if err != nil {
		return nil, err
	}
	maximum := r
	maximum.State = "archive_complete"
	maximum.ArchiveBytes = 1 << 40
	maximum.EntryCount = 1000000
	maximum.SHA256 = strings.Repeat("f", 64)
	terminal, err := encodeTaskArchive(maximum)
	if err != nil {
		return nil, err
	}
	peak := j.logicalBytes + int64(len(b)+len(terminal))
	if peak > j.maxBytes || peak*100 >= j.maxBytes*85 || j.contentFilesLocked()+2 > maxJournalContentFiles {
		return nil, ErrCapacity
	}
	if err = j.persistTaskArchivePendingLocked(ctx, b); err != nil {
		return nil, j.poison(err)
	}
	if _, err = j.authenticateTaskArchiveLocked(ctx, e, w, q, true); err != nil {
		return nil, j.poison(err)
	}
	if !time.Now().Before(deadline) {
		return nil, j.poison(ErrJournalUnavailable)
	}
	a := &AcceptedWorkspaceArchive{journal: j, record: r, deadline: deadline, prior: q}
	a.self = a
	j.workspaceArchive = a
	return a, nil
}

func (j *Journal) taskArchiveOriginalLocked(q *AcceptedUserQuiescence, r TaskWorkspaceArchiveRecord) error {
	if !j.accountingKnown || !j.usersClosed || j.gate.GateState != "closed" {
		return ErrJournalUnavailable
	}
	if err := j.taskCloseInstalledLocked(); err != nil {
		return err
	}
	if q == nil || q.self != q || q.journal != j || q != j.userQuiescence || q.terminal == nil || q.terminal.State != "users_quiesced" || !sameTaskQuiescence(q.record, *q.terminal) || q.terminal.Context != r.Context.QuiescenceContext || q.terminal.TicketDigest != r.Context.QuiescenceTicketDigest {
		return ErrJournalUnavailable
	}
	return q.terminal.Validate()
}

func (j *Journal) authenticateTaskArchiveLocked(ctx context.Context, e p.TaskWorkspaceArchiveEvidence, w []byte, q *AcceptedUserQuiescence, read bool) (time.Time, error) {
	if err := j.taskArchiveOriginalLocked(q, TaskWorkspaceArchiveRecord{Context: e.Context()}); err != nil {
		return time.Time{}, err
	}
	if err := j.taskCloseIssuerLocked(e.Context().Current); err != nil {
		return time.Time{}, err
	}
	if read {
		r, err := j.readTaskQuiescenceLocked(ctx)
		if err != nil {
			return time.Time{}, err
		}
		if r == nil || *r != *q.terminal {
			return time.Time{}, ErrConflict
		}
	}
	now, err := j.authorityNow(ctx)
	if err != nil {
		return time.Time{}, err
	}
	v, err := j.verifier.VerifyTaskWorkspaceArchiveTicket(e.Wire(), j.activation.IssuerCertificate(), e.Context(), now)
	if err != nil {
		return time.Time{}, err
	}
	if v.Digest() != e.Digest() || !v.NotBefore().Equal(e.NotBefore()) || !v.NotAfter().Equal(e.NotAfter()) || !v.SessionNotAfter().Equal(e.SessionNotAfter()) {
		return time.Time{}, ErrInvalidRecord
	}
	c, err := j.verifier.VerifyRuntimeIdentityCertificate(j.activation.RuntimeCertificate(), j.activation.Identity(), now)
	if err != nil {
		return time.Time{}, err
	}
	if !bytes.Equal(c.PublicKey(), j.birth.RuntimePublicKey) || e.NotBefore().Before(c.NotBefore()) || e.SessionNotAfter().After(c.NotAfter()) {
		return time.Time{}, ErrIdentityMismatch
	}
	r := q.terminal
	h, err := j.verifier.VerifyTaskUserQuiescenceReceipt(w, j.activation.RuntimeCertificate(), r.Context, r.TicketDigest, r.NotBefore, r.NotAfter, now)
	if err != nil {
		return time.Time{}, err
	}
	if digestJournalBytes(h.Wire()) != e.Context().QuiescenceReceiptDigest || h.ExecutionSetDigest() != r.ExecutionSetDigest || h.RegisteredCount() != r.RegisteredCount || h.NeverSpawnedCount() != r.NeverSpawnedCount || h.LocalTerminalCount() != r.LocalTerminalCount {
		return time.Time{}, ErrConflict
	}
	return now, ctx.Err()
}

// LookupWorkspaceArchive returns copied structural history, never authority.
func (j *Journal) LookupWorkspaceArchive(ctx context.Context, expected p.TaskWorkspaceArchiveContext, digest string) (*TaskWorkspaceArchiveRecord, error) {
	if j == nil || j.self != j {
		return nil, ErrJournalUnavailable
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.checkLocked(ctx, false); err != nil {
		return nil, err
	}
	if err := expected.Validate(); err != nil {
		return nil, err
	}
	if !journalHash(digest) {
		return nil, ErrInvalidRecord
	}
	if err := j.taskCloseBinding(expected.Current); err != nil {
		return nil, err
	}
	r, err := j.readTaskArchiveLocked(ctx)
	if err != nil || r == nil {
		return r, err
	}
	if r.Context != expected || r.TicketDigest != digest {
		return nil, ErrConflict
	}
	return r, nil
}
