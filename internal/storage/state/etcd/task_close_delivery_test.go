package etcd

import (
	"context"
	"crypto/ed25519"
	"fmt"
	clientv3 "go.etcd.io/etcd/client/v3"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTaskCloseDeliveryRejectsOrigin(t *testing.T) {
	b := new(Backend)
	for _, p := range []*PreparedTaskCloseData{nil, {}} {
		r, err := b.DeliverTaskCloseData(context.Background(), p, nil)
		require.Error(t, err)
		require.Nil(t, r.Receipt)
	}
	r, err := b.QueryTaskCloseData(context.Background(), TaskCloseDataReference{}, nil)
	require.Error(t, err)
	require.Nil(t, r.Receipt)
}
func TestTaskCloseDeliveryNative(t *testing.T) {
	for _, lose := range []bool{false, true} {
		t.Run(fmt.Sprint(lose), func(t *testing.T) {
			f := newTaskDeliveryFixture(t, true)
			prepared, err := f.b.PrepareTaskCloseData(f.ctx, f.c)
			require.NoError(t, err)
			require.NotNil(t, prepared.Prepared)
			target := newTaskDeliveryTarget(t, f, prepared.Prepared)
			target.lose.Store(lose)
			certCalls, signCalls := f.p.certCalls, f.p.signCalls
			result, err := f.b.DeliverTaskCloseData(f.ctx, prepared.Prepared, target.destination)
			if lose {
				require.ErrorIs(t, err, ErrDeliveryUnknown)
				require.Nil(t, result.Receipt)
			} else {
				require.NoError(t, err)
				require.NotNil(t, result.Receipt)
			}
			require.Equal(t, int32(1), target.closes.Load())
			target.lose.Store(false)
			_, err = f.b.DeliverTaskCloseData(f.ctx, prepared.Prepared, target.destination)
			require.Error(t, err)
			require.Equal(t, int32(1), target.closes.Load())
			result, err = f.b.QueryTaskCloseData(f.ctx, prepared.Reference, target.destination)
			require.NoError(t, err)
			require.NotNil(t, result.Receipt)
			require.Equal(t, int32(1), target.queries.Load())
			require.Equal(t, "data_closed", result.Receipt.State())
			// Revoke the original native lease; query-only restart has no claim/provider.
			_, err = f.raw.Revoke(f.ctx, clientv3.LeaseID(f.c.reference.LeaseID))
			require.NoError(t, err)
			id, err := decodeIdentity(f.b.identityValue, f.b.namespace)
			require.NoError(t, err)
			id.RestoreEpoch = f.b.restoreEpoch
			root := ed25519.NewKeyFromSeed(make([]byte, 32))
			query, err := New(f.ctx, Options{Endpoints: f.raw.Endpoints(), Namespace: f.b.namespace, Identity: id, AllowInsecureLoopback: true, PublicationTrust: &RuntimePublicationTrust{AuthorityID: f.b.publicationAuthorityID, Target: f.b.publicationTarget, Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}, Clock: testAuthorityClock(func(context.Context) (ClockObservation, error) {
				return ClockObservation{UTC: f.now.Add(35 * time.Second)}, nil
			})})
			require.NoError(t, err)
			defer query.Close()
			target.clock.now.Store(f.now.Add(35 * time.Second).UnixNano())
			result, err = query.QueryTaskCloseData(f.ctx, prepared.Reference, target.destination)
			require.NoError(t, err)
			require.NotNil(t, result.Receipt)
			require.Equal(t, int32(1), target.closes.Load())
			no, err := query.PrepareTaskCloseData(f.ctx, f.c)
			require.Error(t, err)
			require.Nil(t, no.Prepared)
			require.Equal(t, certCalls, f.p.certCalls)
			require.Equal(t, signCalls, f.p.signCalls)
		})
	}
}
func TestTaskCloseDeliveryStale(t *testing.T) {
	for _, boundary := range []string{"before-dial", "during-handshake", "before-bytes", "fixed-deadline"} {
		t.Run(boundary, func(t *testing.T) {
			f := newTaskDeliveryFixture(t, false)
			prepared, err := f.b.PrepareTaskCloseData(f.ctx, f.c)
			require.NoError(t, err)
			require.NotNil(t, prepared.Prepared)
			target := newTaskDeliveryTarget(t, f, prepared.Prepared)
			revoked, dialed, checks := false, false, 0
			originalDial := target.dial
			target.dial = func(ctx context.Context) (net.Conn, error) { dialed = true; return originalDial(ctx) }
			revoke := func() {
				_, err := f.raw.Revoke(f.ctx, clientv3.LeaseID(f.c.reference.LeaseID))
				require.NoError(t, err)
				revoked = true
			}
			if boundary == "before-dial" {
				revoke()
			} else if boundary == "before-bytes" {
				original := f.b.client.KV
				defer func() { f.b.client.KV = original }()
				f.b.client.KV = &faultKV{KV: original, match: func(ops []clientv3.Op) bool {
					if len(ops) == 1 && ops[0].IsGet() && string(ops[0].KeyBytes()) == f.b.identityKey {
						checks++
						return checks == 3
					}
					return false
				}, before: revoke}
			} else {
				original := target.dial
				target.dial = func(ctx context.Context) (net.Conn, error) {
					conn, err := original(ctx)
					if err == nil {
						if boundary == "fixed-deadline" {
							prepared.Prepared.draft.deadline = time.Now().Add(-time.Second)
						} else {
							revoke()
						}
					}
					return conn, err
				}
			}
			result, err := f.b.DeliverTaskCloseData(f.ctx, prepared.Prepared, target.destination)
			if boundary == "fixed-deadline" {
				require.ErrorIs(t, err, ErrGuardExpired)
				require.False(t, revoked)
			} else {
				require.ErrorIs(t, err, ErrConflict)
				require.True(t, revoked, "must reach actual revoke boundary")
			}
			require.Equal(t, boundary != "before-dial", dialed, "must reach intended transport boundary")
			if boundary == "before-bytes" {
				require.Equal(t, 3, checks, "must reach third original-fence read before bytes")
			}
			require.Nil(t, result.Receipt)
			require.Zero(t, target.closes.Load())
			require.Equal(t, "open", target.journal.Status().Gate.GateState)
		})
	}
}
