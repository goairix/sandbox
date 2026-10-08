package controlrunner

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"testing"
	"time"
)

// A completed quiescence cannot stop Close's independent isolation owner while
// transport borrowers may still be in journal signing or encrypted output IO.
func TestTaskArchiveCloseRetainsIsolationThroughTransportJoin(t *testing.T) {
	s := &Supervisor{active: map[string]*Execution{}}
	old := newIsolationDeadline(time.Now().Add(time.Second), func() { t.Error("old deadline expired") })
	if !old.succeed() {
		t.Fatal("fixture deadline not joined")
	}
	done := make(chan struct{})
	close(done)
	s.quiescence = &userQuiescenceAttempt{success: true, done: done, timer: old, context: context.Background()}
	s.isolationDeadline = old
	joined, err := s.stopExecutions()
	if err != nil || !joined {
		t.Fatalf("stop: joined=%t err=%v", joined, err)
	}
	if s.isolationDeadline == old {
		t.Fatal("Close reused stopped quiescence timer before transport join")
	}
	s.isolationDeadline.mu.Lock()
	armed := s.isolationDeadline.state == 0
	s.isolationDeadline.mu.Unlock()
	if !armed {
		t.Fatal("Close isolation stopped before transport join")
	}
	if !s.isolationDeadline.succeed() {
		t.Fatal("fixture cleanup timer not joined")
	}
	if !s.closeTimer.succeed() {
		t.Fatal("fixture Close guard not joined")
	}
}

func TestTaskArchiveCloseGuardSurvivesConcurrentQuiescenceSuccess(t *testing.T) {
	s := &Supervisor{active: map[string]*Execution{}}
	old := s.armIsolationDeadline(time.Now().Add(time.Second), "fixture")
	s.registerCloseDeadline()
	if !old.succeed() {
		t.Fatal("fixture quiescence did not succeed")
	}
	s.closeTimer.mu.Lock()
	armed := s.closeTimer.state == 0
	s.closeTimer.mu.Unlock()
	if !armed {
		t.Error("concurrent quiescence stopped original Close guard")
	}
	if !s.closeTimer.succeed() {
		t.Fatal("Close guard cleanup not joined")
	}
}

func TestTaskArchiveTLSCloseNotifyKeepsSealedDeadline(t *testing.T) {
	_, server, client := quiesceDeadlineTLS(t)
	raw, peer := net.Pipe()
	defer raw.Close()
	defer peer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()
	deadline, _ := ctx.Deadline()
	done := make(chan error, 1)
	ready := make(chan struct{})
	go func() {
		guard := &archiveDeadlineConn{Conn: raw}
		guard.seal(deadline)
		conn := tls.Server(guard, server)
		if err := conn.HandshakeContext(ctx); err != nil {
			done <- err
			return
		}
		close(ready)
		// The peer never reads this real encrypted close_notify.
		done <- conn.CloseWrite()
	}()
	conn := tls.Client(peer, client)
	if err := conn.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	<-ready
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("nonreading peer unexpectedly accepted close_notify")
		}
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatalf("expected original IO timeout: %v", err)
		}
	case <-time.After(time.Until(deadline.Add(150 * time.Millisecond))):
		t.Fatal("TLS CloseWrite extended original sealed deadline")
	}
}
