package controltarget

import (
	"bytes"
	"context"
	"crypto/ed25519"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

// SignUserQuiescenceAccepted attests only this live handle's durable pending
// barrier. The trusted Supervisor must own its finite coordinator before sending
// any ACK. No history, caller record, callback or terminal setter is accepted.
func (j *Journal) SignUserQuiescenceAccepted(ctx context.Context, a *AcceptedUserQuiescence, key ed25519.PrivateKey) ([]byte, error) {
	if j == nil || j.self != j {
		return nil, ErrJournalUnavailable
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.checkLocked(ctx, true); err != nil {
		return nil, err
	}
	if a == nil || a.self != a || a.journal != j || a != j.userQuiescence || !j.usersClosed || !j.accountingKnown || j.gate.GateState != "closed" {
		return nil, ErrJournalUnavailable
	}
	if err := j.taskCloseInstalledLocked(); err != nil {
		return nil, err
	}
	if len(key) != ed25519.PrivateKeySize {
		return nil, ErrInvalidRecord
	}
	if !bytes.Equal(key.Public().(ed25519.PublicKey), j.birth.RuntimePublicKey) {
		return nil, ErrIdentityMismatch
	}
	r, err := j.readTaskQuiescenceLocked(ctx)
	if err != nil {
		return nil, err
	}
	if r == nil || r.State != "pending" || !sameTaskQuiescence(*r, a.record) {
		return nil, ErrConflict
	}
	if err = j.authenticateTaskQuiescenceReceiptLocked(ctx, *r); err != nil {
		return nil, err
	}
	wire, err := p.SignTaskUserQuiescenceAccepted(key, p.TaskUserQuiescenceAcceptedClaims{Version: 1, State: "quiescence_accepted", Context: r.Context, TicketDigest: r.TicketDigest, NotBefore: r.NotBefore, NotAfter: r.NotAfter})
	if err != nil {
		return nil, err
	}
	if err = j.authenticateTaskQuiescenceReceiptLocked(ctx, *r); err != nil {
		return nil, err
	}
	return wire, nil
}
func (j *Journal) authenticateTaskQuiescenceReceiptLocked(ctx context.Context, r TaskUserQuiescenceRecord) error {
	return j.authenticateTaskCloseReceiptLocked(ctx, TaskDataCloseRecord{NotBefore: r.NotBefore, NotAfter: r.NotAfter})
}
