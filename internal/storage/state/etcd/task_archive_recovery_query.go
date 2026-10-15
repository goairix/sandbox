package etcd

import (
	"context"
	"errors"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	t "github.com/goairix/sandbox/internal/runtime/controltransport"
)

func (a *taskArchiveRecoveryAuthority) queryProducer(ctx context.Context, destination *t.Destination) (result p.TaskWorkspaceArchiveReceiptEvidence, resultErr error) {
	if destination == nil || !destination.MatchTaskWorkspaceArchive(a.archiveContext(), a.command.Certificate) {
		return result, ErrIdentityMismatch
	}
	if err := a.guard(ctx); err != nil {
		return result, err
	}
	now, err := a.backend.observePublicationClock(ctx)
	if err != nil {
		return result, err
	}
	c := a.command.Claims
	// The runtime certificate is current. Original ticket windows remain only
	// historical receipt attribution; they never authorize another DATA send.
	if err := destination.VerifyTaskWorkspaceArchiveRuntime(a.backend.taskWorkspaceArchiveVerifier, c.Context, c.NotBefore, c.NotAfter, c.SessionNotAfter, now); err != nil {
		return result, err
	}
	if err := a.guard(ctx); err != nil {
		return result, err
	}
	session, err := destination.Open(ctx)
	if err != nil {
		return result, err
	}
	defer func() {
		if err := errors.Join(session.Close(), ctx.Err()); err != nil {
			result, resultErr = p.TaskWorkspaceArchiveReceiptEvidence{}, errors.Join(resultErr, err)
		}
		if resultErr == nil {
			resultErr = a.guard(ctx)
		}
		if resultErr != nil {
			result = p.TaskWorkspaceArchiveReceiptEvidence{}
		}
	}()
	if err := a.guard(ctx); err != nil {
		return result, err
	}
	if err := session.SendTaskWorkspaceArchive(ctx, t.TaskWorkspaceArchiveEnvelope{Version: 1, Purpose: "task_workspace_archive_query", Context: c.Context, TicketDigest: a.manifest.Seed.TicketDigest}); err != nil {
		return result, err
	}
	kind, data, err := session.ReadTaskWorkspaceArchiveFrame(ctx)
	if err != nil || kind != t.ArchiveComplete {
		return result, errors.Join(ErrDeliveryUnknown, err)
	}
	now, err = a.backend.observePublicationClock(ctx)
	if err != nil {
		return result, err
	}
	receipt, err := destination.VerifyTaskWorkspaceArchiveReceipt(a.backend.taskWorkspaceArchiveVerifier, data, c.Context, a.manifest.Seed.TicketDigest, c.NotBefore, c.NotAfter, c.SessionNotAfter, now)
	if err != nil {
		return result, err
	}
	w := a.manifest.Received
	if receipt.State() != "archive_complete" || receipt.ArchiveBytes() != w.Bytes || receipt.EntryCount() != w.Entries || receipt.SHA256() != w.SHA256 {
		return result, ErrIdentityMismatch
	}
	if err := session.RequireTaskWorkspaceArchiveEOF(ctx); err != nil {
		return result, err
	}
	return receipt, nil
}
