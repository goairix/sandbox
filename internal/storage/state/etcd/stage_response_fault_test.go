package etcd

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// All hooks retain the real server RPC. Corrupt replies cannot establish an
// outcome even when the original CAS actually committed.
type stageResponseCorruption struct {
	name   string
	change func(*clientv3.TxnResponse) *clientv3.TxnResponse
}

func TestStageResponseEvidence(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	t.Run("Grant", func(t *testing.T) {
		cases := []struct {
			name    string
			trusted bool
			mutate  func(*clientv3.LeaseGrantResponse) *clientv3.LeaseGrantResponse
		}{
			{"nil", false, func(r *clientv3.LeaseGrantResponse) *clientv3.LeaseGrantResponse { return nil }},
			{"nil-header", false, func(r *clientv3.LeaseGrantResponse) *clientv3.LeaseGrantResponse { r.ResponseHeader = nil; return r }},
			{"foreign", false, func(r *clientv3.LeaseGrantResponse) *clientv3.LeaseGrantResponse { r.ClusterId++; return r }},
			{"revision-zero", false, func(r *clientv3.LeaseGrantResponse) *clientv3.LeaseGrantResponse { r.Revision = 0; return r }},
			{"id-zero", false, func(r *clientv3.LeaseGrantResponse) *clientv3.LeaseGrantResponse { r.ID = 0; return r }},
			{"ttl-zero", true, func(r *clientv3.LeaseGrantResponse) *clientv3.LeaseGrantResponse { r.TTL = 0; return r }},
			{"error", true, func(r *clientv3.LeaseGrantResponse) *clientv3.LeaseGrantResponse { r.Error = "broken"; return r }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				original := b.client.Lease
				var nativeID clientv3.LeaseID
				hook := &stageGrantHook{Lease: original, after: func(r *clientv3.LeaseGrantResponse, e error) (*clientv3.LeaseGrantResponse, error) {
					require.NoError(t, e)
					nativeID = r.ID
					return tc.mutate(r), nil
				}}
				b.client.Lease = hook
				defer func() {
					b.client.Lease = original
					if nativeID != 0 && len(hook.revoked) == 0 {
						_, e := raw.Revoke(ctx, nativeID)
						require.NoError(t, e)
					}
				}()
				var stage *Stage
				var err error
				require.NotPanics(t, func() {
					stage, err = b.BeginStage(ctx, 7, "grant-"+tc.name, "test", Mutation{Writes: []Write{{Key: b.namespace.Root() + "p/07/controls/grant", Value: []byte("x")}}}, time.Second)
				})
				require.Nil(t, stage)
				require.ErrorIs(t, err, ErrOutcomeUnknown)
				if tc.trusted {
					require.Equal(t, []clientv3.LeaseID{nativeID}, hook.revoked)
				} else {
					require.Empty(t, hook.revoked, "untrusted grant must never revoke a claimed ID")
				}
			})
		}
		t.Run("cancelled", func(t *testing.T) {
			callCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			originalLease, originalKV := b.client.Lease, b.client.KV
			defer func() { b.client.Lease = originalLease; b.client.KV = originalKV }()
			b.client.Lease = &stageGrantHook{Lease: originalLease, after: func(r *clientv3.LeaseGrantResponse, e error) (*clientv3.LeaseGrantResponse, error) {
				cancel()
				return r, e
			}}
			guardWrites := 0
			b.client.KV = &faultKV{KV: originalKV, match: func([]clientv3.Op) bool { return true }, before: func() { guardWrites++ }}
			stage, err := b.BeginStage(callCtx, 7, "cancelled", "test", Mutation{Writes: []Write{{Key: b.namespace.Root() + "p/07/controls/cancelled", Value: []byte("x")}}}, time.Second)
			require.Nil(t, stage)
			require.ErrorIs(t, err, ErrOutcomeUnknown)
			require.Zero(t, guardWrites)
		})
	})
	mutations := []stageResponseCorruption{
		{"nil", func(r *clientv3.TxnResponse) *clientv3.TxnResponse { return nil }},
		{"nil-header", func(r *clientv3.TxnResponse) *clientv3.TxnResponse { r.Header = nil; return r }},
		{"foreign", func(r *clientv3.TxnResponse) *clientv3.TxnResponse { r.Header.ClusterId++; return r }},
		{"revision-zero", func(r *clientv3.TxnResponse) *clientv3.TxnResponse { r.Header.Revision = 0; return r }},
		{"cardinality", func(r *clientv3.TxnResponse) *clientv3.TxnResponse {
			r.Responses = r.Responses[:len(r.Responses)-1]
			return r
		}},
		{"nil-op", func(r *clientv3.TxnResponse) *clientv3.TxnResponse { r.Responses[0] = nil; return r }},
		{"wrong-kind", func(r *clientv3.TxnResponse) *clientv3.TxnResponse {
			r.Responses[0] = &pb.ResponseOp{Response: &pb.ResponseOp_ResponseTxn{ResponseTxn: &pb.TxnResponse{}}}
			return r
		}},
	}
	thenMutations := append(append([]stageResponseCorruption{}, mutations...),
		stageResponseCorruption{"prev-kv", func(r *clientv3.TxnResponse) *clientv3.TxnResponse {
			r.Responses[0].GetResponsePut().PrevKv = &mvccpb.KeyValue{}
			return r
		}},
		stageResponseCorruption{"nested", func(r *clientv3.TxnResponse) *clientv3.TxnResponse {
			r.Responses[0].GetResponsePut().Header = &pb.ResponseHeader{Revision: r.Header.Revision + 1}
			return r
		}},
	)
	for _, phase := range []string{"Begin", "Commit", "Resolve"} {
		t.Run(phase+"Then", func(t *testing.T) {
			for i, tc := range thenMutations {
				t.Run(tc.name, func(t *testing.T) {
					key := b.namespace.Root() + fmt.Sprintf("p/07/controls/%s-%d", phase, i)
					mutation := Mutation{Writes: []Write{{Key: key, Value: []byte("original")}}}
					var s *Stage
					var err error
					if phase != "Begin" {
						s, err = b.BeginStage(ctx, 7, phase+fmt.Sprint(i), "test", mutation, 30*time.Second)
						require.NoError(t, err)
						defer func() { require.NoError(t, b.ReleaseStage(ctx, s)) }()
					}
					original := b.client.KV
					b.client.KV = &faultKV{KV: original, match: func([]clientv3.Op) bool { return true }, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
						require.NoError(t, e)
						require.True(t, r.Succeeded)
						return tc.change(r), nil
					}}
					defer func() { b.client.KV = original }()
					var outcome Outcome
					require.NotPanics(t, func() {
						switch phase {
						case "Begin":
							s, err = b.BeginStage(ctx, 7, phase+fmt.Sprint(i), "test", mutation, 30*time.Second)
						case "Commit":
							outcome, err = b.CommitStage(ctx, s)
						case "Resolve":
							outcome, err = b.ResolveStage(ctx, s.Reference())
						}
					})
					require.ErrorIs(t, err, ErrOutcomeUnknown)
					b.client.KV = original
					if phase == "Begin" {
						require.Nil(t, s)
						return
					}
					require.Equal(t, OutcomeUnknown, outcome)
					outcome, err = b.ResolveStage(ctx, s.Reference())
					require.NoError(t, err)
					if phase == "Commit" {
						require.Equal(t, OutcomeCommitted, outcome)
						got, e := raw.Get(ctx, key)
						require.NoError(t, e)
						require.Equal(t, "original", string(got.Kvs[0].Value))
					} else {
						require.Equal(t, OutcomeAborted, outcome)
					}
				})
			}
		})
	}
	pointMutations := []struct {
		name   string
		mutate func(*pb.RangeResponse)
	}{
		{"count", func(r *pb.RangeResponse) { r.Count++ }}, {"more", func(r *pb.RangeResponse) { r.More = true }},
		{"duplicate", func(r *pb.RangeResponse) { r.Kvs = append(r.Kvs, r.Kvs[0]); r.Count = 2 }},
		{"nil-kv", func(r *pb.RangeResponse) { r.Kvs[0] = nil }},
		{"wrong-key", func(r *pb.RangeResponse) { r.Kvs[0].Key = []byte("wrong") }},
		{"crev-zero", func(r *pb.RangeResponse) { r.Kvs[0].CreateRevision = 0 }},
		{"mrev-before-crev", func(r *pb.RangeResponse) { r.Kvs[0].ModRevision = r.Kvs[0].CreateRevision - 1 }},
		{"future-revision", func(r *pb.RangeResponse) { r.Kvs[0].ModRevision = 1 << 62 }},
		{"nested", func(r *pb.RangeResponse) { r.Header = &pb.ResponseHeader{Revision: -1} }},
		{"leased-identity", func(r *pb.RangeResponse) { r.Kvs[0].Lease = 123 }},
		{"identity-value", func(r *pb.RangeResponse) { r.Kvs[0].Value = []byte("changed") }},
	}
	for _, tc := range pointMutations {
		change := tc.mutate
		mutations = append(mutations, stageResponseCorruption{tc.name, func(r *clientv3.TxnResponse) *clientv3.TxnResponse {
			change(r.Responses[0].GetResponseRange())
			return r
		}})
	}
	for _, phase := range []string{"Begin", "Commit", "Resolve"} {
		t.Run(phase+"Else", func(t *testing.T) {
			for i, tc := range mutations {
				t.Run(tc.name, func(t *testing.T) {
					key := b.namespace.Root() + fmt.Sprintf("p/07/controls/else-%s-%d", phase, i)
					mutation := Mutation{Comparisons: []clientv3.Cmp{clientv3.Compare(clientv3.Value(key), "=", "unmet")}, Writes: []Write{{Key: key, Value: []byte("x")}}}
					var s *Stage
					var err error
					if phase != "Begin" {
						s, err = b.BeginStage(ctx, 7, "else-"+phase+fmt.Sprint(i), "test", mutation, 30*time.Second)
						require.NoError(t, err)
						defer func() { require.NoError(t, b.ReleaseStage(ctx, s)) }()
					}
					if phase == "Resolve" {
						_, err = b.ResolveStage(ctx, s.Reference())
						require.NoError(t, err)
					}
					if phase == "Begin" {
						_, err = raw.Put(ctx, b.restoreKey, "other")
						require.NoError(t, err)
						defer func() { _, e := raw.Put(ctx, b.restoreKey, b.restoreEpoch); require.NoError(t, e) }()
					}
					original := b.client.KV
					b.client.KV = &faultKV{KV: original, match: func([]clientv3.Op) bool { return true }, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
						require.NoError(t, e)
						require.False(t, r.Succeeded)
						return tc.change(r), nil
					}}
					defer func() { b.client.KV = original }()
					var outcome Outcome
					require.NotPanics(t, func() {
						switch phase {
						case "Begin":
							s, err = b.BeginStage(ctx, 7, "else-"+phase+fmt.Sprint(i), "test", mutation, 30*time.Second)
						case "Commit":
							outcome, err = b.CommitStage(ctx, s)
						case "Resolve":
							outcome, err = b.ResolveStage(ctx, s.Reference())
						}
					})
					require.ErrorIs(t, err, ErrOutcomeUnknown)
					if phase == "Begin" {
						require.Nil(t, s)
					} else {
						require.Equal(t, OutcomeUnknown, outcome)
					}
				})
			}
		})
	}
	t.Run("GuardModified", func(t *testing.T) {
		key := b.namespace.Root() + "p/07/controls/guard-modified"
		s, err := b.BeginStage(ctx, 7, "guard-modified", "test", Mutation{Comparisons: []clientv3.Cmp{clientv3.Compare(clientv3.Value(key), "=", "unmet")}, Writes: []Write{{Key: key, Value: []byte("x")}}}, 30*time.Second)
		require.NoError(t, err)
		defer func() { require.NoError(t, b.ReleaseStage(ctx, s)) }()
		_, err = raw.Put(ctx, s.guardKey, s.guardValue, clientv3.WithLease(s.leaseID))
		require.NoError(t, err)
		outcome, err := b.CommitStage(ctx, s)
		require.Equal(t, OutcomeUnknown, outcome)
		require.ErrorIs(t, err, ErrGuardExpired)
	})
	t.Run("ValidateEveryPointBeforeReceipt", func(t *testing.T) {
		key := b.namespace.Root() + "p/07/controls/all-points"
		s, err := b.BeginStage(ctx, 7, "all-points", "test", Mutation{Writes: []Write{{Key: key, Value: []byte("x")}}}, 30*time.Second)
		require.NoError(t, err)
		defer func() { require.NoError(t, b.ReleaseStage(ctx, s)) }()
		outcome, err := b.CommitStage(ctx, s)
		require.NoError(t, err)
		require.Equal(t, OutcomeCommitted, outcome)
		original := b.client.KV
		defer func() { b.client.KV = original }()
		b.client.KV = &faultKV{KV: original, match: func([]clientv3.Op) bool { return true }, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
			require.NoError(t, e)
			require.False(t, r.Succeeded)
			r.Responses[3].GetResponseRange().More = true
			return r, nil
		}}
		outcome, err = b.CommitStage(ctx, s)
		require.Equal(t, OutcomeUnknown, outcome)
		require.ErrorIs(t, err, ErrOutcomeUnknown)
	})
	t.Run("BeginFalseCAS", func(t *testing.T) {
		original := b.client.KV
		defer func() { b.client.KV = original }()
		b.client.KV = &faultKV{KV: original, match: func(ops []clientv3.Op) bool {
			_, err := raw.Put(ctx, string(ops[0].KeyBytes()), "occupied")
			require.NoError(t, err)
			return true
		}}
		s, err := b.BeginStage(ctx, 7, "begin-false", "test", Mutation{Writes: []Write{{Key: b.namespace.Root() + "p/07/controls/begin-false", Value: []byte("x")}}}, 30*time.Second)
		require.Nil(t, s)
		require.ErrorIs(t, err, ErrConflict)
	})
	t.Run("Delete", func(t *testing.T) {
		for _, bad := range []string{"zero", "one", "after-put", "negative", "many", "prev", "nested", "outer-minus-two", "future", "zero-revision", "foreign"} {
			t.Run(bad, func(t *testing.T) {
				key := b.namespace.Root() + "p/07/controls/delete-" + bad
				if bad == "one" {
					_, err := raw.Put(ctx, key, "exists")
					require.NoError(t, err)
				}
				writes := []Write{{Key: key, Delete: true}}
				if bad == "after-put" {
					writes = append([]Write{{Key: key + "-other", Value: []byte("written")}}, writes...)
				}
				s, err := b.BeginStage(ctx, 7, "delete-"+bad, "test", Mutation{Writes: writes}, 30*time.Second)
				require.NoError(t, err)
				defer func() { require.NoError(t, b.ReleaseStage(ctx, s)) }()
				original := b.client.KV
				defer func() { b.client.KV = original }()
				b.client.KV = &faultKV{KV: original, match: func([]clientv3.Op) bool { return true }, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
					require.NoError(t, e)
					d := r.Responses[len(writes)-1].GetResponseDeleteRange()
					t.Logf("native delete=%+v outer=%+v", d, r.Header)
					switch bad {
					case "negative":
						d.Deleted = -1
					case "many":
						d.Deleted = 2
					case "prev":
						d.PrevKvs = []*mvccpb.KeyValue{{}}
					case "nested":
						d.Header = &pb.ResponseHeader{Revision: -1}
					case "outer-minus-two":
						d.Header = &pb.ResponseHeader{Revision: r.Header.Revision - 2}
					case "future":
						d.Header = &pb.ResponseHeader{Revision: r.Header.Revision + 1}
					case "zero-revision":
						d.Header = &pb.ResponseHeader{}
					case "foreign":
						d.Header = &pb.ResponseHeader{ClusterId: r.Header.ClusterId + 1, Revision: r.Header.Revision}
					}
					return r, nil
				}}
				outcome, err := b.CommitStage(ctx, s)
				if bad == "zero" || bad == "one" || bad == "after-put" {
					require.NoError(t, err)
					require.Equal(t, OutcomeCommitted, outcome)
				} else {
					require.ErrorIs(t, err, ErrOutcomeUnknown)
					require.Equal(t, OutcomeUnknown, outcome)
					b.client.KV = original
					outcome, err = b.ResolveStage(ctx, s.Reference())
					require.NoError(t, err)
					require.Equal(t, OutcomeCommitted, outcome)
				}
			})
		}
	})
}
