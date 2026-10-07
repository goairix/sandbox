package controlrunner

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/controltarget"
)

func (s *Supervisor) accept(ctx context.Context, start *authenticatedStart) (*Execution, error) {
	if err := s.validateReceiver(); err != nil {
		return nil, err
	}
	if nilValue(ctx) || start == nil || start.supervisor != s {
		return nil, ErrUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.failed || s.failurePending.Load() || !s.admission || s.activation == nil {
		return nil, ErrAdmissionClosed
	}
	if len(s.active) >= int(s.options.MaxActive) {
		return nil, fmt.Errorf("active command registry full")
	}
	request := start.descriptor.Request()
	if request.UID != s.birth.UID || request.GID != s.birth.GID || (request.RequiresNetwork && !s.birth.NetworkAllowed) {
		return nil, ErrInvalidConfiguration
	}
	now, err := observeControlled(ctx, s.options.Clock)
	if err != nil {
		return nil, err
	}
	evidence, err := s.options.Verifier.VerifyExecStartTicket(start.evidence.Wire(), s.activation.IssuerCertificate(), start.evidence.Context(), start.descriptor, now)
	if err != nil {
		return nil, err
	}
	accepted, err := s.journal.AcceptExecution(ctx, evidence)
	if err != nil {
		if s.journal.Status().Poisoned {
			s.admission = false
			s.failurePending.Store(true)
			go s.isolate("accept_persistence")
		}
		return nil, err
	}
	e := &Execution{supervisor: s, accepted: accepted, descriptor: start.descriptor, state: "accepted", ownerDone: make(chan struct{}), waitDone: make(chan struct{}), output: make(chan streamFrame, 2), ack: make(chan renewAck, 1)}
	e.self = e
	e.record, err = accepted.Snapshot()
	if err != nil {
		return nil, err
	}
	s.active[e.record.Context.CommandID] = e
	fail := func(cause error) (*Execution, error) {
		s.admission = false
		s.failurePending.Store(true)
		e.state = "unknown"
		close(e.ownerDone)
		close(e.waitDone)
		close(e.output)
		go s.isolate("monitor_start_failed")
		return nil, errors.Join(ErrExecutionUnknown, cause)
	}
	if err = s.journal.ConsumeStart(ctx, accepted); err != nil {
		return fail(err)
	}
	if err = s.options.Kernel.ValidateCurrent(); err != nil {
		return fail(err)
	}
	current, err := inspectExecutable(s.options.Executable)
	if err != nil || !os.SameFile(current, s.executable) || current.Size() != s.executable.Size() || !current.ModTime().Equal(s.executable.ModTime()) {
		return fail(errors.Join(err, fmt.Errorf("fixed monitor executable changed")))
	}
	e.authorityDeadlineNS, err = s.absoluteDeadline(ctx, e.record.AuthorityDeadline)
	if err != nil {
		return fail(err)
	}
	mono, err := monitorMonotonic()
	if err != nil {
		return fail(err)
	}
	e.commandDeadlineNS = mono + int64(time.Duration(request.TimeoutSeconds)*time.Second)
	if e.commandDeadlineNS < e.authorityDeadlineNS {
		e.authorityDeadlineNS = e.commandDeadlineNS
	}
	e.acceptedReceipt, err = s.signRecord(e.record)
	if err != nil {
		return fail(err)
	}
	if s.failurePending.Load() {
		return fail(ErrAdmissionClosed)
	}
	if err = s.startMonitor(e, monitorStart{Request: request, AuthorityDeadlineNS: e.authorityDeadlineNS, CommandDeadlineNS: e.commandDeadlineNS}); err != nil {
		return fail(err)
	}
	return e, nil
}
func (s *Supervisor) absoluteDeadline(ctx context.Context, deadline time.Time) (int64, error) {
	// Base before the active controlled call is deliberately conservative: the
	// observation accounts elapsed again, so IPC/queue delay can only shorten it.
	base, err := monitorMonotonic()
	if err != nil {
		return 0, err
	}
	now, err := observeControlled(ctx, s.options.Clock)
	if err != nil {
		return 0, err
	}
	remaining := deadline.Sub(now.Add(time.Second))
	if remaining <= 0 || remaining > 30*time.Second {
		return 0, fmt.Errorf("authority outside current monotonic window")
	}
	return base + int64(remaining), nil
}
func (s *Supervisor) signRecord(r controltarget.ExecJournalRecord) ([]byte, error) {
	return controlprotocol.SignLocalExecReceipt(s.key, controlprotocol.LocalExecReceiptClaims{Version: 1, State: r.State, Context: r.Context, DescriptorDigest: r.DescriptorDigest, TicketDigest: r.TicketDigest, NotBefore: r.NotBefore, NotAfter: r.NotAfter, AuthorityDeadline: r.AuthorityDeadline, RootPID: r.RootPID, RootWaitStatus: r.RootWaitStatus, DrainConfirmed: r.DrainConfirmed, Reason: r.Reason})
}

func (s *Supervisor) startMonitor(e *Execution, start monitorStart) error {
	reqR, reqW, err := os.Pipe()
	if err != nil {
		return err
	}
	controlR, controlW, err := os.Pipe()
	if err != nil {
		reqR.Close()
		reqW.Close()
		return err
	}
	resultR, resultW, err := os.Pipe()
	if err != nil {
		reqR.Close()
		reqW.Close()
		controlR.Close()
		controlW.Close()
		return err
	}
	closeAll := func() {
		for _, f := range []*os.File{reqR, reqW, controlR, controlW, resultR, resultW} {
			f.Close()
		}
	}
	cmd := exec.Command(s.options.Executable, "monitor")
	cmd.Env = []string{"GOMAXPROCS=2"}
	cmd.Dir = "/"
	cmd.ExtraFiles = []*os.File{reqR, controlR, resultW}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err = sealExecDescriptors(); err != nil {
		closeAll()
		return err
	}
	if err = cmd.Start(); err != nil {
		closeAll()
		return err
	}
	// Register sole management wait immediately, before assertions or IO errors.
	e.monitorPID = cmd.Process.Pid
	e.control = controlW
	e.request = reqW
	e.result = resultR
	run, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	go func() { e.waitErr = cmd.Wait(); close(e.waitDone) }()
	reqR.Close()
	controlR.Close()
	resultW.Close()
	go s.runExecution(run, e, start)
	return nil
}
func (s *Supervisor) runExecution(ctx context.Context, e *Execution, start monitorStart) {
	defer close(e.ownerDone)
	defer close(e.output)
	defer e.result.Close()
	defer e.control.Close()
	defer e.request.Close()
	defer e.cancel()
	fail := func(reason string) { e.mu.Lock(); e.state = "unknown"; e.mu.Unlock(); go s.isolate(reason) }
	if err := e.request.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		fail("request_io")
		return
	}
	if err := writeMonitorRequest(e.request, start); err != nil {
		fail("request_io")
		return
	}
	e.request.Close()
	// A reader deadline bounds monitor disappearance even when a descendant holds
	// a pipe; authority may renew but command timeout never extends this upper cap.
	mono, err := monitorMonotonic()
	if err != nil {
		fail("monotonic_clock")
		return
	}
	if err = e.result.SetReadDeadline(time.Now().Add(time.Duration(e.commandDeadlineNS-mono) + 31*time.Second)); err != nil {
		fail("result_deadline")
		return
	}
	var completion *monitorCompletion
	for {
		kind, data, readErr := readMonitorFrame(e.result)
		if readErr != nil {
			if errors.Is(readErr, io.EOF) && completion != nil {
				break
			}
			fail("monitor_lost")
			return
		}
		if completion != nil {
			fail("trailing_result")
			return
		}
		switch kind {
		case monitorStdout, monitorStderr:
			timer := time.NewTimer(5 * time.Second)
			select {
			case e.output <- streamFrame{kind: kind, data: data}:
				timer.Stop()
			case <-ctx.Done():
				timer.Stop()
				fail("stream_canceled")
				return
			case <-timer.C:
				fail("stream_blocked")
				return
			}
		case monitorStarted:
			var ready monitorReady
			if err := decodeMonitorJSON(data, &ready); err != nil || ready.MonitorPID != e.monitorPID || ready.RootPID <= 1 {
				fail("invalid_root_registration")
				return
			}
			e.mu.Lock()
			duplicate := e.rootPID != 0
			e.rootPID = ready.RootPID
			e.stats = ready
			e.mu.Unlock()
			if duplicate {
				fail("duplicate_root_registration")
				return
			}
		case monitorRenewAck:
			if len(data) != 16 {
				fail("invalid_renew_ack")
				return
			}
			ack := renewAck{sequence: binary.BigEndian.Uint64(data[:8]), deadline: int64(binary.BigEndian.Uint64(data[8:]))}
			select {
			case e.ack <- ack:
			default:
				fail("unexpected_renew_ack")
				return
			}
		case monitorResult:
			e.terminalSeen.Store(true)
			var result monitorCompletion
			if err := decodeMonitorJSON(data, &result); err != nil || !result.DrainConfirmed {
				fail("invalid_monitor_result")
				return
			}
			e.mu.Lock()
			valid := result.RootPID == e.rootPID && e.state == "accepted"
			if valid {
				e.state = "finishing"
			}
			e.mu.Unlock()
			if !valid {
				fail("unattributed_monitor_result")
				return
			}
			completion = &result
		default:
			fail("invalid_monitor_frame")
			return
		}
	}
	select {
	case <-e.waitDone:
	case <-time.After(time.Second):
		fail("monitor_wait_unjoined")
		return
	}
	if e.waitErr != nil {
		fail("monitor_exit_failed")
		return
	}
	e.mu.Lock()
	if e.state != "finishing" {
		e.mu.Unlock()
		fail("result_state_changed")
		return
	}
	bounded, cancel := context.WithTimeout(context.Background(), time.Second)
	record, err := s.journal.RecordLocalTerminal(bounded, e.accepted, controltarget.LocalExecutionResult{RootPID: completion.RootPID, RootWaitStatus: completion.RootWaitStatus, DrainConfirmed: completion.DrainConfirmed, Reason: completion.Reason})
	cancel()
	if err == nil {
		e.record = *record
		e.resultReceipt, err = s.signRecord(*record)
	}
	if err == nil {
		e.state = "local_terminal"
	}
	e.mu.Unlock()
	if err != nil {
		fail("terminal_persistence")
		return
	}
	s.mu.Lock()
	delete(s.active, e.record.Context.CommandID)
	s.mu.Unlock()
}

