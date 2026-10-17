//go:build linux

package etcd

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type detachedTerminationCutoffObservation struct {
	phase          string
	deadline       time.Time
	bounded        bool
	ticketNotAfter time.Time
}

// The observed renewal follows the loader's actual final native guard ACK.
// Original Grant/KeepAlive paths establish both deadlines; no sleep, edited c.deadline,
// copied authority or synthetic response supplies the claimed renewal window.
// Certificate/Sign expose the execution cutoff regression directly. The fixed
// pair retains the original one-second RPC timeout and is only a shorter-window
// control; it cannot independently detect the outer diagnosis context defect.
func TestDetachedRuntimeTerminationNativeRenewalKeepsCapturedCutoff(t *testing.T) {
	for _, mode := range []string{"execute", "diagnose"} {
		t.Run(mode, func(t *testing.T) {
			h := newDetachedTerminationProviderFixture(t, 10*time.Second)
			f, c := h.f, h.f.job.claim
			commitDetachedTerminationFixtureFact(t, h)
			// Source/drain/fact setup already consumed part of the short
			// original Lease. Renew genuinely before either mode's first
			// operation rather than fabricating a longer Claim deadline.
			require.NoError(t, f.b.RenewTaskClaim(f.ctx, c))
			c.mu.Lock()
			firstWindow := c.deadline
			c.mu.Unlock()
			if mode == "diagnose" {
				// A genuinely completed first call leaves only the original
				// reserved intent. The later read captures its own legal window;
				// it must not inherit this earlier operation's shorter window.
				result, err := f.b.terminateTaskRuntimeDetached(f.ctx, f.job, c, h.destination, nil)
				require.ErrorIs(t, err, ErrOutcomeUnknown)
				require.Equal(t, OutcomeUnknown, result.Outcome)
				require.NoError(t, f.b.RenewTaskClaim(f.ctx, c))
			}

			var observations []detachedTerminationCutoffObservation
			h.issuer.observe = func(ctx context.Context, phase string, notAfter time.Time) {
				deadline, bounded := ctx.Deadline()
				observations = append(observations, detachedTerminationCutoffObservation{phase: phase, deadline: deadline, bounded: bounded, ticketNotAfter: notAfter})
			}
			intent, provider, release, err := f.b.namespace.localRuntimeReleaseKeys(f.ref)
			require.NoError(t, err)
			original := f.b.client.KV
			var renewed atomic.Bool
			var captured, extended time.Time
			f.b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
				loaderGuard := len(cmps) == 54 && len(yes) == 1 && len(no) == 0 && yes[0].IsGet() && string(yes[0].KeyBytes()) == f.b.identityKey && len(yes[0].RangeBytes()) == 0
				pair := len(yes) == 5 && len(no) == 5 && yes[2].IsGet() && yes[3].IsGet() && yes[4].IsGet() && string(yes[2].KeyBytes()) == intent && string(yes[3].KeyBytes()) == provider && string(yes[4].KeyBytes()) == release
				if pair && renewed.Load() {
					deadline, bounded := ctx.Deadline()
					observations = append(observations, detachedTerminationCutoffObservation{phase: "actual-fixed-pair-RPC", deadline: deadline, bounded: bounded})
				}
				response, err := original.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
				if loaderGuard && err == nil && response != nil && response.Succeeded && renewed.CompareAndSwap(false, true) {
					require.NoError(t, f.b.operationResponseHeader(response))
					c.mu.Lock()
					captured = c.deadline
					c.mu.Unlock()
					// The nested renewal fence transactions have forty-four
					// comparisons, so neither can masquerade as this boundary.
					require.NoError(t, f.b.RenewTaskClaim(f.ctx, c))
					c.mu.Lock()
					extended = c.deadline
					c.mu.Unlock()
					require.True(t, extended.After(captured), "the original native Lease must actually extend")
				}
				return response, err
			}}
			t.Cleanup(func() { f.b.client.KV = original })
			result, err := f.b.terminateTaskRuntimeDetached(f.ctx, f.job, c, h.destination, nil)
			if mode == "execute" {
				require.ErrorIs(t, err, ErrOutcomeUnknown)
			} else {
				require.NoError(t, err)
				require.NotNil(t, result.Entry)
				require.Equal(t, "reserved", result.Entry.Phase)
				require.True(t, captured.After(firstWindow), "later diagnosis captures a fresh legitimate Claim window")
			}
			require.Equal(t, OutcomeUnknown, result.Outcome)
			require.True(t, renewed.Load())
			for _, observed := range observations {
				t.Logf("actual %s phase=%s captured_claim_deadline=%s renewed_claim_deadline=%s callback_deadline=%s bounded=%t ticket_not_after=%s", mode, observed.phase, captured.Format(time.RFC3339Nano), extended.Format(time.RFC3339Nano), observed.deadline.Format(time.RFC3339Nano), observed.bounded, observed.ticketNotAfter.Format(time.RFC3339Nano))
			}
			expected := 3 // Fixed pair, Certificate and Sign callbacks.
			if mode == "diagnose" {
				expected = 1 // Short RPC control only; no provider/issuer/observer replay.
			}
			require.Len(t, observations, expected)
			for _, observed := range observations {
				require.True(t, observed.bounded)
				require.False(t, observed.deadline.After(captured), "%s must retain this loader's captured Claim cutoff despite renewal", observed.phase)
				if !observed.ticketNotAfter.IsZero() {
					require.False(t, observed.ticketNotAfter.After(captured), "signed ticket must not outlive the captured authority")
				}
			}
			require.EqualValues(t, 1, h.terminates.Load())
			require.Zero(t, h.queries.Load())
		})
	}
}
