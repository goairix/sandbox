package etcd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Durable coverage belongs to the private backfill CAS protocol. Generic
// Stage must not create or overwrite its checkpoint family or descendants.
func TestCleanupBackfillReservedStageKey(t *testing.T) {
	namespace, err := NewNamespace("/codex-test", "tasks", "cell")
	require.NoError(t, err)

	for name, key := range map[string]string{
		"checkpoint": "/codex-test/tasks/cell/p/aa/cleanup-pending-backfill",
		"descendant": "/codex-test/tasks/cell/p/aa/cleanup-pending-backfill/child",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := prepareMutation(namespace, Mutation{Writes: []Write{{
				Key: key, Value: []byte(`{"version":1}`),
			}}})
			require.ErrorIs(t, err, ErrInvalidMutation)
		})
	}
}

func TestCleanupBackfillRecords(t *testing.T) {
	n, err := NewNamespace("/codex-test", "tasks", "cell")
	require.NoError(t, err)
	r := cleanupBackfillRecord{Version: 1, Namespace: n.Root(), RestoreEpoch: "epoch", IdentityDigest: strings.Repeat("a", 64), Partition: 0xaa, LegacyCutoffRevision: 5, ReadViewRevision: 9, Frontier: "00000001-1111-4111-8111-111111111111", Phase: "list"}
	wire, err := encodeCleanupBackfill(r)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf(`{"version":1,"namespace":%q,"restore_epoch":"epoch","identity_digest":%q,"partition":170,"legacy_cutoff_revision":5,"read_view_revision":9,"frontier":%q,"phase":"list"}`, n.Root(), r.IdentityDigest, r.Frontier), wire)
	key, err := n.cleanupBackfillKey(0xaa)
	require.NoError(t, err)
	require.Equal(t, "/codex-test/tasks/cell/p/aa/cleanup-pending-backfill", key)
	kv := &mvccpb.KeyValue{Key: []byte(key), Value: []byte(wire), Version: 2, CreateRevision: 6, ModRevision: 10}
	got, err := decodeCleanupBackfill(n, "epoch", r.IdentityDigest, 0xaa, kv, 11)
	require.NoError(t, err, "a checkpoint is mutable permanent metadata")
	require.Equal(t, r, got)

	t.Run("strict-schema-and-envelope", func(t *testing.T) {
		refuse := func(value []byte) {
			t.Helper()
			bad := workerTestKV(kv)
			bad.Value = value
			_, err := decodeCleanupBackfill(n, "epoch", r.IdentityDigest, 0xaa, bad, 11)
			require.ErrorIs(t, err, ErrCorruptRecord)
		}
		taskBadFields(t, []byte(wire), refuse, "backfill")
		for _, value := range [][]byte{append([]byte(wire), ' '), []byte("null"), bytes.Repeat([]byte("x"), 2049)} {
			refuse(value)
		}
		for _, change := range []func(*mvccpb.KeyValue){
			func(k *mvccpb.KeyValue) { k.Lease = 1 },
			func(k *mvccpb.KeyValue) { k.Version = 1 },
			func(k *mvccpb.KeyValue) { k.CreateRevision = 5 },
			func(k *mvccpb.KeyValue) { k.ModRevision = 9 },
			func(k *mvccpb.KeyValue) { k.ModRevision = 12 },
			func(k *mvccpb.KeyValue) { k.Key = append(k.Key, []byte("/child")...) },
		} {
			bad := workerTestKV(kv)
			change(bad)
			_, err := decodeCleanupBackfill(n, "epoch", r.IdentityDigest, 0xaa, bad, 11)
			require.ErrorIs(t, err, ErrCorruptRecord)
		}
		for _, identity := range []struct {
			epoch, digest string
			partition     uint8
		}{
			{"other-epoch", r.IdentityDigest, 0xaa},
			{"epoch", strings.Repeat("b", 64), 0xaa},
			{"epoch", r.IdentityDigest, 0xab},
		} {
			_, err := decodeCleanupBackfill(n, identity.epoch, identity.digest, identity.partition, kv, 11)
			require.ErrorIs(t, err, ErrCorruptRecord)
		}
	})
	t.Run("refresh-cannot-advance-coverage", func(t *testing.T) {
		initial := r
		initial.ReadViewRevision, initial.Frontier = initial.LegacyCutoffRevision, ""
		require.NoError(t, validateCleanupBackfillRefresh(nil, initial))
		require.Error(t, validateCleanupBackfillRefresh(nil, r))
		next := r
		next.ReadViewRevision++
		require.NoError(t, validateCleanupBackfillRefresh(&r, next))
		for _, change := range []func(*cleanupBackfillRecord){
			func(v *cleanupBackfillRecord) { v.LegacyCutoffRevision++ },
			func(v *cleanupBackfillRecord) { v.ReadViewRevision = r.ReadViewRevision },
			func(v *cleanupBackfillRecord) { v.Frontier = "" },
			func(v *cleanupBackfillRecord) { v.Phase = "complete" },
			func(v *cleanupBackfillRecord) { v.RestoreEpoch = "other" },
			func(v *cleanupBackfillRecord) { v.IdentityDigest = strings.Repeat("b", 64) },
			func(v *cleanupBackfillRecord) { v.Partition++ },
		} {
			bad := next
			change(&bad)
			require.Error(t, validateCleanupBackfillRefresh(&r, bad))
		}
		complete := r
		complete.Phase, complete.Frontier = "complete", ""
		require.NoError(t, complete.Validate(), "an empty historical set can have a completed checkpoint")
		require.Error(t, validateCleanupBackfillRefresh(&complete, next))
	})
	t.Run("maximum-record-bound", func(t *testing.T) {
		large := r
		n, err := NewNamespace("/"+strings.Repeat("p", 252), strings.Repeat("s", 128), strings.Repeat("c", 128))
		require.NoError(t, err)
		large.Namespace, large.RestoreEpoch = n.Root(), strings.Repeat("e", 128)
		large.LegacyCutoffRevision, large.ReadViewRevision = math.MaxInt64-2, math.MaxInt64-1
		wire, err := encodeCleanupBackfill(large)
		require.NoError(t, err)
		require.Equal(t, 2048, cleanupBackfillBytes)
		require.LessOrEqual(t, len(wire), 2048)
		for _, change := range []func(*cleanupBackfillRecord){
			func(v *cleanupBackfillRecord) { v.Version++ },
			func(v *cleanupBackfillRecord) { v.IdentityDigest = strings.Repeat("A", 64) },
			func(v *cleanupBackfillRecord) { v.LegacyCutoffRevision = 0 },
			func(v *cleanupBackfillRecord) { v.ReadViewRevision = 4 },
			func(v *cleanupBackfillRecord) { v.Frontier = "not-a-uuid" },
			func(v *cleanupBackfillRecord) { v.Phase = "unknown" },
		} {
			bad := r
			change(&bad)
			_, err := encodeCleanupBackfill(bad)
			require.Error(t, err)
		}
	})
}

type backfillBootstrapFixture struct {
	b              *Backend
	server         *workerTestServer
	session        *WorkerSession
	partition      *WorkerPartitionClaim
	ranges, claims *WorkerBudgetShare
}

// Configure the genuine leased session before creation. Mutating a live TTL,
// deadline or lease would bypass the admission contract this test exercises.
func newBackfillBootstrapFixture(t *testing.T, ttl time.Duration) *backfillBootstrapFixture {
	t.Helper()
	server, options := workerTestEndpoint(t)
	options.WorkerControl.SessionTTL = ttl
	b, err := New(context.Background(), options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	session, err := b.OpenWorkerSession(context.Background(), "backfill-worker")
	require.NoError(t, err)
	partition, err := b.ClaimWorkerPartition(context.Background(), session, 0xaa)
	require.NoError(t, err)
	ranges, err := b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetRange, 0)
	require.NoError(t, err)
	claims, err := b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetClaim, 0)
	require.NoError(t, err)
	return &backfillBootstrapFixture{b: b, server: server, session: session, partition: partition, ranges: ranges, claims: claims}
}

// Freeze replenishment to observe spending of the original local bucket.
func backfillBootstrapCredit(b *Backend, share *WorkerBudgetShare, tokens uint64) {
	b.archiveLife.mu.Lock()
	share.bucket.last, share.bucket.tokens, share.bucket.remainder = time.Now().Add(time.Hour), tokens, 0
	b.archiveLife.mu.Unlock()
}

func backfillBootstrapRequests(f *backfillBootstrapFixture) []*pb.TxnRequest {
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	return append([]*pb.TxnRequest(nil), f.server.requests...)
}

func backfillBootstrapRead(t *testing.T, f *backfillBootstrapFixture) *cleanupBackfillObservation {
	t.Helper()
	backfillBootstrapCredit(f.b, f.ranges, 262144)
	observed, err := f.b.readCleanupBackfill(context.Background(), f.partition, f.ranges)
	require.NoError(t, err)
	require.NotNil(t, observed)
	return observed
}

func backfillBootstrapCreate(t *testing.T, f *backfillBootstrapFixture) *cleanupBackfillState {
	t.Helper()
	observed := backfillBootstrapRead(t, f)
	backfillBootstrapCredit(f.b, f.claims, 1)
	state, err := f.b.refreshCleanupBackfill(context.Background(), f.partition, f.claims, observed)
	require.NoError(t, err)
	require.NotNil(t, state)
	return state
}

func TestCleanupBackfillBootstrap(t *testing.T) {
	t.Run("native-cutoff-refresh-and-wire", func(t *testing.T) {
		f := newBackfillBootstrapFixture(t, 60*time.Second)
		leaseID := f.session.Reference().LeaseID
		f.server.mu.Lock()
		actualTTL := f.server.leases[leaseID]
		f.server.mu.Unlock()
		require.Equal(t, int64(60), actualTTL)
		key := f.b.namespace.Root() + "p/aa/cleanup-pending-backfill"
		before := len(backfillBootstrapRequests(f))
		observed := backfillBootstrapRead(t, f)
		require.Nil(t, observed.state)
		backfillBootstrapCredit(f.b, f.claims, 1)
		first, err := f.b.refreshCleanupBackfill(context.Background(), f.partition, f.claims, observed)
		require.NoError(t, err)
		require.Equal(t, observed.revision, first.record.LegacyCutoffRevision)
		require.Equal(t, observed.revision, first.record.ReadViewRevision)
		require.Empty(t, first.record.Frontier)
		require.Equal(t, "list", first.record.Phase)
		require.Equal(t, ownTaskFence(f.server.point(key)), first.fence)
		require.Greater(t, first.fence.create, observed.revision)
		refreshedObservation := backfillBootstrapRead(t, f)
		backfillBootstrapCredit(f.b, f.claims, 1)
		second, err := f.b.refreshCleanupBackfill(context.Background(), f.partition, f.claims, refreshedObservation)
		require.NoError(t, err)
		require.Equal(t, first.record.LegacyCutoffRevision, second.record.LegacyCutoffRevision)
		require.Equal(t, refreshedObservation.revision, second.record.ReadViewRevision)
		require.Equal(t, first.fence.create, second.fence.create)
		require.Greater(t, second.fence.mod, refreshedObservation.revision)

		requests := backfillBootstrapRequests(f)[before:]
		require.Len(t, requests, 4)
		for i, request := range requests {
			require.LessOrEqual(t, request.Size(), 128*1024)
			f.b.archiveLife.mu.Lock()
			share := f.ranges
			if i%2 == 1 {
				share = f.claims
			}
			cmps, err := f.b.workerComparisons(f.session, f.partition, share)
			f.b.archiveLife.mu.Unlock()
			require.NoError(t, err)
			if i == 1 {
				cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
			} else if i == 3 {
				cmps = append(cmps, first.fence.comparisons()...)
			}
			require.Equal(t, taskProtoComparisons(cmps), request.Compare)
			require.Equal(t, []int{25, 24, 25, 27}[i], len(request.Compare)+len(request.Success)+len(request.Failure))
			require.Len(t, request.Success, 1)
			if i%2 == 0 {
				r := request.Success[0].GetRequestRange()
				require.NotNil(t, r)
				require.Equal(t, key, string(r.Key))
				require.Empty(t, r.RangeEnd)
				require.Zero(t, r.Revision, "the view must come from a real current observation")
				require.False(t, r.Serializable)
			} else {
				put := request.Success[0].GetRequestPut()
				require.Equal(t, key, string(put.Key))
				require.Zero(t, put.Lease)
				require.LessOrEqual(t, len(put.Value), 2048)
			}
		}
	})
	t.Run("origin-credit-and-cancellation", func(t *testing.T) {
		f := newBackfillBootstrapFixture(t, 60*time.Second)
		backfillBootstrapCredit(f.b, f.ranges, 0)
		before := len(backfillBootstrapRequests(f))
		_, err := f.b.readCleanupBackfill(context.Background(), f.partition, f.ranges)
		require.ErrorIs(t, err, ErrConflict)
		require.Len(t, backfillBootstrapRequests(f), before)
		backfillBootstrapCreate(t, f)
		observed := backfillBootstrapRead(t, f)
		copy := *observed
		other := newBackfillBootstrapFixture(t, 60*time.Second)
		backfillBootstrapCredit(f.b, f.claims, 1)
		before = len(backfillBootstrapRequests(f))
		var absent *Backend
		for _, attempt := range []func() error{
			func() error {
				_, err := f.b.refreshCleanupBackfill(nil, f.partition, f.claims, observed)
				return err
			},
			func() error {
				_, err := absent.refreshCleanupBackfill(context.Background(), f.partition, f.claims, observed)
				return err
			},
			func() error {
				_, err := f.b.refreshCleanupBackfill(context.Background(), nil, f.claims, observed)
				return err
			},
			func() error {
				_, err := f.b.refreshCleanupBackfill(context.Background(), f.partition, f.claims, nil)
				return err
			},
		} {
			require.Error(t, attempt())
		}
		_, err = f.b.refreshCleanupBackfill(context.Background(), f.partition, f.claims, &copy)
		require.Error(t, err)
		_, err = other.b.refreshCleanupBackfill(context.Background(), other.partition, other.claims, observed)
		require.Error(t, err)
		state := observed.state
		stateCopy := *state
		observed.state = &stateCopy
		_, err = f.b.refreshCleanupBackfill(context.Background(), f.partition, f.claims, observed)
		require.Error(t, err, "a copied nested checkpoint cannot become native evidence")
		observed.state = state
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = f.b.refreshCleanupBackfill(ctx, f.partition, f.claims, observed)
		require.ErrorIs(t, err, context.Canceled)
		require.Len(t, backfillBootstrapRequests(f), before)
		f.b.archiveLife.mu.Lock()
		tokens := f.claims.bucket.tokens
		f.b.archiveLife.mu.Unlock()
		require.Equal(t, uint64(1), tokens)
		backfillBootstrapCredit(f.b, f.claims, 0)
		_, err = f.b.refreshCleanupBackfill(context.Background(), f.partition, f.claims, observed)
		require.ErrorIs(t, err, ErrConflict)
		backfillBootstrapCredit(f.b, f.claims, 1)
		_, err = f.b.refreshCleanupBackfill(context.Background(), f.partition, f.claims, observed)
		require.NoError(t, err)
		f.b.archiveLife.mu.Lock()
		tokens = f.claims.bucket.tokens
		f.b.archiveLife.mu.Unlock()
		require.Zero(t, tokens)
	})
	t.Run("genuine-short-ttl-refuses-cas", func(t *testing.T) {
		f := newBackfillBootstrapFixture(t, 15*time.Second)
		observed := backfillBootstrapRead(t, f)
		backfillBootstrapCredit(f.b, f.claims, 1)
		before := len(backfillBootstrapRequests(f))
		state, err := f.b.refreshCleanupBackfill(context.Background(), f.partition, f.claims, observed)
		require.ErrorIs(t, err, ErrInvalidConfiguration)
		require.Nil(t, state)
		require.Len(t, backfillBootstrapRequests(f), before)
	})
	for _, mutation := range []string{"same-value", "delete-recreate", "lease-attach", "worker-fence"} {
		t.Run("exact-cas/"+mutation, func(t *testing.T) {
			f := newBackfillBootstrapFixture(t, 60*time.Second)
			first := backfillBootstrapCreate(t, f)
			observed := backfillBootstrapRead(t, f)
			key, value, lease := first.fence.key, first.fence.value, int64(0)
			if mutation == "delete-recreate" {
				_, err := f.b.client.Txn(context.Background()).Then(clientv3.OpDelete(key)).Commit()
				require.NoError(t, err)
			} else if mutation == "lease-attach" {
				lease = f.session.Reference().LeaseID
			} else if mutation == "worker-fence" {
				key, value, lease = f.partition.fence.key, f.partition.fence.value, f.partition.fence.lease
			}
			f.server.put(key, value, lease)
			before := f.server.point(first.fence.key)
			backfillBootstrapCredit(f.b, f.claims, 1)
			state, err := f.b.refreshCleanupBackfill(context.Background(), f.partition, f.claims, observed)
			require.Error(t, err)
			require.Nil(t, state)
			require.Equal(t, before, f.server.point(first.fence.key), "a stale original expectation cannot overwrite B")
			f.b.archiveLife.mu.Lock()
			tokens := f.claims.bucket.tokens
			f.b.archiveLife.mu.Unlock()
			require.Zero(t, tokens, "a dispatched CAS conflict never refunds Claim credit")
		})
	}
	t.Run("frontier-preserved-and-complete-dormant", func(t *testing.T) {
		f := newBackfillBootstrapFixture(t, 60*time.Second)
		first := backfillBootstrapCreate(t, f)
		r := first.record
		r.Frontier = "00000001-1111-4111-8111-111111111111"
		wire, err := encodeCleanupBackfill(r)
		require.NoError(t, err)
		f.server.put(first.fence.key, wire, 0)
		observed := backfillBootstrapRead(t, f)
		backfillBootstrapCredit(f.b, f.claims, 1)
		state, err := f.b.refreshCleanupBackfill(context.Background(), f.partition, f.claims, observed)
		require.NoError(t, err)
		require.Equal(t, r.Frontier, state.record.Frontier)
		require.Equal(t, r.LegacyCutoffRevision, state.record.LegacyCutoffRevision)
		r = state.record
		r.Phase = "complete"
		wire, err = encodeCleanupBackfill(r)
		require.NoError(t, err)
		f.server.put(first.fence.key, wire, 0)
		observed = backfillBootstrapRead(t, f)
		before := len(backfillBootstrapRequests(f))
		backfillBootstrapCredit(f.b, f.claims, 0)
		state, err = f.b.refreshCleanupBackfill(context.Background(), f.partition, f.claims, observed)
		require.NoError(t, err)
		require.Equal(t, r, state.record)
		require.Len(t, backfillBootstrapRequests(f), before, "completed B must stay dormant without Claim credit")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		state, err = f.b.refreshCleanupBackfill(ctx, f.partition, f.claims, observed)
		require.ErrorIs(t, err, context.Canceled)
		require.Nil(t, state)
		require.NoError(t, f.b.Close())
		state, err = f.b.refreshCleanupBackfill(context.Background(), f.partition, f.claims, observed)
		require.Error(t, err, "dormant checkpoints still require the original live backend/session")
		require.Nil(t, state)
	})
	t.Run("committed-ack-loss-keeps-cutoff-on-replacement", func(t *testing.T) {
		f := newBackfillBootstrapFixture(t, 60*time.Second)
		observed := backfillBootstrapRead(t, f)
		key := f.b.namespace.Root() + "p/aa/cleanup-pending-backfill"
		f.b.client.KV = &faultKV{KV: f.b.client.KV, match: putsKey(key), after: func(*clientv3.TxnResponse, error) (*clientv3.TxnResponse, error) {
			return nil, errors.New("lost checkpoint ACK after native commit")
		}}
		backfillBootstrapCredit(f.b, f.claims, 1)
		state, err := f.b.refreshCleanupBackfill(context.Background(), f.partition, f.claims, observed)
		require.ErrorIs(t, err, ErrOutcomeUnknown)
		require.Nil(t, state)
		require.NotNil(t, f.server.point(key))
		f.b.archiveLife.mu.Lock()
		liveErr := f.session.live()
		f.b.archiveLife.mu.Unlock()
		require.Error(t, liveErr)
		require.NoError(t, f.b.ReleaseWorkerSession(context.Background(), f.session))
		session, err := f.b.OpenWorkerSession(context.Background(), "replacement")
		require.NoError(t, err)
		partition, err := f.b.ClaimWorkerPartition(context.Background(), session, 0xaa)
		require.NoError(t, err)
		ranges, err := f.b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetRange, 0)
		require.NoError(t, err)
		backfillBootstrapCredit(f.b, ranges, 262144)
		recovered, err := f.b.readCleanupBackfill(context.Background(), partition, ranges)
		require.NoError(t, err)
		require.Equal(t, observed.revision, recovered.state.record.LegacyCutoffRevision)
		require.Equal(t, ownTaskFence(f.server.point(key)), recovered.state.fence)
	})
	t.Run("malformed-native-point-publishes-no-observation", func(t *testing.T) {
		f := newBackfillBootstrapFixture(t, 60*time.Second)
		key := f.b.namespace.Root() + "p/aa/cleanup-pending-backfill"
		f.server.mu.Lock()
		f.server.txnReply = func(request *pb.TxnRequest, response *pb.TxnResponse) {
			if len(request.Success) == 1 && request.Success[0].GetRequestRange() != nil && string(request.Success[0].GetRequestRange().Key) == key {
				response.Responses[0].GetResponseRange().More = true
			}
		}
		f.server.mu.Unlock()
		backfillBootstrapCredit(f.b, f.ranges, 262144)
		observed, err := f.b.readCleanupBackfill(context.Background(), f.partition, f.ranges)
		require.Error(t, err)
		require.Nil(t, observed)
	})
}
