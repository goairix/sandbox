package etcd

import (
	"context"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"strings"
	"testing"
	"time"
)

func TestTaskFinalSyncOriginalValidation(t *testing.T) {
	for _, fault := range []string{"nil-context", "nil-backend", "nil-claim", "copied-claim", "nil-prepared", "copied-prepared", "foreign-backend", "foreign-claim", "changed-reference", "bad-mode", "bad-digest", "expired", "cancelled"} {
		t.Run(fault, func(t *testing.T) {
			b, c, p := finalizationHostFixture(t)
			ctx := context.Background()
			plan := TaskFinalSyncPlan{"sync", strings.Repeat("c", 64)}
			switch fault {
			case "nil-context":
				ctx = nil
			case "nil-backend":
				b = nil
			case "nil-claim":
				c = nil
			case "copied-claim":
				// Preserve the copied self pointer without copying a sync.Mutex.
				c = &TaskClaim{origin: c.origin, self: c, parentCtx: c.parentCtx, reference: c.reference, deadline: c.deadline}
			case "nil-prepared":
				p = nil
			case "copied-prepared":
				copy := *p
				p = &copy
			case "foreign-backend":
				p.origin = new(Backend)
			case "foreign-claim":
				p.claim = &TaskClaim{}
			case "changed-reference":
				p.claimReference.CreateRevision++
			case "bad-mode":
				plan.Mode = "other"
			case "bad-digest":
				plan.Digest = ""
			case "expired":
				p.deadline = time.Now().Add(-time.Second)
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			r, err := b.ReserveTaskFinalSync(ctx, c, p, nil, plan)
			require.Error(t, err)
			require.NotEqual(t, OutcomeCommitted, r.Outcome)
		})
	}
}

func TestTaskFinalSyncWireLimits(t *testing.T) {
	r := finalSyncRecordFixture(t)
	ns := namespaceFromTaskCloseTest(t, r.Task.Reference)
	key, err := ns.taskFinalSyncAttemptKey(r.Task.Reference)
	require.NoError(t, err)
	identity, err := ns.Key("identity")
	require.NoError(t, err)
	restore, err := ns.Key("restore-epoch")
	require.NoError(t, err)
	b := &Backend{namespace: ns, identityKey: identity, restoreKey: restore, identityValue: "identity", restoreEpoch: r.Task.Reference.RestoreEpoch}
	wire, err := encodeTaskFinalSyncAttempt(r)
	require.NoError(t, err)
	cmps := []clientv3.Cmp{clientv3.Compare(clientv3.CreateRevision(key), "=", 0)}
	require.NoError(t, b.preflightTaskReservation(cmps, key, wire))
	require.ErrorIs(t, b.preflightTaskReservation(cmps, key, strings.Repeat("x", 8193)), ErrInvalidMutation)
	for len(cmps) < 64 {
		cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	}
	require.ErrorIs(t, b.preflightTaskReservation(cmps, key, wire), ErrInvalidMutation)
	_, err = (*Backend)(nil).LoadTaskFinalSyncAttempt(context.Background(), r.Task.Reference)
	require.Error(t, err)
	_, err = b.LoadTaskFinalSyncAttempt(nil, r.Task.Reference)
	require.Error(t, err)
}
