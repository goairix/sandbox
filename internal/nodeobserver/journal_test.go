package nodeobserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNodeObserverJournal(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	j, err := openJournal(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	r := journalRecord{Version: 1, State: "enrolled", Enrollment: json.RawMessage(`{"version":1}`)}
	slot, err := j.reserve(r)
	if err != nil || slot != 0 {
		t.Fatalf("reserve slot=%d: %v", slot, err)
	}
	if _, err := j.reserve(r); err == nil {
		t.Fatal("journal reused an unresolved slot")
	}
	r.State, r.Intent, r.Ticket = "accepted", json.RawMessage(`{"version":1}`), json.RawMessage(`{"version":1}`)
	if err := j.put(0, r); err != nil {
		t.Fatal(err)
	}
	r.State = "signal-issued"
	if err := j.put(0, r); err != nil {
		t.Fatal(err)
	}
	if err := j.close(); err != nil {
		t.Fatal(err)
	}
	j2, err := openJournal(dir, 1)
	if err != nil || j2.records[0].State != "signal-issued" {
		t.Fatalf("recovery: %v", err)
	}
	if err := j2.close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "slot-0000.a"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := openJournal(dir, 1); err == nil {
		t.Fatal("corrupt generation accepted")
	}
}

func TestNodeObserverJournalBounds(t *testing.T) {
	for _, n := range []uint32{0, 1025} {
		if _, err := openJournal(t.TempDir(), n); err == nil {
			t.Fatal("invalid slot count accepted")
		}
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	j, err := openJournal(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer j.close()
	if _, err := j.reserve(journalRecord{Version: 1, State: "terminal"}); err == nil {
		t.Fatal("terminal without original enrollment accepted")
	}
	if err := os.Symlink("/dev/null", filepath.Join(dir, "slot-0000.a")); err != nil {
		t.Fatal(err)
	}
	if _, err := openJournal(dir, 1); err == nil {
		t.Fatal("symlink generation accepted")
	}
}

func TestNodeObserverJournalCrashAndTransitions(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	j, err := openJournal(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	r := journalRecord{Version: 1, State: "enrolled", Enrollment: json.RawMessage(`{"version":1}`)}
	if _, err := j.reserve(r); err != nil {
		t.Fatal(err)
	}
	r.State, r.Intent, r.Ticket = "signal-issued", json.RawMessage(`{"version":1}`), json.RawMessage(`{"version":1}`)
	if err := j.put(0, r); err == nil {
		t.Fatal("signal before accepted journal state")
	}
	r.State = "accepted"
	if err := j.put(0, r); err != nil {
		t.Fatal(err)
	}
	if err := j.close(); err != nil {
		t.Fatal(err)
	}
	// Real unpublished torn generation: restart retains accepted and cannot
	// reinterpret temporary bytes as signal-issued or terminal.
	if err := os.Remove(filepath.Join(dir, "slot-0000.a")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "next-0000"), []byte(`{"sequence":3`), 0600); err != nil {
		t.Fatal(err)
	}
	j, err = openJournal(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer j.close()
	if j.records[0].State != "accepted" {
		t.Fatal("torn generation changed accepted attempt")
	}
	r.Intent = json.RawMessage(`{"version":2}`)
	r.State = "signal-issued"
	if err := j.put(0, r); err == nil {
		t.Fatal("attempt bytes were replaced")
	}
	if _, err := os.Stat(filepath.Join(dir, "next-0000")); !os.IsNotExist(err) {
		t.Fatal("torn temporary not removed and fsynced")
	}
}
