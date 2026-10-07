package controlrunner

import (
	"context"
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
