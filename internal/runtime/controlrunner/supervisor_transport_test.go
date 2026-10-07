package controlrunner

import (
	"context"
	"errors"
	protocol "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	transport "github.com/goairix/sandbox/internal/runtime/controltransport"
	"net"
	"testing"
	"time"
)

func TestSupervisorTransportRejectsUntrustedOwner(t *testing.T) {
	for _, s := range []*Supervisor{nil, {}} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Serve(context.Background(), listener); err == nil {
			t.Fatal("untrusted owner served")
		}
		if conn, err := net.Dial("tcp", listener.Addr().String()); err == nil {
			conn.Close()
			t.Fatal("rejected listener not closed")
		}
	}
}

// This is a refusal-only owner-lock test: no constructor capability, journal,
// monitor or successful execution is fabricated. A busy original owner cannot
// create a detached waiter or fall back to an unacknowledged journal snapshot.
func TestQueryBusyOwnerDoesNotWaitOrReadHistory(t *testing.T) {
	e := &Execution{}
	s := &Supervisor{active: map[string]*Execution{"busy-command": e}}
	envelope := transport.Envelope{Context: protocol.ExecStartContext{CommandID: "busy-command"}}
	e.mu.Lock()
	done := make(chan error, 1)
	go func() { _, _, err := s.queryReceipt(context.Background(), envelope); done <- err }()
	select {
	case err := <-done:
		e.mu.Unlock()
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("busy query returned %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		e.mu.Unlock()
		<-done // Join even a broken blocking implementation before failing.
		t.Fatal("query waited on the active owner mutex")
	}
}
