package controlrunner

import (
	"context"
	"encoding/json"
	"errors"
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
	ctx, cancel := context.WithTimeout(context.Background(), 31*time.Second)
	defer cancel()
	err := s.CloseAdmission(ctx)
	s.mu.Lock()
	s.closed = true
	active := make([]*Execution, 0, len(s.active))
	for _, e := range s.active {
		active = append(active, e)
	}
	s.mu.Unlock()
	for _, e := range active {
		e.mu.Lock()
		e.closeRequested = true
		if e.control != nil {
			e.control.SetWriteDeadline(time.Now().Add(time.Second))
			if x := writeMonitorFrame(e.control, monitorCancel, nil); x != nil {
				err = errors.Join(err, x)
			}
		}
		e.mu.Unlock()
	}
	for _, e := range active {
		select {
		case <-e.ownerDone:
		case <-ctx.Done():
			go s.isolate("close_unjoined")
			return false, errors.Join(err, ctx.Err())
		}
	}
	return true, err
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
	s.failureOnce.Do(func() {
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
		finish := func() { s.emitIsolation(reason, active); os.Exit(70) }
		if remaining <= 0 {
			finish()
			return
		}
		fallback := time.AfterFunc(remaining, finish)
		defer fallback.Stop()
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
