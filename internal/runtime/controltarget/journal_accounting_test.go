//go:build linux || darwin

package controltarget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
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
	j, err := OpenClosedJournal(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	s := j.Status()
	if s.Records != 1000 || s.TemporaryFiles != 0 || s.LogicalBytes != want || !s.AccountingKnown {
		t.Fatalf("1000 files status %+v want bytes %d", s, want)
	}
}
