package controlrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	s.mu.Lock()
	alreadyClosed := s.closed
	s.mu.Unlock()
	if alreadyClosed {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 31*time.Second)
	defer cancel()
	err := s.CloseAdmission(ctx)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return err
	}
	s.closed = true
	active := make([]*Execution, 0, len(s.active))
	for _, e := range s.active {
		active = append(active, e)
	}
	s.mu.Unlock()
	for _, e := range active {
		e.mu.Lock()
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
			return errors.Join(err, ctx.Err())
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.journal != nil {
		err = errors.Join(err, s.journal.Close())
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

func (s *Supervisor) isolate(reason string) {
	if s.validateReceiver() != nil {
		return
	}
	s.failureOnce.Do(func() {
		s.mu.Lock()
		s.failed = true
		s.admission = false
		active := make([]*Execution, 0, len(s.active))
		for _, e := range s.active {
			active = append(active, e)
		}
		s.mu.Unlock()
		bounded, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, e := range active {
			if e.cancel != nil {
				e.cancel()
			}
			if e.control != nil {
				e.control.Close()
			}
			if e.request != nil {
				e.request.Close()
			}
			if e.result != nil {
				e.result.Close()
			}
		}
		events := make([]isolationExecution, 0, len(active))
		for _, e := range active {
			select {
			case <-e.ownerDone:
			case <-bounded.Done():
			}
			select {
			case <-e.waitDone:
			case <-bounded.Done():
			}
			e.mu.Lock()
			event := isolationExecution{CommandID: e.record.Context.CommandID, MonitorPID: e.monitorPID, RootPID: e.rootPID, State: "unknown", TerminalReceipt: len(e.resultReceipt) > 0}
			e.state = "unknown"
			ctx, stop := context.WithTimeout(context.Background(), time.Second)
			if s.journal != nil {
				_ = s.journal.RecordExecutionUnknown(ctx, e.accepted, reason)
			}
			stop()
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
			e.mu.Unlock()
			events = append(events, event)
		}
		gateCtx, stop := context.WithTimeout(context.Background(), time.Second)
		_ = s.CloseAdmission(gateCtx)
		stop()
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
			fmt.Fprintf(os.Stderr, "sandbox-isolation %s\n", wire)
		}
		os.Exit(70)
	})
}
