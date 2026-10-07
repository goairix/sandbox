package controltarget

import "context"

// LocalExecutionResult is only a trusted supervisor assertion, not remote proof.
type LocalExecutionResult struct {
	RootPID        int
	RootWaitStatus uint32
	DrainConfirmed bool
	Reason         string
}

func (j *Journal) RecordLocalTerminal(ctx context.Context, a *AcceptedExecution, result LocalExecutionResult) (*ExecJournalRecord, error) {
	if j == nil || j.self != j {
		return nil, ErrJournalUnavailable
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.checkLocked(ctx, true); err != nil {
		return nil, err
	}
	if err := j.handleLocked(a); err != nil {
		return nil, err
	}
	if !a.startReady {
		return nil, ErrInvalidRecord
	}
	next := a.record
	next.State = "local_terminal"
	next.RootPID = result.RootPID
	next.RootWaitStatus = result.RootWaitStatus
	next.DrainConfirmed = result.DrainConfirmed
	next.Reason = result.Reason
	if err := next.Validate(); err != nil {
		return nil, err
	}
	if err := j.replaceCommandLocked(ctx, a.record, next, false); err != nil {
		return nil, err
	}
	a.record = next
	return &next, nil
}

// RecordExecutionUnknown closes admission. It preserves the unresolved command
// forever until an independently specified recovery protocol exists.
func (j *Journal) RecordExecutionUnknown(ctx context.Context, a *AcceptedExecution, reason string) error {
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
	next := a.record
	next.State = "unknown"
	next.Reason = reason
	if err := next.Validate(); err != nil {
		return err
	}
	if err := j.replaceCommandLocked(ctx, a.record, next, false); err != nil {
		return err
	}
	a.record = next
	if err := j.persistClosedGateLocked(ctx); err != nil {
		return j.poison(err)
	}
	return nil
}
