//go:build linux || darwin

package controltarget

import (
	"context"
	"errors"
	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"os"
	"sync"
	"testing"
	"time"
)

func TestLiveJournalRenewExactAndTerminal(t *testing.T) {
	f := liveSetup(t)
	j := f.create(t)
	ctx := context.Background()
	if err := j.InstallActivation(ctx, f.activation); err != nil {
		t.Fatal(err)
	}
	a, err := j.AcceptExecution(ctx, f.e)
	if err != nil {
		t.Fatal(err)
	}
	e := f.renew(t)
	if err := j.RenewAccepted(ctx, a, e); err == nil {
		t.Fatal("unconsumed acceptance renewed")
	}
	if err := j.ConsumeStart(ctx, a); err != nil {
		t.Fatal(err)
	}
	original, _ := a.Snapshot()
	if err := j.RenewAccepted(ctx, a, e); err != nil {
		t.Fatal(err)
	}
	next, _ := a.Snapshot()
	if next.NotAfter != original.NotAfter || next.NotBefore != original.NotBefore || next.AuthorityDeadline != e.NotAfter() {
		t.Fatal("renewal changed original start or lost maximum")
	}
	before, err := os.ReadFile(commandPath(f.o, original.Context.CommandID))
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	j.files.hook = func(op, name string, after bool) error {
		if op == "open-temp" {
			writes++
		}
		return nil
	}
	if err := j.RenewAccepted(ctx, a, e); err != nil {
		t.Fatal("idempotent retry", err)
	}
	j.files.hook = nil
	if writes != 0 {
		t.Fatal("identical renewal rewrote file")
	}
	after, err := os.ReadFile(commandPath(f.o, original.Context.CommandID))
	if err != nil || string(before) != string(after) {
		t.Fatal("retry changed persistent deadline")
	}
	copy := *a
	for _, handle := range []*AcceptedExecution{nil, {}, &copy} {
		if err := j.RenewAccepted(ctx, handle, e); err == nil {
			t.Fatal("invalid origin renewed")
		}
	}
	for _, change := range []func(*controlprotocol.ExecRenewTicketClaims){func(c *controlprotocol.ExecRenewTicketClaims) { c.Context.CommandID = c.Context.OperationID }, func(c *controlprotocol.ExecRenewTicketClaims) { c.Context.DataGateEpoch++ }, func(c *controlprotocol.ExecRenewTicketClaims) { c.DescriptorDigest = c.Context.WorkspaceHash }} {
		claims := controlprotocol.ExecRenewTicketClaims(f.claims)
		claims.Purpose = "operation_exec_renew"
		change(&claims)
		wire, err := controlprotocol.SignExecRenewTicket(f.issuerKey, claims)
		if err != nil {
			t.Fatal(err)
		}
		other, err := f.o.Verifier.VerifyExecRenewTicket(wire, f.issuerWire, claims.Context, claims.DescriptorDigest, f.clock.now)
		if err != nil {
			t.Fatal(err)
		}
		if err = j.RenewAccepted(ctx, a, other); err == nil {
			t.Fatal("wrong accepted binding renewed")
		}
	}
	// The ordinary terminal path must preserve only an actual attributable scalar assertion.
	for _, result := range []LocalExecutionResult{{}, {RootPID: 42, Reason: "missing_drain"}, {RootPID: 42, RootWaitStatus: 0x137f, DrainConfirmed: true, Reason: "stopped"}, {RootPID: 42, RootWaitStatus: 0xffff, DrainConfirmed: true, Reason: "continued"}} {
		if _, err := j.RecordLocalTerminal(ctx, a, result); err == nil {
			t.Fatal("invalid local terminal asserted")
		}
	}
	f.clock.now = f.clock.now.Add(time.Minute)
	terminal, err := j.RecordLocalTerminal(ctx, a, LocalExecutionResult{RootPID: 42, RootWaitStatus: 9, DrainConfirmed: true, Reason: "authority_expired"})
	if err != nil {
		t.Fatal("expired authority must still permit local cleanup record", err)
	}
	if terminal.State != "local_terminal" || terminal.AuthorityDeadline != next.AuthorityDeadline || terminal.RootWaitStatus != 9 {
		t.Fatal("lost local terminal")
	}
	if err := j.RenewAccepted(ctx, a, e); err == nil {
		t.Fatal("terminal widened")
	}
	if err := j.RecordExecutionUnknown(ctx, a, "overwrite"); err == nil {
		t.Fatal("terminal overwritten")
	}
}
func TestLiveJournalRenewTerminalSerialized(t *testing.T) {
	f := liveSetup(t)
	j := f.create(t)
	ctx := context.Background()
	if err := j.InstallActivation(ctx, f.activation); err != nil {
		t.Fatal(err)
	}
	a, err := j.AcceptExecution(ctx, f.e)
	if err != nil {
		t.Fatal(err)
	}
	if err = j.ConsumeStart(ctx, a); err != nil {
		t.Fatal(err)
	}
	e := f.renew(t)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = j.RenewAccepted(ctx, a, e) }()
	go func() {
		defer wg.Done()
		if _, err := j.RecordLocalTerminal(ctx, a, LocalExecutionResult{RootPID: 42, DrainConfirmed: true, Reason: "exited"}); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()
	r, err := j.Lookup(ctx, a.record.Context.CommandID)
	if err != nil || r.State != "local_terminal" {
		t.Fatal("terminal lost", err)
	}
	if err = j.RenewAccepted(ctx, a, e); err == nil {
		t.Fatal("terminal race renewal")
	}
}
func TestLiveJournalUnknownCloseFault(t *testing.T) {
	for _, op := range []string{"file-sync", "rename", "dir-sync"} {
		for _, after := range []bool{false, true} {
			t.Run(op+map[bool]string{false: "before", true: "after"}[after], func(t *testing.T) {
				f := liveSetup(t)
				j := f.create(t)
				ctx := context.Background()
				if err := j.InstallActivation(ctx, f.activation); err != nil {
					t.Fatal(err)
				}
				a, err := j.AcceptExecution(ctx, f.e)
				if err != nil {
					t.Fatal(err)
				}
				count := 0
				j.files.hook = func(operation, name string, isAfter bool) error {
					if op == operation && after == isAfter {
						count++
						if count == 2 {
							return errors.New("uncertain closed gate")
						}
					}
					return nil
				}
				if err = j.RecordExecutionUnknown(ctx, a, "monitor_lost"); err == nil || count != 2 || !j.Status().Poisoned || j.Status().Gate.GateState != "closed" {
					t.Fatal("unknown close failed open", err, count)
				}
				r, err := j.Lookup(ctx, f.e.Context().CommandID)
				if err != nil || r.State != "unknown" {
					t.Fatal("unknown not retained", err)
				}
			})
		}
	}
}
