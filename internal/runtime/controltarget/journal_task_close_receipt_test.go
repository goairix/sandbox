//go:build linux || darwin

package controltarget

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestJournalTaskCloseReceiptHistory(t *testing.T) {
	f := liveSetup(t)
	j := f.create(t)
	ctx := context.Background()
	require.NoError(t, j.InstallActivation(ctx, f.activation))
	e := taskCloseEvidence(t, f, taskCloseClaims(f))
	wire, err := j.SignDataCloseReceipt(ctx, e.Context(), e.Digest(), f.runtimeKey)
	require.Error(t, err)
	require.Nil(t, wire)
	_, err = j.CloseData(ctx, e)
	require.NoError(t, err)
	f.clock.now = f.clock.now.Add(time.Minute) // ticket expired, certificate live
	wire, err = j.SignDataCloseReceipt(ctx, e.Context(), e.Digest(), f.runtimeKey)
	require.NoError(t, err)
	proof, err := f.o.Verifier.VerifyTaskDataClosedReceipt(wire, f.activation.RuntimeCertificate(), e.Context(), e.Digest(), e.NotBefore(), e.NotAfter(), f.clock.now)
	require.NoError(t, err)
	require.Equal(t, "data_closed", proof.State())
	_, wrong, _ := ed25519.GenerateKey(rand.Reader)
	wire, err = j.SignDataCloseReceipt(ctx, e.Context(), e.Digest(), wrong)
	require.Error(t, err)
	require.Nil(t, wire)
	require.NoError(t, j.Close())
	cold, err := OpenClosedJournal(ctx, f.o)
	require.NoError(t, err)
	defer cold.Close()
	wire, err = cold.SignDataCloseReceipt(ctx, e.Context(), e.Digest(), f.runtimeKey)
	require.Error(t, err)
	require.Nil(t, wire)
}
func TestJournalTaskCloseReceiptUncertainTerminal(t *testing.T) {
	f := liveSetup(t)
	j := f.create(t)
	ctx := context.Background()
	require.NoError(t, j.InstallActivation(ctx, f.activation))
	e := taskCloseEvidence(t, f, taskCloseClaims(f))
	phase := 0
	j.files.hook = func(op, name string, after bool) error {
		if op == "open-temp" && !after {
			phase++
		}
		if phase == 3 && op == "dir-sync" && !after {
			return errors.New("terminal rename without durable directory")
		}
		return nil
	}
	_, err := j.CloseData(ctx, e)
	require.Error(t, err)
	j.files.hook = nil
	record, err := j.LookupDataClose(ctx, e.Context(), e.Digest())
	require.NoError(t, err)
	require.Equal(t, "data_closed", record.State)
	wire, err := j.SignDataCloseReceipt(ctx, e.Context(), e.Digest(), f.runtimeKey)
	require.Error(t, err)
	require.Nil(t, wire)
}

type receiptClock func(context.Context) (controlprotocol.ClockObservation, error)

func (f receiptClock) Observe(ctx context.Context) (controlprotocol.ClockObservation, error) {
	return f(ctx)
}
func receiptJoin(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Error("receipt worker did not join")
	}
}
func TestJournalTaskCloseReceiptSerializesPoison(t *testing.T) {
	for _, signFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(signFirst), func(t *testing.T) {
			f := liveSetup(t)
			j := f.create(t)
			ctx := context.Background()
			require.NoError(t, j.InstallActivation(ctx, f.activation))
			a, err := j.AcceptExecution(ctx, f.e)
			require.NoError(t, err)
			require.NoError(t, j.ConsumeStart(ctx, a))
			e := taskCloseEvidence(t, f, taskCloseClaims(f))
			_, err = j.CloseData(ctx, e)
			require.NoError(t, err)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			signDone, terminalDone := make(chan struct{}), make(chan struct{})
			var wire []byte
			var signErr, terminalErr error
			// Both scheduling gates are inside real production operations holding j.mu.
			count := 0
			j.clock = receiptClock(func(ctx context.Context) (controlprotocol.ClockObservation, error) {
				count++
				if signFirst && count == 1 {
					close(entered)
					<-release
				}
				return controlprotocol.ClockObservation{UTC: f.clock.now}, ctx.Err()
			})
			j.files.hook = func(op, name string, after bool) error {
				if op == "file-sync" && !after {
					if !signFirst {
						close(entered)
						<-release
					}
					return errors.New("terminal persistence fault")
				}
				return nil
			}
			signer := func() {
				defer close(signDone)
				wire, signErr = j.SignDataCloseReceipt(ctx, e.Context(), e.Digest(), f.runtimeKey)
			}
			terminal := func() {
				defer close(terminalDone)
				_, terminalErr = j.RecordLocalTerminal(ctx, a, LocalExecutionResult{RootPID: 123, DrainConfirmed: true, Reason: "exited"})
			}
			t.Cleanup(func() { unblock(); receiptJoin(t, signDone); receiptJoin(t, terminalDone); j.files.hook = nil })
			if signFirst {
				go signer()
				receiptJoin(t, entered)
				go terminal()
			} else {
				go terminal()
				receiptJoin(t, entered)
				go signer()
			}
			unblock()
			receiptJoin(t, signDone)
			receiptJoin(t, terminalDone)
			require.Error(t, terminalErr)
			if signFirst {
				require.NoError(t, signErr)
				require.NotEmpty(t, wire)
			} else {
				require.Error(t, signErr)
				require.Nil(t, wire)
			}
			wire, err = j.SignDataCloseReceipt(ctx, e.Context(), e.Digest(), f.runtimeKey)
			require.Error(t, err)
			require.Nil(t, wire)
		})
	}
}
func TestJournalTaskCloseReceiptLateClock(t *testing.T) {
	f := liveSetup(t)
	j := f.create(t)
	ctx := context.Background()
	require.NoError(t, j.InstallActivation(ctx, f.activation))
	e := taskCloseEvidence(t, f, taskCloseClaims(f))
	_, err := j.CloseData(ctx, e)
	require.NoError(t, err)
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelled), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			j.clock = receiptClock(func(context.Context) (controlprotocol.ClockObservation, error) {
				calls++
				if calls == 2 {
					if cancelled {
						cancel()
					} else {
						return controlprotocol.ClockObservation{}, errors.New("late clock")
					}
				}
				return controlprotocol.ClockObservation{UTC: f.clock.now}, nil
			})
			wire, err := j.SignDataCloseReceipt(ctx, e.Context(), e.Digest(), f.runtimeKey)
			require.Error(t, err)
			require.Nil(t, wire)
			require.Equal(t, 2, calls)
		})
	}
}
