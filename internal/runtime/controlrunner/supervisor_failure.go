package controlrunner

import (
	"context"
	"encoding/json"
	"os"
	"time"
)

func (s *Supervisor) Close() error {
	if s == nil {
		return nil
	}
	if err := s.validateReceiver(); err != nil {
		return err
	}
	return s.transport.close(s.stopExecutions, s.destroyResources)
}

// stopExecutions seals admission before requesting the original monitor cancel.
// A failed finite join retains resources for the existing isolation fallback.
func (s *Supervisor) stopExecutions() (bool, error) {

	s.mu.Lock()
	s.closed = true
	s.admission = false
	a := s.quiescence
	active := make([]*Execution, 0, len(s.active))
	for _, e := range s.active {
		active = append(active, e)
	}
	s.mu.Unlock()
	if a != nil {
		select {
		case <-a.done:
			if !a.success {
				go s.isolate("close_quiescence_unknown")
				return false, ErrExecutionUnknown
			}
			<-a.timer.done
			return true, nil
		case <-a.context.Done():
			select {
			case <-a.done:
				if a.success {
					<-a.timer.done
					return true, nil
				}
			default:
			}
			go s.isolate("close_quiescence_unjoined")
			return false, ErrExecutionUnknown
		}
	}
	deadline := time.Now().Add(31 * time.Second)
	owner := s.armIsolationDeadline(deadline, "close_unjoined")
	owner.mu.Lock()
	deadline = owner.deadline
	owner.mu.Unlock()
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	err := s.CloseAdmission(ctx)
	if err == nil {
		err = s.stopOriginalExecutions(ctx, active, deadline)
	}
	if err != nil {
		go s.isolate("close_stop_failed")
		return false, err
	}
	for _, e := range active {
		select {
		case <-e.ownerDone:
		case <-ctx.Done():
			go s.isolate("close_unjoined")
			return false, ctx.Err()
		}
	}
	return s.finishStopExecutions(owner, deadline)
}

// The original standalone Close owner must join within its original bound.
func (s *Supervisor) finishStopExecutions(owner *isolationDeadline, originalDeadline time.Time) (bool, error) {
	if s.failurePending.Load() || !owner.succeed() {
		// succeed can stop its timer but lose the bound while joining its owner.
		// Preserve that bound synchronously before any fallible isolation work.
		s.armIsolationDeadline(originalDeadline, "close_deadline")
		go s.isolate("close_deadline")
		return false, ErrExecutionUnknown
	}
	return true, nil
}

// Every execution and transport borrower has joined before these shared
// resources can be destroyed. There is exactly one Close owner.
func (s *Supervisor) destroyResources() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	if s.journal != nil {
		err = s.journal.Close()
	}
	clear(s.key)
	return err
}

type isolationExecution struct {
	CommandID       string `json:"command_id"`
	MonitorPID      int    `json:"monitor_pid"`
	RootPID         int    `json:"root_pid"`
	State           string `json:"state"`
	TerminalReceipt bool   `json:"terminal_receipt"`
	OwnerJoined     bool   `json:"owner_joined"`
	WaitJoined      bool   `json:"wait_joined"`
}

// Isolation cannot depend on a lock held by a stuck spawn or journal syscall.
// Immutable census publication makes the absolute-deadline fallback independent
// of those owners. Persistence is best effort and is never claimed by diagnostics.
func (s *Supervisor) isolate(reason string) {
	if s == nil || s.self != s {
		return
	}
	s.failurePending.Store(true)

	var active []*Execution
	if view := s.activeView.Load(); view != nil {
		active = *view
	}
	remaining := 30 * time.Second
	now, err := monitorMonotonic()
	if err != nil {
		remaining = 0
	}
	for _, e := range active {
		remaining = min(remaining, time.Duration(max(int64(0), e.watchdogDeadline.Load()-now)))
	}
	s.armIsolationDeadline(time.Now().Add(remaining), reason)
	s.failureOnce.Do(func() {
		finish := func() { s.emitIsolation(reason, active); os.Exit(70) }
		bounded, cancel := context.WithTimeout(context.Background(), remaining)
		defer cancel()
		for _, e := range active {
			if !e.mu.TryLock() {
				continue
			}
			if e.cancel != nil {
				e.cancel()
			}
			for _, f := range []*os.File{e.control, e.request, e.result} {
				if f != nil {
					f.Close()
				}
			}
			e.state = "unknown"
			e.mu.Unlock()
		}
		for _, e := range active {
			select {
			case <-e.ownerDone:
			case <-bounded.Done():
			}
			select {
			case <-e.waitDone:
			case <-bounded.Done():
			}
			if !e.mu.TryLock() {
				continue
			}
			e.state = "unknown"
			if s.journal != nil {
				_ = s.journal.RecordExecutionUnknown(bounded, e.accepted, reason)
			}
			e.mu.Unlock()
		}
		// A journal lock can be uninterruptible; the independent fallback remains due.
		_ = s.CloseAdmission(bounded)
		finish()
	})
}
func (s *Supervisor) emitIsolation(reason string, active []*Execution) {
	if !s.isolationPublished.CompareAndSwap(false, true) {
		return
	}
	events := make([]isolationExecution, 0, min(len(active), 64))
	for _, e := range active {
		if len(events) == 64 {
			break
		}
		event := isolationExecution{State: "unknown"}
		if d := e.diagnostic.Load(); d != nil {
			event = *d
		}
		event.TerminalReceipt = e.receiptIssued.Load()
		select {
		case <-e.ownerDone:
			event.OwnerJoined = true
		default:
		}
		select {
		case <-e.waitDone:
			event.WaitJoined = true
		default:
		}
		events = append(events, event)
	}
	event := struct {
		Version    int                  `json:"version"`
		PID        int                  `json:"pid"`
		Reason     string               `json:"reason"`
		GateClosed bool                 `json:"gate_closed"`
		Executions []isolationExecution `json:"executions"`
		ExitCode   int                  `json:"exit_code"`
	}{1, 1, reason, true, events, 70}
	wire, err := json.Marshal(event)
	if err == nil && len(wire) <= 32768 {
		writeIsolationDiagnostic(append(append([]byte("sandbox-isolation "), wire...), '\n'))
	}
}
