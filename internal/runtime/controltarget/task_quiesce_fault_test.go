//go:build linux || darwin

package controltarget

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTaskQuiesceJournalFaults(t *testing.T) {
	for _, op := range []string{"open-temp", "write", "file-sync", "rename", "dir-sync"} {
		for _, after := range []bool{false, true} {
			for _, cancelled := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/after=%t/cancel=%t", op, after, cancelled), func(t *testing.T) {
					f := quiesceJournalSetup(t)
					j := f.j
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					injected := errors.New("quiescence IO fault")
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
					a, err := j.AcceptUserQuiescence(ctx, f.e, f.receipt)
					require.True(t, seen)
					require.Nil(t, a)
					require.ErrorIs(t, err, ErrJournalUnavailable)
					if cancelled {
						require.ErrorIs(t, err, context.Canceled)
					} else {
						require.ErrorIs(t, err, injected)
					}
					require.True(t, j.usersClosed)
					require.True(t, j.Status().Poisoned)
					require.False(t, j.Status().AccountingKnown)
					j.files.hook = nil
					r, err := j.LookupUserQuiescence(context.Background(), f.e.Context(), f.e.Digest())
					require.NoError(t, err)
					if r != nil {
						require.Equal(t, "pending", r.State)
					}
					_, err = j.AcceptUserQuiescence(context.Background(), f.e, f.receipt)
					require.Error(t, err)
					entries, err := os.ReadDir(f.o.Directory)
					require.NoError(t, err)
					retained := map[string][]byte{}
					incomplete := false
					for _, e := range entries {
						if taskQuiescenceTempName(e.Name()) {
							b, err := os.ReadFile(filepath.Join(f.o.Directory, e.Name()))
							require.NoError(t, err)
							retained[e.Name()] = b
							if len(b) == 0 {
								incomplete = true
							}
						}
					}
					require.NoError(t, j.Close())
					cold, err := OpenClosedJournal(context.Background(), f.o)
					if incomplete {
						require.Error(t, err)
						require.Nil(t, cold)
					} else {
						require.NoError(t, err)
						require.Equal(t, uint64(len(retained)), cold.Status().TemporaryFiles)
						if r != nil || len(retained) > 0 {
							require.True(t, cold.usersClosed)
						}
						_, err = cold.AcceptUserQuiescence(context.Background(), f.e, f.receipt)
						require.Error(t, err)
						require.NoError(t, cold.Close())
					}
					for n, want := range retained {
						got, err := os.ReadFile(filepath.Join(f.o.Directory, n))
						require.NoError(t, err)
						require.Equal(t, want, got)
					}
				})
			}
		}
	}
}
func TestTaskQuiesceJournalLate(t *testing.T) {
	for _, kind := range []string{"expiry", "clock", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			f := quiesceJournalSetup(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.j.files.hook = func(op, name string, after bool) error {
				if op == "dir-sync" && after {
					switch kind {
					case "expiry":
						f.clock.now = f.e.NotAfter()
					case "clock":
						f.clock.err = errors.New("no clock")
					case "cancel":
						cancel()
					}
				}
				return nil
			}
			a, err := f.j.AcceptUserQuiescence(ctx, f.e, f.receipt)
			require.Error(t, err)
			require.Nil(t, a)
			require.True(t, f.j.Status().Poisoned)
			f.j.files.hook = nil
			r, err := f.j.LookupUserQuiescence(context.Background(), f.e.Context(), f.e.Digest())
			require.NoError(t, err)
			require.Equal(t, "pending", r.State)
		})
	}
}
func TestTaskQuiesceCapacity(t *testing.T) {
	for _, kind := range []string{"bytes", "slots", "allowed"} {
		t.Run(kind, func(t *testing.T) {
			f := quiesceJournalSetup(t)
			j := f.j
			r := TaskUserQuiescenceRecord{Version: 1, State: "pending", Context: f.e.Context(), TicketDigest: f.e.Digest(), NotBefore: f.e.NotBefore(), NotAfter: f.e.NotAfter()}
			pending, err := encodeTaskQuiescence(r)
			require.NoError(t, err)
			r.State = "users_quiesced"
			r.ExecutionSetDigest = strings.Repeat("f", 64)
			r.RegisteredCount = 64
			r.NeverSpawnedCount = 32
			r.LocalTerminalCount = 32
			terminal, err := encodeTaskQuiescence(r)
			require.NoError(t, err)
			peak := j.logicalBytes + int64(len(pending)) + int64(len(terminal))
			originalRecords := j.records
			if kind == "slots" {
				j.temporaryFiles = maxJournalContentFiles - j.contentFilesLocked() - 1
			} else {
				j.maxBytes = peak * 100 / 85
				if kind == "allowed" {
					j.maxBytes++
				}
			}
			writes := 0
			j.files.hook = func(op, name string, after bool) error {
				if op == "open-temp" && !after {
					writes++
				}
				return nil
			}
			a, err := j.AcceptUserQuiescence(context.Background(), f.e, f.receipt)
			if kind == "allowed" {
				require.NoError(t, err)
				require.NotNil(t, a)
				require.Equal(t, 1, writes)
				require.Equal(t, originalRecords, j.Status().Records)
			} else {
				require.ErrorIs(t, err, ErrCapacity)
				require.Nil(t, a)
				require.Zero(t, writes)
				require.False(t, j.usersClosed)
			}
			j.files.hook = nil
		})
	}
}
func TestTaskQuiesceHistory(t *testing.T) {
	for _, state := range []string{"pending", "users_quiesced"} {
		for _, temp := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/temp=%t", state, temp), func(t *testing.T) {
				f := quiesceJournalSetup(t)
				j := f.j
				before := j.Status()
				require.NoError(t, j.Close())
				r := TaskUserQuiescenceRecord{Version: 1, State: state, Context: f.e.Context(), TicketDigest: f.e.Digest(), NotBefore: f.e.NotBefore(), NotAfter: f.e.NotAfter()}
				if state == "users_quiesced" {
					r.ExecutionSetDigest = digestJournalBytes([]byte("[]"))
				}
				w, err := encodeTaskQuiescence(r)
				require.NoError(t, err)
				// Actual read limit is new-record-only. Canonical JSON is smaller, but retained
				// whitespace at the exact bound must be read and charged without truncation.
				w = append(w, bytes.Repeat([]byte(" "), 16384-len(w))...)
				name := "users-quiesce.json"
				if temp {
					name = ".users-quiesce.d1111111-1111-4111-8111-111111111111.tmp"
				}
				require.NoError(t, os.WriteFile(filepath.Join(f.o.Directory, name), w, 0600))
				f.clock.now = f.clock.now.Add(2 * time.Hour)
				cold, err := OpenClosedJournal(context.Background(), f.o)
				require.NoError(t, err)
				require.True(t, cold.usersClosed)
				require.Equal(t, before.LogicalBytes+int64(len(w)), cold.Status().LogicalBytes)
				require.Equal(t, before.Records, cold.Status().Records)
				rptr, err := cold.LookupUserQuiescence(context.Background(), f.e.Context(), f.e.Digest())
				require.NoError(t, err)
				if temp {
					require.Nil(t, rptr)
				} else {
					require.Equal(t, state, rptr.State)
				}
				require.NoError(t, cold.Close())
				require.NoError(t, os.WriteFile(filepath.Join(f.o.Directory, name), append(w, ' '), 0600))
				cold, err = OpenClosedJournal(context.Background(), f.o)
				require.Error(t, err)
				require.Nil(t, cold)
			})
		}
	}
}
