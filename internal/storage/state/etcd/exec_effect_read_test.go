package etcd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type execHistoryFixture struct {
	b                                                                        *Backend
	raw                                                                      *clientv3.Client
	record                                                                   ExecEffectRecord
	ref                                                                      ExecEffectReference
	effectKey, receiptKey, issuerKey, effectValue, receiptValue, issuerValue string
	revision                                                                 int64
}

func newExecHistoryFixture(t *testing.T, present bool) *execHistoryFixture {
	t.Helper()
	b, raw := integrationBackend(t)
	ctx := context.Background()
	r := execEffectFixture()
	r.Operation.Reference.Namespace = b.namespace.Root()
	r.Operation.Reference.RestoreEpoch = b.restoreEpoch
	r.Operation.ExpiresAt = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	r.Attempt.Namespace = b.namespace.Root()
	r.Attempt.RestoreEpoch = b.restoreEpoch
	cert := []byte(`{"expired":true}`)
	r.IssuerCertificateDigest, _ = snapshotDigest(cert)
	issuer := CommandIssuerRecord{Version: 1, Namespace: b.namespace.Root(), RestoreEpoch: b.restoreEpoch, CertificateID: r.IssuerCertificateID, CertificateDigest: r.IssuerCertificateDigest, Certificate: cert}
	iv, err := encodeCommandIssuerRecord(issuer)
	require.NoError(t, err)
	ik, err := b.namespace.commandIssuerKey(r.IssuerCertificateID)
	require.NoError(t, err)
	put, err := raw.Put(ctx, ik, iv)
	require.NoError(t, err)
	r.IssuerRevision = put.Header.Revision
	ref := execEffectRef(r)
	ek, err := b.namespace.execEffectKey(ref.Operation)
	require.NoError(t, err)
	_, rk, err := b.stageKeys(ref.Stage)
	require.NoError(t, err)
	ev, err := encodeExecEffectRecord(r)
	require.NoError(t, err)
	rv, err := encodeReceipt(ref.Stage, OutcomeCommitted)
	require.NoError(t, err)
	f := &execHistoryFixture{b: b, raw: raw, record: r, ref: ref, effectKey: ek, receiptKey: rk, issuerKey: ik, effectValue: ev, receiptValue: rv, issuerValue: iv}
	if present {
		f.writeEffect(t)
	}
	return f
}
func (f *execHistoryFixture) writeEffect(t *testing.T) {
	t.Helper()
	// Build a fresh permanent pair even when a corruption case changes its
	// payload. Otherwise rewriting the effect hides the intended linkage error.
	_, err := f.raw.Txn(context.Background()).Then(clientv3.OpDelete(f.effectKey), clientv3.OpDelete(f.receiptKey)).Commit()
	require.NoError(t, err)
	res, err := f.raw.Txn(context.Background()).Then(clientv3.OpPut(f.effectKey, f.effectValue), clientv3.OpPut(f.receiptKey, f.receiptValue)).Commit()
	require.NoError(t, err)
	f.revision = res.Header.Revision
}
func installExecHistoryGuards(t *testing.T, f *execHistoryFixture) (*registryCountKV, *registryLease) {
	t.Helper()
	b := f.b
	originalKV, originalLease, originalClock, originalProvider := b.client.KV, b.client.Lease, b.authorityClock, b.execIssuer
	t.Cleanup(func() {
		b.client.KV = originalKV
		b.client.Lease = originalLease
		b.authorityClock = originalClock
		b.execIssuer = originalProvider
	})
	counter := &registryCountKV{KV: originalKV, t: t, b: b}
	lease := &registryLease{creationFaultLease: &creationFaultLease{Lease: originalLease}}
	b.client.KV = counter
	b.client.Lease = lease
	b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
		t.Fatal("historical Load consulted authority clock")
		return ClockObservation{}, nil
	})
	b.execIssuer = &registryProvider{certificate: func(context.Context) ([]byte, error) { t.Fatal("historical Load consulted provider"); return nil, nil }}
	return counter, lease
}
func requireExecHistoryNoLease(t *testing.T, l *registryLease) {
	t.Helper()
	require.Zero(t, l.grants.Load())
	require.Zero(t, l.keeps.Load())
	require.Zero(t, l.revokes.Load())
}
func TestLoadExecEffectHistory(t *testing.T) {
	f := newExecHistoryFixture(t, true)
	counter, lease := installExecHistoryGuards(t, f)
	ctx := context.Background()
	var keys [][]string
	original := f.b.client.KV
	f.b.client.KV = &faultKV{KV: original, match: func(ops []clientv3.Op) bool {
		var got []string
		for _, op := range ops {
			got = append(got, string(op.KeyBytes()))
		}
		keys = append(keys, got)
		return true
	}}
	entry, err := f.b.LoadExecEffect(ctx, f.ref)
	require.NoError(t, err)
	require.NotNil(t, entry)
	require.Equal(t, f.record, *entry.Record)
	require.Equal(t, f.ref, entry.Reference)
	require.Equal(t, OutcomeCommitted, entry.Outcome)
	require.Equal(t, f.revision, entry.Revision)
	require.Equal(t, [][]string{{f.b.identityKey, f.b.restoreKey, f.effectKey}, {f.b.identityKey, f.b.restoreKey, f.effectKey, f.receiptKey, f.issuerKey}}, keys)
	require.Equal(t, []int64{2, 8, 0}, counter.counts())
	requireExecHistoryNoLease(t, lease)
	entry.Record.Ticket[0] = '!'
	entry.Record.CommandID = "changed"
	entry.Reference.CommandID = "changed"
	next, err := f.b.LoadExecEffect(ctx, f.ref)
	require.NoError(t, err)
	require.Equal(t, f.record, *next.Record)
	require.Equal(t, f.ref, next.Reference)
	require.Equal(t, []int64{4, 16, 0}, counter.counts())
	requireExecHistoryNoLease(t, lease)
}
func TestLoadExecEffectAbsenceIsSnapshot(t *testing.T) {
	f := newExecHistoryFixture(t, false)
	counter, lease := installExecHistoryGuards(t, f)
	original := f.b.client.KV
	once := true
	f.b.client.KV = &faultKV{KV: original, match: func([]clientv3.Op) bool { return true }, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
		if once {
			once = false
			f.writeEffect(t)
		}
		return r, e
	}}
	entry, err := f.b.LoadExecEffect(context.Background(), f.ref)
	require.NoError(t, err)
	require.Nil(t, entry)
	require.Equal(t, []int64{1, 3, 0}, counter.counts())
	// A late native write can become visible after that absent snapshot.
	entry, err = f.b.LoadExecEffect(context.Background(), f.ref)
	require.NoError(t, err)
	require.NotNil(t, entry)
	require.Equal(t, OutcomeCommitted, entry.Outcome)
	require.Equal(t, []int64{3, 11, 0}, counter.counts())
	requireExecHistoryNoLease(t, lease)
}
func TestLoadExecEffectInvalidBeforeRPC(t *testing.T) {
	f := newExecHistoryFixture(t, false)
	counter, lease := installExecHistoryGuards(t, f)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		change func(*ExecEffectReference)
		want   error
	}{
		{"nil context", nil, func(*ExecEffectReference) {}, ErrInvalidRecord}, {"canceled", canceled, func(*ExecEffectReference) {}, context.Canceled}, {"partial", context.Background(), func(r *ExecEffectReference) { r.Stage = StageReference{} }, ErrInvalidRecord}, {"other namespace", context.Background(), func(r *ExecEffectReference) {
			r.Operation.Namespace = "/other/scope/cell/"
			r.Stage.Namespace = r.Operation.Namespace
		}, ErrIdentityMismatch}, {"other restore", context.Background(), func(r *ExecEffectReference) { r.Operation.RestoreEpoch = "other"; r.Stage.RestoreEpoch = "other" }, ErrIdentityMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := f.ref
			tc.change(&ref)
			entry, err := f.b.LoadExecEffect(tc.ctx, ref)
			require.Nil(t, entry)
			require.ErrorIs(t, err, tc.want)
		})
	}
	require.Equal(t, []int64{0, 0, 0}, counter.counts())
	requireExecHistoryNoLease(t, lease)
}
func TestLoadExecEffectCorruptEvidence(t *testing.T) {
	cases := []struct {
		name   string
		change func(*testing.T, *execHistoryFixture)
		want   error
	}{
		{"missing receipt", func(t *testing.T, f *execHistoryFixture) {
			_, e := f.raw.Delete(context.Background(), f.receiptKey)
			require.NoError(t, e)
		}, ErrCorruptReceipt},
		{"missing issuer", func(t *testing.T, f *execHistoryFixture) {
			_, e := f.raw.Delete(context.Background(), f.issuerKey)
			require.NoError(t, e)
		}, ErrCorruptRecord},
		{"corrupt receipt", func(t *testing.T, f *execHistoryFixture) {
			_, e := f.raw.Put(context.Background(), f.receiptKey, "{}")
			require.NoError(t, e)
		}, ErrCorruptReceipt},
		{"aborted receipt", func(t *testing.T, f *execHistoryFixture) {
			v, e := encodeReceipt(f.ref.Stage, OutcomeAborted)
			require.NoError(t, e)
			f.receiptValue = v
			f.writeEffect(t)
		}, ErrCorruptReceipt},
		{"receipt original revision", func(t *testing.T, f *execHistoryFixture) {
			_, e := f.raw.Delete(context.Background(), f.receiptKey)
			require.NoError(t, e)
			_, e = f.raw.Put(context.Background(), f.receiptKey, f.receiptValue)
			require.NoError(t, e)
		}, ErrCorruptReceipt},
		{"receipt digest", func(t *testing.T, f *execHistoryFixture) {
			ref := f.ref.Stage
			ref.Digest = strings.Repeat("f", 64)
			v, e := encodeReceipt(ref, OutcomeCommitted)
			require.NoError(t, e)
			f.receiptValue = v
			f.writeEffect(t)
		}, ErrCorruptReceipt},
		{"corrupt issuer", func(t *testing.T, f *execHistoryFixture) {
			_, e := f.raw.Put(context.Background(), f.issuerKey, "{}")
			require.NoError(t, e)
		}, ErrCorruptRecord},
		{"same certificate recreated", func(t *testing.T, f *execHistoryFixture) {
			_, e := f.raw.Delete(context.Background(), f.issuerKey)
			require.NoError(t, e)
			p, e := f.raw.Put(context.Background(), f.issuerKey, f.issuerValue)
			require.NoError(t, e)
			require.Greater(t, p.Header.Revision, f.record.IssuerRevision)
		}, ErrCorruptRecord},
		{"issuer digest", func(t *testing.T, f *execHistoryFixture) {
			r := f.record
			r.IssuerCertificateDigest = strings.Repeat("a", 64)
			v, e := encodeExecEffectRecord(r)
			require.NoError(t, e)
			f.effectValue = v
			f.writeEffect(t)
		}, ErrCorruptRecord},
		{"issuer namespace", func(t *testing.T, f *execHistoryFixture) {
			v := strings.Replace(f.issuerValue, f.b.namespace.Root(), "/other/scope/cell/", 1)
			_, e := f.raw.Delete(context.Background(), f.issuerKey)
			require.NoError(t, e)
			_, e = f.raw.Put(context.Background(), f.issuerKey, v)
			require.NoError(t, e)
		}, ErrCorruptRecord},
		{"issuer restore", func(t *testing.T, f *execHistoryFixture) {
			v := strings.Replace(f.issuerValue, f.b.restoreEpoch, "other", 1)
			_, e := f.raw.Delete(context.Background(), f.issuerKey)
			require.NoError(t, e)
			p, e := f.raw.Put(context.Background(), f.issuerKey, v)
			require.NoError(t, e)
			f.record.IssuerRevision = p.Header.Revision
			f.effectValue, e = encodeExecEffectRecord(f.record)
			require.NoError(t, e)
			f.writeEffect(t)
		}, ErrCorruptRecord},
		{"command reference", func(t *testing.T, f *execHistoryFixture) { f.ref.CommandID = "42345678-1234-4234-8234-123456789abc" }, ErrCorruptRecord},
		{"operation reference", func(t *testing.T, f *execHistoryFixture) { f.ref.Operation.Digest = strings.Repeat("f", 64) }, ErrCorruptRecord},
		{"attempt reference", func(t *testing.T, f *execHistoryFixture) {
			f.ref.Stage.AttemptID = "42345678-1234-4234-8234-123456789abc"
		}, ErrCorruptRecord},
		{"corrupt effect", func(t *testing.T, f *execHistoryFixture) {
			_, e := f.raw.Put(context.Background(), f.effectKey, "{}")
			require.NoError(t, e)
		}, ErrCorruptRecord},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecHistoryFixture(t, true)
			tc.change(t, f)
			counter, lease := installExecHistoryGuards(t, f)
			entry, err := f.b.LoadExecEffect(context.Background(), f.ref)
			require.Nil(t, entry)
			require.ErrorIs(t, err, tc.want)
			require.Zero(t, counter.puts.Load())
			requireExecHistoryNoLease(t, lease)
		})
	}
}
func TestLoadExecEffectDiscoveryDoesNotAdoptRecreation(t *testing.T) {
	for _, name := range []string{"issuer recreated", "effect missing", "same effect body recreated", "coherent effect and issuer recreation"} {
		t.Run(name, func(t *testing.T) {
			f := newExecHistoryFixture(t, true)
			_, lease := installExecHistoryGuards(t, f)
			first := true
			original := f.b.client.KV
			f.b.client.KV = &faultKV{KV: original, match: func([]clientv3.Op) bool { return true }, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
				if !first {
					return r, e
				}
				first = false
				switch name {
				case "same effect body recreated":
					_, err := f.raw.Txn(context.Background()).Then(clientv3.OpDelete(f.effectKey), clientv3.OpDelete(f.receiptKey)).Commit()
					require.NoError(t, err)
					f.writeEffect(t)
				case "effect missing":
					_, err := f.raw.Delete(context.Background(), f.effectKey)
					require.NoError(t, err)
				default:
					_, err := f.raw.Delete(context.Background(), f.issuerKey)
					require.NoError(t, err)
					p, err := f.raw.Put(context.Background(), f.issuerKey, f.issuerValue)
					require.NoError(t, err)
					if name == "coherent effect and issuer recreation" {
						f.record.IssuerRevision = p.Header.Revision
						f.effectValue, err = encodeExecEffectRecord(f.record)
						require.NoError(t, err)
						_, err = f.raw.Txn(context.Background()).Then(clientv3.OpDelete(f.effectKey), clientv3.OpDelete(f.receiptKey)).Commit()
						require.NoError(t, err)
						f.writeEffect(t)
					}
				}
				return r, e
			}}
			entry, err := f.b.LoadExecEffect(context.Background(), f.ref)
			require.Nil(t, entry)
			require.ErrorIs(t, err, ErrCorruptRecord)
			requireExecHistoryNoLease(t, lease)
		})
	}
}
func TestLoadExecEffectNativeResponseEnvelopes(t *testing.T) {
	cases := []stageResponseCorruption{
		{"nil", func(*clientv3.TxnResponse) *clientv3.TxnResponse { return nil }},
		{"nil header", func(r *clientv3.TxnResponse) *clientv3.TxnResponse { r.Header = nil; return r }},
		{"foreign", func(r *clientv3.TxnResponse) *clientv3.TxnResponse { r.Header.ClusterId++; return r }},
		{"zero revision", func(r *clientv3.TxnResponse) *clientv3.TxnResponse { r.Header.Revision = 0; return r }},
		{"cardinality", func(r *clientv3.TxnResponse) *clientv3.TxnResponse {
			r.Responses = r.Responses[:len(r.Responses)-1]
			return r
		}},
	}
	for _, pass := range []int{1, 2} {
		for _, tc := range cases {
			t.Run(tc.name+map[int]string{1: "/discovery", 2: "/coherent"}[pass], func(t *testing.T) {
				f := newExecHistoryFixture(t, true)
				_, lease := installExecHistoryGuards(t, f)
				call := 0
				original := f.b.client.KV
				f.b.client.KV = &faultKV{KV: original, match: func([]clientv3.Op) bool { return true }, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
					call++
					if call == pass {
						return tc.change(r), e
					}
					return r, e
				}}
				entry, err := f.b.LoadExecEffect(context.Background(), f.ref)
				require.Nil(t, entry)
				require.ErrorIs(t, err, ErrOutcomeUnknown)
				requireExecHistoryNoLease(t, lease)
			})
		}
	}
	// Every position's native header and point envelope must be validated even
	// when earlier business evidence would already imply corruption.
	pointCases := []struct {
		name   string
		change func(*clientv3.TxnResponse, int)
	}{
		{"nil op", func(r *clientv3.TxnResponse, i int) { r.Responses[i] = nil }},
		{"wrong op", func(r *clientv3.TxnResponse, i int) {
			r.Responses[i] = &pb.ResponseOp{Response: &pb.ResponseOp_ResponsePut{ResponsePut: &pb.PutResponse{}}}
		}},
		{"foreign header", func(r *clientv3.TxnResponse, i int) {
			r.Responses[i].GetResponseRange().Header.ClusterId = r.Header.ClusterId + 1
		}},
		{"zero revision", func(r *clientv3.TxnResponse, i int) { r.Responses[i].GetResponseRange().Header.Revision = 0 }},
		{"stale revision", func(r *clientv3.TxnResponse, i int) {
			r.Responses[i].GetResponseRange().Header.Revision = r.Header.Revision - 1
		}},
		{"more", func(r *clientv3.TxnResponse, i int) { r.Responses[i].GetResponseRange().More = true }},
		{"count", func(r *clientv3.TxnResponse, i int) { r.Responses[i].GetResponseRange().Count++ }},
		{"nil kv", func(r *clientv3.TxnResponse, i int) { r.Responses[i].GetResponseRange().Kvs[0] = nil }},
		{"wrong key", func(r *clientv3.TxnResponse, i int) { r.Responses[i].GetResponseRange().Kvs[0].Key = []byte("other") }},
		{"future kv", func(r *clientv3.TxnResponse, i int) {
			r.Responses[i].GetResponseRange().Kvs[0].ModRevision = r.Header.Revision + 1
		}},
		{"zero first revision", func(r *clientv3.TxnResponse, i int) { r.Responses[i].GetResponseRange().Kvs[0].CreateRevision = 0 }},
		{"two kvs", func(r *clientv3.TxnResponse, i int) {
			p := r.Responses[i].GetResponseRange()
			p.Kvs = append(p.Kvs, p.Kvs[0])
			p.Count = 2
		}},
	}
	for _, pass := range []int{1, 2} {
		count := 3
		if pass == 2 {
			count = 5
		}
		for i := 0; i < count; i++ {
			for _, tc := range pointCases {
				t.Run(tc.name+"/"+string(rune('0'+pass))+"/"+string(rune('0'+i)), func(t *testing.T) {
					f := newExecHistoryFixture(t, true)
					_, lease := installExecHistoryGuards(t, f)
					call := 0
					original := f.b.client.KV
					f.b.client.KV = &faultKV{KV: original, match: func([]clientv3.Op) bool { return true }, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
						call++
						if call == pass {
							tc.change(r, i)
							r.Succeeded = false
						}
						return r, e
					}}
					entry, err := f.b.LoadExecEffect(context.Background(), f.ref)
					require.Nil(t, entry)
					require.ErrorIs(t, err, ErrOutcomeUnknown)
					requireExecHistoryNoLease(t, lease)
				})
			}
		}
	}
	t.Run("native omitted nested headers", func(t *testing.T) {
		f := newExecHistoryFixture(t, true)
		counter, lease := installExecHistoryGuards(t, f)
		original := f.b.client.KV
		f.b.client.KV = &faultKV{KV: original, match: func([]clientv3.Op) bool { return true }, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
			for _, op := range r.Responses {
				op.GetResponseRange().Header = nil
			}
			return r, e
		}}
		entry, err := f.b.LoadExecEffect(context.Background(), f.ref)
		require.NoError(t, err)
		require.Equal(t, f.record, *entry.Record)
		require.Equal(t, []int64{2, 8, 0}, counter.counts())
		requireExecHistoryNoLease(t, lease)
	})
	t.Run("all headers before missing receipt", func(t *testing.T) {
		f := newExecHistoryFixture(t, true)
		_, err := f.raw.Delete(context.Background(), f.receiptKey)
		require.NoError(t, err)
		call := 0
		original := f.b.client.KV
		f.b.client.KV = &faultKV{KV: original, match: func([]clientv3.Op) bool { return true }, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
			call++
			if call == 2 {
				r.Responses[4].GetResponseRange().Header.Revision = 0
			}
			return r, e
		}}
		entry, err := f.b.LoadExecEffect(context.Background(), f.ref)
		require.Nil(t, entry)
		require.ErrorIs(t, err, ErrOutcomeUnknown)
		require.False(t, errors.Is(err, ErrCorruptReceipt))
	})
}
func TestLoadExecEffectNativeIdentityFence(t *testing.T) {
	for _, pass := range []int{1, 2} {
		for _, key := range []string{"identity", "restore"} {
			t.Run(key+"/"+string(rune('0'+pass)), func(t *testing.T) {
				f := newExecHistoryFixture(t, true)
				_, lease := installExecHistoryGuards(t, f)
				call := 0
				original := f.b.client.KV
				f.b.client.KV = &faultKV{KV: original, match: func([]clientv3.Op) bool { return true }, before: func() {
					call++
					if call == pass {
						k := f.b.identityKey
						if key == "restore" {
							k = f.b.restoreKey
						}
						_, err := f.raw.Put(context.Background(), k, "other")
						require.NoError(t, err)
					}
				}}
				entry, err := f.b.LoadExecEffect(context.Background(), f.ref)
				require.Nil(t, entry)
				require.ErrorIs(t, err, ErrIdentityMismatch)
				requireExecHistoryNoLease(t, lease)
			})
		}
	}
}
func TestLoadExecEffectSecondResponseBytesCopied(t *testing.T) {
	f := newExecHistoryFixture(t, true)
	original := f.b.client.KV
	var borrowed []*mvccpb.KeyValue
	f.b.client.KV = &faultKV{KV: original, match: func(ops []clientv3.Op) bool { return len(ops) == 5 }, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
		for _, p := range r.Responses {
			borrowed = append(borrowed, p.GetResponseRange().Kvs...)
		}
		return r, e
	}}
	entry, err := f.b.LoadExecEffect(context.Background(), f.ref)
	require.NoError(t, err)
	require.Equal(t, f.record, *entry.Record)
	for _, kv := range borrowed {
		for i := range kv.Value {
			kv.Value[i] = '!'
		}
	}
	require.Equal(t, f.record, *entry.Record)
	require.Equal(t, f.ref, entry.Reference)
}

func TestLoadExecEffectDiscoveryOwnsPinnedBytes(t *testing.T) {
	f := newExecHistoryFixture(t, true)
	original := f.b.client.KV
	var borrowed *mvccpb.KeyValue
	call := 0
	f.b.client.KV = &faultKV{KV: original, match: func([]clientv3.Op) bool { return true }, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
		call++
		if call == 1 {
			borrowed = r.Responses[2].GetResponseRange().Kvs[0]
			// A native coherent replacement must not become acceptable by mutating
			// the discovery response that the caller already consumed.
			_, err := f.raw.Delete(context.Background(), f.issuerKey)
			require.NoError(t, err)
			put, err := f.raw.Put(context.Background(), f.issuerKey, f.issuerValue)
			require.NoError(t, err)
			f.record.IssuerRevision = put.Header.Revision
			f.effectValue, err = encodeExecEffectRecord(f.record)
			require.NoError(t, err)
			f.writeEffect(t)
		} else {
			current := r.Responses[2].GetResponseRange().Kvs[0]
			borrowed.Value = append(borrowed.Value[:0], current.Value...)
			borrowed.CreateRevision = current.CreateRevision
		}
		return r, e
	}}
	entry, err := f.b.LoadExecEffect(context.Background(), f.ref)
	require.Nil(t, entry)
	require.ErrorIs(t, err, ErrCorruptRecord)
}
