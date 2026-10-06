//go:build linux || darwin

package controltarget

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

func TestJournalCapacity(t *testing.T) {
	o := journalOptionsFixture(t)
	o.MaxBytes = 65536
	j, err := CreateClosedJournal(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	var first controlprotocol.ExecStartEvidence
	warned := false
	for n := 1; n <= 100; n++ {
		r := journalRecordFixture()
		r.Context.CommandID = fmt.Sprintf("22000000-0000-4000-8000-%012x", n)
		e := journalEvidence(t, r, "payload")
		before := j.Status()
		got, err := j.RecordUnknown(context.Background(), e)
		if errors.Is(err, ErrCapacity) {
			if got != nil || j.Status() != before {
				t.Fatal("capacity rejection mutated state")
			}
			if !warned {
				t.Fatal("never warned at 70 percent")
			}
			if retry, err := j.RecordUnknown(context.Background(), first); err != nil || retry == nil {
				t.Fatal("retry rejected", err)
			}
			if found, err := j.Lookup(context.Background(), first.Context().CommandID); err != nil || found == nil {
				t.Fatal("query rejected", err)
			}
			if _, err := j.CloseGate(context.Background(), 2); err != nil {
				t.Fatal("soft cap blocked close", err)
			}
			t.Logf("64KiB authentic record fill: %d records %dB, warning=%t; next %dB would reach85%% and was rejected, original retry/query/close available", before.Records, before.LogicalBytes, before.Warning, 1095)
			return
		}
		if err != nil || got == nil {
			t.Fatalf("record %+v %v", got, err)
		}
		if n == 1 {
			first = e
		}
		s := j.Status()
		if s.Warning != (s.LogicalBytes*100 >= 65536*70) {
			t.Fatal("warning boundary")
		}
		warned = warned || s.Warning
	}
	t.Fatal("no bounded capacity denial")
}

// Add actual valid unknown files to an exact content-byte boundary. Whitespace
// padding is strict-schema legal, bounded to8192B, and included in accounting.
func fillUnknownBytes(t *testing.T, o JournalOptions, current, target int64) {
	t.Helper()
	template := journalRecordFixture()
	for n := 1; current < target; n++ {
		r := template
		r.Context.CommandID = fmt.Sprintf("23000000-0000-4000-8000-%012x", n)
		wire, err := encodeExecJournalRecord(r)
		if err != nil {
			t.Fatal(err)
		}
		remaining := target - current
		size := int64(8192)
		if remaining < size {
			size = remaining
		}
		if next := remaining - size; next > 0 && next < int64(len(wire)) {
			size -= int64(len(wire)) - next
		}
		if size < int64(len(wire)) {
			t.Fatal("boundary fixture remainder too small")
		}
		wire = append(wire, bytes.Repeat([]byte(" "), int(size)-len(wire))...)
		bucket := filepath.Join(o.Directory, "commands", "23")
		if err := os.MkdirAll(bucket, 0700); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, commandPath(o, r.Context.CommandID), wire)
		current += size
	}
}

func TestJournalCapacityExistingAboveSoftLimit(t *testing.T) {
	for _, target := range []int64{55706, 65536 - 361, 65536} {
		t.Run(fmt.Sprint(target), func(t *testing.T) {
			o := journalOptionsFixture(t)
			o.MaxBytes = 65536
			j, err := CreateClosedJournal(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			e := journalEvidence(t, journalRecordFixture(), "payload")
			original, err := j.RecordUnknown(context.Background(), e)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(commandPath(o, e.Context().CommandID))
			if err != nil {
				t.Fatal(err)
			}
			base := j.Status().LogicalBytes
			if err = j.Close(); err != nil {
				t.Fatal(err)
			}
			fillUnknownBytes(t, o, base, target)
			if actual := treeBytes(t, o.Directory); actual != target {
				t.Fatalf("fixture bytes%d target%d", actual, target)
			}
			j, err = OpenClosedJournal(context.Background(), o)
			if target == 65536 {
				if j != nil || !errors.Is(err, ErrCapacity) {
					if j != nil {
						j.Close()
					}
					t.Fatalf("hard budget open %+v %v", j, err)
				}
				after, err := os.ReadFile(commandPath(o, e.Context().CommandID))
				if err != nil || !bytes.Equal(before, after) || treeBytes(t, o.Directory) != target {
					t.Fatal("hard budget erased unknown", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer j.Close()
			s := j.Status()
			if s.LogicalBytes != target || !s.Warning || !s.NewRecordsStopped || !s.AccountingKnown {
				t.Fatalf("capacity status %+v", s)
			}
			if got, err := j.RecordUnknown(context.Background(), e); err != nil || got == nil || *got != *original {
				t.Fatalf("existing retry above85 %+v %v", got, err)
			}
			if got, err := j.Lookup(context.Background(), e.Context().CommandID); err != nil || got == nil || *got != *original {
				t.Fatal("existing lookup", err)
			}
			r := journalRecordFixture()
			r.Context.CommandID = "24000000-0000-4000-8000-000000000001"
			if got, err := j.RecordUnknown(context.Background(), journalEvidence(t, r, "payload")); got != nil || !errors.Is(err, ErrCapacity) {
				t.Fatalf("new above85 %+v %v", got, err)
			}
			if _, err := j.CloseGate(context.Background(), 2); err != nil {
				t.Fatal("hard-limit reserved gate bytes", err)
			}
			after, err := os.ReadFile(commandPath(o, e.Context().CommandID))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("capacity modified existing bytes", err)
			}
			t.Logf("actual valid unknown content=%dB records=%d, original authentic retry/query/closed-gate write available; transient gate bytes=361B", target, s.Records)
		})
	}
}

func TestJournalFixedPointCost(t *testing.T) {
	var baseline []map[string]int
	for _, history := range []int{0, 1000} {
		t.Run(fmt.Sprint(history), func(t *testing.T) {
			j, o := createJournalFixture(t)
			j.Close()
			for n := 1; n <= history; n++ {
				writeHistoryFixture(t, o, fmt.Sprintf("22000000-0000-4000-8000-%012x", n), false)
			}
			// Both fixtures deliberately use the same existing/warm bucket22.
			if err := os.MkdirAll(filepath.Join(o.Directory, "commands", "22"), 0700); err != nil {
				t.Fatal(err)
			}
			var err error
			j, err = OpenClosedJournal(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			defer j.Close()
			counts := map[string]int{}
			j.files.observe = func(op string) { counts[op]++ }
			j.files.hook = func(op, name string, after bool) error {
				if op == "read-page" && !after {
					counts["directory-page"]++
				}
				return nil
			}
			r := journalRecordFixture()
			r.Context.CommandID = "22ffffff-ffff-4fff-8fff-ffffffffffff"
			e := journalEvidence(t, r, "payload")
			operations := []struct {
				name string
				call func() error
				want map[string]int
			}{
				{"new", func() error { _, err := j.RecordUnknown(context.Background(), e); return err }, map[string]int{"openat": 6, "fstat": 4, "write": 1, "file-fsync": 1, "renameat": 1, "directory-fsync": 1, "close": 4}},
				// ReadAll reads1095B in512/384/199B chunks, then EOF: four
				// real reads. Count attempts instead of inventing a single read.
				{"retry", func() error { _, err := j.RecordUnknown(context.Background(), e); return err }, map[string]int{"openat": 2, "fstat": 3, "read": 4, "close": 2}},
				{"lookup", func() error { _, err := j.Lookup(context.Background(), r.Context.CommandID); return err }, map[string]int{"openat": 2, "fstat": 3, "read": 4, "close": 2}},
				{"absent", func() error {
					_, err := j.Lookup(context.Background(), "22eeeeee-eeee-4eee-8eee-eeeeeeeeeeee")
					return err
				}, map[string]int{"openat": 2, "fstat": 1, "close": 1}},
			}
			var observed []map[string]int
			for _, op := range operations {
				clear(counts)
				if err := op.call(); err != nil {
					t.Fatal(err)
				}
				snapshot := map[string]int{}
				for key, n := range counts {
					snapshot[key] = n
				}
				if !reflect.DeepEqual(snapshot, op.want) {
					t.Fatalf("%s actual IO%v want%v", op.name, snapshot, op.want)
				}
				observed = append(observed, snapshot)
				t.Logf("synthetic-history=%d warm-bucket22 %s actual-syscall-attempts=%v; dirscan=0", history, op.name, snapshot)
			}
			if baseline == nil {
				baseline = observed
			} else if !reflect.DeepEqual(baseline, observed) {
				t.Fatalf("history changed point costs %v versus%v", baseline, observed)
			}
			st, err := os.Stat(commandPath(o, r.Context.CommandID))
			if err != nil {
				t.Fatal(err)
			}
			if st.Size() != 1095 {
				t.Fatalf("record bytes%d", st.Size())
			}
			t.Logf("persisted record=%dB, cold setup excluded; new includes public+private second absence read, warm bucket excludes mkdir/fsync directory creation; no Pod/QPS/p99 claim", st.Size())
		})
	}
}
