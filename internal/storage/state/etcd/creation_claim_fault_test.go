package etcd

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type creationFaultLease struct {
	clientv3.Lease
	grants, keeps, revokes atomic.Int64
	requestedTTL           atomic.Int64
	grant                  func(*clientv3.LeaseGrantResponse, error) (*clientv3.LeaseGrantResponse, error)
	keep                   func(*clientv3.LeaseKeepAliveResponse, error) (*clientv3.LeaseKeepAliveResponse, error)
	revoke                 func(context.Context, clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error)
}

func (l *creationFaultLease) Grant(ctx context.Context, ttl int64) (*clientv3.LeaseGrantResponse, error) {
	l.grants.Add(1)
	l.requestedTTL.Store(ttl)
	r, e := l.Lease.Grant(ctx, ttl)
	if l.grant != nil {
		return l.grant(r, e)
	}
	return r, e
}
func (l *creationFaultLease) KeepAliveOnce(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseKeepAliveResponse, error) {
	l.keeps.Add(1)
	r, e := l.Lease.KeepAliveOnce(ctx, id)
	if l.keep != nil {
		return l.keep(r, e)
	}
	return r, e
}
func (l *creationFaultLease) Revoke(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
	l.revokes.Add(1)
	if l.revoke != nil {
		return l.revoke(ctx, id)
	}
	return l.Lease.Revoke(ctx, id)
}
func pendingCreationFixture(t *testing.T) (*Backend, *clientv3.Client, AcquireIntentInput) {
	t.Helper()
	b, raw := integrationBackend(t)
	input := acquisitionInput(t, "pending-claim")
	_, err := b.AcquireIntent(context.Background(), input)
	require.NoError(t, err)
	return b, raw, input
}

func TestCreationClaimUnknownGrantNeverReturnsCapability(t *testing.T) {
	for _, kind := range []string{"known unknown", "nil unknown", "nil success", "wrong cluster", "missing header", "zero id", "zero ttl"} {
		t.Run(kind, func(t *testing.T) {
			b, raw, input := pendingCreationFixture(t)
			var original clientv3.LeaseID
			lease := &creationFaultLease{Lease: b.client.Lease, grant: func(r *clientv3.LeaseGrantResponse, e error) (*clientv3.LeaseGrantResponse, error) {
				require.NoError(t, e)
				original = r.ID
				switch kind {
				case "known unknown":
					return r, context.DeadlineExceeded
				case "nil unknown":
					return nil, context.DeadlineExceeded
				case "nil success":
					return nil, nil
				case "wrong cluster":
					header := *r.ResponseHeader
					header.ClusterId++
					r.ResponseHeader = &header
				case "missing header":
					r.ResponseHeader = nil
				case "zero id":
					r.ID = 0
				case "zero ttl":
					r.TTL = 0
				}
				return r, nil
			}}
			b.client.Lease = lease
			c, err := b.ClaimCreation(context.Background(), input.Workspace, input.IntentID, "worker", 30*time.Second)
			require.Nil(t, c)
			require.Error(t, err)
			if kind == "known unknown" || kind == "zero ttl" {
				require.Equal(t, int64(1), lease.revokes.Load())
				ttl, e := raw.TimeToLive(context.Background(), original)
				require.NoError(t, e)
				require.Equal(t, int64(-1), ttl.TTL)
			} else {
				require.Zero(t, lease.revokes.Load(), "cannot revoke a lease without known original cluster evidence")
				_, e := raw.Revoke(context.Background(), original)
				require.NoError(t, e)
			}
			owner, err := b.LoadOwner(context.Background(), input.Workspace)
			require.NoError(t, err)
			require.NotNil(t, owner)
		})
	}
}
func TestCreationClaimLostReplyNeverReturnsCapability(t *testing.T) {
	b, raw, input := pendingCreationFixture(t)
	ik, err := b.namespace.intentKey(input.Workspace.Partition(), input.IntentID)
	require.NoError(t, err)
	b.client.KV = &faultKV{KV: b.client.KV, match: putsKey(ik + "/claim"), after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
		require.NoError(t, e)
		require.True(t, r.Succeeded)
		return nil, context.DeadlineExceeded
	}}
	c, err := b.ClaimCreation(context.Background(), input.Workspace, input.IntentID, "worker", 30*time.Second)
	require.Nil(t, c)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	got, err := raw.Get(context.Background(), ik+"/", clientv3.WithPrefix())
	require.NoError(t, err)
	require.Empty(t, got.Kvs)
	owner, err := b.LoadOwner(context.Background(), input.Workspace)
	require.NoError(t, err)
	require.NotNil(t, owner)
}
func TestCreationClaimDelayedResponseUsesSendDeadline(t *testing.T) {
	for _, kind := range []string{"grant", "claim"} {
		t.Run(kind, func(t *testing.T) {
			b, raw, input := pendingCreationFixture(t)
			ik, err := b.namespace.intentKey(input.Workspace.Partition(), input.IntentID)
			require.NoError(t, err)
			if kind == "grant" {
				b.client.Lease = &creationFaultLease{Lease: b.client.Lease, grant: func(r *clientv3.LeaseGrantResponse, e error) (*clientv3.LeaseGrantResponse, error) {
					require.NoError(t, e)
					time.Sleep(time.Duration(r.TTL)*time.Second + 50*time.Millisecond)
					return r, e
				}}
			} else {
				var grantedTTL int64
				b.client.Lease = &creationFaultLease{Lease: b.client.Lease, grant: func(r *clientv3.LeaseGrantResponse, e error) (*clientv3.LeaseGrantResponse, error) {
					require.NoError(t, e)
					grantedTTL = r.TTL
					return r, e
				}}
				b.client.KV = &faultKV{KV: b.client.KV, match: putsKey(ik + "/claim"), after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
					require.NoError(t, e)
					require.True(t, r.Succeeded)
					time.Sleep(time.Duration(grantedTTL)*time.Second + 50*time.Millisecond)
					return r, e
				}}
			}
			c, err := b.ClaimCreation(context.Background(), input.Workspace, input.IntentID, "worker", time.Second)
			require.Nil(t, c)
			require.ErrorIs(t, err, ErrGuardExpired)
			got, err := raw.Get(context.Background(), ik+"/", clientv3.WithPrefix())
			require.NoError(t, err)
			require.Empty(t, got.Kvs)
		})
	}
}

func TestCreationClaimDelayedRenewCannotRevive(t *testing.T) {
	b, raw, _, c := claimFixture(t)
	stage, key := creationStage(t, b, c, "delayed-renew")
	c.mu.Lock()
	c.deadline = time.Now().Add(150 * time.Millisecond)
	c.mu.Unlock()
	lease := &creationFaultLease{Lease: b.client.Lease, keep: func(r *clientv3.LeaseKeepAliveResponse, e error) (*clientv3.LeaseKeepAliveResponse, error) {
		require.NoError(t, e)
		require.Positive(t, r.TTL)
		time.Sleep(200 * time.Millisecond)
		return r, e
	}}
	b.client.Lease = lease
	err := b.RenewCreationClaim(context.Background(), c)
	require.ErrorIs(t, err, ErrGuardExpired, "a response with a future candidate deadline cannot revive an expired original local deadline")
	_, err = b.creationClaimComparisons(c)
	require.ErrorIs(t, err, ErrGuardExpired)
	require.ErrorIs(t, b.RenewCreationClaim(context.Background(), c), ErrGuardExpired)
	require.Equal(t, int64(1), lease.keeps.Load())
	// Local loss did not revoke the original server Lease or retract Stage copies.
	outcome, err := b.CommitStage(context.Background(), stage)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	got, err := raw.Get(context.Background(), key)
	require.NoError(t, err)
	require.Len(t, got.Kvs, 1)
	require.NoError(t, b.ReleaseCreationClaim(context.Background(), c))
}
func TestCreationClaimRenewCancelledAfterReplyStopsCapability(t *testing.T) {
	b, _, _, c := claimFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lease := &creationFaultLease{Lease: b.client.Lease, keep: func(r *clientv3.LeaseKeepAliveResponse, e error) (*clientv3.LeaseKeepAliveResponse, error) {
		require.NoError(t, e)
		cancel()
		return r, e
	}}
	b.client.Lease = lease
	require.ErrorIs(t, b.RenewCreationClaim(ctx, c), context.Canceled)
	_, err := b.creationClaimComparisons(c)
	require.ErrorIs(t, err, ErrGuardExpired)
	require.NoError(t, b.ReleaseCreationClaim(context.Background(), c))
}

func TestCreationClaimUnknownRenewStopsSameCapability(t *testing.T) {
	b, raw, _, c := claimFixture(t)
	stage, key := creationStage(t, b, c, "unknown-renew")
	lease := &creationFaultLease{Lease: b.client.Lease, keep: func(r *clientv3.LeaseKeepAliveResponse, e error) (*clientv3.LeaseKeepAliveResponse, error) {
		require.NoError(t, e)
		require.Positive(t, r.TTL)
		return nil, context.DeadlineExceeded
	}}
	b.client.Lease = lease
	require.ErrorIs(t, b.RenewCreationClaim(context.Background(), c), ErrOutcomeUnknown)
	_, err := b.creationClaimComparisons(c)
	require.ErrorIs(t, err, ErrGuardExpired)
	require.ErrorIs(t, b.RenewCreationClaim(context.Background(), c), ErrGuardExpired)
	require.Equal(t, int64(1), lease.keeps.Load())
	// Unknown renew stops local use, but the already copied Stage is still arbitrated by server evidence.
	outcome, err := b.CommitStage(context.Background(), stage)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	got, err := raw.Get(context.Background(), key)
	require.NoError(t, err)
	require.Len(t, got.Kvs, 1)
	require.NoError(t, b.ReleaseCreationClaim(context.Background(), c))
}
func TestCreationClaimPositiveRenewAfterRevocationCannotCommit(t *testing.T) {
	b, raw, _, c := claimFixture(t)
	stage, key := creationStage(t, b, c, "revoked-positive-renew")
	b.client.Lease = &creationFaultLease{Lease: b.client.Lease, keep: func(r *clientv3.LeaseKeepAliveResponse, e error) (*clientv3.LeaseKeepAliveResponse, error) {
		require.NoError(t, e)
		require.Positive(t, r.TTL)
		_, e = raw.Revoke(context.Background(), r.ID)
		require.NoError(t, e)
		return r, nil
	}}
	// A positive response cannot prove continued ownership; the actual domain Txn must CAS the original keys.
	require.NoError(t, b.RenewCreationClaim(context.Background(), c))
	assertCreationStageAborted(t, b, raw, stage, key)
	require.ErrorIs(t, b.RenewCreationClaim(context.Background(), c), ErrGuardExpired)
	require.NoError(t, b.ReleaseCreationClaim(context.Background(), c))
}
func TestCreationClaimRenewResponseValidation(t *testing.T) {
	for _, kind := range []string{"nil", "no header", "wrong cluster", "wrong id", "zero ttl", "negative ttl", "oversized ttl"} {
		t.Run(kind, func(t *testing.T) {
			b, _, _, c := claimFixture(t)
			lease := &creationFaultLease{Lease: b.client.Lease, keep: func(r *clientv3.LeaseKeepAliveResponse, e error) (*clientv3.LeaseKeepAliveResponse, error) {
				require.NoError(t, e)
				switch kind {
				case "nil":
					return nil, nil
				case "no header":
					r.ResponseHeader = nil
				case "wrong cluster":
					header := *r.ResponseHeader
					header.ClusterId++
					r.ResponseHeader = &header
				case "wrong id":
					r.ID++
				case "zero ttl":
					r.TTL = 0
				case "negative ttl":
					r.TTL = -1
				case "oversized ttl":
					r.TTL = 86401
				}
				return r, e
			}}
			b.client.Lease = lease
			require.Error(t, b.RenewCreationClaim(context.Background(), c))
			_, err := b.creationClaimComparisons(c)
			require.ErrorIs(t, err, ErrGuardExpired)
			require.ErrorIs(t, b.RenewCreationClaim(context.Background(), c), ErrGuardExpired)
			require.Equal(t, int64(1), lease.keeps.Load())
			require.NoError(t, b.ReleaseCreationClaim(context.Background(), c))
		})
	}
}
func TestCreationClaimConcurrentRenewSerializesAndCountsWaitContext(t *testing.T) {
	b, _, _, c := claimFixture(t)
	ctx := context.Background()
	arrived, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unlock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unlock)
	var active, maximum atomic.Int64
	lease := &creationFaultLease{Lease: b.client.Lease, keep: func(r *clientv3.LeaseKeepAliveResponse, e error) (*clientv3.LeaseKeepAliveResponse, error) {
		if e != nil {
			return r, e
		}
		current := active.Add(1)
		for previous := maximum.Load(); current > previous; previous = maximum.Load() {
			if maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		select {
		case arrived <- struct{}{}:
		default:
		}
		<-release
		active.Add(-1)
		return r, e
	}}
	b.client.Lease = lease
	first := make(chan error, 1)
	go func() { first <- b.RenewCreationClaim(ctx, c) }()
	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("renew not intercepted")
	}
	waitingCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	second := make(chan error, 1)
	go func() { second <- b.RenewCreationClaim(waitingCtx, c) }()
	<-waitingCtx.Done()
	unlock()
	require.NoError(t, <-first)
	require.ErrorIs(t, <-second, context.DeadlineExceeded)
	require.Equal(t, int64(1), lease.keeps.Load())
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() { results <- b.RenewCreationClaim(ctx, c) }()
	}
	for i := 0; i < 8; i++ {
		require.NoError(t, <-results)
	}
	require.Equal(t, int64(1), maximum.Load())
	require.Equal(t, int64(9), lease.keeps.Load())
	require.NoError(t, b.ReleaseCreationClaim(ctx, c))
}

func TestCreationClaimCancellationAfterReplyNeverReturnsCapability(t *testing.T) {
	for _, kind := range []string{"grant", "claim"} {
		t.Run(kind, func(t *testing.T) {
			b, raw, input := pendingCreationFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ik, err := b.namespace.intentKey(input.Workspace.Partition(), input.IntentID)
			require.NoError(t, err)
			if kind == "grant" {
				b.client.Lease = &creationFaultLease{Lease: b.client.Lease, grant: func(r *clientv3.LeaseGrantResponse, e error) (*clientv3.LeaseGrantResponse, error) {
					require.NoError(t, e)
					cancel()
					return r, e
				}}
			} else {
				b.client.KV = &faultKV{KV: b.client.KV, match: putsKey(ik + "/claim"), after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
					require.NoError(t, e)
					require.True(t, r.Succeeded)
					cancel()
					return r, e
				}}
			}
			c, err := b.ClaimCreation(ctx, input.Workspace, input.IntentID, "worker", 30*time.Second)
			require.Nil(t, c)
			require.ErrorIs(t, err, context.Canceled)
			got, err := raw.Get(context.Background(), ik+"/", clientv3.WithPrefix())
			require.NoError(t, err)
			require.Empty(t, got.Kvs)
			owner, err := b.LoadOwner(context.Background(), input.Workspace)
			require.NoError(t, err)
			require.NotNil(t, owner)
		})
	}
}
