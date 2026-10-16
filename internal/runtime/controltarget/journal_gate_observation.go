package controltarget

import "context"

// ObserveGate returns only current scalar metadata. It neither persists a gate
// nor waits behind a journal operation or grants an admission capability.
func (j *Journal) ObserveGate(ctx context.Context) (GateManifest, error) {
	if nilJournalDependency(ctx) {
		return GateManifest{}, ErrInvalidConfiguration
	}
	if err := ctx.Err(); err != nil {
		return GateManifest{}, err
	}
	if j == nil || j.self != j {
		return GateManifest{}, ErrJournalUnavailable
	}
	if !j.mu.TryLock() {
		return GateManifest{}, ErrBusy
	}
	defer j.mu.Unlock()
	if err := j.checkLocked(ctx, false); err != nil {
		return GateManifest{}, err
	}
	if j.poisoned || !j.accountingKnown {
		return GateManifest{}, ErrJournalUnavailable
	}
	return j.gate, nil
}
