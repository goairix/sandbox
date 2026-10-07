package controlrunner

import (
	"testing"
	"time"
)

func TestTaskQuiesceShutdownDeadlineEarlier(t *testing.T) {
	expired := make(chan struct{})
	d := newIsolationDeadline(time.Now().Add(time.Hour), func() { close(expired) })
	d.shorten(time.Now().Add(20 * time.Millisecond))
	select {
	case <-expired:
	case <-time.After(time.Second):
		t.Fatal("earlier deadline ignored")
	}
	<-d.done
	if d.succeed() {
		t.Fatal("expired owner revived")
	}
}
func TestTaskQuiesceShutdownDeadlineJoin(t *testing.T) {
	expired := make(chan struct{}, 1)
	d := newIsolationDeadline(time.Now().Add(time.Second), func() { expired <- struct{}{} })
	if !d.succeed() {
		t.Fatal("healthy owner could not finish")
	}
	select {
	case <-d.done:
	default:
		t.Fatal("success exposed before timer owner joined")
	}
	d.shorten(time.Now().Add(-time.Second))
	select {
	case <-expired:
		t.Fatal("stopped owner called callback")
	default:
	}
}
func TestTaskQuiesceShutdownDeadlineCallbackJoin(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	d := newIsolationDeadline(time.Now().Add(-time.Second), func() { close(entered); <-release })
	defer func() {
		close(release)
		select {
		case <-d.done:
		case <-time.After(time.Second):
			t.Error("callback owner unjoined")
		}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("deadline never entered")
	}
	if d.succeed() {
		t.Fatal("success during callback")
	}
	select {
	case <-d.done:
		t.Fatal("callback reported joined early")
	default:
	}
}

// The real standalone Close final branch calls succeed, whose original owner is
// deliberately held after state2 until its join crosses the original deadline.
// Private timer completion is controlled; no PID1 or physical drain is claimed.
func TestTaskQuiesceShutdownOriginalBoundAfterLateJoin(t *testing.T) {
	s := &Supervisor{}
	s.self = s
	// Prevent the cooperative sweep from terminating the host test. The independent
	// timer remains active and its existing private callback is observed below.
	s.failureOnce.Do(func() {})
	original := time.Now().Add(30 * time.Millisecond)
	owner := &isolationDeadline{deadline: original, wake: make(chan struct{}, 1), done: make(chan struct{})}
	s.isolationDeadline = owner
	result := make(chan bool, 1)
	go func() { joined, _ := s.finishStopExecutions(owner, original); result <- joined }()
	select {
	case <-owner.wake:
	case <-time.After(time.Second):
		t.Fatal("actual succeed never entered state2")
	}
	s.deadlineMu.Lock()
	wait := time.NewTimer(time.Until(original.Add(10 * time.Millisecond)))
	<-wait.C
	close(owner.done)
	early := false
	select {
	case joined := <-result:
		early = true
		if joined {
			t.Error("late owner reported joined success")
		}
		t.Error("failure returned before preserving original absolute deadline")
	case <-time.After(20 * time.Millisecond):
	}
	// Only the callback is replaced for host observation. Production deadline
	// arbitration and finishStopExecutions run unchanged; there is no public hook.
	expired := make(chan struct{}, 1)
	safe := newIsolationDeadline(time.Now().Add(time.Hour), func() { expired <- struct{}{} })
	s.isolationDeadline = safe
	s.deadlineMu.Unlock()
	if !early {
		select {
		case joined := <-result:
			if joined {
				t.Error("late owner reported success")
			}
		case <-time.After(time.Second):
			t.Fatal("failure branch never returned")
		}
	}
	select {
	case <-expired:
	case <-time.After(time.Second):
		t.Fatal("expired original deadline not enforced")
	}
	select {
	case <-safe.done:
	case <-time.After(time.Second):
		t.Fatal("fallback callback did not join")
	}
	safe.mu.Lock()
	actual := safe.deadline
	safe.mu.Unlock()
	if actual.After(original) {
		t.Errorf("fallback received a fresh interval: original=%s actual=%s", original, actual)
	}
}
