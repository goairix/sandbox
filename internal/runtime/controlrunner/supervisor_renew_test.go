package controlrunner

import "testing"

func TestRenewalRejectsFailurePendingWhileOwnerLocked(t *testing.T) {
	s := &Supervisor{}
	e := &Execution{supervisor: s, state: "accepted", rootPID: 42, authorityDeadlineNS: 200, commandDeadlineNS: 300, waitDone: make(chan struct{})}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.renewableLocked(100) {
		t.Fatal("model's original accepted root should be renewable")
	}
	failed := make(chan struct{})
	go func() { s.failurePending.Store(true); close(failed) }()
	<-failed
	if e.mu.TryLock() {
		e.mu.Unlock()
		t.Fatal("test must hold the owner lock that isolation cannot acquire")
	}
	if e.renewableLocked(100) {
		t.Fatal("renewal ignored atomic target failure while owner lock remained held")
	}
}
