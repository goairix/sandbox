//go:build linux || darwin

package controltarget

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

func TestJournalExecPersistenceFaults(t *testing.T) {
	for _, op := range []string{"write", "file-sync", "rename", "dir-sync"} {
		for _, after := range []bool{false, true} {
			for _, cancelled := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/after=%t/cancel=%t", op, after, cancelled), func(t *testing.T) {
					j, o := createJournalFixture(t)
					e := journalEvidence(t, journalRecordFixture(), "payload")
					want := evidenceRecord(e)
					// Warm bucket isolates command persistence from directory creation.
					if err := os.Mkdir(filepath.Join(o.Directory, "commands", "22"), 0700); err != nil {
						t.Fatal(err)
					}
					before := j.Status()
					injected := errors.New("actual command boundary failure")
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					seen := false
					j.files.hook = func(got, name string, done bool) error {
						if got == op && done == after {
							seen = true
							if cancelled {
								cancel()
								return nil
							}
							return injected
						}
						return nil
					}
					got, err := j.RecordUnknown(ctx, e)
					cause := injected
					if cancelled {
						cause = context.Canceled
					}
					if !seen || got != nil || !errors.Is(err, cause) || !errors.Is(err, ErrJournalUnavailable) {
						t.Fatalf("false persistence result %+v %v seen=%t", got, err, seen)
					}
					s := j.Status()
					if !s.Poisoned || s.AccountingKnown || !s.NewRecordsStopped || s.LogicalBytes != before.LogicalBytes || s.Records != before.Records {
						t.Fatalf("uncertain status %+v", s)
					}
					j.files.hook = nil
					if _, err = j.RecordUnknown(context.Background(), e); !errors.Is(err, ErrJournalUnavailable) {
						t.Fatal("poison retry", err)
					}
					if _, err = j.CloseGate(context.Background(), 2); !errors.Is(err, ErrJournalUnavailable) {
						t.Fatal("poison close", err)
					}
					committed := (op == "rename" && after) || op == "dir-sync"
					got, err = j.Lookup(context.Background(), want.Context.CommandID)
					if err != nil || (got != nil) != committed || (got != nil && *got != want) {
						t.Fatalf("possible commit lookup %+v %v committed=%t", got, err, committed)
					}
					entries, err := os.ReadDir(filepath.Join(o.Directory, "commands", "22"))
					if err != nil || len(entries) != 1 {
						t.Fatalf("retained files %v %v", entries, err)
					}
					path := filepath.Join(o.Directory, "commands", "22", entries[0].Name())
					retained, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if !(op == "write" && !after) {
						var r ExecJournalRecord
						if err = decodeExecJournalRecord(retained, &r); err != nil || r != want {
							t.Fatalf("retained full record %+v %v", r, err)
						}
					} else if len(retained) != 0 {
						t.Fatal("prewrite fixture unexpectedly nonempty")
					}
					if err = j.Close(); err != nil {
						t.Fatal(err)
					}
					reopened, err := OpenClosedJournal(context.Background(), o)
					if op == "write" && !after {
						if err == nil {
							reopened.Close()
							t.Fatal("incomplete temp admitted")
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					defer reopened.Close()
					s = reopened.Status()
					wantRecords, wantTemps := uint64(0), uint64(1)
					if committed {
						wantRecords, wantTemps = 1, 0
					}
					if s.Gate.GateState != "closed" || s.Poisoned || !s.AccountingKnown || s.Records != wantRecords || s.TemporaryFiles != wantTemps || s.LogicalBytes != before.LogicalBytes+int64(len(retained)) {
						t.Fatalf("reopen status %+v", s)
					}
					afterBytes, err := os.ReadFile(path)
					if err != nil || !bytes.Equal(retained, afterBytes) {
						t.Fatal("uncertain bytes changed", err)
					}
					if committed {
						retry, err := reopened.RecordUnknown(context.Background(), e)
						if err != nil || retry == nil || *retry != want {
							t.Fatalf("reopen immutable retry %+v %v", retry, err)
						}
					}
				})
			}
		}
	}
}

func TestJournalExecRestartRetainedUnknown(t *testing.T) {
	j, o := createJournalFixture(t)
	e := journalEvidence(t, journalRecordFixture(), "payload")
	want, err := j.RecordUnknown(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(commandPath(o, want.Context.CommandID))
	if err != nil {
		t.Fatal(err)
	}
	gateBefore, err := os.ReadFile(filepath.Join(o.Directory, "gate.json"))
	if err != nil {
		t.Fatal(err)
	}
	statusBefore := j.Status()
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	// Actual child opens the public-produced intent and exits without Close.
	runJournalChild(t, "exit", o.Directory, 23)
	reopened, err := OpenClosedJournal(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.Lookup(context.Background(), want.Context.CommandID)
	if err != nil || got == nil || *got != *want {
		t.Fatalf("retainedunknown %+v %v", got, err)
	}
	after, err := os.ReadFile(commandPath(o, want.Context.CommandID))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("hard exit changed full bytes", err)
	}
	gateAfter, err := os.ReadFile(filepath.Join(o.Directory, "gate.json"))
	if err != nil || !bytes.Equal(gateBefore, gateAfter) {
		t.Fatal("closed gate changed", err)
	}
	if statusAfter := reopened.Status(); statusAfter != statusBefore || statusAfter.Gate.GateState != "closed" || statusAfter.Records != 1 || statusAfter.TemporaryFiles != 0 || !statusAfter.AccountingKnown {
		t.Fatalf("hard exit counters before %+v after %+v", statusBefore, statusAfter)
	}
	if retry, err := reopened.RecordUnknown(context.Background(), e); err != nil || retry == nil || *retry != *want {
		t.Fatalf("restart retry %+v %v", retry, err)
	}
	r := journalRecordFixture()
	r.NotBefore = r.NotBefore.Add(time.Second)
	r.NotAfter = r.NotAfter.Add(time.Second)
	r.Context.ExpiresAt = r.Context.ExpiresAt.Add(time.Second)
	if _, err = reopened.RecordUnknown(context.Background(), journalEvidence(t, r, "payload")); !errors.Is(err, ErrConflict) {
		t.Fatal("restart extended window", err)
	}
	t.Logf("M1 real public RecordUnknown -> child hard exit23 -> cold open: exact unknown=%dB gate=%dB logical=%d records=1 temps=0 accounting-known=true closed gate; full context/digests/window retained, no replay", len(before), len(gateBefore), statusBefore.LogicalBytes)
}

// Block the public record only after a real file fsync. Cleanup cancellation,
// release and <=5s join always precede hook restoration and descriptor cleanup.
func blockUnknownSync(t *testing.T, j *Journal, e controlprotocol.ExecStartEvidence) (func(), context.CancelFunc, <-chan struct{}, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	unblock := make(chan struct{})
	finished := make(chan struct{})
	result := make(chan error, 1)
	var once sync.Once
	release := func() { once.Do(func() { close(unblock) }) }
	previous := j.files.hook
	j.files.hook = func(op, name string, after bool) error {
		if op == "file-sync" && after {
			close(entered)
			<-unblock
		}
		return nil
	}
	t.Cleanup(func() {
		cancel()
		release()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("unknown worker failed to join within 5s")
			return
		}
		j.files.hook = previous
	})
	go func() { defer close(finished); _, err := j.RecordUnknown(ctx, e); result <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("unknown worker did not reach actual fsync")
	}
	return release, cancel, finished, result
}

func TestJournalExecConcurrentLifecycle(t *testing.T) {
	j, _ := createJournalFixture(t)
	e := journalEvidence(t, journalRecordFixture(), "payload")
	release, cancel, done, result := blockUnknownSync(t, j, e)
	finished := make(chan struct{})
	results := make(chan error, 2)
	// The observer goroutine serially invokes both disk query and Close. A
	// separate status call contends for the same mutex while record is blocked.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := j.Lookup(context.Background(), e.Context().CommandID)
		results <- err
		results <- j.Close()
	}()
	go func() { defer wg.Done(); _ = j.Status() }()
	go func() { wg.Wait(); close(finished) }()
	t.Cleanup(func() {
		cancel()
		release()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("lifecycle workers did not join within 5s")
		}
	})
	cancel()
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("record worker join")
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("lifecycle worker join")
	}
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	if s := j.Status(); !s.Closed || !s.Poisoned || s.AccountingKnown {
		t.Fatalf("concurrent status %+v", s)
	}
}

func TestJournalExecFatalCleanup(t *testing.T) {
	for _, mode := range []string{"complete", "missing-retained-evidence"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestJournalExecFatalHelper$")
			cmd.WaitDelay = time.Second
			cmd.Env = append(os.Environ(), "JOURNAL_EXEC_FATAL="+mode)
			b, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(b), "intentional unknown Fatal") {
				t.Fatalf("Fatal child %v\n%s", err, b)
			}
			marker := strings.Contains(string(b), "UNKNOWN_WORKER_JOINED_AND_RETAINED")
			if marker != (mode == "complete") {
				t.Fatalf("cleanup false result\n%s", b)
			}
			if mode != "complete" && (!strings.Contains(string(b), "NEGATIVE_UNKNOWN_REMOVED") || !strings.Contains(string(b), "retained unknown temp missing")) {
				t.Fatalf("negative postcondition not checked\n%s", b)
			}
		})
	}
}

func TestJournalExecFatalHelper(t *testing.T) {
	mode := os.Getenv("JOURNAL_EXEC_FATAL")
	if mode == "" {
		return
	}
	j, o := createJournalFixture(t)
	e := journalEvidence(t, journalRecordFixture(), "payload")
	want := evidenceRecord(e)
	t.Cleanup(func() {
		ok := true
		fail := func(args ...any) { ok = false; t.Error(args...) }
		entries, err := os.ReadDir(filepath.Join(o.Directory, "commands", "22"))
		if err != nil {
			fail(err)
		}
		if mode == "missing-retained-evidence" {
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), ".tmp") {
					if err := os.Remove(filepath.Join(o.Directory, "commands", "22", entry.Name())); err != nil {
						fail(err)
					}
				}
			}
			fmt.Println("NEGATIVE_UNKNOWN_REMOVED")
		}
		entries, err = os.ReadDir(filepath.Join(o.Directory, "commands", "22"))
		if err != nil {
			fail(err)
		}
		found := false
		for _, entry := range entries {
			b, err := os.ReadFile(filepath.Join(o.Directory, "commands", "22", entry.Name()))
			if err != nil {
				fail(err)
				continue
			}
			var r ExecJournalRecord
			if err := decodeExecJournalRecord(b, &r); err != nil || r != want {
				fail("retained record invalid", err)
			}
			found = true
		}
		if !found {
			fail("retained unknown temp missing")
		}
		if s := j.Status(); !s.Poisoned || s.AccountingKnown {
			fail("worker cancellation status", s)
		}
		if err := j.Close(); err != nil {
			fail(err)
		}
		reopened, err := OpenClosedJournal(context.Background(), o)
		if err != nil {
			fail("lock or recovery", err)
		} else {
			if s := reopened.Status(); s.Gate.GateState != "closed" || s.TemporaryFiles != 1 || s.Records != 0 {
				fail("recovered temp counters", s)
			}
			if err := reopened.Close(); err != nil {
				fail(err)
			}
		}
		if ok {
			fmt.Println("UNKNOWN_WORKER_JOINED_AND_RETAINED")
		}
	})
	blockUnknownSync(t, j, e)
	t.Fatal("intentional unknown Fatal exercises cancellation/release/join")
}
