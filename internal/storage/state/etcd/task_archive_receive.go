package etcd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"time"

	t "github.com/goairix/sandbox/internal/runtime/controltransport"
)

// receive owns the original Session from Open through Close, preventing a
// caller from supplying a sealed Session authenticated to another destination.
func (r *taskArchiveSpool) receive(ctx context.Context, e taskArchiveExpected) (result *taskReceivedArchive, resultErr error) {
	if r == nil || r.self != r || ctx == nil || r.store == nil || r.store.self != r.store {
		return nil, errTaskArchive
	}
	s := r.store
	s.mu.Lock()
	if s.closed || r.closing || r.active || r.started || r.file == nil {
		s.mu.Unlock()
		return nil, errTaskArchive
	}
	r.started, r.active = true, true
	r.ownerDone = make(chan struct{})
	s.mu.Unlock()
	defer func() { s.mu.Lock(); r.active = false; close(r.ownerDone); s.mu.Unlock() }()
	bounded, cancel := context.WithDeadline(r.ctx, e.sessionAfter)
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	defer cancel()
	if e.now == nil || e.verifier == nil || e.destination == nil || e.envelope.Purpose != "task_workspace_archive" || e.envelope.Context != e.context || e.envelope.TicketDigest != e.digest || !e.destination.MatchTaskWorkspaceArchive(e.context, e.envelope.IssuerCertificate) {
		return nil, errTaskArchive
	}
	now, err := e.now(bounded)
	if err != nil {
		return nil, err
	}
	if err = bounded.Err(); err != nil {
		return nil, err
	}
	ticket, err := e.verifier.VerifyTaskWorkspaceArchiveTicket(e.envelope.Ticket, e.envelope.IssuerCertificate, e.context, now)
	if err != nil {
		return nil, err
	}
	if ticket.Digest() != e.digest || !ticket.NotBefore().Equal(e.before) || !ticket.NotAfter().Equal(e.after) || !ticket.SessionNotAfter().Equal(e.sessionAfter) {
		return nil, errTaskArchive
	}
	if err = e.destination.VerifyTaskWorkspaceArchiveRuntime(e.verifier, e.context, e.before, e.after, e.sessionAfter, now); err != nil {
		return nil, err
	}
	if err = taskArchiveCheck(s.root, r.name, r.file, r.identity); err != nil {
		return nil, err
	}
	if e.beforeOpen != nil {
		if err = e.beforeOpen(bounded); err != nil {
			return nil, err
		}
	}
	if err = r.checkRecoveryRoot(); err != nil {
		return nil, err
	}
	session, err := e.destination.Open(bounded)
	if err != nil {
		return nil, err
	}
	defer func() {
		// Physically join original Session.Close before the owner can finish.
		// Cancellation during a blocking Close cannot leave a verified result.
		if err := errors.Join(session.Close(), bounded.Err()); err != nil {
			result, resultErr = nil, errors.Join(resultErr, err)
			s.mu.Lock()
			r.witness = nil
			s.mu.Unlock()
		}
	}()
	if e.beforeSend != nil {
		if err = e.beforeSend(bounded); err != nil {
			return nil, err
		}
	}
	if err = r.checkRecoveryRoot(); err != nil {
		return nil, err
	}
	if err = session.SendTaskWorkspaceArchive(bounded, e.envelope); err != nil {
		return nil, err
	}
	hash := sha256.New()
	var total uint64
	for {
		kind, data, err := session.ReadTaskWorkspaceArchiveFrame(bounded)
		if err != nil {
			return nil, err
		}
		if kind == t.ArchiveData {
			if uint64(len(data)) > r.archiveBytesLimit()-total {
				return nil, errTaskArchive
			}
			if err := bounded.Err(); err != nil {
				return nil, err
			}
			n, err := r.file.Write(data)
			if err != nil {
				return nil, err
			}
			if n != len(data) {
				return nil, io.ErrShortWrite
			}
			total += uint64(n)
			hash.Write(data)
			continue
		}
		if kind != t.ArchiveComplete || total == 0 {
			return nil, errTaskArchive
		}
		now, err = e.now(bounded)
		if err != nil {
			return nil, err
		}
		if err = bounded.Err(); err != nil {
			return nil, err
		}
		if now.IsZero() || now.Location() != time.UTC || !now.Before(e.sessionAfter) {
			return nil, errTaskArchive
		}
		receipt, err := e.destination.VerifyTaskWorkspaceArchiveReceipt(e.verifier, data, e.context, e.digest, e.before, e.after, e.sessionAfter, now)
		if err != nil {
			return nil, err
		}
		digest := hex.EncodeToString(hash.Sum(nil))
		if receipt.ArchiveBytes() != total || receipt.SHA256() != digest || receipt.EntryCount() > s.limits.Entries {
			return nil, errTaskArchive
		}
		if err := session.RequireTaskWorkspaceArchiveEOF(bounded); err != nil {
			return nil, err
		}
		if err := taskArchiveCheck(s.root, r.name, r.file, r.identity); err != nil {
			return nil, err
		}
		count, err := taskArchiveValidateTar(bounded, r.file, total, s.limits.Entries, r.index)
		if err != nil {
			return nil, err
		}
		if count != receipt.EntryCount() {
			return nil, errTaskArchive
		}
		if err := bounded.Err(); err != nil {
			return nil, err
		}
		if e.beforeWitness != nil {
			if err = e.beforeWitness(bounded); err != nil {
				return nil, err
			}
		}
		w := &taskReceivedArchive{origin: r, diagnostics: taskArchiveDiagnostics{Bytes: total, Entries: count, SHA256: digest, Context: e.context, TicketDigest: e.digest, Before: e.before, After: e.after, SessionAfter: e.sessionAfter}}
		w.self = w
		s.mu.Lock()
		if r.closing || s.closed {
			s.mu.Unlock()
			return nil, errTaskArchive
		}
		r.witness = w
		s.mu.Unlock()
		return w, nil
	}
}
