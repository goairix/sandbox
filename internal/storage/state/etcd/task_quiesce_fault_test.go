package etcd

import (
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"strings"
	"testing"
	"time"
)

func quiescenceNativeUnknown(t *testing.T) {
	for _, fault := range []string{"registration", "grant", "begin", "commit", "resolver", "resolve-reply", "cleanup"} {
		t.Run(fault, func(t *testing.T) {
			f := newQuiescenceNativeFixture(t)
			b := f.b
			original := b.client.KV
			defer func() { b.client.KV = original }()
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			key, e := b.namespace.taskQuiescenceKey(f.c.reference.Task)
			require.NoError(t, e)
			issuerKey, e := b.namespace.commandIssuerKey(f.issuer.old.identity.CertificateID())
			require.NoError(t, e)
			if fault == "grant" {
				lease.grant = func(r *clientv3.LeaseGrantResponse, e error) (*clientv3.LeaseGrantResponse, error) {
					require.NoError(t, e)
					return r, context.DeadlineExceeded
				}
			} else if fault == "cleanup" {
				lease.revoke = func(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
					r, e := lease.Lease.Revoke(ctx, id)
					require.NoError(t, e)
					return r, errors.New("cleanup reply lost")
				}
			} else {
				b.client.KV = &faultKV{KV: original, match: func(ops []clientv3.Op) bool {
					if fault == "registration" {
						return putsKey(issuerKey)(ops)
					}
					if fault == "begin" || fault == "resolve-reply" {
						return len(ops) == 1 && ops[0].IsPut() && strings.Contains(string(ops[0].KeyBytes()), "/attempts/")
					}
					return putsKey(key)(ops)
				}, before: func() {
					if fault == "resolver" {
						out, e := b.ResolveStage(f.ctx, f.c.quiescenceDraft.reference.Stage)
						require.NoError(t, e)
						require.Equal(t, OutcomeAborted, out)
					}
				}, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
					require.NoError(t, e)
					if fault == "resolver" {
						require.False(t, r.Succeeded)
						return r, nil
					}
					require.True(t, r.Succeeded)
					return nil, context.DeadlineExceeded
				}}
			}
			first, e := b.PrepareTaskQuiescence(f.ctx, f.c, f.target.destination)
			d := f.c.quiescenceDraft
			require.NotNil(t, d)
			if fault == "cleanup" {
				require.NoError(t, e)
				require.Equal(t, OutcomeCommitted, first.Outcome)
				require.NotNil(t, first.Prepared)
				require.ErrorContains(t, first.GuardCleanupError, "cleanup reply lost")
			} else if fault == "resolver" {
				require.NoError(t, e)
				require.Equal(t, OutcomeAborted, first.Outcome)
				require.Nil(t, first.Prepared)
			} else {
				require.ErrorIs(t, e, ErrOutcomeUnknown)
				require.Equal(t, OutcomeUnknown, first.Outcome)
				require.Nil(t, first.Prepared)
			}
			saved, command, deadline := first.Reference, d.commandID, d.deadline
			certificate := append([]byte(nil), d.certificate...)
			b.client.KV = original
			lease.grant = nil
			lease.revoke = nil
			if fault == "registration" {
				require.Zero(t, f.issuer.calls)
				f.issuer.old.wire = nil
				require.NotEmpty(t, certificate)
			} else {
				require.NoError(t, saved.Validate())
				require.Equal(t, int64(1), lease.grants.Load())
			}
			if fault == "resolve-reply" {
				b.client.KV = &faultKV{KV: original, match: func(ops []clientv3.Op) bool {
					return len(ops) == 1 && ops[0].IsPut() && strings.HasSuffix(string(ops[0].KeyBytes()), "/receipt")
				}, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
					require.NoError(t, e)
					return nil, context.DeadlineExceeded
				}}
				retry, e := b.PrepareTaskQuiescence(f.ctx, f.c, f.target.destination)
				require.ErrorIs(t, e, ErrOutcomeUnknown)
				require.Equal(t, saved, retry.Reference)
				require.Nil(t, retry.Prepared)
				b.client.KV = original
			}
			retry, e := b.PrepareTaskQuiescence(f.ctx, f.c, f.target.destination)
			require.NoError(t, e)
			require.Equal(t, command, d.commandID)
			require.Equal(t, deadline, d.deadline)
			require.Equal(t, certificate, d.certificate)
			require.Equal(t, 1, f.issuer.old.certCalls)
			if fault == "commit" || fault == "registration" || fault == "cleanup" {
				require.Equal(t, OutcomeCommitted, retry.Outcome)
				require.NotNil(t, retry.Prepared)
				entry, e := b.LoadTaskQuiescence(f.ctx, f.c.reference.Task)
				require.NoError(t, e)
				require.Equal(t, retry.Reference, entry.Reference)
			} else {
				require.Equal(t, OutcomeAborted, retry.Outcome)
				require.Nil(t, retry.Prepared)
				entry, e := b.LoadTaskQuiescence(f.ctx, f.c.reference.Task)
				require.NoError(t, e)
				require.Nil(t, entry)
			}
			if fault != "registration" {
				require.Equal(t, saved, retry.Reference)
			}
			require.Equal(t, int64(1), lease.grants.Load())
			require.Equal(t, 1, f.issuer.calls)
		})
	}
}
func quiescenceNativePostcommit(t *testing.T) {
	for _, fault := range []string{"cancel", "deadline", "clock", "fence"} {
		t.Run(fault, func(t *testing.T) {
			f := newQuiescenceNativeFixture(t)
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			key, e := f.b.namespace.taskQuiescenceKey(f.c.reference.Task)
			require.NoError(t, e)
			original := f.b.client.KV
			defer func() { f.b.client.KV = original }()
			f.b.client.KV = &faultKV{KV: original, match: putsKey(key), after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
				require.NoError(t, e)
				require.True(t, r.Succeeded)
				switch fault {
				case "cancel":
					cancel()
				case "deadline":
					f.c.quiescenceDraft.deadline = time.Now().Add(-time.Second)
				case "clock":
					f.b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
						return ClockObservation{UTC: f.now.Add(time.Minute)}, nil
					})
				case "fence":
					_, e = f.raw.Put(f.ctx, f.c.fences[5].key, f.c.fences[5].value)
					require.NoError(t, e)
				}
				return r, nil
			}}
			r, e := f.b.PrepareTaskQuiescence(ctx, f.c, f.target.destination)
			require.Error(t, e)
			require.Equal(t, OutcomeCommitted, r.Outcome)
			require.Nil(t, r.Prepared)
			f.b.client.KV = original
			entry, e := f.b.LoadTaskQuiescence(f.ctx, f.c.reference.Task)
			require.NoError(t, e)
			require.Equal(t, r.Reference, entry.Reference)
			require.Equal(t, OutcomeCommitted, entry.Outcome)
		})
	}
}
