package controlrunner

import (
	"context"
	"testing"
	"time"
)

func TestAuthorityWatchdogAbsoluteBudget(t *testing.T) {
	const second = int64(time.Second)
	cases := []struct {
		name                    string
		now, authority, command int64
		want                    time.Duration
	}{
		{"short-authority-long-command", 100 * second, 102 * second, 3700 * second, 32 * time.Second},
		{"queued-no-reset", 110 * second, 102 * second, 3700 * second, 22 * time.Second},
		{"already-exhausted", 133 * second, 102 * second, 3700 * second, 0},
		{"command-caps-renewal", 100 * second, 125 * second, 104 * second, 34 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := watchdogRemaining(c.now, c.authority, c.command); got != c.want {
				t.Fatalf("got %s want %s", got, c.want)
			}
		})
	}
}

func TestPreparedStartCloseOrdering(t *testing.T) {
	s := &Supervisor{}
	newExecution := func() *Execution {
		return &Execution{supervisor: s, runContext: context.Background(), authorityDeadlineNS: 42, commandDeadlineNS: 99}
	}
	t.Run("close-before-inflight-start", func(t *testing.T) {
		e := newExecution()
		e.closeRequested = true
		if e.beginStartLocked() == nil || e.startCommitted {
			t.Fatal("closed prepared registration could start")
		}
	})
	t.Run("one-inflight-start-remains-owned", func(t *testing.T) {
		e := newExecution()
		if err := e.beginStartLocked(); err != nil {
			t.Fatal(err)
		}
		e.closeRequested = true
		if e.beginStartLocked() == nil {
			t.Fatal("second start after Close")
		}
		if !e.startCommitted || e.authorityDeadlineNS != 42 || e.commandDeadlineNS != 99 {
			t.Fatal("inflight identity/deadline lost")
		}
	})
	t.Run("global-failure-before-start", func(t *testing.T) {
		e := newExecution()
		s.failurePending.Store(true)
		if e.beginStartLocked() == nil {
			t.Fatal("failed target started prepared registration")
		}
	})
}
