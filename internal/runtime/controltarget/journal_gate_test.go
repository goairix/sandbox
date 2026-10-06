//go:build linux || darwin

package controltarget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func journalOptionsFixture(t *testing.T) JournalOptions {
	t.Helper()
	return JournalOptions{Directory: filepath.Join(protectedParent(t), "journal"), Identity: journalIdentityFixture(), DataGateEpoch: 2, ManagementUID: uint32(os.Geteuid())}
}
func createJournalFixture(t *testing.T) (*Journal, JournalOptions) {
	t.Helper()
	o := journalOptionsFixture(t)
	j, err := CreateClosedJournal(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	return j, o
}

func TestJournalGate(t *testing.T) {
	t.Run("durable-closed-and-lifecycle", func(t *testing.T) {
		j, o := createJournalFixture(t)
		s := j.Status()
		if s.Gate.GateState != "closed" || !s.AccountingKnown || s.Poisoned || s.Closed || s.LogicalBytes == 0 {
			t.Fatalf("status %+v", s)
		}
		if other, err := CreateClosedJournal(context.Background(), o); err == nil {
			other.Close()
			t.Fatal("overwrote existing journal")
		}
		if other, err := OpenClosedJournal(context.Background(), o); !errors.Is(err, ErrBusy) {
			if other != nil {
				other.Close()
			}
			t.Fatalf("lock: %v", err)
		}
		if _, err := j.CloseGate(context.Background(), 3); !errors.Is(err, ErrIdentityMismatch) {
			t.Fatalf("epoch: %v", err)
		}
		if _, err := j.CloseGate(nil, 2); !errors.Is(err, ErrInvalidConfiguration) {
			t.Fatal(err)
		}
		if _, err := j.CloseGate(context.Background(), 2); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(o.Directory, "gate.json"))
		if err != nil {
			t.Fatal(err)
		}
		var m GateManifest
		if err = json.Unmarshal(b, &m); err != nil || m.GateState != "closed" {
			t.Fatalf("persisted %s: %v", b, err)
		}
		if j.Status().LogicalBytes != int64(len(b)) {
			t.Fatalf("accounting %+v bytes %d", j.Status(), len(b))
		}
		if err = j.Close(); err != nil {
			t.Fatal(err)
		}
		if err = j.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err = j.CloseGate(context.Background(), 2); !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
		if s = j.Status(); !s.Closed || !s.NewRecordsStopped {
			t.Fatalf("closed %+v", s)
		}
		reopened, err := OpenClosedJournal(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
	})
	t.Run("invalid-handles-and-options", func(t *testing.T) {
		for _, j := range []*Journal{nil, {}} {
			if _, err := j.CloseGate(context.Background(), 2); !errors.Is(err, ErrJournalUnavailable) {
				t.Fatal(err)
			}
			if _, err := j.CloseGate(nil, 2); !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatal(err)
			}
			if err := j.Close(); err != nil {
				t.Fatal(err)
			}
			if s := j.Status(); !s.Closed || s.AccountingKnown || !s.NewRecordsStopped {
				t.Fatalf("zero status %+v", s)
			}
		}
		for _, mutate := range []func(*JournalOptions){func(o *JournalOptions) { o.ManagementUID++ }, func(o *JournalOptions) { o.MaxBytes = 65535 }, func(o *JournalOptions) { o.MaxBytes = 256*1024*1024 + 1 }, func(o *JournalOptions) { o.DataGateEpoch = 0 }, func(o *JournalOptions) { o.Identity.Generation = 0 }} {
			o := journalOptionsFixture(t)
			mutate(&o)
			if j, err := CreateClosedJournal(context.Background(), o); !errors.Is(err, ErrInvalidConfiguration) {
				if j != nil {
					j.Close()
				}
				t.Fatalf("options %+v: %v", o, err)
			}
		}
		if _, err := CreateClosedJournal(nil, journalOptionsFixture(t)); !errors.Is(err, ErrInvalidConfiguration) {
			t.Fatal(err)
		}
	})
	t.Run("historical-open-first-closes", func(t *testing.T) {
		j, o := createJournalFixture(t)
		m := j.Status().Gate
		j.Close()
		m.GateState = "open"
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(o.Directory, "gate.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
		j, err = OpenClosedJournal(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		defer j.Close()
		b, err = os.ReadFile(filepath.Join(o.Directory, "gate.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(b, &m); err != nil || m.GateState != "closed" {
			t.Fatalf("historical gate remained open: %s %v", b, err)
		}
	})
	t.Run("actual-process-lock-and-hard-exit", func(t *testing.T) {
		j, o := createJournalFixture(t)
		runJournalChild(t, "busy", o.Directory, 0)
		j.Close()
		runJournalChild(t, "hard-exit", o.Directory, 23)
		reopened, err := OpenClosedJournal(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		if reopened.Status().Gate.GateState != "closed" {
			t.Fatal("restart gate")
		}
	})
}

// Commands are synchronous and context-bounded. No test-owned goroutine or
// hook survives Fatal; CommandContext kills and CombinedOutput joins the child.
func runJournalChild(t *testing.T, mode, path string, want int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestJournalProcessHelper$")
	cmd.WaitDelay = time.Second
	cmd.Env = append(os.Environ(), "JOURNAL_TEST_CHILD="+mode, "JOURNAL_TEST_PATH="+path)
	b, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var e *exec.ExitError
		if !errors.As(err, &e) {
			t.Fatalf("child: %v %s", err, b)
		}
		code = e.ExitCode()
	}
	if code != want {
		t.Fatalf("child %s exit %d want %d: %s", mode, code, want, b)
	}
}
func TestJournalProcessHelper(t *testing.T) {
	mode := os.Getenv("JOURNAL_TEST_CHILD")
	if mode == "" {
		return
	}
	o := JournalOptions{Directory: os.Getenv("JOURNAL_TEST_PATH"), Identity: journalIdentityFixture(), DataGateEpoch: 2, ManagementUID: uint32(os.Geteuid())}
	j, err := OpenClosedJournal(context.Background(), o)
	if mode == "busy" {
		if !errors.Is(err, ErrBusy) {
			t.Fatalf("expected busy: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = j
	os.Exit(23)
}

func TestJournalPersistenceFaults(t *testing.T) {
	injected := errors.New("injected actual syscall boundary")
	for _, op := range []string{"open-temp", "write", "file-sync", "rename", "dir-sync"} {
		for _, after := range []bool{false, true} {
			for _, cancelled := range []bool{false, true} {
				name := fmt.Sprintf("%s/after=%t/cancel=%t", op, after, cancelled)
				t.Run(name, func(t *testing.T) {
					j, o := createJournalFixture(t)
					before := j.Status()
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					hits := 0
					j.files.hook = func(got, name string, done bool) error {
						if got == op && done == after {
							hits++
							if cancelled {
								cancel()
								return nil
							}
							return injected
						}
						return nil
					}
					receipt, err := j.CloseGate(ctx, 2)
					if err == nil || receipt != (GateManifest{}) {
						t.Fatalf("false close ack %+v: %v", receipt, err)
					}
					cause := injected
					if cancelled {
						cause = context.Canceled
					}
					if !errors.Is(err, cause) || !errors.Is(err, ErrJournalUnavailable) || hits != 1 {
						t.Fatalf("error %v hits %d", err, hits)
					}
					s := j.Status()
					if !s.Poisoned || s.AccountingKnown || !s.NewRecordsStopped || s.LogicalBytes != before.LogicalBytes {
						t.Fatalf("uncertain status %+v", s)
					}
					if _, err = j.CloseGate(context.Background(), 2); !errors.Is(err, ErrJournalUnavailable) {
						t.Fatal(err)
					}
					names, err := os.ReadDir(o.Directory)
					if err != nil {
						t.Fatal(err)
					}
					temps := 0
					for _, n := range names {
						if strings.HasPrefix(n.Name(), ".gate.") {
							temps++
						}
					}
					wantTemp := 1
					if (op == "open-temp" && !after) || (op == "rename" && after) || op == "dir-sync" {
						wantTemp = 0
					}
					if temps != wantTemp {
						t.Fatalf("retained temps %d want %d", temps, wantTemp)
					}
					if err = j.Close(); err != nil {
						t.Fatal(err)
					}
					// Incomplete empty temps deliberately make cold recovery fail; complete
					// possible-committed bytes are preserved and validated on restart.
					reopened, reopenErr := OpenClosedJournal(context.Background(), o)
					if reopened != nil {
						reopened.Close()
					}
					incomplete := (op == "open-temp" && after) || (op == "write" && !after)
					if incomplete && reopenErr == nil {
						t.Fatal("incomplete temp accepted on recovery")
					}
					if !incomplete && reopenErr != nil {
						t.Fatalf("complete retained bytes: %v", reopenErr)
					}
				})
			}
		}
	}
	t.Run("create-directory-durability-chain", func(t *testing.T) {
		o := journalOptionsFixture(t)
		var dirs []string
		hook := func(op, name string, after bool) error {
			if op == "dir-sync" && after {
				dirs = append(dirs, name)
			}
			return nil
		}
		j, err := newJournal(context.Background(), o, true, hook)
		if err != nil {
			t.Fatal(err)
		}
		defer j.Close()
		want := []string{"journal", filepath.Dir(o.Directory), "commands", "journal", "journal", filepath.Dir(o.Directory)}
		if !reflect.DeepEqual(dirs, want) {
			t.Fatalf("actual directory fsync sequence %q want %q", dirs, want)
		}
	})
	t.Run("failed-create-retains-evidence", func(t *testing.T) {
		o := journalOptionsFixture(t)
		hook := func(op, name string, after bool) error {
			if op == "file-sync" && strings.HasPrefix(name, ".gate.") && after {
				return injected
			}
			return nil
		}
		j, err := newJournal(context.Background(), o, true, hook)
		if j != nil || !errors.Is(err, injected) {
			t.Fatalf("create %v %v", j, err)
		}
		names, err := os.ReadDir(o.Directory)
		if err != nil || len(names) != 3 {
			t.Fatalf("retained files %v %v", names, err)
		}
		if _, err = OpenClosedJournal(context.Background(), o); err == nil {
			t.Fatal("missing manifest rebuilt from complete temp")
		}
	})
}

// blockJournalSync registers release/cancel/join before a caller can Fatal.
// Hooks and descriptors are restored/closed only after the worker has joined.
func blockJournalSync(t *testing.T, j *Journal) (release func(), cancel context.CancelFunc, done <-chan struct{}, result <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	unblock := make(chan struct{})
	finished := make(chan struct{})
	errorsOut := make(chan error, 1)
	var once sync.Once
	release = func() { once.Do(func() { close(unblock) }) }
	previous := j.files.hook
	j.files.hook = func(op, name string, after bool) error {
		if op == "file-sync" && !after {
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
			t.Error("journal worker failed to join within 5s")
			return
		}
		j.files.hook = previous
	})
	go func() { defer close(finished); _, err := j.CloseGate(ctx, 2); errorsOut <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not reach actual file fsync boundary")
	}
	return release, cancel, finished, errorsOut
}
func TestJournalPersistenceFaultsWorker(t *testing.T) {
	t.Run("cancel-blocked-worker", func(t *testing.T) {
		j, _ := createJournalFixture(t)
		release, cancel, done, result := blockJournalSync(t, j)
		cancel()
		release()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("worker join")
		}
		if err := <-result; !errors.Is(err, context.Canceled) || !j.Status().Poisoned {
			t.Fatalf("cancelled worker: %v %+v", err, j.Status())
		}
	})
	t.Run("fatal-cleanup-regression", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestJournalFatalCleanupHelper$")
		cmd.WaitDelay = time.Second
		cmd.Env = append(os.Environ(), "JOURNAL_TEST_FATAL=1")
		b, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(b), "intentional Fatal exercises release/cancel/join") || !strings.Contains(string(b), "WORKER_JOINED_AND_LOCK_RELEASED") {
			t.Fatalf("Fatal cleanup regression: %v\n%s", err, b)
		}
	})
}
func TestJournalFatalCleanupHelper(t *testing.T) {
	if os.Getenv("JOURNAL_TEST_FATAL") == "" {
		return
	}
	j, o := createJournalFixture(t)
	// Registered before worker cleanup: executes after join/hook restoration.
	t.Cleanup(func() {
		if err := j.Close(); err != nil {
			t.Error(err)
		}
		if !j.Status().Poisoned {
			t.Error("cancel did not poison")
		}
		entries, err := os.ReadDir(o.Directory)
		if err != nil {
			t.Error(err)
		}
		found := false
		for _, e := range entries {
			found = found || strings.HasPrefix(e.Name(), ".gate.")
		}
		if !found {
			t.Error("uncertain evidence removed")
		}
		reopened, err := OpenClosedJournal(context.Background(), o)
		if err != nil {
			t.Error("lock not released or retained bytes invalid", err)
		} else {
			reopened.Close()
		}
		fmt.Println("WORKER_JOINED_AND_LOCK_RELEASED")
	})
	blockJournalSync(t, j)
	t.Fatal("intentional Fatal exercises release/cancel/join")
}

func TestJournalPersistenceFaultsDirectoryChain(t *testing.T) {
	injected := errors.New("directory durability failure")
	for nth := 1; nth <= 6; nth++ {
		for _, after := range []bool{false, true} {
			t.Run(fmt.Sprintf("sync-%d/after=%t", nth, after), func(t *testing.T) {
				o := journalOptionsFixture(t)
				seen := 0
				hook := func(op, name string, done bool) error {
					if op == "dir-sync" {
						if !done {
							seen++
						}
						if seen == nth && done == after {
							return injected
						}
					}
					return nil
				}
				j, err := newJournal(context.Background(), o, true, hook)
				if j != nil || !errors.Is(err, injected) {
					t.Fatalf("false create receipt %v %v", j, err)
				}
				if _, err = os.Stat(o.Directory); err != nil {
					t.Fatalf("created root removed: %v", err)
				}
				if _, err = CreateClosedJournal(context.Background(), o); err == nil {
					t.Fatal("partial create overwritten")
				}
			})
		}
	}
}
