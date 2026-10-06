package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestBeginOperationUnknownReplies(t *testing.T) {
	for _, phase := range []string{"grant", "known grant", "guard", "admit"} {
		for _, cleanupFails := range []bool{false, true} {
			t.Run(phase+map[bool]string{false: "", true: " cleanup failure"}[cleanupFails], func(t *testing.T) {
				b, raw, in, _ := operationControlFixture(t, "plain")
				ctx := context.Background()
				var original clientv3.LeaseID
				var observed OperationReference
				writes := 0
				lease := &creationFaultLease{Lease: b.client.Lease, grant: func(r *clientv3.LeaseGrantResponse, e error) (*clientv3.LeaseGrantResponse, error) {
					require.NoError(t, e)
					original = r.ID
					if phase == "grant" {
						return nil, context.DeadlineExceeded
					}
					if phase == "known grant" {
						return r, context.DeadlineExceeded
					}
					return r, e
				}}
				cleanupErr := errors.New("owned operation cleanup failed")
				if cleanupFails {
					lease.revoke = func(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
						require.NoError(t, ctx.Err())
						_, bounded := ctx.Deadline()
						require.True(t, bounded)
						require.Equal(t, original, id)
						return nil, cleanupErr
					}
				}
				b.client.Lease = lease
				t.Cleanup(func() {
					b.client.Lease = lease.Lease
					if original > 0 {
						_, _ = raw.Revoke(context.Background(), original)
					}
				})
				b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
					if len(ops) == 0 || !ops[0].IsPut() {
						return false
					}
					writes++
					var record OperationRecord
					require.NoError(t, json.Unmarshal(ops[0].ValueBytes(), &record))
					observed = record.Reference
					return phase == "guard" && len(ops) == 1 || phase == "admit" && len(ops) > 1
				}, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
					require.NoError(t, e)
					require.True(t, r.Succeeded)
					return nil, context.DeadlineExceeded
				}}
				result, err := b.BeginOperation(ctx, BeginOperationInput{SandboxID: in.SandboxID, RequestID: "unknown", Kind: OperationData})
				require.ErrorIs(t, err, ErrOutcomeUnknown)
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.Equal(t, OperationUnknown, result.Outcome)
				require.Nil(t, result.Capability)
				require.NotEmpty(t, result.Reference.OperationID)
				require.NotEmpty(t, result.Reference.Digest)
				if phase == "grant" {
					require.Zero(t, result.Reference.LeaseID)
					require.Zero(t, writes)
					require.Zero(t, lease.revokes.Load())
				} else {
					require.Equal(t, int64(original), result.Reference.LeaseID)
					require.Equal(t, int64(1), lease.revokes.Load())
					if phase == "known grant" {
						require.Zero(t, writes)
					} else {
						require.Equal(t, observed, result.Reference)
					}
					if phase == "guard" {
						require.Equal(t, 1, writes)
					}
				}
				if cleanupFails && phase != "grant" {
					require.ErrorIs(t, result.GuardCleanupError, cleanupErr)
					require.NotErrorIs(t, err, cleanupErr)
				} else {
					require.NoError(t, result.GuardCleanupError)
				}
				if cleanupFails && phase == "admit" {
					token, _, receipt, _, e := b.namespace.operationKeys(result.Reference)
					require.NoError(t, e)
					got, e := b.readDomain(ctx, token, receipt)
					require.NoError(t, e)
					require.NoError(t, validateOperationCompletion(got[0], got[1]))
				}
			})
		}
	}
}

func TestBeginOperationCannotDeliverCancelledOrRevoked(t *testing.T) {
	for _, phase := range []string{"grant", "guard", "admit", "revoked after admit"} {
		t.Run(phase, func(t *testing.T) {
			b, raw, in, _ := operationControlFixture(t, "plain")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var original clientv3.LeaseID
			lease := &creationFaultLease{Lease: b.client.Lease, grant: func(r *clientv3.LeaseGrantResponse, e error) (*clientv3.LeaseGrantResponse, error) {
				require.NoError(t, e)
				original = r.ID
				if phase == "grant" {
					cancel()
				}
				return r, e
			}}
			b.client.Lease = lease
			b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
				if len(ops) == 0 || !ops[0].IsPut() {
					return false
				}
				return phase == "guard" && len(ops) == 1 || strings.Contains(phase, "admit") && len(ops) > 1
			}, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
				require.NoError(t, e)
				require.True(t, r.Succeeded)
				if phase == "revoked after admit" {
					_, e = raw.Revoke(context.Background(), original)
					require.NoError(t, e)
				} else {
					cancel()
				}
				return r, e
			}}
			result, err := b.BeginOperation(ctx, BeginOperationInput{SandboxID: in.SandboxID, RequestID: "canceled", Kind: OperationData})
			require.Error(t, err)
			require.Nil(t, result.Capability)
			if strings.Contains(phase, "admit") {
				require.Equal(t, OperationCommitted, result.Outcome)
			}
			require.Equal(t, int64(1), lease.revokes.Load())
			require.NoError(t, result.GuardCleanupError)
		})
	}
}

// A transport response's nested headers must not be able to contradict the
// top-level cluster/revision while still yielding a usable capability.
func TestBeginOperationMalformedReplies(t *testing.T) {
	for _, phase := range []string{"guard", "admit", "live", "failed admit"} {
		for _, defect := range []string{"nested cluster", "nested revision", "nil response", "count", "type", "top revision"} {
			t.Run(phase+" "+defect, func(t *testing.T) {
				b, raw, in, keys := operationControlFixture(t, "plain")
				if phase == "failed admit" {
					b.client.Lease = &creationFaultLease{Lease: b.client.Lease, grant: func(r *clientv3.LeaseGrantResponse, e error) (*clientv3.LeaseGrantResponse, error) {
						require.NoError(t, e)
						operationChangePoint(t, raw, keys[1], "phase", PhaseDestroying)
						return r, e
					}}
				}
				b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
					if phase == "live" {
						return len(ops) == 5 && ops[0].IsGet() && string(ops[0].KeyBytes()) == b.identityKey
					}
					return len(ops) > 0 && ops[0].IsPut() && ((phase == "guard" && len(ops) == 1) || (phase != "guard" && len(ops) > 1))
				}, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
					require.NoError(t, e)
					switch defect {
					case "nested cluster", "nested revision":
						header := *r.Header
						if defect == "nested cluster" {
							header.ClusterId++
						} else {
							header.Revision++
						}
						if put := r.Responses[0].GetResponsePut(); put != nil {
							put.Header = &header
						} else {
							r.Responses[0].GetResponseRange().Header = &header
						}
					case "nil response":
						r.Responses[0] = nil
					case "count":
						r.Responses = append(r.Responses, r.Responses[0])
					case "type":
						r.Responses[0] = &pb.ResponseOp{}
					case "top revision":
						r.Header.Revision = 0
					}
					return r, e
				}}
				result, err := b.BeginOperation(context.Background(), BeginOperationInput{SandboxID: in.SandboxID, RequestID: "malformed", Kind: OperationData})
				require.Error(t, err)
				require.Nil(t, result.Capability)
				if phase == "live" {
					require.Equal(t, OperationCommitted, result.Outcome)
				}
				if phase == "failed admit" {
					require.NotErrorIs(t, err, ErrConflict, "malformed failure evidence is not a validated CAS rejection")
				}
			})
		}
	}
}

func TestBeginOperationFailureCompletionEnvelope(t *testing.T) {
	for _, defect := range []string{"completion revision", "guard body", "token lease", "valid historical"} {
		t.Run(defect, func(t *testing.T) {
			b, raw, in, _ := operationControlFixture(t, "plain")
			var ref OperationReference
			b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
				if len(ops) != 2 || !ops[0].IsPut() {
					return false
				}
				var record OperationRecord
				require.NoError(t, json.Unmarshal(ops[0].ValueBytes(), &record))
				ref = record.Reference
				return true
			}, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
				require.NoError(t, e)
				require.True(t, r.Succeeded)
				token, guard, receipt, _, e := b.namespace.operationKeys(ref)
				require.NoError(t, e)
				evidence, e := raw.Txn(context.Background()).Then(clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey), clientv3.OpGet(receipt), clientv3.OpGet(guard), clientv3.OpGet(token)).Commit()
				require.NoError(t, e)
				evidence.Succeeded = false
				switch defect {
				case "completion revision":
					kv := evidence.Responses[2].GetResponseRange().Kvs[0]
					kv.CreateRevision--
					kv.ModRevision--
				case "guard body":
					evidence.Responses[3].GetResponseRange().Kvs[0].Value = []byte("null")
				case "token lease":
					evidence.Responses[4].GetResponseRange().Kvs[0].Lease++
				}
				return evidence, nil
			}}
			result, err := b.BeginOperation(context.Background(), BeginOperationInput{SandboxID: in.SandboxID, RequestID: "failure-envelope", Kind: OperationData})
			want := ErrCorruptRecord
			if defect == "completion revision" {
				want = ErrCorruptReceipt
			} else if defect == "valid historical" {
				want = ErrConflict
			}
			require.ErrorIs(t, err, want)
			require.Nil(t, result.Capability)
			if defect == "valid historical" {
				require.Equal(t, OperationCommitted, result.Outcome, "complete validated failure evidence may retain a historical commit")
			} else {
				require.Equal(t, OperationUnknown, result.Outcome, "partially validated failure evidence cannot publish a historical outcome")
			}
		})
	}
}

func TestBeginOperationSendDeadline(t *testing.T) {
	for _, phase := range []string{"grant", "guard", "admit"} {
		t.Run(phase, func(t *testing.T) {
			b, _, in, _ := operationControlFixture(t, "plain")
			b.client.Lease = &creationFaultLease{Lease: b.client.Lease, grant: func(r *clientv3.LeaseGrantResponse, e error) (*clientv3.LeaseGrantResponse, error) {
				require.NoError(t, e)
				// A shorter positive server response TTL makes the deadline fault cheap;
				// the real native Lease was still granted with the required 30-second RPC.
				r.TTL = 1
				if phase == "grant" {
					time.Sleep(1100 * time.Millisecond)
				}
				return r, e
			}}
			b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
				return len(ops) > 0 && ops[0].IsPut() && (phase == "guard" && len(ops) == 1 || phase == "admit" && len(ops) == 2)
			}, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
				require.NoError(t, e)
				time.Sleep(1100 * time.Millisecond)
				return r, e
			}}
			result, err := b.BeginOperation(context.Background(), BeginOperationInput{SandboxID: in.SandboxID, RequestID: "late-reply", Kind: OperationData})
			require.ErrorIs(t, err, ErrGuardExpired)
			require.Nil(t, result.Capability)
			if phase == "admit" {
				require.Equal(t, OperationCommitted, result.Outcome)
			}
		})
	}
}

func TestBeginOperationDelayedDispatchRemainsUnknown(t *testing.T) {
	for _, phase := range []string{"guard", "admit"} {
		t.Run(phase, func(t *testing.T) {
			b, raw, in, _ := operationControlFixture(t, "plain")
			reached, release := make(chan OperationReference, 1), make(chan struct{})
			var once sync.Once
			unlock := func() { once.Do(func() { close(release) }) }
			defer unlock()
			var ref OperationReference
			lease := &creationFaultLease{Lease: b.client.Lease, revoke: func(context.Context, clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
				return nil, errors.New("retain original Lease for delayed dispatch evidence")
			}}
			b.client.Lease = lease
			defer func() { b.client.Lease = lease.Lease }()
			b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
				match := len(ops) > 0 && ops[0].IsPut() && (phase == "guard" && len(ops) == 1 || phase == "admit" && len(ops) == 2)
				if match {
					var record OperationRecord
					require.NoError(t, json.Unmarshal(ops[0].ValueBytes(), &record))
					ref = record.Reference
				}
				return match
			}, before: func() { reached <- ref; <-release }, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
				require.NoError(t, e)
				require.True(t, r.Succeeded)
				return nil, context.DeadlineExceeded
			}}
			type answer struct {
				result BeginOperationResult
				err    error
			}
			done := make(chan answer, 1)
			go func() {
				r, e := b.BeginOperation(context.Background(), BeginOperationInput{SandboxID: in.SandboxID, RequestID: "delayed", Kind: OperationData})
				done <- answer{r, e}
			}()
			select {
			case ref = <-reached:
			case <-time.After(5 * time.Second):
				t.Fatal("dispatch not intercepted")
			}
			token, guard, receipt, _, err := b.namespace.operationKeys(ref)
			require.NoError(t, err)
			for _, key := range []string{token, receipt} {
				got, e := raw.Get(context.Background(), key)
				require.NoError(t, e)
				require.Empty(t, got.Kvs)
			}
			unlock()
			select {
			case a := <-done:
				require.ErrorIs(t, a.err, ErrOutcomeUnknown)
				require.Equal(t, OperationUnknown, a.result.Outcome)
				require.Nil(t, a.result.Capability)
				require.Equal(t, ref, a.result.Reference)
				require.Error(t, a.result.GuardCleanupError)
			case <-time.After(5 * time.Second):
				t.Fatal("delayed dispatch did not finish")
			}
			got, err := raw.Get(context.Background(), guard)
			require.NoError(t, err)
			require.Len(t, got.Kvs, 1)
			if phase == "admit" {
				got, err := b.readDomain(context.Background(), token, receipt)
				require.NoError(t, err)
				require.NoError(t, validateOperationCompletion(got[0], got[1]))
			}
			_, err = raw.Revoke(context.Background(), clientv3.LeaseID(ref.LeaseID))
			require.NoError(t, err)
		})
	}
}

func TestBeginOperationGrantEnvelope(t *testing.T) {
	for _, defect := range []string{"nil", "header", "cluster", "zero id", "negative id", "zero ttl", "negative ttl", "oversized ttl", "overflow ttl"} {
		t.Run(defect, func(t *testing.T) {
			b, raw, in, _ := operationControlFixture(t, "plain")
			var original clientv3.LeaseID
			lease := &creationFaultLease{Lease: b.client.Lease, grant: func(r *clientv3.LeaseGrantResponse, e error) (*clientv3.LeaseGrantResponse, error) {
				require.NoError(t, e)
				original = r.ID
				switch defect {
				case "nil":
					return nil, nil
				case "header":
					r.ResponseHeader = nil
				case "cluster":
					r.ClusterId++
				case "zero id":
					r.ID = 0
				case "negative id":
					r.ID = -1
				case "zero ttl":
					r.TTL = 0
				case "negative ttl":
					r.TTL = -1
				case "oversized ttl":
					r.TTL = 31
				case "overflow ttl":
					r.TTL = math.MaxInt64
				}
				return r, nil
			}}
			b.client.Lease = lease
			writes := 0
			b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
				for _, op := range ops {
					if op.IsPut() {
						writes++
					}
				}
				return false
			}}
			result, err := b.BeginOperation(context.Background(), BeginOperationInput{SandboxID: in.SandboxID, RequestID: "grant-envelope", Kind: OperationData})
			require.Error(t, err)
			require.Nil(t, result.Capability)
			require.Zero(t, writes)
			require.NotEmpty(t, result.Reference.OperationID)
			require.NotEmpty(t, result.Reference.Digest)
			if strings.Contains(defect, "ttl") {
				require.Equal(t, int64(1), lease.revokes.Load())
				require.Equal(t, int64(original), result.Reference.LeaseID)
			} else {
				require.Zero(t, lease.revokes.Load())
				require.Zero(t, result.Reference.LeaseID)
				_, err = raw.Revoke(context.Background(), original)
				require.NoError(t, err)
			}
		})
	}
}
