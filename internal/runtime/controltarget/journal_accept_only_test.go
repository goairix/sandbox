//go:build linux || darwin

package controltarget

import (
	"context"
	"errors"
	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"testing"
	"time"
)

func TestLiveJournalAcceptOnly(t *testing.T) {
	f := liveSetup(t)
	j := f.create(t)
	ctx := context.Background()
	if a, err := j.AcceptExecution(ctx, f.e); a != nil || err == nil {
		t.Fatal("closed admission")
	}
	if err := j.InstallActivation(ctx, f.activation); err != nil {
		t.Fatal(err)
	}
	a, err := j.AcceptExecution(ctx, f.e)
	if err != nil {
		t.Fatal(err)
	}
	r, err := j.Lookup(ctx, f.e.Context().CommandID)
	if err != nil || r.State != "accepted" {
		t.Fatal("registration preceded persistence", err)
	}
	snapshot, err := a.Snapshot()
	if err != nil || snapshot != *r {
		t.Fatal("snapshot", err)
	}
	snapshot.Context.CommandID = "mutated"
	copy := *a
	if err = j.ConsumeStart(ctx, &copy); err == nil {
		t.Fatal("copy used")
	}
	if err = j.ConsumeStart(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err = j.ConsumeStart(ctx, a); !errors.Is(err, ErrStartConsumed) {
		t.Fatal("replayed", err)
	}
}
func TestLiveJournalExactExistingIsDiagnostic(t *testing.T) {
	f := liveSetup(t)
	j := f.create(t)
	ctx := context.Background()
	if err := j.InstallActivation(ctx, f.activation); err != nil {
		t.Fatal(err)
	}
	if _, err := j.AcceptExecution(ctx, f.e); err != nil {
		t.Fatal(err)
	}
	f.clock.now = f.clock.now.Add(time.Hour)
	if a, err := j.AcceptExecution(ctx, f.e); a != nil || !errors.Is(err, ErrExecutionExists) {
		t.Fatal("exact existing should only return diagnostic exists", err)
	}
	j.Close()
	cold, err := OpenClosedJournal(ctx, f.o)
	if err != nil {
		t.Fatal(err)
	}
	defer cold.Close()
	if a, err := cold.AcceptExecution(ctx, f.e); a != nil || !errors.Is(err, ErrExecutionExists) {
		t.Fatal("history should only return diagnostic exists", err)
	}
}
func TestLiveJournalAcceptBoundaries(t *testing.T) {
	for _, kind := range []string{"expired", "issuer", "birth", "zero"} {
		t.Run(kind, func(t *testing.T) {
			f := liveSetup(t)
			j := f.create(t)
			ctx := context.Background()
			if err := j.InstallActivation(ctx, f.activation); err != nil {
				t.Fatal(err)
			}
			e := f.e
			switch kind {
			case "expired":
				f.clock.now = f.clock.now.Add(21 * time.Second)
			case "issuer":
				other := liveSetup(t)
				e = other.e
			case "birth":
				j.birth.BootID = f.e.Context().CommandID
			case "zero":
				e = controlprotocol.ExecStartEvidence{}
			}
			if a, err := j.AcceptExecution(ctx, e); a != nil || err == nil {
				t.Fatal("invalid admission")
			}
			if j.Status().Records != 0 {
				t.Fatal("invalid admission persisted")
			}
		})
	}
}
func TestLiveJournalAcceptWriteAndLateFaults(t *testing.T) {
	for _, op := range []string{"file-sync", "rename", "dir-sync", "late-clock"} {
		for _, after := range []bool{false, true} {
			t.Run(op+map[bool]string{false: "before", true: "after"}[after], func(t *testing.T) {
				f := liveSetup(t)
				j := f.create(t)
				ctx := context.Background()
				if err := j.InstallActivation(ctx, f.activation); err != nil {
					t.Fatal(err)
				}
				hit := false
				j.files.hook = func(operation, name string, isAfter bool) error {
					if op == "late-clock" && operation == "dir-sync" && isAfter == after {
						hit = true
						f.clock.now = f.clock.now.Add(time.Hour)
						return nil
					}
					if operation == op && isAfter == after {
						hit = true
						return errors.New("actual boundary error")
					}
					return nil
				}
				if a, err := j.AcceptExecution(ctx, f.e); a != nil || err == nil || !hit || !j.Status().Poisoned || j.Status().Gate.GateState != "closed" {
					t.Fatal("unsafe failed acceptance", a, err, hit, j.Status())
				}
			})
		}
	}
}
