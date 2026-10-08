package controltarget

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/launcher"
	"io"
	"time"
)

// StreamWorkspaceArchive consumes the original admission before physical IO.
// A writer calling Close on this journal is outside the trusted IO contract.
func (j *Journal) StreamWorkspaceArchive(ctx context.Context, a *AcceptedWorkspaceArchive, source *launcher.WorkspaceArchiveSource, dst io.Writer, key ed25519.PrivateKey) ([]byte, error) {
	if j == nil || j.self != j {
		return nil, ErrJournalUnavailable
	}
	j.mu.Lock()
	if a == nil || a.self != a || a.journal != j || j.workspaceArchive != a || a.consumed {
		j.mu.Unlock()
		return nil, ErrJournalUnavailable
	}
	a.consumed = true
	if err := j.archiveLiveLocked(ctx, a, a.record, key, true); err != nil {
		err = j.poison(err)
		j.mu.Unlock()
		// Poison only the original owner, never a foreign/copied handle.
		return nil, err
	}
	streamCtx, cancel := context.WithDeadline(ctx, a.deadline)
	a.cancel, a.done = cancel, make(chan struct{})
	key = bytes.Clone(key)
	j.mu.Unlock()
	defer func() {
		cancel()
		j.mu.Lock()
		close(a.done)
		a.done, a.cancel = nil, nil
		j.mu.Unlock()
	}()
	// Launcher closes its pinned source during unwind before this recovery.
	summary, err := writeOriginalArchive(streamCtx, source, dst)
	j.mu.Lock()
	defer j.mu.Unlock()
	if err != nil {
		return nil, j.poison(err)
	}
	if err = j.archiveLiveLocked(streamCtx, a, a.record, key, true); err != nil {
		return nil, j.poison(err)
	}
	next := a.record
	next.State = "archive_complete"
	next.ArchiveBytes, next.EntryCount = summary.ArchiveBytes, summary.EntryCount
	next.SHA256 = hex.EncodeToString(summary.SHA256[:])
	wire, err := encodeTaskArchive(next)
	if err != nil {
		return nil, j.poison(err)
	}
	if err = j.persistTaskArchiveTerminalLocked(streamCtx, wire); err != nil {
		return nil, j.poison(err)
	}
	signed, err := j.signArchiveLocked(streamCtx, a, next, key, true)
	if err != nil {
		return nil, j.poison(err)
	}
	a.terminal = &next
	return signed, nil
}

func writeOriginalArchive(ctx context.Context, source *launcher.WorkspaceArchiveSource, dst io.Writer) (summary launcher.WorkspaceArchiveSummary, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			summary = launcher.WorkspaceArchiveSummary{}
			err = fmt.Errorf("%w: source or writer panic", launcher.ErrWorkspaceArchive)
		}
	}()
	if nilJournalDependency(dst) {
		dst = nil
	}
	return source.WriteArchive(ctx, dst)
}

// archiveLiveLocked authenticates sealed attribution, not the expired admission
// start window. The installed certificates must cover the full signed session.
func (j *Journal) archiveLiveLocked(ctx context.Context, a *AcceptedWorkspaceArchive, want TaskWorkspaceArchiveRecord, key ed25519.PrivateKey, requireSession bool) error {
	if err := j.checkLocked(ctx, true); err != nil {
		return err
	}
	if a == nil || a.self != a || a.journal != j || a != j.workspaceArchive || (requireSession && !time.Now().Before(a.deadline)) {
		return ErrJournalUnavailable
	}
	if err := j.taskArchiveOriginalLocked(a.prior, a.record); err != nil {
		return err
	}
	if err := j.taskCloseBinding(a.record.Context.Current); err != nil {
		return err
	}
	if len(key) != ed25519.PrivateKeySize || !bytes.Equal(ed25519.NewKeyFromSeed(key[:ed25519.SeedSize]), key) || !bytes.Equal(key[32:], j.birth.RuntimePublicKey) {
		return ErrIdentityMismatch
	}
	q, err := j.readTaskQuiescenceLocked(ctx)
	if err != nil {
		return err
	}
	if q == nil || *q != *a.prior.terminal {
		return ErrConflict
	}
	r, err := j.readTaskArchiveLocked(ctx)
	if err != nil {
		return err
	}
	if r == nil || *r != want {
		return ErrConflict
	}
	now, err := j.authorityNow(ctx)
	if err != nil {
		return err
	}
	if now.Before(a.record.NotBefore) || (requireSession && !now.Before(a.record.SessionNotAfter)) {
		return ErrJournalUnavailable
	}
	i, err := j.verifier.VerifyCommandIssuerCertificate(j.activation.IssuerCertificate(), now)
	if err != nil {
		return err
	}
	c := a.record.Context.Current
	if i.Digest() != c.IssuerCertificateDigest || i.CertificateID() != c.IssuerCertificateID || a.record.NotBefore.Before(i.NotBefore()) || a.record.SessionNotAfter.After(i.NotAfter()) {
		return ErrIdentityMismatch
	}
	runtimeCert, err := j.verifier.VerifyRuntimeIdentityCertificate(j.activation.RuntimeCertificate(), j.activation.Identity(), now)
	if err != nil {
		return err
	}
	if !bytes.Equal(runtimeCert.PublicKey(), j.birth.RuntimePublicKey) || a.record.NotBefore.Before(runtimeCert.NotBefore()) || a.record.SessionNotAfter.After(runtimeCert.NotAfter()) {
		return ErrIdentityMismatch
	}
	if requireSession && !time.Now().Before(a.deadline) {
		return ErrJournalUnavailable
	}
	return ctx.Err()
}

func (j *Journal) signArchiveLocked(ctx context.Context, a *AcceptedWorkspaceArchive, r TaskWorkspaceArchiveRecord, key ed25519.PrivateKey, requireSession bool) ([]byte, error) {
	if err := j.archiveLiveLocked(ctx, a, r, key, requireSession); err != nil {
		return nil, err
	}
	wire, err := p.SignTaskWorkspaceArchiveReceipt(key, p.TaskWorkspaceArchiveReceiptClaims{Version: r.Version, State: r.State, Context: r.Context, TicketDigest: r.TicketDigest, NotBefore: r.NotBefore, NotAfter: r.NotAfter, SessionNotAfter: r.SessionNotAfter, ArchiveBytes: r.ArchiveBytes, EntryCount: r.EntryCount, SHA256: r.SHA256})
	if err != nil {
		return nil, err
	}
	if err = j.archiveLiveLocked(ctx, a, r, key, requireSession); err != nil {
		return nil, err
	}
	return wire, nil
}

// SignWorkspaceArchiveReceipt only re-signs the original successful producer.
// Historical completion outlives its session; fresh census, context, original
// records and current certificates remain mandatory without reopening a stream.
func (j *Journal) SignWorkspaceArchiveReceipt(ctx context.Context, a *AcceptedWorkspaceArchive, o *launcher.UserNamespaceQuiescenceObservation, key ed25519.PrivateKey) ([]byte, error) {
	if j == nil || j.self != j {
		return nil, ErrJournalUnavailable
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if a == nil || a.self != a || a.journal != j || a != j.workspaceArchive || a.terminal == nil || a.done != nil {
		return nil, ErrJournalUnavailable
	}
	if err := j.archiveLiveLocked(ctx, a, *a.terminal, key, false); err != nil {
		return nil, err
	}
	if err := o.RevalidateCurrent(ctx); err != nil {
		return nil, err
	}
	wire, err := j.signArchiveLocked(ctx, a, *a.terminal, key, false)
	if err != nil {
		return nil, err
	}
	if err = o.RevalidateCurrent(ctx); err != nil {
		return nil, err
	}
	if err = j.archiveLiveLocked(ctx, a, *a.terminal, key, false); err != nil {
		return nil, err
	}
	return wire, nil
}
