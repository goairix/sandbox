//go:build linux || darwin

package controltarget

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func writeHistoryFixture(t *testing.T, o JournalOptions, id string, temp bool) int64 {
	t.Helper()
	r := journalRecordFixture()
	r.Context.CommandID = id
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	bucket := filepath.Join(o.Directory, "commands", id[:2])
	if err = os.MkdirAll(bucket, 0700); err != nil {
		t.Fatal(err)
	}
	name := id + ".json"
	if temp {
		name = "." + id + ".11111111-1111-4111-8111-111111111111.tmp"
	}
	if err = os.WriteFile(filepath.Join(bucket, name), b, 0600); err != nil {
		t.Fatal(err)
	}
	return int64(len(b))
}

func TestJournalRecovery(t *testing.T) {
	t.Run("complete-history-and-temps", func(t *testing.T) {
		j, o := createJournalFixture(t)
		base := j.Status().LogicalBytes
		m := j.Status().Gate
		j.Close()
		size := writeHistoryFixture(t, o, "22222222-2222-4222-8222-222222222222", false)
		size += writeHistoryFixture(t, o, "22222222-2222-4222-8222-222222222222", true)
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(o.Directory, ".gate.11111111-1111-4111-8111-111111111111.tmp")
		if err = os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
		j, err = OpenClosedJournal(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		defer j.Close()
		s := j.Status()
		if s.Records != 1 || s.TemporaryFiles != 2 || s.LogicalBytes != base+size+int64(len(b)) || !s.AccountingKnown {
			t.Fatalf("accounting %+v", s)
		}
		if _, err = os.Stat(path); err != nil {
			t.Fatal("temp removed", err)
		}
	})
	for _, field := range []string{"identity", "generation", "boot", "epoch"} {
		t.Run("binding/"+field, func(t *testing.T) {
			j, o := createJournalFixture(t)
			j.Close()
			switch field {
			case "identity":
				o.Identity.Target = "other"
			case "generation":
				o.Identity.Generation++
			case "boot":
				o.Identity.Runtime.BootID = "other"
			case "epoch":
				o.DataGateEpoch++
			}
			if j, err := OpenClosedJournal(context.Background(), o); !errors.Is(err, ErrIdentityMismatch) {
				if j != nil {
					j.Close()
				}
				t.Fatalf("binding: %v", err)
			}
		})
	}
	cases := map[string]func(t *testing.T, o JournalOptions){
		"unknown-root": func(t *testing.T, o JournalOptions) {
			mustWrite(t, filepath.Join(o.Directory, "unknown"), []byte("unknown"))
		},
		"missing-manifest": func(t *testing.T, o JournalOptions) {
			p := filepath.Join(o.Directory, "gate.json")
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			mustWrite(t, filepath.Join(o.Directory, ".gate.11111111-1111-4111-8111-111111111111.tmp"), b)
			if err = os.Remove(p); err != nil {
				t.Fatal(err)
			}
		},
		"malformed-gate-temp": func(t *testing.T, o JournalOptions) {
			mustWrite(t, filepath.Join(o.Directory, ".gate.11111111-1111-4111-8111-111111111111.tmp"), []byte("{"))
		},
		"invalid-temp-nonce": func(t *testing.T, o JournalOptions) {
			mustWrite(t, filepath.Join(o.Directory, ".gate.bad.tmp"), []byte("{}"))
		},
		"invalid-bucket": func(t *testing.T, o JournalOptions) {
			if err := os.Mkdir(filepath.Join(o.Directory, "commands", "GG"), 0700); err != nil {
				t.Fatal(err)
			}
		},
		"bucket-mismatch": func(t *testing.T, o JournalOptions) {
			writeHistoryFixture(t, o, "22222222-2222-4222-8222-222222222222", false)
			if err := os.Rename(filepath.Join(o.Directory, "commands", "22"), filepath.Join(o.Directory, "commands", "33")); err != nil {
				t.Fatal(err)
			}
		},
		"record-binding": func(t *testing.T, o JournalOptions) {
			r := journalRecordFixture()
			r.Context.Runtime.BootID = "other"
			writeHistoryFixture(t, o, r.Context.CommandID, false)
			b, _ := json.Marshal(r)
			mustWrite(t, filepath.Join(o.Directory, "commands", "22", r.Context.CommandID+".json"), b)
		},
		"record-filename": func(t *testing.T, o JournalOptions) {
			writeHistoryFixture(t, o, "22222222-2222-4222-8222-222222222222", false)
			if err := os.Rename(filepath.Join(o.Directory, "commands", "22", "22222222-2222-4222-8222-222222222222.json"), filepath.Join(o.Directory, "commands", "22", "22222222-2222-4222-8222-222222222223.json")); err != nil {
				t.Fatal(err)
			}
		},
		"malformed-command-temp": func(t *testing.T, o JournalOptions) {
			writeHistoryFixture(t, o, "22222222-2222-4222-8222-222222222222", true)
			mustWrite(t, filepath.Join(o.Directory, "commands", "22", ".22222222-2222-4222-8222-222222222222.11111111-1111-4111-8111-111111111111.tmp"), []byte("{"))
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			j, o := createJournalFixture(t)
			j.Close()
			mutate(t, o)
			before := treeBytes(t, o.Directory)
			if j, err := OpenClosedJournal(context.Background(), o); err == nil {
				j.Close()
				t.Fatal("invalid journal recovered")
			}
			after := treeBytes(t, o.Directory)
			if before != after {
				t.Fatalf("evidence changed: before %d after %d", before, after)
			}
		})
	}
}
func mustWrite(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}
func treeBytes(t *testing.T, path string) int64 {
	t.Helper()
	var n int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			n += info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestJournalPagedAccounting(t *testing.T) {
	j, o := createJournalFixture(t)
	want := j.Status().LogicalBytes
	j.Close()
	for n := 0; n < 1000; n++ {
		id := fmt.Sprintf("22000000-0000-4000-8000-%012x", n+1)
		want += writeHistoryFixture(t, o, id, false)
	}
	pages := 0
	hook := func(op, name string, after bool) error {
		if op == "read-page" && after {
			pages++
		}
		return nil
	}
	j, err := newJournal(context.Background(), o, false, hook)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if pages != 13 {
		t.Fatalf("page128 expected 2 root + 2 commands + 9 bucket pages, got %d", pages)
	}
	s := j.Status()
	if s.Records != 1000 || s.TemporaryFiles != 0 || s.LogicalBytes != want || !s.AccountingKnown {
		t.Fatalf("1000 files status %+v want bytes %d", s, want)
	}
	var directoryBytes, allocatedBytes int64
	var directories, files int
	err = filepath.Walk(o.Directory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		var st unix.Stat_t
		if err := unix.Lstat(path, &st); err != nil {
			return err
		}
		if info.IsDir() {
			directories++
			directoryBytes += st.Blocks * 512
		} else {
			files++
			allocatedBytes += st.Blocks * 512
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("observed filesystem: content=%d bytes, directories=%d allocated-directory=%d bytes, files-including-lock=%d allocated-files=%d bytes, steady-journal-FDs=3; block allocation is platform-specific", s.LogicalBytes, directories, directoryBytes, files, allocatedBytes)
}

// These are private point primitives for the next evidence consumer. Public
// same-record retry and evidence verification are deliberately not added here.
func TestJournalCommandPrimitives(t *testing.T) {
	j, o := createJournalFixture(t)
	r := journalRecordFixture()
	j.mu.Lock()
	err := j.persistNewCommandLocked(context.Background(), r)
	j.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	j.mu.Lock()
	got, err := j.readCommandLocked(context.Background(), r.Context.CommandID)
	j.mu.Unlock()
	if err != nil || got == nil || *got != r {
		t.Fatalf("point read %+v %v", got, err)
	}
	b, err := os.ReadFile(filepath.Join(o.Directory, "commands", "22", r.Context.CommandID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if s := j.Status(); s.Records != 1 || s.LogicalBytes != sizedGate(t, o)+int64(len(b)) {
		t.Fatalf("point accounting %+v", s)
	}
	changed := r
	changed.TicketDigest = strings.Repeat("f", 64)
	j.mu.Lock()
	err = j.persistNewCommandLocked(context.Background(), changed)
	j.mu.Unlock()
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("overwritten record: %v", err)
	}
	gotBytes, err := os.ReadFile(filepath.Join(o.Directory, "commands", "22", r.Context.CommandID+".json"))
	if err != nil || !bytes.Equal(b, gotBytes) {
		t.Fatal("immutable bytes changed", err)
	}
	j.mu.Lock()
	_, err = j.readCommandLocked(context.Background(), "../gate.json")
	j.mu.Unlock()
	if !errors.Is(err, ErrInvalidRecord) {
		t.Fatal(err)
	}
	r.Context.Runtime.BootID = "wrong"
	j.mu.Lock()
	err = j.persistNewCommandLocked(context.Background(), r)
	j.mu.Unlock()
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Fatal(err)
	}
	j.mu.Lock()
	missing, err := j.readCommandLocked(context.Background(), "33000000-0000-4000-8000-000000000001")
	j.mu.Unlock()
	if err != nil || missing != nil {
		t.Fatalf("absent %+v %v", missing, err)
	}
}
func sizedGate(t *testing.T, o JournalOptions) int64 {
	t.Helper()
	st, err := os.Stat(filepath.Join(o.Directory, "gate.json"))
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

func TestJournalCommandCapacity(t *testing.T) {
	o := journalOptionsFixture(t)
	o.MaxBytes = 65536
	j, err := CreateClosedJournal(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	var warned bool
	var count int
	for ; count < 100; count++ {
		r := journalRecordFixture()
		r.Context.CommandID = fmt.Sprintf("22000000-0000-4000-8000-%012x", count+1)
		wire, _ := encodeExecJournalRecord(r)
		before := j.Status()
		j.mu.Lock()
		err = j.persistNewCommandLocked(context.Background(), r)
		j.mu.Unlock()
		if (before.LogicalBytes+int64(len(wire)))*100 >= 65536*85 {
			if !errors.Is(err, ErrCapacity) {
				t.Fatalf("85%% admitted: %v", err)
			}
			if j.Status() != before {
				t.Fatal("capacity denial changed status")
			}
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		s := j.Status()
		if s.Warning != (s.LogicalBytes*100 >= 65536*70) {
			t.Fatal("70% warning")
		}
		warned = warned || s.Warning
	}
	if !warned || count == 100 {
		t.Fatalf("warned %t records %d", warned, count)
	}
	if _, err = j.CloseGate(context.Background(), 2); err != nil {
		t.Fatalf("soft limit blocked close: %v", err)
	}
}

func TestJournalRecoveryHardBudget(t *testing.T) {
	o := journalOptionsFixture(t)
	o.MaxBytes = 65536
	j, err := CreateClosedJournal(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	m := j.Status().Gate
	base := j.Status().LogicalBytes
	j.Close()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	remaining := int64(65536) - base
	for n := 1; remaining > 0; n++ {
		size := int64(8192)
		if remaining < size {
			size = remaining
		}
		if size < int64(len(b)) {
			t.Fatal("fixture remainder too short")
		}
		padded := append(append([]byte(nil), b...), bytes.Repeat([]byte(" "), int(size)-len(b))...)
		mustWrite(t, filepath.Join(o.Directory, fmt.Sprintf(".gate.00000000-0000-4000-8000-%012x.tmp", n)), padded)
		remaining -= size
	}
	before := treeBytes(t, o.Directory)
	if before != 65536 {
		t.Fatalf("fixture bytes %d", before)
	}
	if j, err = OpenClosedJournal(context.Background(), o); !errors.Is(err, ErrCapacity) || j != nil {
		if j != nil {
			j.Close()
		}
		t.Fatalf("hard budget close %v", err)
	}
	if treeBytes(t, o.Directory) != before {
		t.Fatal("failed close changed bytes")
	}
}

func TestJournalCommandPersistenceFaults(t *testing.T) {
	for _, op := range []string{"write", "file-sync", "rename", "dir-sync"} {
		for _, after := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/after=%t", op, after), func(t *testing.T) {
				j, o := createJournalFixture(t)
				r := journalRecordFixture()
				if err := os.Mkdir(filepath.Join(o.Directory, "commands", "22"), 0700); err != nil {
					t.Fatal(err)
				}
				injected := errors.New("command syscall failure")
				j.files.hook = func(got, name string, done bool) error {
					if got == op && done == after {
						return injected
					}
					return nil
				}
				j.mu.Lock()
				err := j.persistNewCommandLocked(context.Background(), r)
				j.mu.Unlock()
				if !errors.Is(err, injected) {
					t.Fatal(err)
				}
				if s := j.Status(); !s.Poisoned || s.AccountingKnown || s.Records != 0 {
					t.Fatalf("uncertain command %+v", s)
				}
				j.mu.Lock()
				got, readErr := j.readCommandLocked(context.Background(), r.Context.CommandID)
				j.mu.Unlock()
				committed := (op == "rename" && after) || op == "dir-sync"
				if readErr != nil || (got != nil) != committed {
					t.Fatalf("poisoned history read %+v %v committed=%t", got, readErr, committed)
				}
				if got != nil && *got != r {
					t.Fatal("committed bytes mismatch")
				}
				j.Close()
				reopened, err := OpenClosedJournal(context.Background(), o)
				incomplete := op == "write" && !after
				if incomplete {
					if err == nil {
						reopened.Close()
						t.Fatal("incomplete command temp accepted")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				s := reopened.Status()
				if committed {
					if s.Records != 1 || s.TemporaryFiles != 0 {
						t.Fatalf("committed recovery %+v", s)
					}
				} else if s.Records != 0 || s.TemporaryFiles != 1 {
					t.Fatalf("retained temp %+v", s)
				}
			})
		}
	}
}

func TestJournalRecoveryCancelledScan(t *testing.T) {
	j, o := createJournalFixture(t)
	j.Close()
	writeHistoryFixture(t, o, "22222222-2222-4222-8222-222222222222", false)
	before := treeBytes(t, o.Directory)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hook := func(op, name string, after bool) error {
		if op == "read-page" && after {
			cancel()
		}
		return nil
	}
	if j, err := newJournal(ctx, o, false, hook); j != nil || !errors.Is(err, context.Canceled) {
		if j != nil {
			j.Close()
		}
		t.Fatalf("cancelled scan %v", err)
	}
	if treeBytes(t, o.Directory) != before {
		t.Fatal("cancelled scan mutated evidence")
	}
	reopened, err := OpenClosedJournal(context.Background(), o)
	if err != nil {
		t.Fatal("lock retained after failed scan", err)
	}
	reopened.Close()
}

func TestJournalPagedAccountingFileLimit(t *testing.T) {
	j, o := createJournalFixture(t)
	m := j.Status().Gate
	j.Close()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	// These are real valid retained temp files. 65535 content files leave one
	// transient slot for a gate write, but no slot for another immutable command.
	for n := 1; n <= 65534; n++ {
		mustWrite(t, filepath.Join(o.Directory, fmt.Sprintf(".gate.00000000-0000-4000-8000-%012x.tmp", n)), b)
	}
	j, err = OpenClosedJournal(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	s := j.Status()
	if s.TemporaryFiles != 65534 || !s.NewRecordsStopped || !s.AccountingKnown {
		j.Close()
		t.Fatalf("file limit status %+v", s)
	}
	j.mu.Lock()
	err = j.persistNewCommandLocked(context.Background(), journalRecordFixture())
	j.mu.Unlock()
	if !errors.Is(err, ErrCapacity) {
		j.Close()
		t.Fatalf("reserved gate slot consumed: %v", err)
	}
	if _, err = j.CloseGate(context.Background(), 2); err != nil {
		j.Close()
		t.Fatal("reserved gate slot unavailable", err)
	}
	j.Close()
	mustWrite(t, filepath.Join(o.Directory, ".gate.00000000-0000-4000-8000-ffffffffffff.tmp"), b)
	if j, err = OpenClosedJournal(context.Background(), o); j != nil || !errors.Is(err, ErrCapacity) {
		if j != nil {
			j.Close()
		}
		t.Fatalf("gate exceeded 65536 content files: %v", err)
	}
}
