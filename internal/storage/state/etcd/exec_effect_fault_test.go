package etcd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// All fault hooks here run synchronously on the producer call stack. In
// particular, arbitration executes before the intercepted full commit reaches
// etcd. There are no background fault workers to outlive Fatal/Goexit.
func TestPrepareExecEffectUnknown(t *testing.T) {
	for _, fault := range []string{"commit reply lost", "begin reply lost", "grant reply lost", "resolver wins", "resolve reply lost"} {
		t.Run(fault, func(t *testing.T) {
			f := newExecEffectFixture(t)
			b := f.b
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			effectKey, err := b.namespace.execEffectKey(f.cap.Reference())
			require.NoError(t, err)
			originalKV := b.client.KV
			defer func() { b.client.KV = originalKV }()
			var saved ExecEffectReference
			if fault == "grant reply lost" {
				lease.grant = func(r *clientv3.LeaseGrantResponse, e error) (*clientv3.LeaseGrantResponse, error) {
					require.NoError(t, e)
					saved = f.cap.execDraft.reference
					require.NoError(t, saved.Validate())
					require.NotEmpty(t, f.cap.execDraft.recordValue)
					return r, context.DeadlineExceeded
				}
			} else {
				b.client.KV = &faultKV{KV: originalKV, match: func(ops []clientv3.Op) bool {
					if fault == "begin reply lost" || fault == "resolve reply lost" {
						return len(ops) == 1 && ops[0].IsPut() && strings.Contains(string(ops[0].KeyBytes()), "/attempts/")
					}
					return putsKey(effectKey)(ops)
				}, before: func() {
					saved = f.cap.execDraft.reference
					require.NoError(t, saved.Validate())
					require.NotEmpty(t, f.cap.execDraft.recordValue)
					if fault == "resolver wins" {
						out, e := b.ResolveStage(context.Background(), saved.Stage)
						require.NoError(t, e)
						require.Equal(t, OutcomeAborted, out)
					}
				}, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
					require.NoError(t, e)
					if fault == "resolver wins" {
						require.False(t, r.Succeeded)
						return r, nil
					}
					require.True(t, r.Succeeded)
					return nil, context.DeadlineExceeded
				}}
			}
			first, err := b.PrepareExecEffect(context.Background(), f.cap, execEffectRequest())
			require.Nil(t, first.Prepared)
			if fault == "resolver wins" {
				require.NoError(t, err)
				require.Equal(t, OutcomeAborted, first.Outcome)
			} else {
				require.ErrorIs(t, err, ErrOutcomeUnknown)
				require.Equal(t, OutcomeUnknown, first.Outcome)
			}
			require.Equal(t, saved, first.Reference)
			require.Equal(t, int64(1), lease.grants.Load())
			require.Equal(t, 1, f.provider.signCalls)
			if fault == "commit reply lost" {
				require.Zero(t, lease.revokes.Load())
				require.NotNil(t, f.cap.execDraft.stage)
			}
			original, err := f.raw.Get(context.Background(), effectKey)
			require.NoError(t, err)
			if fault == "commit reply lost" {
				require.Len(t, original.Kvs, 1)
			} else {
				require.Empty(t, original.Kvs)
			}
			if first.Outcome == OutcomeUnknown && fault != "commit reply lost" {
				history, e := b.LoadExecEffect(context.Background(), first.Reference)
				require.NoError(t, e)
				require.Nil(t, history)
				require.Equal(t, OutcomeUnknown, f.cap.execDraft.outcome, "absent history cannot decide an unknown attempt")
			}
			b.client.KV = originalKV
			lease.grant = nil
			if fault == "resolve reply lost" {
				b.client.KV = &faultKV{KV: originalKV, match: func(ops []clientv3.Op) bool { return len(ops) == 1 && ops[0].IsPut() }, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
					require.NoError(t, e)
					return nil, context.DeadlineExceeded
				}}
				unresolved, e := b.PrepareExecEffect(context.Background(), f.cap, execEffectRequest())
				require.ErrorIs(t, e, ErrOutcomeUnknown)
				require.Equal(t, OutcomeUnknown, unresolved.Outcome)
				require.Nil(t, unresolved.Prepared)
				require.Equal(t, saved, unresolved.Reference)
				b.client.KV = originalKV
			}
			retry, err := b.PrepareExecEffect(context.Background(), f.cap, execEffectRequest())
			require.NoError(t, err)
			require.Equal(t, saved, retry.Reference)
			require.Equal(t, int64(1), lease.grants.Load())
			require.Equal(t, 1, f.provider.signCalls)
			if fault == "commit reply lost" {
				require.Equal(t, OutcomeCommitted, retry.Outcome)
				require.NotNil(t, retry.Prepared)
				got, e := f.raw.Get(context.Background(), effectKey)
				require.NoError(t, e)
				require.Equal(t, original.Kvs, got.Kvs)
			} else {
				require.Equal(t, OutcomeAborted, retry.Outcome)
				require.Nil(t, retry.Prepared)
			}
			final, e := b.PrepareExecEffect(context.Background(), f.cap, execEffectRequest())
			require.NoError(t, e)
			require.Equal(t, retry.Reference, final.Reference)
			require.Equal(t, retry.Outcome, final.Outcome)
			require.Equal(t, int64(1), lease.grants.Load())
		})
	}
}

func TestPrepareExecEffectOriginalFences(t *testing.T) {
	for _, phase := range []string{"before commit", "after commit", "cached"} {
		t.Run(phase, func(t *testing.T) {
			for _, family := range []string{"placement", "control", "owner", "fence", "index", "guard", "token", "receipt", "issuer"} {
				t.Run(family, func(t *testing.T) {
					for _, defect := range []string{"value", "lease", "recreation"} {
						t.Run(defect, func(t *testing.T) {
							f := newExecEffectFixture(t)
							b := f.b
							ctx := context.Background()
							var addedLease clientv3.LeaseID
							change := func() {
								keys := map[string]string{"placement": f.keys[0], "control": f.keys[1], "owner": f.keys[2], "fence": f.keys[3], "index": f.keys[4], "guard": f.cap.guardKey, "token": f.cap.tokenKey, "receipt": f.cap.receiptKey, "issuer": f.cap.execDraft.issuerKey}
								key := keys[family]
								got, e := f.raw.Get(ctx, key)
								require.NoError(t, e)
								require.Len(t, got.Kvs, 1)
								kv := got.Kvs[0]
								value := string(kv.Value)
								options := []clientv3.OpOption{}
								if kv.Lease != 0 {
									options = append(options, clientv3.WithLease(clientv3.LeaseID(kv.Lease)))
								}
								switch defect {
								case "value":
									value += " "
								case "lease":
									l, e := f.raw.Grant(ctx, 30)
									require.NoError(t, e)
									addedLease = l.ID
									options = []clientv3.OpOption{clientv3.WithLease(l.ID)}
								case "recreation":
									_, e = f.raw.Delete(ctx, key)
									require.NoError(t, e)
								}
								_, e = f.raw.Put(ctx, key, value, options...)
								require.NoError(t, e)
							}
							defer func() {
								if addedLease != 0 {
									cleanup, cancel := context.WithTimeout(ctx, 5*time.Second)
									defer cancel()
									_, _ = f.raw.Revoke(cleanup, addedLease)
								}
							}()
							effectKey, e := b.namespace.execEffectKey(f.cap.Reference())
							require.NoError(t, e)
							if phase == "cached" {
								r, e := b.PrepareExecEffect(ctx, f.cap, execEffectRequest())
								require.NoError(t, e)
								require.NotNil(t, r.Prepared)
								change()
							} else {
								b.client.KV = &faultKV{KV: b.client.KV, match: putsKey(effectKey), before: func() {
									if phase == "before commit" {
										change()
									}
								}, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
									if phase == "after commit" {
										require.NoError(t, e)
										require.True(t, r.Succeeded)
										change()
									}
									return r, e
								}}
							}
							result, e := b.PrepareExecEffect(ctx, f.cap, execEffectRequest())
							require.Error(t, e)
							require.Nil(t, result.Prepared)
							if phase == "before commit" {
								require.Equal(t, OutcomeUnknown, result.Outcome)
								require.ErrorIs(t, e, ErrConflict)
								got, e := f.raw.Get(ctx, effectKey)
								require.NoError(t, e)
								require.Empty(t, got.Kvs)
							} else {
								require.Equal(t, OutcomeCommitted, result.Outcome)
							}
						})
					}
				})
			}
		})
	}
}

func TestPrepareExecEffectDeadline(t *testing.T) {
	for _, fault := range []string{"signer caller cancel", "signer parent cancel", "signer deadline", "stage deadline", "postcommit deadline", "postcommit clock", "postcommit uncertainty", "final clock", "cached expired", "lost"} {
		t.Run(fault, func(t *testing.T) {
			f := newExecEffectFixture(t)
			b := f.b
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			parent, parentCancel := context.WithCancel(context.Background())
			defer parentCancel()
			f.cap.parentCtx = parent
			effectKey, e := b.namespace.execEffectKey(f.cap.Reference())
			require.NoError(t, e)
			if fault == "signer caller cancel" || fault == "signer parent cancel" || fault == "signer deadline" {
				f.provider.sign = func(bound context.Context, digest string, c controlprotocol.ExecStartTicketClaims) ([]byte, error) {
					deadline, ok := bound.Deadline()
					require.True(t, ok)
					require.False(t, deadline.After(f.cap.execDraft.deadline))
					if fault == "signer caller cancel" {
						cancel()
					} else if fault == "signer parent cancel" {
						parentCancel()
					} else {
						f.cap.deadline = time.Now().Add(-time.Second)
					}
					if fault != "signer deadline" {
						select {
						case <-bound.Done():
						case <-time.After(time.Second):
							t.Fatal("provider context did not cancel")
						}
					}
					return controlprotocol.SignExecStartTicket(f.provider.key, f.provider.identity, c)
				}
			}
			if fault == "stage deadline" {
				b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
					return len(ops) == 1 && ops[0].IsPut() && strings.Contains(string(ops[0].KeyBytes()), "/attempts/")
				}, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
					f.cap.deadline = time.Now().Add(-time.Second)
					return r, e
				}}
			}
			if strings.HasPrefix(fault, "postcommit") {
				b.client.KV = &faultKV{KV: b.client.KV, match: putsKey(effectKey), after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
					require.NoError(t, e)
					require.True(t, r.Succeeded)
					if fault == "postcommit deadline" {
						f.cap.deadline = time.Now().Add(-time.Second)
					} else {
						b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
							if fault == "postcommit clock" {
								return ClockObservation{}, errors.New("clock lost")
							}
							return ClockObservation{UTC: f.now, Uncertainty: 2 * time.Second}, nil
						})
					}
					return r, e
				}}
			}
			if fault == "final clock" {
				b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
					return len(ops) == 1 && ops[0].IsGet() && string(ops[0].KeyBytes()) == b.identityKey
				}, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
					b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
						return ClockObservation{UTC: f.now.Add(time.Minute)}, nil
					})
					return r, e
				}}
			}
			if fault == "cached expired" || fault == "lost" {
				r, e := b.PrepareExecEffect(ctx, f.cap, execEffectRequest())
				require.NoError(t, e)
				require.NotNil(t, r.Prepared)
				if fault == "lost" {
					f.cap.lost = true
				} else {
					b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
						return ClockObservation{UTC: f.now.Add(time.Minute)}, nil
					})
				}
			}
			result, e := b.PrepareExecEffect(ctx, f.cap, execEffectRequest())
			require.Error(t, e)
			require.Nil(t, result.Prepared)
			if strings.HasPrefix(fault, "postcommit") || fault == "final clock" || fault == "cached expired" || fault == "lost" {
				require.Equal(t, OutcomeCommitted, result.Outcome)
			} else {
				require.Equal(t, OutcomeUnknown, result.Outcome)
			}
		})
	}
}

func TestPrepareExecEffectDeadlineRealDelay(t *testing.T) {
	for _, phase := range []string{"signer", "stage", "committed reply"} {
		t.Run(phase, func(t *testing.T) {
			f := newExecEffectFixture(t)
			f.cap.deadline = time.Now().Add(2300 * time.Millisecond)
			old := f.cap.deadline
			lease := &creationFaultLease{Lease: f.b.client.Lease}
			f.b.client.Lease = lease
			delay := func() {
				wait := time.Until(old) + 20*time.Millisecond
				if wait > 0 {
					time.Sleep(wait)
				}
			}
			effectKey, e := f.b.namespace.execEffectKey(f.cap.Reference())
			require.NoError(t, e)
			if phase == "signer" {
				f.provider.sign = func(ctx context.Context, _ string, c controlprotocol.ExecStartTicketClaims) ([]byte, error) {
					deadline, ok := ctx.Deadline()
					require.True(t, ok)
					require.Equal(t, old, deadline)
					<-ctx.Done()
					return controlprotocol.SignExecStartTicket(f.provider.key, f.provider.identity, c)
				}
			} else {
				f.b.client.KV = &faultKV{KV: f.b.client.KV, match: func(ops []clientv3.Op) bool {
					if phase == "stage" {
						return len(ops) == 1 && ops[0].IsPut() && strings.Contains(string(ops[0].KeyBytes()), "/attempts/")
					}
					return putsKey(effectKey)(ops)
				}, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
					require.NoError(t, e)
					require.True(t, r.Succeeded)
					delay()
					return r, e
				}}
			}
			result, e := f.b.PrepareExecEffect(context.Background(), f.cap, execEffectRequest())
			require.Error(t, e)
			require.Nil(t, result.Prepared)
			require.Equal(t, old, f.cap.execDraft.deadline)
			require.Equal(t, old, f.cap.deadline)
			if phase == "committed reply" {
				require.Equal(t, OutcomeCommitted, result.Outcome)
				require.Error(t, result.GuardCleanupError)
				require.Zero(t, lease.revokes.Load(), "expired context must not dispatch cleanup")
			} else {
				require.Equal(t, OutcomeUnknown, result.Outcome)
			}
		})
	}
}

// Sampling m before the delayed Observe would add the observation delay to
// the ticket window. The independently measured return bounds detect that bug.
func TestPrepareExecEffectDeadlineObservationAnchor(t *testing.T) {
	f := newExecEffectFixture(t)
	f.cap.deadline = time.Now().Add(5 * time.Second)
	calls := 0
	var returned time.Time
	var elapsed time.Duration
	f.b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
		calls++
		if calls == 2 {
			start := time.Now()
			time.Sleep(200 * time.Millisecond)
			returned = time.Now()
			elapsed = time.Since(start)
		}
		return ClockObservation{UTC: f.now}, nil
	})
	result, err := f.b.PrepareExecEffect(context.Background(), f.cap, execEffectRequest())
	require.NoError(t, err)
	require.NotNil(t, result.Prepared)
	upper := f.now.Add(elapsed).Add(-time.Second).Add(f.cap.execDraft.deadline.Sub(returned))
	require.WithinDuration(t, upper, f.cap.execDraft.claims.NotAfter, 20*time.Millisecond)
	require.Less(t, f.cap.execDraft.claims.NotAfter.Sub(f.cap.execDraft.claims.NotBefore), 7*time.Second)
}

func TestPrepareExecEffectAbortRechecksCaller(t *testing.T) {
	for _, phase := range []string{"resolve", "cleanup"} {
		t.Run(phase, func(t *testing.T) {
			f := newExecEffectFixture(t)
			b := f.b
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			native := b.client.KV
			if phase == "resolve" {
				b.client.KV = &faultKV{KV: native, match: func(ops []clientv3.Op) bool {
					return len(ops) == 1 && ops[0].IsPut() && strings.Contains(string(ops[0].KeyBytes()), "/attempts/")
				}, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
					require.NoError(t, e)
					return nil, context.DeadlineExceeded
				}}
				first, err := b.PrepareExecEffect(ctx, f.cap, execEffectRequest())
				require.ErrorIs(t, err, ErrOutcomeUnknown)
				require.Equal(t, OutcomeUnknown, first.Outcome)
				b.client.KV = &faultKV{KV: native, match: func(ops []clientv3.Op) bool { return len(ops) == 1 && ops[0].IsPut() }, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
					require.NoError(t, e)
					require.True(t, r.Succeeded)
					cancel()
					return r, e
				}}
			} else {
				effectKey, err := b.namespace.execEffectKey(f.cap.Reference())
				require.NoError(t, err)
				b.client.KV = &faultKV{KV: native, match: putsKey(effectKey), before: func() {
					out, e := b.ResolveStage(ctx, f.cap.execDraft.reference.Stage)
					require.NoError(t, e)
					require.Equal(t, OutcomeAborted, out)
				}}
				originalLease := b.client.Lease
				b.client.Lease = &creationFaultLease{Lease: originalLease, revoke: func(bound context.Context, id clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
					r, e := originalLease.Revoke(bound, id)
					cancel()
					return r, e
				}}
			}
			result, err := b.PrepareExecEffect(ctx, f.cap, execEffectRequest())
			require.ErrorIs(t, err, context.Canceled)
			require.Equal(t, OutcomeAborted, result.Outcome)
			require.Nil(t, result.Prepared)
		})
	}
}

func TestPrepareExecEffectCommittedRetryDiagnostics(t *testing.T) {
	for _, fault := range []string{"nil caller", "cancelled caller", "cancelled parent", "old deadline", "lost"} {
		t.Run(fault, func(t *testing.T) {
			f := newExecEffectFixture(t)
			parent, parentCancel := context.WithCancel(context.Background())
			defer parentCancel()
			f.cap.parentCtx = parent
			first, err := f.b.PrepareExecEffect(context.Background(), f.cap, execEffectRequest())
			require.NoError(t, err)
			require.NotNil(t, first.Prepared)
			ctx := context.Background()
			switch fault {
			case "nil caller":
				ctx = nil
			case "cancelled caller":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "cancelled parent":
				parentCancel()
			case "old deadline":
				f.cap.execDraft.deadline = time.Now().Add(-time.Second)
			case "lost":
				f.cap.lost = true
			}
			denied, err := f.b.PrepareExecEffect(ctx, f.cap, execEffectRequest())
			require.Error(t, err)
			require.Nil(t, denied.Prepared)
			require.Equal(t, OutcomeCommitted, denied.Outcome)
			require.Equal(t, first.Reference, denied.Reference)
			require.Equal(t, 1, f.provider.signCalls)
		})
	}
}
