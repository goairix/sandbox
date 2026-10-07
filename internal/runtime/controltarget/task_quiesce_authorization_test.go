//go:build linux || darwin

package controltarget

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func quiesceResign(t *testing.T, f quiesceFixture, c p.TaskUserQuiescenceTicketClaims) p.TaskUserQuiescenceEvidence {
	t.Helper()
	i, err := f.o.Verifier.VerifyCommandIssuerCertificate(f.issuerWire, f.clock.now)
	require.NoError(t, err)
	w, err := p.SignTaskUserQuiescenceTicket(f.issuerKey, i, c)
	require.NoError(t, err)
	e, err := f.o.Verifier.VerifyTaskUserQuiescenceTicket(w, f.issuerWire, c.Context, f.clock.now)
	require.NoError(t, err)
	return e
}
func quiesceTicket(f quiesceFixture) p.TaskUserQuiescenceTicketClaims {
	return p.TaskUserQuiescenceTicketClaims{Version: 1, Purpose: "task_quiesce_users", Context: f.e.Context(), NotBefore: f.e.NotBefore(), NotAfter: f.e.NotAfter()}
}
func TestTaskQuiesceJournalAuthorization(t *testing.T) {
	for _, kind := range []string{"zero", "receipt-signature", "receipt-digest", "missing-close", "pending-close", "different-task", "cold", "activation", "birth-key", "expired-ticket", "clock", "wrong-attempt"} {
		t.Run(kind, func(t *testing.T) {
			f := quiesceJournalSetup(t)
			c := quiesceTicket(f)
			ctx := context.Background()
			switch kind {
			case "zero":
				f.e = p.TaskUserQuiescenceEvidence{}
			case "receipt-signature":
				f.receipt[len(f.receipt)-5] ^= 1
			case "receipt-digest":
				c.Context.CloseDataReceiptDigest = c.Context.CloseDataTicketDigest
				f.e = quiesceResign(t, f, c)
			case "missing-close":
				require.NoError(t, os.Remove(filepath.Join(f.o.Directory, "data-close.json")))
			case "pending-close":
				old, err := f.j.readTaskCloseLocked(ctx)
				require.NoError(t, err)
				old.State = "pending"
				w, err := encodeTaskDataClose(*old)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(f.o.Directory, "data-close.json"), w, 0600))
			case "different-task":
				c.Context.Current.TaskID = c.Context.Current.CommandID
				c.Context.CloseDataContext.TaskID = c.Context.Current.TaskID
				f.e = quiesceResign(t, f, c)
			case "cold":
				f.j.fresh = false
			case "activation":
				f.j.activation = nil
			case "birth-key":
				f.j.birth.RuntimePublicKey[0] ^= 1
			case "expired-ticket":
				f.clock.now = f.e.NotAfter()
			case "clock":
				f.clock.err = errors.New("clock unavailable")
			case "wrong-attempt":
				_, err := f.j.AcceptUserQuiescence(ctx, f.e, f.receipt)
				require.NoError(t, err)
				c.Context.Current.CommandID = "d1111111-1111-4111-8111-111111111111"
				f.e = quiesceResign(t, f, c)
			}
			writes := 0
			f.j.files.hook = func(op, name string, after bool) error {
				if op == "open-temp" && !after {
					writes++
				}
				return nil
			}
			a, err := f.j.AcceptUserQuiescence(ctx, f.e, f.receipt)
			require.Error(t, err)
			require.Nil(t, a)
			require.Zero(t, writes)
			f.j.files.hook = nil
		})
	}
}
func TestTaskQuiesceJournalHistoricalPrerequisite(t *testing.T) {
	f := quiesceJournalSetup(t)
	f.clock.now = f.clock.now.Add(40 * time.Minute) // old ticket, business TTL and activation expired; original certs still live
	c := quiesceTicket(f)
	c.NotBefore = f.clock.now.Add(-2 * time.Second)
	c.NotAfter = f.clock.now.Add(20 * time.Second)
	f.e = quiesceResign(t, f, c)
	a, err := f.j.AcceptUserQuiescence(context.Background(), f.e, f.receipt)
	require.NoError(t, err)
	require.NotNil(t, a)
	f.clock.now = f.clock.now.Add(time.Minute)
	w, err := f.j.SignUserQuiescenceAccepted(context.Background(), a, f.runtimeKey)
	require.NoError(t, err)
	_, err = f.o.Verifier.VerifyTaskUserQuiescenceAccepted(w, f.activation.RuntimeCertificate(), f.e.Context(), f.e.Digest(), f.e.NotBefore(), f.e.NotAfter(), f.clock.now)
	require.NoError(t, err)
}
func TestTaskQuiesceJournalSigner(t *testing.T) {
	for _, kind := range []string{"nil", "copy", "foreign", "wrong-key", "poison", "cold", "deleted", "changed", "clock-after", "cancel-after", "expired-runtime"} {
		t.Run(kind, func(t *testing.T) {
			f := quiesceJournalSetup(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a, err := f.j.AcceptUserQuiescence(ctx, f.e, f.receipt)
			require.NoError(t, err)
			key := f.runtimeKey
			switch kind {
			case "nil":
				a = nil
			case "copy":
				copied := *a
				a = &copied
			case "foreign":
				g := quiesceJournalSetup(t)
				a, err = g.j.AcceptUserQuiescence(ctx, g.e, g.receipt)
				require.NoError(t, err)
			case "wrong-key":
				_, key, err = ed25519.GenerateKey(rand.Reader)
				require.NoError(t, err)
			case "poison":
				f.j.poison(errors.New("uncertain concurrent persistence"))
			case "cold":
				f.j.fresh = false
			case "deleted":
				require.NoError(t, os.Remove(filepath.Join(f.o.Directory, "users-quiesce.json")))
			case "changed":
				r, err := f.j.LookupUserQuiescence(ctx, f.e.Context(), f.e.Digest())
				require.NoError(t, err)
				r.Context.Current.WorkerID = "worker-3"
				w, err := encodeTaskQuiescence(*r)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(f.o.Directory, "users-quiesce.json"), w, 0600))
			case "expired-runtime":
				f.clock.now = f.clock.now.Add(2 * time.Hour)
			case "clock-after", "cancel-after":
				calls := 0
				f.j.clock = receiptClock(func(ctx context.Context) (p.ClockObservation, error) {
					calls++
					if calls == 2 {
						if kind == "cancel-after" {
							cancel()
						} else {
							return p.ClockObservation{}, errors.New("post-sign clock failure")
						}
					}
					return p.ClockObservation{UTC: f.clock.now}, nil
				})
			}
			w, err := f.j.SignUserQuiescenceAccepted(ctx, a, key)
			require.Error(t, err)
			require.Nil(t, w)
		})
	}
}
func TestTaskQuiesceJournalSignerClose(t *testing.T) {
	f := quiesceJournalSetup(t)
	ctx := context.Background()
	a, err := f.j.AcceptUserQuiescence(ctx, f.e, f.receipt)
	require.NoError(t, err)
	entered, release, signDone, closeDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	calls := 0
	f.j.clock = receiptClock(func(ctx context.Context) (p.ClockObservation, error) {
		calls++
		if calls == 1 {
			close(entered)
			<-release
		}
		return p.ClockObservation{UTC: f.clock.now}, nil
	})
	var wire []byte
	var signErr, closeErr error
	go func() { defer close(signDone); wire, signErr = f.j.SignUserQuiescenceAccepted(ctx, a, f.runtimeKey) }()
	t.Cleanup(func() { unblock(); receiptJoin(t, signDone); receiptJoin(t, closeDone) })
	receiptJoin(t, entered)
	go func() { defer close(closeDone); closeErr = f.j.Close() }()
	select {
	case <-closeDone:
		t.Error("journal closed while signing held its borrow")
	default:
	}
	unblock()
	receiptJoin(t, signDone)
	receiptJoin(t, closeDone)
	require.NoError(t, signErr)
	require.NotEmpty(t, wire)
	require.NoError(t, closeErr)
	wire, err = f.j.SignUserQuiescenceAccepted(ctx, a, f.runtimeKey)
	require.Error(t, err)
	require.Nil(t, wire)
}
