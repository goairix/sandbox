//go:build linux || darwin

package controltarget

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLiveJournalPersistenceFaults(t *testing.T) {
	for _, stage := range []string{"activation", "accepted", "renew", "terminal", "unknown"} {
		for _, op := range []string{"file-sync", "rename", "dir-sync"} {
			for _, after := range []bool{false, true} {
				t.Run(stage+"/"+op+"/"+map[bool]string{false: "before", true: "after"}[after], func(t *testing.T) {
					f := liveSetup(t)
					j := f.create(t)
					ctx := context.Background()
					var a *AcceptedExecution
					if stage != "activation" {
						if err := j.InstallActivation(ctx, f.activation); err != nil {
							t.Fatal(err)
						}
					}
					if stage == "renew" || stage == "terminal" || stage == "unknown" {
						var err error
						a, err = j.AcceptExecution(ctx, f.e)
						if err != nil {
							t.Fatal(err)
						}
						if err = j.ConsumeStart(ctx, a); err != nil {
							t.Fatal(err)
						}
					}
					injected := false
					j.files.hook = func(operation, name string, isAfter bool) error {
						if !injected && operation == op && isAfter == after {
							injected = true
							return errors.New("injected real persistence boundary")
						}
						return nil
					}
					var err error
					switch stage {
					case "activation":
						err = j.InstallActivation(ctx, f.activation)
					case "accepted":
						a, err = j.AcceptExecution(ctx, f.e)
						if a != nil {
							t.Fatal("uncertain acceptance returned start")
						}
					case "renew":
						err = j.RenewAccepted(ctx, a, f.renew(t))
					case "unknown":
						err = j.RecordExecutionUnknown(ctx, a, "monitor_lost")
					case "terminal":
						_, err = j.RecordLocalTerminal(ctx, a, LocalExecutionResult{RootPID: 42, DrainConfirmed: true, Reason: "exited"})
					}
					if !injected || err == nil || !j.Status().Poisoned || j.Status().Gate.GateState != "closed" {
						t.Fatalf("fault failed closed: %v %+v hit %v", err, j.Status(), injected)
					}
					j.files.hook = nil
					if retry, err := j.AcceptExecution(ctx, f.e); retry != nil || err == nil {
						t.Fatal("poison replay")
					}
					j.Close()
					cold, err := OpenClosedJournal(ctx, f.o)
					if err == nil {
						defer cold.Close()
						if err = cold.InstallActivation(ctx, f.activation); err == nil {
							t.Fatal("recovery activated")
						}
						if cold.Status().Gate.GateState != "closed" {
							t.Fatal("recovery open")
						}
					}
				})
			}
		}
	}
}
func TestLiveJournalLatePersistenceNoACK(t *testing.T) {
	for _, stage := range []string{"activation", "accepted", "renew"} {
		t.Run(stage, func(t *testing.T) {
			f := liveSetup(t)
			j := f.create(t)
			ctx := context.Background()
			var a *AcceptedExecution
			if stage != "activation" {
				if err := j.InstallActivation(ctx, f.activation); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "renew" {
				var err error
				a, err = j.AcceptExecution(ctx, f.e)
				if err != nil {
					t.Fatal(err)
				}
				if err = j.ConsumeStart(ctx, a); err != nil {
					t.Fatal(err)
				}
			}
			renew := f.renew(t)
			hit := false
			j.files.hook = func(op, name string, after bool) error {
				if op == "dir-sync" && after {
					hit = true
					f.clock.now = f.clock.now.Add(time.Hour)
				}
				return nil
			}
			var err error
			switch stage {
			case "activation":
				err = j.InstallActivation(ctx, f.activation)
			case "accepted":
				a, err = j.AcceptExecution(ctx, f.e)
				if a != nil {
					t.Fatal("late registration")
				}
			case "renew":
				err = j.RenewAccepted(ctx, a, renew)
			}
			if !hit || err == nil || !j.Status().Poisoned || j.Status().Gate.GateState != "closed" {
				t.Fatal("late ACK", err, j.Status())
			}
		})
	}
}
func TestLiveJournalUnknownAndExpiredStart(t *testing.T) {
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
	if _, err := j.RecordLocalTerminal(ctx, a, LocalExecutionResult{RootPID: 42, DrainConfirmed: true, Reason: "exited"}); err == nil {
		t.Fatal("unconsumed command terminal")
	}
	f.clock.now = f.clock.now.Add(21 * time.Second)
	if err := j.ConsumeStart(ctx, a); err == nil {
		t.Fatal("expired start consumed")
	}
	if err := j.RenewAccepted(ctx, a, f.renew(t)); err == nil {
		t.Fatal("expired unstarted command renewed")
	}
	if err := j.RecordExecutionUnknown(ctx, a, "not_started_before_deadline"); err != nil {
		t.Fatal(err)
	}
	r, err := j.Lookup(ctx, a.record.Context.CommandID)
	if err != nil || r.State != "unknown" || r.Reason == "" {
		t.Fatal(r, err)
	}
	if j.Status().Gate.GateState != "closed" {
		t.Fatal("unknown kept admission open")
	}
}
func TestLiveJournalHistoricalUnknownNoStart(t *testing.T) {
	f := liveSetup(t)
	j := f.create(t)
	ctx := context.Background()
	if _, err := j.RecordUnknown(ctx, f.e); err != nil {
		t.Fatal(err)
	}
	if err := j.InstallActivation(ctx, f.activation); err != nil {
		t.Fatal(err)
	}
	if a, err := j.AcceptExecution(ctx, f.e); a != nil || !errors.Is(err, ErrExecutionExists) {
		t.Fatal("unknown replay", err)
	}
}
func TestLiveJournalEncodedCapacity(t *testing.T) {
	f := liveSetup(t)
	f.o.MaxBytes = minJournalMaxBytes
	j := f.create(t)
	ctx := context.Background()
	if err := j.InstallActivation(ctx, f.activation); err != nil {
		t.Fatal(err)
	}
	var first *AcceptedExecution
	seenWarning := false
	for n := 0; n < 100; n++ {
		c := f.claims
		c.Context.CommandID = fmt.Sprintf("%08x-2222-4222-8222-222222222222", n+1)
		issuer, err := f.o.Verifier.VerifyCommandIssuerCertificate(f.issuerWire, f.clock.now)
		if err != nil {
			t.Fatal(err)
		}
		w, err := controlprotocol.SignExecStartTicket(f.issuerKey, issuer, c)
		if err != nil {
			t.Fatal(err)
		}
		e, err := f.o.Verifier.VerifyExecStartTicket(w, f.issuerWire, c.Context, f.descriptor, f.clock.now)
		if err != nil {
			t.Fatal(err)
		}
		before := j.Status()
		a, err := j.AcceptExecution(ctx, e)
		if errors.Is(err, ErrCapacity) {
			r := evidenceRecord(e)
			r.Version = 2
			r.State = "accepted"
			r.AuthorityDeadline = r.NotAfter
			b, err := encodeExecJournalRecord(r)
			if err != nil {
				t.Fatal(err)
			}
			if (before.LogicalBytes+int64(len(b)))*100 < j.maxBytes*85 {
				t.Fatal("capacity ignored actual encoded bytes")
			}
			if a != nil || j.Status() != before {
				t.Fatal("capacity mutated admission")
			}
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = a
		}
		seenWarning = seenWarning || j.Status().Warning
		var total int64
		var files uint64
		err = filepath.WalkDir(f.o.Directory, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && d.Name() != "lock" {
				s, err := d.Info()
				if err != nil {
					return err
				}
				total += s.Size()
				files++
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if j.Status().LogicalBytes != total || files != j.contentFilesLocked() {
			t.Fatal("accounting differs from real files")
		}
	}
	if !seenWarning {
		t.Fatal("70 percent warning missing")
	}
	if err := j.ConsumeStart(ctx, first); err != nil {
		t.Fatal(err)
	}
	old, _ := first.Snapshot()
	next := old
	next.State = "local_terminal"
	next.RootPID = 42
	next.DrainConfirmed = true
	next.Reason = "exited"
	b, _ := encodeExecJournalRecord(next)
	// Set the configured hard bound to the actual old+replacement peak minus one,
	// then verify refusal did not change disk. This is not synthetic byte accounting.
	j.mu.Lock()
	saved := j.maxBytes
	j.maxBytes = j.logicalBytes + int64(len(b)) - 1
	j.mu.Unlock()
	if _, err := j.RecordLocalTerminal(ctx, first, LocalExecutionResult{RootPID: 42, DrainConfirmed: true, Reason: "exited"}); !errors.Is(err, ErrCapacity) {
		t.Fatal("hard peak not enforced", err)
	}
	j.mu.Lock()
	j.maxBytes = saved
	j.mu.Unlock()
	if _, err := j.RecordLocalTerminal(ctx, first, LocalExecutionResult{RootPID: 42, DrainConfirmed: true, Reason: "exited"}); err != nil {
		t.Fatal(err)
	}
	j.Close()
	cold, err := OpenClosedJournal(ctx, f.o)
	if err != nil {
		t.Fatal(err)
	}
	defer cold.Close()
	if cold.Status().LogicalBytes != j.logicalBytes+int64(len("closed")-len("open")) {
		t.Fatal("cold accounting delta", cold.Status(), j.logicalBytes)
	}
}

func TestLiveJournalStrictVersionDispatch(t *testing.T) {
	r := journalRecordFixture()
	legacy, err := encodeExecJournalRecord(r)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(legacy, []byte("authority_deadline")) {
		t.Fatal("new fields in version1")
	}
	r.Version = 2
	r.State = "accepted"
	r.AuthorityDeadline = r.NotAfter
	b, err := encodeExecJournalRecord(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{bytes.Replace(b, []byte(`"version":2`), []byte(`"version":1,"version":2`), 1), bytes.Replace(b, []byte(`"version":2`), []byte(`"version":2,"version":1`), 1), bytes.Replace(b, []byte(`"drain_confirmed":false`), []byte(`"drain_confirmed":null`), 1), append(b, []byte(`{}`)...), bytes.Replace(legacy, []byte(`"version":1`), []byte(`"version":2`), 1)} {
		var decoded ExecJournalRecord
		if err := decodeExecJournalRecord(bad, &decoded); err == nil {
			t.Fatal("ambiguous schema accepted")
		}
	}
	var decoded ExecJournalRecord
	if err := decodeExecJournalRecord(append(b, bytes.Repeat([]byte(" "), 8192-len(b))...), &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decodeExecJournalRecord(append(b, bytes.Repeat([]byte(" "), 8193-len(b))...), &decoded); err == nil {
		t.Fatal("8193 record accepted")
	}
}

func TestLiveJournalActivationAndOneUse(t *testing.T) {
	f := liveSetup(t)
	j := f.create(t)
	ctx := context.Background()
	if f.clock.calls != 0 {
		t.Fatal("idle construction called clock")
	}
	if a, err := j.AcceptExecution(ctx, f.e); err == nil || a != nil {
		t.Fatal("closed gate admitted")
	}
	if err := j.InstallActivation(ctx, f.activation); err != nil {
		t.Fatal(err)
	}
	if j.Status().Gate.GateState != "open" {
		t.Fatal("gate closed")
	}
	if err := j.InstallActivation(ctx, f.activation); err != nil {
		t.Fatal("same activation retry", err)
	}
	if _, err := os.Stat(filepath.Join(f.o.Directory, "activation.json")); err != nil {
		t.Fatal(err)
	}
	a, err := j.AcceptExecution(ctx, f.e)
	if err != nil || a == nil {
		t.Fatal(err)
	}
	disk, err := j.Lookup(ctx, f.e.Context().CommandID)
	if err != nil || disk.State != "accepted" || disk.Version != 2 {
		t.Fatal("not durable before registration", disk, err)
	}
	if retry, err := j.AcceptExecution(ctx, f.e); retry != nil || !errors.Is(err, ErrExecutionExists) {
		t.Fatal("retry acquired start", err)
	}
	copy := *a
	if err := j.ConsumeStart(ctx, &copy); err == nil {
		t.Fatal("copied handle consumed")
	}
	if err := j.ConsumeStart(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := j.ConsumeStart(ctx, a); !errors.Is(err, ErrStartConsumed) {
		t.Fatal("start replay", err)
	}
	if err := j.RenewAccepted(ctx, a, f.renew(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := j.RecordLocalTerminal(ctx, a, LocalExecutionResult{RootPID: 42, RootWaitStatus: 0, DrainConfirmed: true, Reason: "exited"}); err != nil {
		t.Fatal(err)
	}
	if err := j.RenewAccepted(ctx, a, f.renew(t)); err == nil {
		t.Fatal("terminal renewed")
	}
	if retry, err := j.AcceptExecution(ctx, f.e); retry != nil || !errors.Is(err, ErrExecutionExists) {
		t.Fatal("terminal replay", err)
	}
	j.Close()
	reopened, err := OpenClosedJournal(ctx, f.o)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Status().Gate.GateState != "closed" {
		t.Fatal("history reopened live")
	}
	if err := reopened.InstallActivation(ctx, f.activation); err == nil {
		t.Fatal("cold handle activated")
	}
	if err := reopened.ConsumeStart(ctx, a); err == nil {
		t.Fatal("foreign origin consumed")
	}
	if err := reopened.RenewAccepted(ctx, a, f.renew(t)); err == nil {
		t.Fatal("cold handle renewed")
	}
}
