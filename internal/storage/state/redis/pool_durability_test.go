package redis

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	redisclient "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type poolWaitCountHook struct{ waits atomic.Int32 }

func (h *poolWaitCountHook) DialHook(next redisclient.DialHook) redisclient.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) { return next(ctx, network, addr) }
}

func (h *poolWaitCountHook) ProcessHook(next redisclient.ProcessHook) redisclient.ProcessHook {
	return func(ctx context.Context, cmd redisclient.Cmder) error {
		if strings.EqualFold(cmd.Name(), "wait") {
			h.waits.Add(1)
		}
		return next(ctx, cmd)
	}
}

func (h *poolWaitCountHook) ProcessPipelineHook(next redisclient.ProcessPipelineHook) redisclient.ProcessPipelineHook {
	return next
}

func TestFUSEPoolReadsAndDefaultWritesDoNotWaitForReplicas(t *testing.T) {
	skipIfNoRedis(t)
	for _, mode := range []DurabilityMode{DurabilityBestEffort, DurabilityNative, DurabilityReplicaAck} {
		t.Run(string(mode), func(t *testing.T) {
			s := testStore(t)
			hook := &poolWaitCountHook{}
			s.client.AddHook(hook)
			configured := &Store{client: s.client, durability: mode, ackReplicas: 99, ackTimeout: time.Millisecond}
			repo := NewFUSEPoolRepository(configured)
			poolKey := poolTestID("no-wait-pool")
			cleanupFUSEPool(t, s, []string{poolKey}, nil)
			ctx := context.Background()
			if mode != DurabilityReplicaAck {
				locked, err := repo.TryRefillLock(ctx, poolKey, "controller", time.Hour)
				require.NoError(t, err)
				require.True(t, locked)
				renewed, err := repo.RenewRefillLock(ctx, poolKey, "controller", time.Hour)
				require.NoError(t, err)
				require.True(t, renewed)
				require.NoError(t, repo.UnlockRefill(ctx, poolKey, "controller"))
			}
			_, err := repo.ListByPoolKey(ctx, poolKey)
			require.NoError(t, err)
			_, err = repo.ListPoolKeys(ctx)
			require.NoError(t, err)
			_, err = repo.CountPreparingAndPrepared(ctx, poolKey)
			require.NoError(t, err)
			_, err = repo.ServerTime(ctx)
			require.NoError(t, err)
			require.Zero(t, hook.waits.Load())
		})
	}
}

func TestFUSEPoolSafetyWritesRequireReplicaAck(t *testing.T) {
	skipIfNoRedis(t)
	for _, operation := range []string{"create", "bind", "publish", "reserve", "transition", "return", "claim", "confirm", "delete", "lock", "renew", "unlock", "drain"} {
		t.Run(operation, func(t *testing.T) {
			s := testStore(t)
			strong := &Store{client: s.client, durability: DurabilityReplicaAck, ackReplicas: 99, ackTimeout: time.Millisecond}
			best, repo := NewFUSEPoolRepository(s), NewFUSEPoolRepository(strong)
			ctx := context.Background()
			poolKey, id, uid := poolTestID("ack-pool"), poolTestID("ack-preparation"), poolTestID("ack-uid")
			cleanupFUSEPool(t, s, []string{poolKey}, []string{id, uid})
			var current *state.FUSEPoolRecord
			if operation != "lock" {
				require.True(t, mustRefillLock(t, best, poolKey, "controller", time.Hour))
			}
			if operation != "create" && operation != "lock" && operation != "renew" && operation != "unlock" && operation != "drain" {
				require.NoError(t, best.CreatePreparingWithAdmission(ctx, preparingIntent(poolKey, id), "controller", 2, time.Hour))
				current = &state.FUSEPoolRecord{Revision: 1}
				if operation != "bind" {
					var err error
					current, err = best.BindPreparingRuntime(ctx, id, "runtime", uid, "controller", current.Revision)
					require.NoError(t, err)
					if operation != "publish" {
						current, err = best.TransitionWithRefillLock(ctx, id, state.FUSEPoolPreparing, state.FUSEPoolPrepared, "maintainer-a", "controller", current.Revision, 0)
						require.NoError(t, err)
						if operation != "reserve" {
							current, err = best.ReservePrepared(ctx, poolKey, "reservation", time.Hour)
							require.NoError(t, err)
							if operation != "transition" && operation != "return" {
								current, err = best.Transition(ctx, id, state.FUSEPoolReserved, state.FUSEPoolBinding, "reservation", current.Revision)
								require.NoError(t, err)
								current, err = best.Transition(ctx, id, state.FUSEPoolBinding, state.FUSEPoolConsumed, "reservation", current.Revision)
								require.NoError(t, err)
								if operation != "claim" {
									current, err = best.ClaimCleanup(ctx, id, state.FUSEPoolConsumed, "maintainer-a", "reservation", current.Revision, "runtime", uid, "cleaner", time.Hour)
									require.NoError(t, err)
									if operation == "delete" {
										current, err = best.ConfirmCleanupTermination(ctx, id, "cleaner", current.Revision, "runtime", uid, state.FUSEPoolTerminationEvidence{RuntimeUID: uid, ProcessExited: true})
										require.NoError(t, err)
									}
								}
							}
						}
					}
				}
			}
			var result *state.FUSEPoolRecord
			var capability bool
			var err error
			switch operation {
			case "create":
				err = repo.CreatePreparingWithAdmission(ctx, preparingIntent(poolKey, id), "controller", 2, time.Hour)
			case "bind":
				result, err = repo.BindPreparingRuntime(ctx, id, "runtime", uid, "controller", current.Revision)
			case "publish":
				result, err = repo.TransitionWithRefillLock(ctx, id, state.FUSEPoolPreparing, state.FUSEPoolPrepared, "maintainer-a", "controller", current.Revision, 0)
			case "reserve":
				result, err = repo.ReservePrepared(ctx, poolKey, "reservation", time.Hour)
			case "transition":
				result, err = repo.Transition(ctx, id, state.FUSEPoolReserved, state.FUSEPoolBinding, "reservation", current.Revision)
			case "return":
				result, err = repo.ReturnPreparedWithAdmission(ctx, id, "reservation", current.Revision, 2)
			case "claim":
				result, err = repo.ClaimCleanup(ctx, id, state.FUSEPoolConsumed, "maintainer-a", "reservation", current.Revision, "runtime", uid, "cleaner", time.Hour)
			case "confirm":
				result, err = repo.ConfirmCleanupTermination(ctx, id, "cleaner", current.Revision, "runtime", uid, state.FUSEPoolTerminationEvidence{RuntimeUID: uid, ProcessExited: true})
			case "delete":
				capability, err = repo.DeleteCleanup(ctx, id, "cleaner", current.Revision)
			case "lock":
				capability, err = repo.TryRefillLock(ctx, poolKey, "controller", time.Hour)
			case "renew":
				capability, err = repo.RenewRefillLock(ctx, poolKey, "controller", time.Hour)
			case "unlock":
				err = repo.UnlockRefill(ctx, poolKey, "controller")
			case "drain":
				err = repo.DrainRefillLocks(ctx)
			}
			require.ErrorIs(t, err, state.ErrDurabilityUnconfirmed)
			switch operation {
			case "bind", "publish", "reserve", "transition", "return", "claim", "confirm":
				require.NotNil(t, result, "ambiguous ACK must retain the exact committed capability")
				require.Equal(t, id, result.PreparationID)
			case "lock", "renew", "delete":
				require.True(t, capability, "ambiguous ACK must preserve the successful Lua result")
			}
		})
	}
}

func TestFUSEPoolDeleteAbsentCleanupRequiresReplicaAck(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	strong := &Store{client: s.client, durability: DurabilityReplicaAck, ackReplicas: 99, ackTimeout: time.Millisecond}
	confirmed, err := NewFUSEPoolRepository(strong).DeleteCleanup(context.Background(), poolTestID("absent-preparation"), "cleaner", 1)
	require.ErrorIs(t, err, state.ErrDurabilityUnconfirmed)
	require.True(t, confirmed, "absence is an idempotent Lua result, not durable without ACK")
}
