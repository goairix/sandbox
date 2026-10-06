package etcd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Removing the atomic token/receipt/lock writes, the mutation exclusion, or
// isolating by anything but original Lease must break this real-server test.
func TestBeginOperationAtomicAdmission(t *testing.T) {
	ctx := context.Background()
	b, raw, in, keys := operationControlFixture(t, "plain")
	before, err := b.readDomain(ctx, keys...)
	require.NoError(t, err)
	lease := &creationFaultLease{Lease: b.client.Lease}
	b.client.Lease = lease
	begin := func(kind OperationKind) BeginOperationResult {
		t.Helper()
		result, err := b.BeginOperation(ctx, BeginOperationInput{SandboxID: in.SandboxID, RequestID: "op-request", Kind: kind})
		require.NoError(t, err)
		require.Equal(t, OperationCommitted, result.Outcome)
		require.NotNil(t, result.Capability)
		require.Equal(t, result.Reference, result.Capability.Reference())
		require.NoError(t, result.GuardCleanupError)
		t.Cleanup(func() { _, _ = raw.Revoke(ctx, clientv3.LeaseID(result.Reference.LeaseID)) })
		token, guard, receipt, mutation, err := b.namespace.operationKeys(result.Reference)
		require.NoError(t, err)
		got, err := b.readDomain(ctx, token, guard, receipt, mutation)
		require.NoError(t, err)
		require.NoError(t, validateOperationCompletion(got[0], got[2]))
		require.Less(t, got[1].CreateRevision, got[0].CreateRevision)
		for _, kv := range got[:3] {
			require.Equal(t, result.Reference.LeaseID, kv.Lease)
		}
		require.Equal(t, got[1].Value, got[0].Value)
		if kind == OperationMutation {
			require.NotNil(t, got[3])
			require.Equal(t, got[0].CreateRevision, got[3].CreateRevision)
			require.Equal(t, got[0].Lease, got[3].Lease)
			require.Equal(t, got[0].Value, got[3].Value)
		} else {
			require.Nil(t, got[3])
		}
		var record OperationRecord
		require.NoError(t, decodeOperationRecord(got[0], &record))
		require.Equal(t, before[1].ModRevision, record.ControlRevision)
		control := result.Capability.Control()
		control.Runtime.UID = "changed outside capability"
		require.Equal(t, "uid", result.Capability.Control().Runtime.UID)
		result.Reference.RequestID = "outside"
		require.Equal(t, "op-request", result.Capability.Reference().RequestID)
		result.Reference = result.Capability.Reference()
		return result
	}
	a, c := begin(OperationData), begin(OperationData)
	require.NotEqual(t, a.Reference.LeaseID, c.Reference.LeaseID)
	require.NotEqual(t, a.Reference.OperationID, c.Reference.OperationID)
	m := begin(OperationMutation)
	for _, kind := range []OperationKind{OperationData, OperationMutation} {
		rejected, err := b.BeginOperation(ctx, BeginOperationInput{SandboxID: in.SandboxID, RequestID: "competing", Kind: kind})
		require.ErrorIs(t, err, ErrConflict)
		require.Equal(t, OperationAborted, rejected.Outcome)
		require.Nil(t, rejected.Capability)
	}
	_, err = raw.Revoke(ctx, clientv3.LeaseID(a.Reference.LeaseID))
	require.NoError(t, err)
	for _, ref := range []OperationReference{c.Reference, m.Reference} {
		token, _, _, _, err := b.namespace.operationKeys(ref)
		require.NoError(t, err)
		got, err := raw.Get(ctx, token)
		require.NoError(t, err)
		require.Len(t, got.Kvs, 1, "one local termination must not revoke any other operation")
	}
	after, err := b.readDomain(ctx, keys...)
	require.NoError(t, err)
	require.Equal(t, before, after, "admission cannot change any permanent authority record")
	require.Equal(t, int64(30), lease.requestedTTL.Load())
	require.Zero(t, lease.keeps.Load())
}

func TestBeginOperationPreGrantValidation(t *testing.T) {
	for _, scenario := range []string{"nil context", "canceled", "kind", "exclusive", "sandbox", "request", "clock missing", "clock unknown", "clock budget", "expired", "expiry margin", "record budget"} {
		t.Run(scenario, func(t *testing.T) {
			b, raw, in, keys := operationControlFixture(t, "plain")
			ctx := context.Background()
			input := BeginOperationInput{SandboxID: in.SandboxID, RequestID: "request", Kind: OperationData}
			want := ErrInvalidRecord
			switch scenario {
			case "nil context":
				ctx = nil
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
				want = context.Canceled
			case "kind":
				input.Kind = ""
			case "exclusive":
				input.Kind = "exclusive"
			case "sandbox":
				input.SandboxID = "../invalid"
			case "request":
				input.RequestID = "../invalid"
			case "clock missing":
				b.authorityClock = nil
				want = ErrInvalidConfiguration
			case "clock unknown":
				b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) { return ClockObservation{}, context.DeadlineExceeded })
				want = context.DeadlineExceeded
			case "clock budget":
				b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
					return ClockObservation{UTC: time.Now().UTC(), Uncertainty: time.Second + 1}, nil
				})
			case "expired", "expiry margin":
				control, err := b.LoadControl(ctx, in.Workspace.Partition(), in.SandboxID)
				require.NoError(t, err)
				now := control.ExpiresAt
				if scenario == "expiry margin" {
					now = now.Add(-time.Second)
				}
				b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) { return ClockObservation{UTC: now}, nil })
				want = ErrOperationAdmissionClosed
			case "record budget":
				for _, key := range keys {
					got, err := raw.Get(ctx, key)
					require.NoError(t, err)
					padded := string(got.Kvs[0].Value) + strings.Repeat(" ", 60000-len(got.Kvs[0].Value))
					_, err = raw.Delete(ctx, key)
					require.NoError(t, err)
					_, err = raw.Put(ctx, key, padded)
					require.NoError(t, err)
				}
				want = ErrCorruptRecord
			}
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			result, err := b.BeginOperation(ctx, input)
			require.ErrorIs(t, err, want)
			require.Nil(t, result.Capability)
			require.Equal(t, OperationUnknown, result.Outcome)
			require.Zero(t, lease.grants.Load(), "validation must finish before any native Lease Grant")
		})
	}
}

func TestBeginOperationOriginalFences(t *testing.T) {
	for _, scenario := range []string{"destroying", "placement rewrite", "control rewrite", "owner rewrite", "fence rewrite", "index rewrite", "placement recreate", "control recreate", "owner recreate", "fence recreate", "index recreate", "restore", "guard recreate", "clock after guard"} {
		t.Run(scenario, func(t *testing.T) {
			b, raw, in, keys := operationControlFixture(t, "plain")
			ctx := context.Background()
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			lease.grant = func(r *clientv3.LeaseGrantResponse, e error) (*clientv3.LeaseGrantResponse, error) {
				require.NoError(t, e)
				switch scenario {
				case "destroying":
					operationChangePoint(t, raw, keys[1], "phase", PhaseDestroying)
				case "restore":
					_, e = raw.Put(ctx, b.restoreKey, "changed-restore")
					require.NoError(t, e)
				default:
					for i, name := range []string{"placement", "control", "owner", "fence", "index"} {
						if strings.HasPrefix(scenario, name+" ") {
							got, e := raw.Get(ctx, keys[i])
							require.NoError(t, e)
							if strings.HasSuffix(scenario, "recreate") {
								_, e = raw.Delete(ctx, keys[i])
								require.NoError(t, e)
							}
							_, e = raw.Put(ctx, keys[i], string(got.Kvs[0].Value))
							require.NoError(t, e)
						}
					}
				}
				return r, e
			}
			if scenario == "guard recreate" || scenario == "clock after guard" {
				var guard string
				b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
					if len(ops) == 1 && ops[0].IsPut() && strings.HasSuffix(string(ops[0].KeyBytes()), "/guard") {
						guard = string(ops[0].KeyBytes())
						return true
					}
					return false
				}, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
					require.NoError(t, e)
					require.True(t, r.Succeeded)
					if scenario == "clock after guard" {
						b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) { return ClockObservation{}, context.DeadlineExceeded })
					} else {
						got, e := raw.Get(ctx, guard)
						require.NoError(t, e)
						_, e = raw.Delete(ctx, guard)
						require.NoError(t, e)
						_, e = raw.Put(ctx, guard, string(got.Kvs[0].Value), clientv3.WithLease(clientv3.LeaseID(got.Kvs[0].Lease)))
						require.NoError(t, e)
					}
					return r, e
				}}
			}
			result, err := b.BeginOperation(ctx, BeginOperationInput{SandboxID: in.SandboxID, RequestID: "fenced", Kind: OperationData})
			want := ErrConflict
			if scenario == "restore" {
				want = ErrIdentityMismatch
			}
			if scenario == "clock after guard" {
				want = context.DeadlineExceeded
			}
			require.ErrorIs(t, err, want)
			require.Nil(t, result.Capability)
			require.Equal(t, int64(1), lease.grants.Load())
			require.Equal(t, int64(1), lease.revokes.Load())
			token, _, receipt, _, e := b.namespace.operationKeys(result.Reference)
			require.NoError(t, e)
			for _, key := range []string{token, receipt} {
				got, e := raw.Get(ctx, key)
				require.NoError(t, e)
				require.Empty(t, got.Kvs)
			}
		})
	}
}
