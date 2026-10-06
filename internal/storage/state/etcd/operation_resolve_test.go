package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Delaying the whole native admission (not just its reply) distinguishes
// receipt arbitration from unsafe missing-token inference.
func TestResolveOperationLateCompleteTransaction(t *testing.T) {
	for _, scenario := range []string{"abort", "commit reply lost", "revoke", "natural expiry"} {
		t.Run(scenario, func(t *testing.T) {
			b, raw, in, _ := operationControlFixture(t, "plain")
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			reached, release := make(chan OperationReference, 1), make(chan struct{})
			var once sync.Once
			unlock := func() { once.Do(func() { close(release) }) }
			lease := &creationFaultLease{Lease: b.client.Lease, revoke: func(context.Context, clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
				return nil, errors.New("retain original Lease")
			}}
			b.client.Lease = lease
			// Keep the actual delayed Txn context alive through natural original expiry.
			b.requestTimeout = 45 * time.Second
			var ref OperationReference
			b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
				if len(ops) != 3 || !ops[0].IsPut() {
					return false
				}
				var r OperationRecord
				require.NoError(t, json.Unmarshal(ops[0].ValueBytes(), &r))
				ref = r.Reference
				return true
			}, before: func() {
				if scenario != "commit reply lost" {
					reached <- ref
					<-release
					if os.Getenv("SANDBOX_TEST_DELAYED_OPERATION_FATAL") == "1" {
						require.Same(t, lease, b.client.Lease, "hook restored while admission running")
						t.Fatal("injected worker assertion failure")
					}
				}
			}, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
				require.NoError(t, e)
				if scenario == "commit reply lost" {
					require.True(t, r.Succeeded)
					reached <- ref
					<-release
				} else {
					require.False(t, r.Succeeded)
				}
				return nil, context.DeadlineExceeded
			}}
			done := make(chan BeginOperationResult, 1)
			finished := make(chan struct{})
			if os.Getenv("SANDBOX_TEST_DELAYED_OPERATION_FATAL") == "1" {
				t.Cleanup(func() {
					select {
					case <-finished:
						t.Log("delayed admission completed before fixture cleanup")
					default:
						t.Error("admission still running at fixture cleanup")
					}
				})
			}
			// Registered after the fixture cleanups: release and join first on
			// every exit, including parent Fatal and worker require/Goexit.
			t.Cleanup(func() {
				unlock()
				cancel()
				select {
				case <-finished:
				case <-time.After(5 * time.Second):
					t.Error("admission did not finish before fixture cleanup")
					return // Do not race a still-running worker by restoring hooks.
				}
				b.client.Lease = lease.Lease
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cleanupCancel()
				if ref.LeaseID > 0 {
					_, _ = raw.Revoke(cleanupCtx, clientv3.LeaseID(ref.LeaseID))
				}
			})
			go func() {
				defer close(finished)
				r, e := b.BeginOperation(ctx, BeginOperationInput{SandboxID: in.SandboxID, RequestID: "delayed-resolve", Kind: OperationMutation})
				require.ErrorIs(t, e, ErrOutcomeUnknown)
				done <- r
			}()
			select {
			case ref = <-reached:
			case <-time.After(5 * time.Second):
				t.Fatal("admission not intercepted")
			}
			if os.Getenv("SANDBOX_TEST_DELAYED_OPERATION_FATAL") == "1" {
				t.Fatal("injected early admission assertion failure")
			}
			want := OperationAborted
			if scenario == "commit reply lost" {
				want = OperationCommitted
			}
			if scenario == "revoke" {
				_, e := raw.Revoke(ctx, clientv3.LeaseID(ref.LeaseID))
				require.NoError(t, e)
				want = OperationExpired
			}
			if scenario == "natural expiry" {
				require.Eventually(t, func() bool {
					r, e := raw.TimeToLive(ctx, clientv3.LeaseID(ref.LeaseID))
					return e == nil && r.TTL == -1
				}, 36*time.Second, 200*time.Millisecond)
				want = OperationExpired
			}
			outcome, e := b.ResolveOperation(ctx, ref)
			require.NoError(t, e)
			require.Equal(t, want, outcome)
			unlock()
			select {
			case r := <-done:
				require.Nil(t, r.Capability)
			case <-time.After(5 * time.Second):
				t.Fatal("admission did not resume")
			}
			outcome, e = b.ResolveOperation(ctx, ref)
			require.NoError(t, e)
			require.Equal(t, want, outcome)
			token, _, _, lock, e := b.namespace.operationKeys(ref)
			require.NoError(t, e)
			if want != OperationCommitted {
				for _, k := range []string{token, lock} {
					r, e := raw.Get(ctx, k)
					require.NoError(t, e)
					require.Empty(t, r.Kvs)
				}
			}
			require.Zero(t, lease.keeps.Load())
			require.Equal(t, int64(1), lease.grants.Load())
		})
	}
}

// Run the intentional parent/worker Fatal paths in a child test process so an
// expected assertion failure cannot mark the actual regression suite failed.
func TestDelayedOperationCleanupAfterFatal(t *testing.T) {
	if os.Getenv("TEST_ETCD_ENDPOINTS") == "" {
		t.Skip("set TEST_ETCD_ENDPOINTS to a real three-member etcd cluster")
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestResolveOperationLateCompleteTransaction$/^abort$", "-test.v")
	command.Env = append(os.Environ(), "SANDBOX_TEST_DELAYED_OPERATION_FATAL=1")
	output, err := command.CombinedOutput()
	require.Error(t, err, "child must encounter the intentionally injected Fatal")
	require.NoError(t, ctx.Err(), string(output))
	require.Contains(t, string(output), "injected early admission assertion failure")
	require.Contains(t, string(output), "injected worker assertion failure")
	require.Contains(t, string(output), "delayed admission completed before fixture cleanup")
	require.NotContains(t, string(output), "admission still running at fixture cleanup")
	require.NotContains(t, string(output), "hook restored while admission running")
	require.NotContains(t, string(output), "DATA RACE")
}

func admittedOperation(t *testing.T, b *Backend, raw *clientv3.Client, id string, kind OperationKind) *OperationCapability {
	t.Helper()
	r, e := b.BeginOperation(context.Background(), BeginOperationInput{SandboxID: id, RequestID: "lifecycle", Kind: kind})
	require.NoError(t, e)
	require.NotNil(t, r.Capability)
	t.Cleanup(func() { _, _ = raw.Revoke(context.Background(), clientv3.LeaseID(r.Reference.LeaseID)) })
	return r.Capability
}

func TestResolveOperationStrictEvidence(t *testing.T) {
	for _, scenario := range []string{"valid mutation", "valid data foreign lock", "expired foreign lock", "aborted foreign lock", "zero Lease", "old reference", "restore", "malformed guard", "missing guard", "missing token", "missing receipt", "unleased receipt", "wrong receipt Lease", "recreated guard", "recreated token", "mutation missing", "foreign malformed", "guard rewrite"} {
		t.Run(scenario, func(t *testing.T) {
			b, raw, in, _ := operationControlFixture(t, "plain")
			ctx := context.Background()
			kind := OperationData
			if scenario == "valid mutation" || scenario == "mutation missing" {
				kind = OperationMutation
			}
			c := admittedOperation(t, b, raw, in.SandboxID, kind)
			ref := c.Reference()
			want := OperationUnknown
			put := func(k, v string, id int64) {
				_, e := raw.Put(ctx, k, v, clientv3.WithLease(clientv3.LeaseID(id)))
				require.NoError(t, e)
			}
			del := func(k string) { _, e := raw.Delete(ctx, k); require.NoError(t, e) }
			switch scenario {
			case "valid mutation":
				want = OperationCommitted
			case "valid data foreign lock", "expired foreign lock", "aborted foreign lock":
				if scenario == "expired foreign lock" {
					_, e := raw.Revoke(ctx, clientv3.LeaseID(ref.LeaseID))
					require.NoError(t, e)
					want = OperationExpired
				}
				if scenario == "aborted foreign lock" {
					del(c.tokenKey)
					del(c.receiptKey)
					v, e := encodeOperationReceipt(operationReceipt{Version: 1, Reference: ref, Outcome: OperationAborted})
					require.NoError(t, e)
					put(c.receiptKey, v, ref.LeaseID)
					want = OperationAborted
				}
				admittedOperation(t, b, raw, in.SandboxID, OperationMutation)
				if scenario == "valid data foreign lock" {
					want = OperationCommitted
				}
			case "zero Lease":
				ref.LeaseID = 0
			case "old reference":
				ref.RequestID = "old"
			case "restore":
				put(b.restoreKey, "new-restore", 0)
			case "malformed guard":
				del(c.guardKey)
				put(c.guardKey, "null", ref.LeaseID)
			case "missing guard":
				del(c.guardKey)
			case "missing token":
				del(c.tokenKey)
			case "missing receipt":
				del(c.receiptKey)
			case "unleased receipt", "wrong receipt Lease":
				g, e := raw.Get(ctx, c.receiptKey)
				require.NoError(t, e)
				del(c.receiptKey)
				lease := int64(0)
				if scenario == "wrong receipt Lease" {
					r, e := raw.Grant(ctx, 30)
					require.NoError(t, e)
					lease = int64(r.ID)
					defer raw.Revoke(ctx, r.ID)
				}
				put(c.receiptKey, string(g.Kvs[0].Value), lease)
			case "recreated guard":
				del(c.guardKey)
				put(c.guardKey, c.value, ref.LeaseID)
			case "recreated token":
				del(c.tokenKey)
				put(c.tokenKey, c.value, ref.LeaseID)
			case "mutation missing":
				del(c.mutationKey)
			case "foreign malformed":
				put(c.mutationKey, "null", ref.LeaseID)
			case "guard rewrite":
				put(c.guardKey, c.value, ref.LeaseID)
			}
			got, e := b.ResolveOperation(ctx, ref)
			if want == OperationUnknown {
				require.Error(t, e)
			} else {
				require.NoError(t, e)
			}
			require.Equal(t, want, got)
		})
	}
}

func TestResolveOperationMalformedResponse(t *testing.T) {
	for _, defect := range []string{"nil", "cluster", "revision", "count", "nested header", "wrong key", "range count", "more", "nil point", "abort reply lost", "abort wrong type"} {
		t.Run(defect, func(t *testing.T) {
			b, raw, in, _ := operationControlFixture(t, "plain")
			c := admittedOperation(t, b, raw, in.SandboxID, OperationData)
			abort := defect == "abort reply lost" || defect == "abort wrong type"
			if abort {
				_, e := raw.Delete(context.Background(), c.tokenKey)
				require.NoError(t, e)
				_, e = raw.Delete(context.Background(), c.receiptKey)
				require.NoError(t, e)
			}
			b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
				if abort {
					return len(ops) == 1 && ops[0].IsPut()
				}
				return len(ops) == 6
			}, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
				require.NoError(t, e)
				switch defect {
				case "nil":
					return nil, nil
				case "cluster":
					r.Header.ClusterId++
				case "revision":
					r.Header.Revision = 0
				case "count":
					r.Responses = r.Responses[:5]
				case "nested header":
					r.Responses[0].GetResponseRange().Header = r.Header
					r.Responses[0].GetResponseRange().Header.Revision++
				case "wrong key":
					r.Responses[2].GetResponseRange().Kvs[0].Key = []byte("wrong")
				case "range count":
					r.Responses[2].GetResponseRange().Count++
				case "more":
					r.Responses[2].GetResponseRange().More = true
				case "nil point":
					r.Responses[2] = nil
				case "abort reply lost":
					return nil, context.DeadlineExceeded
				case "abort wrong type":
					r.Responses[0] = nil
				}
				return r, e
			}}
			got, e := b.ResolveOperation(context.Background(), c.Reference())
			require.Error(t, e)
			require.Equal(t, OperationUnknown, got)
		})
	}
}
