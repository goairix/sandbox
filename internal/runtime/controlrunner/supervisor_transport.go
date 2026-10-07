package controlrunner

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	t "github.com/goairix/sandbox/internal/runtime/controltransport"
)

// Serve owns the protected listener and every bounded connection. The idle main
// path only blocks in Accept; no target clock call or refresh actor runs idle.
func (s *Supervisor) Serve(ctx context.Context, listener net.Listener) (err error) {
	if nilValue(listener) {
		return ErrInvalidConfiguration
	}
	defer listener.Close()
	if err := s.validateReceiver(); err != nil {
		return err
	}
	if nilValue(ctx) {
		return ErrInvalidConfiguration
	}
	active, cancel := context.WithCancel(ctx)
	defer cancel()
	if !s.transport.listen(listener, cancel) {
		return ErrAdmissionClosed
	}
	var mu sync.Mutex
	connections := map[net.Conn]bool{}
	var workers sync.WaitGroup
	closeConnections := func() {
		listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for c := range connections {
			c.Close()
		}
	}
	stopped := make(chan struct{})
	stop := context.AfterFunc(active, func() { defer close(stopped); closeConnections() })
	defer func() {
		cancel()
		if !stop() {
			<-stopped
		}
		closeConnections()
		err = errors.Join(err, s.Close())
		workers.Wait()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if active.Err() != nil {
				return nil
			}
			return err
		}
		if nilValue(conn) {
			return ErrInvalidConfiguration
		}
		mu.Lock()
		if active.Err() != nil || len(connections) >= 16 {
			mu.Unlock()
			conn.Close()
			continue
		}
		borrow, handlerContext := s.transport.borrow(active, conn)
		if borrow == nil {
			mu.Unlock()
			conn.Close()
			return ErrAdmissionClosed
		}
		connections[conn] = true
		workers.Add(1)
		mu.Unlock()
		go func() {
			defer workers.Done()
			defer borrow.release()
			defer func() { conn.Close(); mu.Lock(); delete(connections, conn); mu.Unlock() }()
			s.serveConnection(handlerContext, conn)
		}()
	}
}
func (s *Supervisor) serveConnection(ctx context.Context, raw net.Conn) {
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	deadline, _ := bounded.Deadline()
	if raw.SetDeadline(deadline) != nil {
		return
	}
	// At most one bounded bootstrap owner can transition the listener. Once the
	// transition completes every later connection takes the TLS-only branch.
	if !s.bootstrapMu.TryLock() {
		return
	}
	s.mu.Lock()
	config := s.tlsConfig
	closed := s.closed || s.failed || s.failurePending.Load()
	s.mu.Unlock()
	if closed {
		s.bootstrapMu.Unlock()
		return
	}
	if config == nil {
		s.serveBootstrap(bounded, raw)
		s.bootstrapMu.Unlock()
		return
	}
	s.bootstrapMu.Unlock()
	conn := tls.Server(raw, config)
	if conn.HandshakeContext(bounded) != nil {
		return
	}
	envelope, task, err := t.ReadControlRequest(conn)
	if err != nil || bounded.Err() != nil {
		return
	}
	// Only this real handshake owner constructs private authenticatedStart.
	if task != nil {
		err = s.dispatchTaskClose(bounded, conn, *task)
	} else {
		err = s.dispatchTransport(bounded, ctx, conn, envelope)
	}
	if err != nil {
		conn.SetWriteDeadline(time.Now().Add(time.Second))
		_ = t.WriteEvent(conn, t.EventError, []byte("request refused or outcome unknown"))
	}
}
func (s *Supervisor) serveBootstrap(ctx context.Context, conn net.Conn) {
	var request t.BootstrapRequest
	if t.ReadBootstrap(conn, &request) != nil || ctx.Err() != nil || request.Version != 1 {
		return
	}
	s.mu.Lock()
	closed := s.closed || s.failed || s.activation != nil || s.failurePending.Load()
	s.mu.Unlock()
	if closed {
		return
	}
	switch request.Purpose {
	case "inspect":
		if len(request.Activation) != 0 || len(request.RuntimeCertificate) != 0 || len(request.IssuerCertificate) != 0 {
			return
		}
		wire, err := s.diagnostics(ctx)
		if err != nil {
			return
		}
		_ = t.WriteBootstrap(conn, json.RawMessage(wire))
	case "hello":
		if len(request.Activation) != 0 || len(request.RuntimeCertificate) != 0 || len(request.IssuerCertificate) != 0 {
			return
		}
		b := s.Birth()
		_ = t.WriteBootstrap(conn, t.BootstrapResponse{Version: 1, Purpose: "closed_hello", Birth: &t.Birth{BootID: b.BootID, RuntimePublicKey: b.RuntimePublicKey, UID: b.UID, GID: b.GID, NetworkAllowed: b.NetworkAllowed, ContractDigest: b.ContractDigest}})
	case "activate":
		now, err := observeControlled(ctx, s.options.Clock)
		if err != nil {
			return
		}
		e, err := s.options.Verifier.VerifyTargetActivation(request.Activation, request.RuntimeCertificate, request.IssuerCertificate, s.birth, now)
		if err != nil {
			return
		}
		if s.Activate(ctx, e) != nil {
			return
		}
		_ = t.WriteBootstrap(conn, t.BootstrapResponse{Version: 1, Purpose: "activated"})
	}
}
func (s *Supervisor) transportContext(c p.ExecStartContext) error {
	return s.transportExecContext(c, false)
}
func (s *Supervisor) transportExecContext(c p.ExecStartContext, query bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.failed || s.failurePending.Load() || s.activation == nil || (!query && !s.admission) {
		return ErrAdmissionClosed
	}
	a := s.activation
	b, i := a.Binding(), a.Identity()
	if c.Namespace != b.Namespace || c.AuthorityID != b.AuthorityID || c.Target != b.Target || c.RestoreEpoch != b.RestoreEpoch || c.SandboxID != i.SandboxID || c.WorkspaceHash != i.WorkspaceHash || c.Generation != i.Generation || c.Runtime != i.Runtime || c.DataGateEpoch != a.DataGateEpoch() {
		return ErrInvalidConfiguration
	}
	return nil
}
func (s *Supervisor) dispatchTransport(ctx, streamContext context.Context, conn net.Conn, envelope t.Envelope) error {
	if envelope.Purpose == "exec_inspect" {
		wire, err := s.diagnostics(ctx)
		if err != nil {
			return err
		}
		return writeTransportEvent(conn, t.EventDiagnostics, wire)
	}
	if err := s.transportExecContext(envelope.Context, envelope.Purpose == "exec_query"); err != nil {
		return err
	}
	switch envelope.Purpose {
	case "exec_query":
		return s.queryTransport(ctx, conn, envelope)
	case "exec_renew":
		s.mu.Lock()
		e := s.active[envelope.Context.CommandID]
		issuer := s.activation.IssuerCertificate()
		s.mu.Unlock()
		if e == nil || !bytes.Equal(issuer, envelope.IssuerCertificate) {
			return ErrUnavailable
		}
		if !e.mu.TryLock() {
			return ErrUnavailable
		}
		record := e.record
		e.mu.Unlock()
		if record.Context != envelope.Context || record.DescriptorDigest != envelope.DescriptorDigest {
			return ErrUnavailable
		}
		now, err := observeControlled(ctx, s.options.Clock)
		if err != nil {
			return err
		}
		renew, err := s.options.Verifier.VerifyExecRenewTicket(envelope.Ticket, issuer, record.Context, record.DescriptorDigest, now)
		if err != nil {
			return err
		}
		if err = s.renew(ctx, e, renew); err != nil {
			return err
		}
		return s.queryTransport(ctx, conn, envelope)
	case "exec_start":
		s.mu.Lock()
		issuer := s.activation.IssuerCertificate()
		journal := s.journal
		s.mu.Unlock()
		if !bytes.Equal(issuer, envelope.IssuerCertificate) {
			return ErrUnavailable
		}
		descriptor, err := envelope.Descriptor()
		if err != nil {
			return err
		}
		now, err := observeControlled(ctx, s.options.Clock)
		if err != nil {
			return err
		}
		evidence, err := s.options.Verifier.VerifyExecStartTicket(envelope.Ticket, issuer, envelope.Context, descriptor, now)
		if err != nil {
			return err
		}
		old, err := journal.Lookup(ctx, envelope.Context.CommandID)
		if err != nil {
			return err
		}
		if old != nil {
			if old.Context != envelope.Context || old.DescriptorDigest != descriptor.Digest() || old.TicketDigest != evidence.Digest() || !old.NotBefore.Equal(evidence.NotBefore()) || !old.NotAfter.Equal(evidence.NotAfter()) {
				return ErrUnavailable
			}
			return s.queryTransport(ctx, conn, envelope)
		}
		e, err := s.accept(ctx, &authenticatedStart{supervisor: s, descriptor: descriptor, evidence: evidence})
		if err != nil {
			return err
		}
		// Signed durable acceptance is emitted before consuming any user output.
		err = writeTransportEvent(conn, t.EventAccepted, e.acceptedReceipt)
		if err != nil {
			s.failurePending.Store(true)
			go s.isolate("control_ack_lost")
		}
		s.streamTransport(streamContext, conn, e, err == nil)
		return nil
	default:
		return t.ErrFrame
	}
}
func writeTransportEvent(conn net.Conn, kind byte, wire []byte) error {
	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	return t.WriteEvent(conn, kind, wire)
}
func (s *Supervisor) queryTransport(ctx context.Context, conn net.Conn, envelope t.Envelope) error {
	kind, wire, err := s.queryReceipt(ctx, envelope)
	if err != nil {
		return err
	}
	return writeTransportEvent(conn, kind, wire)
}
func (s *Supervisor) queryReceipt(ctx context.Context, envelope t.Envelope) (byte, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.active[envelope.Context.CommandID]
	journal := s.journal
	admission := s.admission && !s.closed && !s.failed && !s.failurePending.Load()
	if e != nil {
		if !e.mu.TryLock() {
			return 0, nil, ErrUnavailable
		}
		defer e.mu.Unlock()
		if e.record.Context != envelope.Context || e.record.DescriptorDigest != envelope.DescriptorDigest {
			return 0, nil, ErrUnavailable
		}
		mono, err := monitorMonotonic()
		if err != nil {
			return 0, nil, err
		}
		if admission && e.renewableLocked(mono) {
			wire, err := s.signRecord(e.record)
			if err != nil {
				return 0, nil, err
			}
			mono, err = monitorMonotonic()
			if err != nil || !e.renewableLocked(mono) {
				return 0, nil, ErrUnavailable
			}
			return t.EventAccepted, wire, nil
		}
		// Never issue a confirmation from durable-before-monitor-ACK state, or sign
		// an accepted record after its physical owner has entered uncertainty.
		if e.state != "local_terminal" || e.record.State != "local_terminal" {
			return 0, nil, ErrUnavailable
		}
		wire, err := s.signRecord(e.record)
		return t.EventReceipt, wire, err
	}
	record, err := journal.Lookup(ctx, envelope.Context.CommandID)
	if err != nil {
		return 0, nil, err
	}
	if record == nil || record.Context != envelope.Context || record.DescriptorDigest != envelope.DescriptorDigest {
		return 0, nil, ErrUnavailable
	}
	if !admission && record.State != "local_terminal" {
		return 0, nil, ErrUnavailable
	}
	wire, err := s.signRecord(*record)
	return t.EventReceipt, wire, err
}
func (s *Supervisor) streamTransport(ctx context.Context, conn net.Conn, e *Execution, writable bool) {
	// Even a lost API connection keeps one bounded drain owner for monitor output.
	// On failed output it closes admission/isolation and still drains the bounded
	// monitor pipe until the existing owner has completed or PID1 terminates.
	for frame := range e.output {
		if ctx.Err() != nil {
			writable = false
		}
		if writable {
			kind := t.EventStdout
			if frame.kind == monitorStderr {
				kind = t.EventStderr
			}
			if writeTransportEvent(conn, kind, frame.data) != nil {
				s.failurePending.Store(true)
				go s.isolate("control_stream_lost")
				writable = false
			}
		}
	}
	<-e.ownerDone
	if writable {
		e.mu.Lock()
		wire := bytes.Clone(e.resultReceipt)
		e.mu.Unlock()
		if len(wire) > 0 {
			_ = writeTransportEvent(conn, t.EventReceipt, wire)
		} else {
			_ = writeTransportEvent(conn, t.EventError, []byte(ErrExecutionUnknown.Error()))
		}
	}
}
