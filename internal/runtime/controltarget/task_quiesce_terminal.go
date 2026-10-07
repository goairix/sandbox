package controltarget

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"sort"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/launcher"
)

// QuiescedExecution is input from the trusted original Supervisor producer.
// It cannot itself prove owner joins, never-committed Start or set completeness.
// Those obligations remain with the private frozen Supervisor registration set.
type QuiescedExecution struct {
	Accepted        *AcceptedExecution
	Disposition     string
	TerminalReceipt []byte
}

// CompleteUserQuiescence verifies original handles, protected records and exact
// signed terminal results, then freshly checks namespace absence before and after
// persistence/signing under j.mu. Only the trusted Supervisor can establish the
// complete original owner set and never-spawned disposition; this DTO is not a
// cryptographic assertion of joins. No structural history reconstructs authority.
func (j *Journal) CompleteUserQuiescence(ctx context.Context, a *AcceptedUserQuiescence, o *launcher.UserNamespaceQuiescenceObservation, executions []QuiescedExecution, key ed25519.PrivateKey) ([]byte, error) {
	if j == nil || j.self != j {
		return nil, ErrJournalUnavailable
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.quiescenceTerminalLiveLocked(ctx, a, key); err != nil {
		return nil, err
	}
	if a.terminal != nil {
		return nil, ErrConflict
	}
	old, err := j.readTaskQuiescenceLocked(ctx)
	if err != nil {
		return nil, err
	}
	if old == nil || old.State != "pending" || !sameTaskQuiescence(*old, a.record) {
		return nil, ErrConflict
	}
	entries, never, terminal, err := j.quiescenceExecutionsLocked(ctx, executions)
	if err != nil {
		return nil, err
	}
	digest, err := p.DigestTaskQuiescenceExecutions(entries)
	if err != nil {
		return nil, err
	}
	if err = o.RevalidateCurrent(ctx); err != nil {
		return nil, err
	}
	next := *old
	next.State = "users_quiesced"
	next.ExecutionSetDigest = digest
	next.RegisteredCount = uint32(len(entries))
	next.NeverSpawnedCount = never
	next.LocalTerminalCount = terminal
	if err = j.authenticateTaskQuiescenceReceiptLocked(ctx, next); err != nil {
		return nil, err
	}
	wire, err := encodeTaskQuiescence(next)
	if err != nil {
		return nil, err
	}
	if err = j.persistTaskQuiescenceTerminalLocked(ctx, wire); err != nil {
		return nil, j.poison(err)
	}
	signed, err := j.signQuiescenceTerminalLocked(ctx, next, o, key)
	if err != nil {
		return nil, j.poison(err)
	}
	a.terminal = &next
	return signed, nil
}

// SignUserQuiescenceReceipt needs original successful live journal completion.
// Supervisor additionally requires its original successful coordinator and joined
// deadline owner. Expired/failed attempts are never revived by readable bytes.
func (j *Journal) SignUserQuiescenceReceipt(ctx context.Context, expected p.TaskUserQuiescenceContext, digest string, o *launcher.UserNamespaceQuiescenceObservation, key ed25519.PrivateKey) ([]byte, error) {
	if j == nil || j.self != j {
		return nil, ErrJournalUnavailable
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	a := j.userQuiescence
	if err := j.quiescenceTerminalLiveLocked(ctx, a, key); err != nil {
		return nil, err
	}
	if a.terminal == nil || a.record.Context != expected || a.record.TicketDigest != digest {
		return nil, ErrConflict
	}
	r, err := j.readTaskQuiescenceLocked(ctx)
	if err != nil {
		return nil, err
	}
	if r == nil || *r != *a.terminal {
		return nil, ErrConflict
	}
	return j.signQuiescenceTerminalLocked(ctx, *r, o, key)
}
func (j *Journal) quiescenceTerminalLiveLocked(ctx context.Context, a *AcceptedUserQuiescence, key ed25519.PrivateKey) error {
	if err := j.checkLocked(ctx, true); err != nil {
		return err
	}
	if a == nil || a.self != a || a.journal != j || a != j.userQuiescence || !j.usersClosed || !j.accountingKnown || j.gate.GateState != "closed" {
		return ErrJournalUnavailable
	}
	if err := j.taskCloseInstalledLocked(); err != nil {
		return err
	}
	if len(key) != ed25519.PrivateKeySize || !bytes.Equal(ed25519.NewKeyFromSeed(key[:ed25519.SeedSize]), key) || !bytes.Equal(key[32:], j.birth.RuntimePublicKey) {
		return ErrIdentityMismatch
	}
	return j.authenticateTaskQuiescenceReceiptLocked(ctx, a.record)
}
func (j *Journal) signQuiescenceTerminalLocked(ctx context.Context, r TaskUserQuiescenceRecord, o *launcher.UserNamespaceQuiescenceObservation, key ed25519.PrivateKey) ([]byte, error) {
	if err := j.authenticateTaskQuiescenceReceiptLocked(ctx, r); err != nil {
		return nil, err
	}
	if err := o.RevalidateCurrent(ctx); err != nil {
		return nil, err
	}
	wire, err := p.SignTaskUserQuiescenceReceipt(key, p.TaskUserQuiescenceReceiptClaims{Version: 1, State: r.State, Context: r.Context, TicketDigest: r.TicketDigest, NotBefore: r.NotBefore, NotAfter: r.NotAfter, ExecutionSetDigest: r.ExecutionSetDigest, RegisteredCount: r.RegisteredCount, NeverSpawnedCount: r.NeverSpawnedCount, LocalTerminalCount: r.LocalTerminalCount})
	if err != nil {
		return nil, err
	}
	if err = o.RevalidateCurrent(ctx); err != nil {
		return nil, err
	}
	if err = j.authenticateTaskQuiescenceReceiptLocked(ctx, r); err != nil {
		return nil, err
	}
	return wire, nil
}
func (j *Journal) quiescenceExecutionsLocked(ctx context.Context, input []QuiescedExecution) ([]p.TaskQuiescenceExecution, uint32, uint32, error) {
	if len(input) > 64 {
		return nil, 0, 0, ErrInvalidRecord
	}
	entries := make([]p.TaskQuiescenceExecution, 0, len(input))
	seen := make(map[*AcceptedExecution]bool, len(input))
	var never, terminal uint32
	for _, e := range input {
		a := e.Accepted
		if a == nil || a.self != a || a.journal != j || seen[a] || !a.consumed || !a.startReady {
			return nil, 0, 0, ErrInvalidRecord
		}
		seen[a] = true
		r, err := j.readCommandLocked(ctx, a.record.Context.CommandID)
		if err != nil {
			return nil, 0, 0, err
		}
		if r == nil || *r != a.record || r.Version != 2 {
			return nil, 0, 0, ErrConflict
		}
		if err = j.recordBinding(*r); err != nil {
			return nil, 0, 0, err
		}
		entry := p.TaskQuiescenceExecution{CommandID: r.Context.CommandID, ExecContext: r.Context, TicketDigest: r.TicketDigest, Disposition: e.Disposition}
		switch e.Disposition {
		case "never_spawned":
			if r.State != "accepted" || len(e.TerminalReceipt) != 0 {
				return nil, 0, 0, ErrInvalidRecord
			}
			never++
		case "local_terminal":
			if r.State != "local_terminal" || len(e.TerminalReceipt) > 8192 {
				return nil, 0, 0, ErrInvalidRecord
			}
			now, err := j.authorityNow(ctx)
			if err != nil {
				return nil, 0, 0, err
			}
			receipt, err := j.verifier.VerifyLocalExecReceipt(bytes.Clone(e.TerminalReceipt), j.activation.RuntimeCertificate(), r.Context, r.DescriptorDigest, r.TicketDigest, r.NotBefore, r.NotAfter, r.AuthorityDeadline, now)
			if err != nil {
				return nil, 0, 0, err
			}
			if receipt.State() != r.State || receipt.RootPID() != r.RootPID || receipt.RootWaitStatus() != r.RootWaitStatus || receipt.DrainConfirmed() != r.DrainConfirmed || receipt.Reason() != r.Reason {
				return nil, 0, 0, ErrConflict
			}
			entry.TerminalReceiptDigest = digestJournalBytes(receipt.Wire())
			terminal++
		default:
			return nil, 0, 0, ErrInvalidRecord
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].CommandID < entries[j].CommandID })
	if _, err := p.DigestTaskQuiescenceExecutions(entries); err != nil {
		return nil, 0, 0, err
	}
	return entries, never, terminal, nil
}
