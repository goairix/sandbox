package controlrunner

import (
	"context"
	"fmt"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"os"
	"testing"
	"time"
)

func TestTaskQuiesceSoleWaitRetirement(t *testing.T) {
	s := &Supervisor{active: make(map[string]*Execution)}
	e := &Execution{supervisor: s, commandID: "original", ownerDone: make(chan struct{}), waitDone: make(chan struct{}), watchdogDone: make(chan struct{}), output: make(chan streamFrame)}
	_, e.cancel = context.WithCancel(context.Background())
	s.active[e.commandID] = e
	returned := make(chan struct{})
	go func() { s.finishExecution(e); close(returned) }()
	defer func() {
		select {
		case <-returned:
		case <-time.After(time.Second):
			t.Error("original owner did not join")
		}
	}()
	close(e.watchdogDone)
	s.mu.Lock()
	if s.active[e.commandID] != e {
		t.Error("retired before sole Wait")
	}
	select {
	case <-e.ownerDone:
		t.Error("owner joined before sole Wait")
	default:
	}
	s.mu.Unlock()
	close(e.waitDone)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("owner unjoined")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.active) != 0 {
		t.Fatal("joined owner remained charged")
	}
	select {
	case <-e.ownerDone:
	default:
		t.Fatal("retired without owner completion")
	}
}
func TestTaskQuiesceRegisteredBeforeSpawn(t *testing.T) {
	s := &Supervisor{}
	e := &Execution{supervisor: s, runContext: context.Background()}
	s.usersClosed.Store(true)
	if err := e.beginStartLocked(); err == nil || e.startCommitted {
		t.Fatal("USERS barrier allowed original Start")
	}
}

func TestTaskQuiesceStartInFlight(t *testing.T) {
	s := &Supervisor{}
	e := &Execution{supervisor: s, runContext: context.Background()}
	if err := e.beginStartLocked(); err != nil {
		t.Fatal(err)
	}
	s.usersClosed.Store(true)
	if !e.startCommitted {
		t.Fatal("lost original committed Start")
	}
	if err := e.beginStartLocked(); err == nil {
		t.Fatal("replacement Start allowed")
	}
	e.state = "accepted"
	e.rootPID = 42
	e.authorityDeadlineNS = 100
	e.commandDeadlineNS = 100
	e.waitDone = make(chan struct{})
	if e.renewableLocked(1) {
		t.Fatal("renew crossed permanent USERS barrier")
	}
}
func TestTaskQuiesceSoleWaitTerminalBeforeWatchdogJoin(t *testing.T) {
	s := &Supervisor{active: make(map[string]*Execution)}
	e := &Execution{supervisor: s, commandID: "terminal", state: "local_terminal", ownerDone: make(chan struct{}), waitDone: make(chan struct{}), watchdogDone: make(chan struct{}), output: make(chan streamFrame)}
	_, e.cancel = context.WithCancel(context.Background())
	close(e.waitDone)
	s.active[e.commandID] = e
	done := make(chan struct{})
	go func() { s.finishExecution(e); close(done) }()
	s.mu.Lock()
	if s.active[e.commandID] != e {
		t.Error("terminal was dropped before callback join")
	}
	select {
	case <-e.ownerDone:
		t.Error("owner prematurely joined")
	default:
	}
	s.mu.Unlock()
	close(e.watchdogDone)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("owner did not finish")
	}
	for i := 0; i < 100; i++ {
		e := &Execution{supervisor: s, commandID: fmt.Sprint(i), ownerDone: make(chan struct{}), waitDone: make(chan struct{}), output: make(chan streamFrame)}
		_, e.cancel = context.WithCancel(context.Background())
		close(e.waitDone)
		s.mu.Lock()
		s.active[e.commandID] = e
		s.mu.Unlock()
		s.finishExecution(e)
	}
	if len(s.active) != 0 || len(*s.activeView.Load()) != 0 {
		t.Fatal("historical completions accumulated in frozen set")
	}
}
func TestTaskQuiesceShutdownAggregateStop(t *testing.T) {
	s := &Supervisor{}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	if err = write.SetWriteDeadline(time.Now().Add(10 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	_, err = write.Write(make([]byte, 1<<20))
	if err == nil {
		t.Fatal("failed to fill bounded test pipe")
	}
	executions := make([]*Execution, 64)
	for i := range executions {
		executions[i] = &Execution{ownerDone: make(chan struct{}), control: write, startCommitted: true, state: "accepted"}
	}
	started := time.Now()
	err = s.stopOriginalExecutions(context.Background(), executions, started.Add(100*time.Millisecond))
	if err == nil {
		t.Fatal("blocked stop treated as delivered")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("aggregate stop extended absolute deadline: %s", elapsed)
	}
}

func TestTaskQuiesceShutdownAcceptedBoundary(t *testing.T) {
	for _, fault := range []string{"expired", "failure", "cancel", "closed"} {
		t.Run(fault, func(t *testing.T) {
			s := &Supervisor{activation: &p.TargetActivationEvidence{}}
			a := &userQuiescenceAttempt{supervisor: s, deadline: time.Now().Add(time.Hour), acceptedWire: []byte("historical bytes")}
			a.self = a
			s.quiescence = a
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch fault {
			case "expired":
				a.deadline = time.Now().Add(-time.Second)
			case "failure":
				s.failurePending.Store(true)
			case "cancel":
				cancel()
			case "closed":
				s.closed = true
			}
			wire, err := s.quiescenceAcceptedReceipt(ctx, a)
			if err == nil || wire != nil {
				t.Fatal("ineligible cached ACK escaped")
			}
		})
	}
}
