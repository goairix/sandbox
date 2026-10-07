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

func TestTaskCloseNativeUnknown(t *testing.T) {
	for _, fault := range []string{"registration", "grant", "begin", "commit", "resolver", "resolve-reply", "cleanup"} {
		t.Run(fault, func(t *testing.T) {
			f := newTaskCloseFixture(t, false)
			b := f.b
			original := b.client.KV
			defer func() { b.client.KV = original }()
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			key, e := b.namespace.taskCloseDataKey(f.c.reference.Task)
			require.NoError(t, e)
			issuerKey, e := b.namespace.commandIssuerKey(f.p.identity.CertificateID())
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
						out, e := b.ResolveStage(f.ctx, f.c.closeDraft.reference.Stage)
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
			first, e := b.PrepareTaskCloseData(f.ctx, f.c)
			d := f.c.closeDraft
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
				require.Zero(t, f.p.signCalls)
				f.p.wire = nil
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
				retry, e := b.PrepareTaskCloseData(f.ctx, f.c)
				require.ErrorIs(t, e, ErrOutcomeUnknown)
				require.Equal(t, saved, retry.Reference)
				require.Nil(t, retry.Prepared)
				b.client.KV = original
			}
			retry, e := b.PrepareTaskCloseData(f.ctx, f.c)
			require.NoError(t, e)
			require.Equal(t, command, d.commandID)
			require.Equal(t, deadline, d.deadline)
			require.Equal(t, certificate, d.certificate)
			require.Equal(t, 1, f.p.certCalls)
			if fault == "commit" || fault == "registration" || fault == "cleanup" {
				require.Equal(t, OutcomeCommitted, retry.Outcome)
				require.NotNil(t, retry.Prepared)
				entry, e := b.LoadTaskCloseData(f.ctx, f.c.reference.Task)
				require.NoError(t, e)
				require.Equal(t, retry.Reference, entry.Reference)
			} else {
				require.Equal(t, OutcomeAborted, retry.Outcome)
				require.Nil(t, retry.Prepared)
				entry, e := b.LoadTaskCloseData(f.ctx, f.c.reference.Task)
				require.NoError(t, e)
				require.Nil(t, entry)
			}
			if fault != "registration" {
				require.Equal(t, saved, retry.Reference)
			}
			require.Equal(t, int64(1), lease.grants.Load())
			require.Equal(t, 1, f.p.signCalls)
		})
	}
}
func TestTaskCloseNativePostcommit(t *testing.T) {
	for _, fault := range []string{"cancel", "deadline", "clock", "fence"} {
		t.Run(fault, func(t *testing.T) {
			f := newTaskCloseFixture(t, false)
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			key, e := f.b.namespace.taskCloseDataKey(f.c.reference.Task)
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
					f.c.closeDraft.deadline = time.Now().Add(-time.Second)
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
			r, e := f.b.PrepareTaskCloseData(ctx, f.c)
			require.Error(t, e)
			require.Equal(t, OutcomeCommitted, r.Outcome)
			require.Nil(t, r.Prepared)
			f.b.client.KV = original
			entry, e := f.b.LoadTaskCloseData(f.ctx, f.c.reference.Task)
			require.NoError(t, e)
			require.Equal(t, r.Reference, entry.Reference)
			require.Equal(t, OutcomeCommitted, entry.Outcome)
		})
	}
}
func TestTaskCloseNativeFences(t *testing.T) {
	for _, name := range []string{"task", "intent", "link", "placement", "control", "owner", "workspace-fence", "runtime-index", "claim", "guard", "issuer"} {
		t.Run(name, func(t *testing.T) {
			for _, kind := range []string{"value", "same-bytes", "lease", "recreate"} {
				t.Run(kind, func(t *testing.T) {
					f := newTaskCloseFixture(t, false)
					key, e := f.b.namespace.taskCloseDataKey(f.c.reference.Task)
					require.NoError(t, e)
					original := f.b.client.KV
					defer func() { f.b.client.KV = original }()
					var changed string
					f.b.client.KV = &faultKV{KV: original, match: putsKey(key), before: func() {
						d := f.c.closeDraft
						switch name {
						case "claim":
							changed = f.c.claimKey
						case "guard":
							changed = f.c.guardKey
						case "issuer":
							changed = d.issuerKey
						default:
							for i, n := range []string{"task", "intent", "link", "placement", "control", "owner", "workspace-fence", "runtime-index"} {
								if n == name {
									changed = f.c.fences[i].key
								}
							}
						}
						r, e := f.raw.Get(f.ctx, changed)
						require.NoError(t, e)
						require.Len(t, r.Kvs, 1)
						kv := r.Kvs[0]
						value := string(kv.Value)
						opts := []clientv3.OpOption{}
						if kv.Lease > 0 {
							opts = append(opts, clientv3.WithLease(clientv3.LeaseID(kv.Lease)))
						}
						switch kind {
						case "value":
							value = "changed"
						case "lease":
							grant, e := f.raw.Grant(f.ctx, 30)
							require.NoError(t, e)
							opts = []clientv3.OpOption{clientv3.WithLease(grant.ID)}
							t.Cleanup(func() {
								ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
								defer cancel()
								_, _ = f.raw.Revoke(ctx, grant.ID)
							})
						case "recreate":
							_, e = f.raw.Delete(f.ctx, changed)
							require.NoError(t, e)
						}
						_, e = f.raw.Put(f.ctx, changed, value, opts...)
						require.NoError(t, e)
					}}
					r, e := f.b.PrepareTaskCloseData(f.ctx, f.c)
					require.Error(t, e)
					require.Nil(t, r.Prepared)
					require.Equal(t, OutcomeUnknown, r.Outcome)
					require.NotEmpty(t, changed)
					require.Equal(t, 1, f.p.signCalls)
					require.NotNil(t, f.c.closeDraft.stage)
					f.b.client.KV = original
					got, e := f.raw.Get(f.ctx, key)
					require.NoError(t, e)
					require.Empty(t, got.Kvs)
					out, e := f.b.ResolveStage(f.ctx, r.Reference.Stage)
					require.NoError(t, e)
					require.Equal(t, OutcomeAborted, out)
					require.NoError(t, f.b.ReleaseStage(f.ctx, f.c.closeDraft.stage))
				})
			}
		})
	}
}

func TestTaskCloseNativeResponses(t *testing.T) {
	for _, fault := range []string{"fence-empty", "fence-cluster", "fence-identity", "commit-header", "commit-put", "read-header"} {
		t.Run(fault, func(t *testing.T) {
			f := newTaskCloseFixture(t, false)
			original := f.b.client.KV
			defer func() { f.b.client.KV = original }()
			key, e := f.b.namespace.taskCloseDataKey(f.c.reference.Task)
			require.NoError(t, e)
			seen := false
			f.b.client.KV = &faultKV{KV: original, match: func(ops []clientv3.Op) bool {
				if seen {
					return false
				}
				if strings.HasPrefix(fault, "fence-") {
					return len(ops) == 1 && ops[0].IsGet() && string(ops[0].KeyBytes()) == f.b.identityKey
				}
				if fault == "read-header" {
					return len(ops) == 3 && ops[2].IsGet() && string(ops[2].KeyBytes()) == key
				}
				return putsKey(key)(ops)
			}, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
				require.NoError(t, e)
				seen = true
				switch fault {
				case "fence-empty":
					r.Responses = nil
				case "fence-cluster":
					r.Header.ClusterId++
				case "fence-identity":
					r.Responses[0].GetResponseRange().Kvs[0].Value = []byte("other")
				case "commit-header", "read-header":
					r.Header = nil
				case "commit-put":
					r.Responses = nil
				}
				return r, nil
			}}
			r, e := f.b.PrepareTaskCloseData(f.ctx, f.c)
			require.Error(t, e)
			require.True(t, seen)
			require.Nil(t, r.Prepared)
			require.Equal(t, OutcomeUnknown, r.Outcome)
			f.b.client.KV = original
			if strings.HasPrefix(fault, "commit-") {
				entry, e := f.b.LoadTaskCloseData(f.ctx, f.c.reference.Task)
				require.NoError(t, e)
				require.Equal(t, r.Reference, entry.Reference)
				retry, e := f.b.PrepareTaskCloseData(f.ctx, f.c)
				require.NoError(t, e)
				require.Equal(t, OutcomeCommitted, retry.Outcome)
				require.NotNil(t, retry.Prepared)
			} else {
				got, e := f.raw.Get(f.ctx, key)
				require.NoError(t, e)
				require.Empty(t, got.Kvs)
				require.Zero(t, f.p.signCalls)
			}
		})
	}
}
