package controlrunner

import (
	"context"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/launcher"
)

type fixedClock struct {
	observation controlprotocol.ClockObservation
}

func (c *fixedClock) Observe(context.Context) (controlprotocol.ClockObservation, error) {
	return c.observation, nil
}

func TestSupervisorRejectsUntrustedConstruction(t *testing.T) {
	for _, kernel := range []*launcher.KernelBoundary{nil, {}} {
		s, err := NewSupervisor(SupervisorOptions{Kernel: kernel, UID: 1000, GID: 1000, MaxActive: 4, Executable: "/task3/sandbox-launcher", JournalDirectory: "/journal/current"})
		if err == nil || s != nil {
			t.Fatal("untrusted boundary created supervisor")
		}
	}
	for _, limit := range []uint32{65, ^uint32(0)} {
		if _, err := NewSupervisor(SupervisorOptions{MaxActive: limit}); err == nil {
			t.Fatal("oversized registry accepted")
		}
	}
	var missing *fixedClock
	if _, err := NewSupervisor(SupervisorOptions{Clock: missing}); err == nil {
		t.Fatal("typed-nil clock accepted")
	}
	for _, s := range []*Supervisor{nil, {}} {
		if len(s.Birth().RuntimePublicKey) != 0 {
			t.Fatal("empty birth disclosed")
		}
		if err := s.Activate(context.Background(), controlprotocol.TargetActivationEvidence{}); err == nil {
			t.Fatal("zero supervisor activated")
		}
	}
}

func TestAuthorityObservationRejectsLateAndInvalid(t *testing.T) {
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	for _, obs := range []controlprotocol.ClockObservation{{}, {UTC: now, Uncertainty: -1}, {UTC: now, Uncertainty: 2 * time.Second}} {
		if _, err := observeControlled(context.Background(), &fixedClock{obs}); err == nil {
			t.Fatal("invalid clock accepted")
		}
	}
	got, err := observeControlled(context.Background(), &fixedClock{controlprotocol.ClockObservation{UTC: now}})
	if err != nil || got.Before(now) || got.After(now.Add(time.Second)) {
		t.Fatalf("controlled observation %v %v", got, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = observeControlled(canceled, &fixedClock{controlprotocol.ClockObservation{UTC: now}}); err == nil {
		t.Fatal("canceled clock accepted")
	}
}
