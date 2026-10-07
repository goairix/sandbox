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
	"time"

	"github.com/stretchr/testify/require"
)

func TestJournalTaskClosePersistenceFaults(t *testing.T) {
	for phase := 1; phase <= 3; phase++ {
		for _, op := range []string{"open-temp", "write", "file-sync", "rename", "dir-sync"} {
			for _, after := range []bool{false, true} {
				for _, cancelled := range []bool{false, true} {
					t.Run(fmt.Sprintf("phase%d/%s/after=%t/cancel=%t", phase, op, after, cancelled), func(t *testing.T) {
						f := liveSetup(t)
						j := f.create(t)
						require.NoError(t, j.InstallActivation(context.Background(), f.activation))
						e := taskCloseEvidence(t, f, taskCloseClaims(f))
						ctx, cancel := context.WithCancel(context.Background())
						defer cancel()
						injected := errors.New("task close IO boundary")
						current := 0
						seen := false
						j.files.hook = func(got, name string, done bool) error {
							if got == "open-temp" && !done {
								current++
							}
							if current == phase && got == op && done == after {
								seen = true
								if cancelled {
									cancel()
									return nil
								}
								return injected
							}
							return nil
						}
						r, err := j.CloseData(ctx, e)
						require.True(t, seen)
						require.Nil(t, r)
						require.ErrorIs(t, err, ErrJournalUnavailable)
						if cancelled {
							require.ErrorIs(t, err, context.Canceled)
						} else {
							require.ErrorIs(t, err, injected)
						}
						s := j.Status()
						require.True(t, s.Poisoned)
						require.False(t, s.AccountingKnown)
						require.Equal(t, "closed", s.Gate.GateState)
						j.files.hook = nil
						r, err = j.CloseData(context.Background(), e)
						require.Nil(t, r)
						require.ErrorIs(t, err, ErrJournalUnavailable)
						history, err := j.LookupDataClose(context.Background(), e.Context(), e.Digest())
						require.NoError(t, err)
						terminalRenamed := phase == 3 && ((op == "rename" && after) || op == "dir-sync")
						if terminalRenamed {
							require.NotNil(t, history)
							require.Equal(t, "data_closed", history.State)
							require.True(t, j.Status().Poisoned)
						} else if history != nil {
							require.Equal(t, "pending", history.State)
						}
						// Preserve every uncertain byte; valid complete temporaries can be counted
						// cold, but partial/empty temporaries must make recovery fail closed.
						entries, err := os.ReadDir(f.o.Directory)
						require.NoError(t, err)
						retained := map[string][]byte{}
						incomplete := false
						for _, entry := range entries {
							if strings.HasSuffix(entry.Name(), ".tmp") {
								b, err := os.ReadFile(filepath.Join(f.o.Directory, entry.Name()))
								require.NoError(t, err)
								retained[entry.Name()] = b
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
							require.Equal(t, "closed", cold.Status().Gate.GateState)
							require.True(t, cold.Status().AccountingKnown)
							require.Equal(t, uint64(len(retained)), cold.Status().TemporaryFiles)
							r, err = cold.CloseData(context.Background(), e)
							require.Error(t, err)
							require.Nil(t, r)
							require.NoError(t, cold.Close())
						}
						for name, want := range retained {
							got, err := os.ReadFile(filepath.Join(f.o.Directory, name))
							require.NoError(t, err)
							require.Equal(t, want, got)
						}
					})
				}
			}
		}
	}
}

func TestJournalTaskCloseLateAuthentication(t *testing.T) {
	for _, phase := range []int{1, 2, 3} {
		for _, failure := range []string{"clock", "expiry", "cancel"} {
			t.Run(fmt.Sprintf("phase%d/%s", phase, failure), func(t *testing.T) {
				f := liveSetup(t)
				j := f.create(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				require.NoError(t, j.InstallActivation(ctx, f.activation))
				e := taskCloseEvidence(t, f, taskCloseClaims(f))
				current := 0
				j.files.hook = func(op, name string, after bool) error {
					if op == "dir-sync" && after {
						current++
						if current == phase {
							switch failure {
							case "clock":
								f.clock.err = errors.New("clock unavailable")
							case "expiry":
								f.clock.now = e.NotAfter()
							case "cancel":
								cancel()
							}
						}
					}
					return nil
				}
				r, err := j.CloseData(ctx, e)
				require.Error(t, err)
				require.Nil(t, r)
				require.True(t, j.Status().Poisoned)
				require.False(t, j.Status().AccountingKnown)
				require.Equal(t, "closed", j.Status().Gate.GateState)
				j.files.hook = nil
				history, err := j.LookupDataClose(context.Background(), e.Context(), e.Digest())
				require.NoError(t, err)
				require.NotNil(t, history)
				if phase == 3 {
					require.Equal(t, "data_closed", history.State)
				} else {
					require.Equal(t, "pending", history.State)
				}
			})
		}
	}
}

func TestJournalTaskCloseLostAttribution(t *testing.T) {
	f := liveSetup(t)
	j := f.create(t)
	ctx := context.Background()
	require.NoError(t, j.InstallActivation(ctx, f.activation))
	e := taskCloseEvidence(t, f, taskCloseClaims(f))
	_, err := j.CloseData(ctx, e)
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(f.o.Directory, "data-close.json")))
	c := taskCloseClaims(f)
	c.Context.ClaimID = "b1111111-1111-4111-8111-111111111111"
	r, err := j.CloseData(ctx, taskCloseEvidence(t, f, c))
	require.Error(t, err)
	require.Nil(t, r)
	_, err = os.Stat(filepath.Join(f.o.Directory, "data-close.json"))
	require.True(t, os.IsNotExist(err), "missing attribution must not be replaced")
}

func TestJournalTaskCloseConflictingTemporary(t *testing.T) {
	f := liveSetup(t)
	j := f.create(t)
	ctx := context.Background()
	require.NoError(t, j.InstallActivation(ctx, f.activation))
	e := taskCloseEvidence(t, f, taskCloseClaims(f))
	r, err := j.CloseData(ctx, e)
	require.NoError(t, err)
	require.NoError(t, j.Close())
	r.Context.ClaimID = "b1111111-1111-4111-8111-111111111111"
	wire, err := encodeTaskDataClose(*r)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(f.o.Directory, ".data-close.c1111111-1111-4111-8111-111111111111.tmp"), wire, 0600))
	cold, err := OpenClosedJournal(ctx, f.o)
	if cold != nil {
		require.NoError(t, cold.Close())
	}
	require.Error(t, err)
}

// The blocked syscall owner must finish before hook restoration or descriptor
// teardown, even when the test exits early. No unjoined cancellation worker.
func TestJournalTaskCloseCancellationJoin(t *testing.T) {
	f := liveSetup(t)
	j := f.create(t)
	require.NoError(t, j.InstallActivation(context.Background(), f.activation))
	e := taskCloseEvidence(t, f, taskCloseClaims(f))
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	result := make(chan error, 1)
	j.files.hook = func(op, name string, after bool) error {
		if op == "file-sync" && after {
			close(entered)
			<-release
		}
		return nil
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-release:
		default:
			close(release)
		}
		select {
		case <-finished:
			j.files.hook = nil
		case <-time.After(5 * time.Second):
			t.Error("task close worker did not join")
		}
	})
	go func() {
		defer close(finished)
		r, err := j.CloseData(ctx, e)
		if r != nil {
			result <- errors.New("false close ACK")
			return
		}
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("file sync not reached")
	}
	cancel()
	close(release)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("worker join timeout")
	}
	require.ErrorIs(t, <-result, context.Canceled)
	require.True(t, j.Status().Poisoned)
}

// Byte boundaries use actual protected records and cold-scan accounting on the
// live fixture; this is a bounded ~64KiB fixture, not a fleet capacity model.
func fillTaskCloseBytes(t *testing.T, j *Journal, f liveFixture, target int64) {
	t.Helper()
	r, err := recordFromEvidence(f.e)
	require.NoError(t, err)
	current := j.logicalBytes
	for n := 1; current < target; n++ {
		r.Context.CommandID = fmt.Sprintf("23000000-0000-4000-8000-%012x", n)
		b, err := encodeExecJournalRecord(r)
		require.NoError(t, err)
		remaining := target - current
		size := int64(8192)
		if remaining < size {
			size = remaining
		}
		if next := remaining - size; next > 0 && next < int64(len(b)) {
			size -= int64(len(b)) - next
		}
		require.GreaterOrEqual(t, size, int64(len(b)))
		b = append(b, bytes.Repeat([]byte(" "), int(size)-len(b))...)
		require.NoError(t, os.MkdirAll(filepath.Join(f.o.Directory, "commands", "23"), 0700))
		require.NoError(t, os.WriteFile(commandPath(f.o, r.Context.CommandID), b, 0600))
		current += size
	}
	_, err = j.root.Seek(0, 0)
	require.NoError(t, err)
	_, err = j.commands.Seek(0, 0)
	require.NoError(t, err)
	require.NoError(t, j.scanJournalLocked(context.Background()))
	require.Equal(t, target, j.logicalBytes)
	require.Equal(t, target, treeBytes(t, f.o.Directory))
}
func TestJournalTaskCloseCapacity(t *testing.T) {
	for _, peak := range []int64{55705, 55706, 65537} {
		t.Run(fmt.Sprintf("actual-bytes-%d", peak), func(t *testing.T) {
			f := liveSetup(t)
			f.o.MaxBytes = 65536
			j := f.create(t)
			ctx := context.Background()
			require.NoError(t, j.InstallActivation(ctx, f.activation))
			e := taskCloseEvidence(t, f, taskCloseClaims(f))
			pending := TaskDataCloseRecord{Version: 1, State: "pending", Context: e.Context(), TicketDigest: e.Digest(), NotBefore: e.NotBefore(), NotAfter: e.NotAfter()}
			terminal := pending
			terminal.State = "data_closed"
			encode := func(v any) []byte {
				var b bytes.Buffer
				enc := json.NewEncoder(&b)
				enc.SetEscapeHTML(false)
				require.NoError(t, enc.Encode(v))
				return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
			}
			p := encode(pending)
			d := encode(terminal)
			g := j.gate
			g.GateState = "closed"
			gw := encode(g)
			target := peak - int64(len(p)+len(d)+len(gw)) + j.manifestBytes
			fillTaskCloseBytes(t, j, f, target)
			before := j.Status()
			require.True(t, before.Warning)
			writes := 0
			var observedPeak int64
			j.files.hook = func(op, name string, after bool) error {
				if op == "write" && after {
					writes++
					n := treeBytes(t, f.o.Directory)
					if n > observedPeak {
						observedPeak = n
					}
				}
				return nil
			}
			r, err := j.CloseData(ctx, e)
			if peak == 55705 {
				require.NoError(t, err)
				require.NotNil(t, r)
				require.Equal(t, peak, observedPeak)
				require.Equal(t, 3, writes)
			} else {
				require.ErrorIs(t, err, ErrCapacity)
				require.Nil(t, r)
				require.Zero(t, writes)
				require.Equal(t, before, j.Status())
			}
		})
	}
	// Synthetic count boundary checks the actual allocator's two-file reservation
	// without creating65536 files; every admitted write still executes real IO.
	for _, available := range []uint64{1, 2} {
		t.Run(fmt.Sprintf("synthetic-file-slots-%d", available), func(t *testing.T) {
			f := liveSetup(t)
			j := f.create(t)
			ctx := context.Background()
			require.NoError(t, j.InstallActivation(ctx, f.activation))
			e := taskCloseEvidence(t, f, taskCloseClaims(f))
			j.records = maxJournalContentFiles - available - 2
			r, err := j.CloseData(ctx, e)
			if available == 1 {
				require.ErrorIs(t, err, ErrCapacity)
				require.Nil(t, r)
			} else {
				require.NoError(t, err)
				require.NotNil(t, r)
				require.Equal(t, maxJournalContentFiles-1, j.contentFilesLocked())
			}
		})
	}
}
