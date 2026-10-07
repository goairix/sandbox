package controlrunner

import (
	"sync"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/controltarget"
)

func TestRegistryLookupConcurrentRecordPublication(t *testing.T) {
	const commandID = "test-original-command"
	s := &Supervisor{admission: true, active: make(map[string]*Execution)}
	e := &Execution{supervisor: s, commandID: commandID, record: controltarget.ExecJournalRecord{Context: controlprotocol.ExecStartContext{CommandID: commandID}}}
	s.active[commandID] = e
	start := make(chan struct{})
	var workers sync.WaitGroup
	for _, terminal := range []bool{false, true} {
		workers.Add(1)
		go func(terminal bool) {
			defer workers.Done()
			<-start
			for i := 0; i < 20000; i++ {
				// The same owner lock and whole-record replacement used by successful
				// renewal and terminal persistence; command ID never semantically changes.
				e.mu.Lock()
				next := e.record
				next.AuthorityDeadline = time.Unix(int64(i+1), 0).UTC()
				if terminal {
					next.RootPID = 42
					next.RootWaitStatus = 0
					next.DrainConfirmed = true
				}
				e.record = next
				e.mu.Unlock()
			}
		}(terminal)
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		<-start
		for i := 0; i < 40000; i++ {
			if !s.isActiveExecution(e) {
				t.Error("original registry identity changed during record publication")
				return
			}
		}
	}()
	close(start)
	workers.Wait()
	s.mu.Lock()
	delete(s.active, commandID)
	s.mu.Unlock()
	if s.isActiveExecution(e) {
		t.Fatal("terminal registry removal ignored")
	}
}
