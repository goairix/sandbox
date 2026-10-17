package etcd

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// Only the existing decoded source-history model is borrowed. The original
// session, partition and shares were acquired through real backend calls; this
// fixture grants neither physical release authority nor worker driver wiring.
type coldQuotaWorkerPrepareFixture struct {
	*coldQuotaWorkerJournalFixture
	state    *coldQuotaWorkerPrepare
	readyKey string
	slotKey  string
	freeKey  string
}

func newColdQuotaWorkerPrepareFixture(t *testing.T) *coldQuotaWorkerPrepareFixture {
	t.Helper()
	f := &coldQuotaWorkerPrepareFixture{coldQuotaWorkerJournalFixture: newColdQuotaWorkerJournalFixture(t)}
	f.b.preparationTotal = 33 // The inventory has 32 preparation slots and one quota slot.
	var err error
	f.readyKey, err = f.b.namespace.Key("preparation", "inventory", "ready")
	require.NoError(t, err)
	wire, err := coldEncode(preparationInventoryRecord{Version: 1, RestoreEpoch: f.b.restoreEpoch, ProfileDigest: f.b.preparationDigest, Next: 33, Total: 33, Ready: true})
	require.NoError(t, err)
	f.server.mu.Lock()
	f.server.values[f.readyKey] = &mvccpb.KeyValue{Key: []byte(f.readyKey), Value: []byte(wire), Version: 1, CreateRevision: 190, ModRevision: 190}
	f.server.mu.Unlock()
	f.slotKey, f.freeKey, err = f.b.namespace.coldSlotKeys(f.prepared.history.reservation.Quota)
	require.NoError(t, err)

	// Fund before taking fingerprints. Construction must neither charge nor
	// dispatch, and subsequent strict retries refill Range alone.
	f.fundRange()
	f.b.archiveLife.mu.Lock()
	ranges, claims := f.ranges.bucket, f.claims.bucket
	f.b.archiveLife.mu.Unlock()
	f.state, err = f.b.newColdQuotaWorkerPrepare(f.owner, f.prepared.history, f.session, f.part, f.ranges)
	require.NoError(t, err)
	require.NotNil(t, f.state)
	require.Same(t, f.state, f.state.self)
	require.Empty(t, f.requests())
	f.requireCredit(t, ranges, claims)
	return f
}

func (f *coldQuotaWorkerPrepareFixture) fundRange() {
	f.b.archiveLife.mu.Lock()
	defer f.b.archiveLife.mu.Unlock()
	f.ranges.bucket.last = time.Now().Add(time.Hour)
	f.ranges.bucket.tokens = 262144
}

func (f *coldQuotaWorkerPrepareFixture) step() (*coldQuotaReturnPrepared, error) {
	return f.b.stepColdQuotaWorkerPrepare(f.owner, f.prepared.history, f.state, f.session, f.part, f.ranges)
}

func (f *coldQuotaWorkerPrepareFixture) requests() []*pb.TxnRequest {
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	return append([]*pb.TxnRequest(nil), f.server.requests...)
}

func (f *coldQuotaWorkerPrepareFixture) requireCredit(t *testing.T, ranges, claims workerBucket) {
	t.Helper()
	f.b.archiveLife.mu.Lock()
	actualRange, actualClaim := f.ranges.bucket, f.claims.bucket
	f.b.archiveLife.mu.Unlock()
	require.Equal(t, ranges, actualRange)
	require.Equal(t, claims, actualClaim)
}

func (f *coldQuotaWorkerPrepareFixture) requireRead(t *testing.T, request *pb.TxnRequest, keys ...string) {
	t.Helper()
	want := taskProtoPoints(append([]string{f.b.identityKey, f.b.restoreKey}, keys...))
	require.Equal(t, want, request.Success)
	require.Equal(t, want, request.Failure)
	require.Len(t, request.Compare, 20)
	for _, fence := range []taskFence{f.b.workerControl.profile, f.session.guard, f.part.fence, f.ranges.fence} {
		coldQuotaWorkerJournalRequireFence(t, request, fence)
	}
	for _, comparison := range request.Compare {
		require.NotEqual(t, f.claims.fence.key, string(comparison.Key), "prepare is Range-only")
	}
}

// Each step has one original paid read. A P2 credit refusal must retain ready
// and retry only slot/free, then return the same ordinary preparation forever
// while it remains unused. Mutation identity comes from the existing builder.
func TestColdQuotaWorkerPreparePaidYieldAndResume(t *testing.T) {
	f := newColdQuotaWorkerPrepareFixture(t)
	originalContext, history := f.owner.context, f.prepared.history
	f.b.archiveLife.mu.Lock()
	claims := f.claims.bucket
	f.b.archiveLife.mu.Unlock()
	prepared, err := f.step()
	require.NoError(t, err)
	require.Nil(t, prepared)
	require.Nil(t, f.state.prepared)
	ready := f.state.ready
	require.NotNil(t, ready)
	require.Len(t, f.requests(), 1)
	f.requireRead(t, f.requests()[0], f.readyKey)
	f.b.archiveLife.mu.Lock()
	ranges := f.ranges.bucket
	f.b.archiveLife.mu.Unlock()
	require.Zero(t, ranges.tokens)
	f.requireCredit(t, ranges, claims)

	prepared, err = f.step()
	require.ErrorIs(t, err, ErrConflict)
	require.Nil(t, prepared)
	require.Same(t, ready, f.state.ready)
	require.Len(t, f.requests(), 1)
	f.requireCredit(t, ranges, claims)

	// A changed remote ready point cannot trigger a P1 reread or replace the
	// original comparison after that bounded phase was validated and retained.
	f.server.put(f.readyKey, "changed after validated P1", 0)
	slot := f.server.point(f.slotKey)
	f.fundRange()
	prepared, err = f.step()
	require.NoError(t, err)
	require.NotNil(t, prepared)
	require.Same(t, prepared, prepared.self)
	require.Same(t, history, prepared.history, "authenticated history is borrowed, never cloned")
	require.Same(t, originalContext, f.owner.context)
	require.Same(t, f.owner, prepared.owner)
	require.Same(t, f.b, prepared.backend)
	require.Zero(t, atomic.LoadUint32(&prepared.consumed))
	require.Zero(t, atomic.LoadUint32(&prepared.journalAttempted))
	require.Equal(t, f.prepared.returnKey, prepared.returnKey)
	require.Len(t, f.requests(), 2)
	f.requireRead(t, f.requests()[1], f.slotKey, f.freeKey)
	ref := prepared.reference
	locator := StageAttemptLocator{Namespace: ref.Namespace, RestoreEpoch: ref.RestoreEpoch, Partition: ref.Partition, RequestID: ref.RequestID, StageID: ref.StageID, AttemptID: ref.AttemptID}
	require.NoError(t, locator.Validate())
	require.Equal(t, "cold_runtime_quota_return", locator.StageID)
	want, err := f.b.coldQuotaReturnMutation(history, ready, slot, prepared.returnKey, f.freeKey, locator)
	require.NoError(t, err)
	want, digest, err := prepareMutation(f.b.namespace, want)
	require.NoError(t, err)
	require.Equal(t, want, prepared.mutation)
	require.Equal(t, locator.reference(digest), prepared.reference)
	f.b.archiveLife.mu.Lock()
	ranges = f.ranges.bucket
	f.b.archiveLife.mu.Unlock()
	require.Zero(t, ranges.tokens)
	f.requireCredit(t, ranges, claims)

	for i := 0; i < 2; i++ {
		again, err := f.step()
		require.NoError(t, err)
		require.Same(t, prepared, again, "done cannot allocate a replacement UUID or mutation")
		require.Equal(t, ref, again.reference)
	}
	require.Len(t, f.requests(), 2)
	f.requireCredit(t, ranges, claims)
	f.requireNoContinuation(t)
}

// The shared pure decoder must preserve coldReady's public error distinctions
// and immutable canonical profile, total, next and ready validation.
func TestColdQuotaWorkerPrepareReadyErrorsKeepP1(t *testing.T) {
	for _, fault := range []string{"absent", "malformed", "noncanonical", "mutable", "lease", "version", "restore", "profile", "total", "next", "not-ready"} {
		t.Run(fault, func(t *testing.T) {
			f := newColdQuotaWorkerPrepareFixture(t)
			original := f.server.point(f.readyKey)
			var r preparationInventoryRecord
			require.NoError(t, coldDecode(original, &r))
			changed := workerTestKV(original)
			switch fault {
			case "malformed":
				changed.Value = []byte("invalid inventory")
			case "noncanonical":
				changed.Value = append(changed.Value, ' ')
			case "mutable":
				changed.Version, changed.ModRevision = 2, changed.ModRevision+1
			case "lease":
				changed.Lease = 77
			case "version":
				r.Version++
			case "restore":
				r.RestoreEpoch = "other-restore"
			case "profile":
				r.ProfileDigest = strings.Repeat("0", 64)
			case "total":
				r.Total++
			case "next":
				r.Next--
			case "not-ready":
				r.Ready = false
			}
			if fault != "malformed" && fault != "noncanonical" {
				wire, err := coldEncode(r)
				require.NoError(t, err)
				changed.Value = []byte(wire)
			}
			f.server.mu.Lock()
			f.server.values[f.readyKey] = changed
			if fault == "absent" {
				delete(f.server.values, f.readyKey)
			}
			f.server.mu.Unlock()
			phase := f.state.phase
			prepared, err := f.step()
			if fault == "absent" {
				require.ErrorIs(t, err, ErrPreparationNotReady)
			} else {
				require.ErrorIs(t, err, ErrCorruptRecord)
			}
			require.Nil(t, prepared)
			require.Nil(t, f.state.prepared)
			require.Nil(t, f.state.ready)
			require.Equal(t, phase, f.state.phase)
			require.Len(t, f.requests(), 1)
			f.server.mu.Lock()
			f.server.values[f.readyKey] = original
			f.server.mu.Unlock()
			f.fundRange()
			prepared, err = f.step()
			require.NoError(t, err)
			require.Nil(t, prepared)
			require.NotNil(t, f.state.ready)
			require.Len(t, f.requests(), 2)
			f.requireRead(t, f.requests()[1], f.readyKey)
		})
	}
}

// UUID randomness is observed only within sequential tests in this package.
// The secure reader remains the source; the counter proves invalid reads and
// admission refusals never manufacture a replacement attempt before freezing.
type coldQuotaPrepareUUIDReader struct {
	bytes atomic.Int64
}

func (r *coldQuotaPrepareUUIDReader) Read(p []byte) (int, error) {
	n, err := rand.Read(p)
	r.bytes.Add(int64(n))
	return n, err
}

func TestColdQuotaWorkerPrepareSlotErrorsAllocateNoAttempt(t *testing.T) {
	for _, fault := range []string{"absent", "malformed", "noncanonical", "free-slot", "owner", "slot", "profile", "free-present"} {
		t.Run(fault, func(t *testing.T) {
			f := newColdQuotaWorkerPrepareFixture(t)
			random := new(coldQuotaPrepareUUIDReader)
			uuid.SetRand(random)
			defer uuid.SetRand(nil)
			prepared, err := f.step()
			require.NoError(t, err)
			require.Nil(t, prepared)
			require.Zero(t, random.bytes.Load(), "P1 does not allocate an attempt")
			ready, phase := f.state.ready, f.state.phase
			original := f.server.point(f.slotKey)
			changed := workerTestKV(original)
			var slot coldSlotRecord
			require.NoError(t, coldDecode(original, &slot))
			switch fault {
			case "malformed":
				changed.Value = []byte("invalid slot")
			case "noncanonical":
				changed.Value = append(changed.Value, ' ')
			case "free-slot":
				slot.Occupied, slot.Owner = false, ColdSlotOwner{}
			case "owner":
				slot.Owner.Generation++
			case "slot":
				slot.Slot.Epoch++
			case "profile":
				slot.ProfileDigest = strings.Repeat("0", 64)
			}
			if fault != "malformed" && fault != "noncanonical" {
				wire, err := coldEncode(slot)
				require.NoError(t, err)
				changed.Value = []byte(wire)
			}
			f.server.mu.Lock()
			f.server.values[f.slotKey] = changed
			if fault == "absent" {
				delete(f.server.values, f.slotKey)
			}
			if fault == "free-present" {
				free := workerTestKV(original)
				free.Key = []byte(f.freeKey)
				f.server.values[f.freeKey] = free
			}
			f.server.mu.Unlock()
			prepared, err = f.step()
			require.ErrorIs(t, err, ErrConflict, "the first P2 turn has no Range credit")
			require.Nil(t, prepared)
			require.Len(t, f.requests(), 1)
			require.Zero(t, random.bytes.Load())
			f.fundRange()
			prepared, err = f.step()
			require.ErrorIs(t, err, ErrConflict)
			require.Nil(t, prepared)
			require.Nil(t, f.state.prepared)
			require.Same(t, ready, f.state.ready)
			require.Equal(t, phase, f.state.phase)
			require.Zero(t, random.bytes.Load(), "invalid slot/free cannot allocate a UUID")
			require.Len(t, f.requests(), 2)
			f.requireRead(t, f.requests()[1], f.slotKey, f.freeKey)

			f.server.mu.Lock()
			f.server.values[f.slotKey] = original
			delete(f.server.values, f.freeKey)
			f.server.mu.Unlock()
			f.fundRange()
			prepared, err = f.step()
			require.NoError(t, err)
			require.NotNil(t, prepared)
			require.Equal(t, int64(16), random.bytes.Load(), "only the successful frozen preparation allocates one UUID")
			require.Len(t, f.requests(), 3)
			f.requireRead(t, f.requests()[2], f.slotKey, f.freeKey)
		})
	}
}

// Each business point retains its original 16 KiB value cap independently of
// metadata's wider transport bound. No typed partial can survive an overrun.
func TestColdQuotaWorkerPrepareBusinessCaps(t *testing.T) {
	for _, point := range []string{"ready", "slot", "free"} {
		t.Run(point, func(t *testing.T) {
			f := newColdQuotaWorkerPrepareFixture(t)
			key := f.readyKey
			if point != "ready" {
				prepared, err := f.step()
				require.NoError(t, err)
				require.Nil(t, prepared)
				key = f.slotKey
				if point == "free" {
					key = f.freeKey
				}
				f.fundRange()
			}
			phase, ready := f.state.phase, f.state.ready
			f.server.put(key, strings.Repeat("x", 16385), 0)
			prepared, err := f.step()
			require.ErrorIs(t, err, ErrOutcomeUnknown)
			require.Nil(t, prepared)
			require.Nil(t, f.state.prepared)
			require.Equal(t, phase, f.state.phase)
			require.Same(t, ready, f.state.ready)
			f.b.archiveLife.mu.Lock()
			lost := f.session.lost
			f.b.archiveLife.mu.Unlock()
			require.True(t, lost)
		})
	}
}

// The cursor belongs to its original ordinary authenticated input and worker
// members. Even a live replacement must be rejected before payment or RPC.
func TestColdQuotaWorkerPrepareRejectsRebinding(t *testing.T) {
	for _, afterP1 := range []bool{false, true} {
		for _, fault := range []string{"nil-state", "state-copy", "history-copy", "history-task", "history-birth", "history-owner", "owner-copy", "owner-context", "owner-finished", "owner-cancelled", "session-copy", "partition-copy", "range-copy", "claim-role"} {
			t.Run(fmt.Sprintf("afterP1-%t/%s", afterP1, fault), func(t *testing.T) {
				f := newColdQuotaWorkerPrepareFixture(t)
				if afterP1 {
					prepared, err := f.step()
					require.NoError(t, err)
					require.Nil(t, prepared)
				}
				f.fundRange()
				owner, history, state := f.owner, f.prepared.history, f.state
				session, partition, ranges := f.session, f.part, f.ranges
				switch fault {
				case "nil-state":
					state = nil
				case "state-copy":
					copy := *state
					state = &copy
				case "history-copy":
					copy := *history
					history = &copy
				case "history-task":
					history.release.record.Task.TaskID += "-changed"
				case "history-birth":
					history.release.record.TaskBirth++
				case "history-owner":
					history.reservation.Owner.Generation++
				case "owner-copy":
					copy := *owner
					owner = &copy
				case "owner-context":
					owner.context = context.Background()
				case "owner-finished":
					owner.finish()
				case "owner-cancelled":
					owner.cancel()
				case "session-copy":
					copy := *session
					copy.self = &copy
					session = &copy
				case "partition-copy":
					copy := *partition
					copy.self = &copy
					partition = &copy
				case "range-copy":
					copy := *ranges
					copy.self = &copy
					ranges = &copy
				case "claim-role":
					ranges = f.claims
				}
				before, phase, ready := len(f.requests()), f.state.phase, f.state.ready
				f.b.archiveLife.mu.Lock()
				paidRange, untouchedClaim := f.ranges.bucket, f.claims.bucket
				f.b.archiveLife.mu.Unlock()
				prepared, err := f.b.stepColdQuotaWorkerPrepare(owner, history, state, session, partition, ranges)
				require.Error(t, err)
				require.Nil(t, prepared)
				require.Nil(t, f.state.prepared)
				require.Equal(t, phase, f.state.phase)
				require.Same(t, ready, f.state.ready)
				require.Len(t, f.requests(), before)
				f.requireCredit(t, paidRange, untouchedClaim)
			})
		}
	}
}

func TestColdQuotaWorkerPrepareConstructorRejectsInvalidInput(t *testing.T) {
	for _, fault := range []string{"nil-history", "publication", "entry", "release", "task", "birth", "owner-partition", "source-point", "profile", "cancelled", "claim-role"} {
		t.Run(fault, func(t *testing.T) {
			f := newColdQuotaWorkerPrepareFixture(t)
			history, ranges := f.prepared.history, f.ranges
			switch fault {
			case "nil-history":
				history = nil
			case "publication":
				history.publication = nil
			case "entry":
				history.publication.entry = nil
			case "release":
				history.release = nil
			case "task":
				history.release.record.Task.Namespace = "/foreign/"
			case "birth":
				history.release.record.TaskBirth = 0
			case "owner-partition":
				history.reservation.Owner.Partition++
			case "source-point":
				history.settledKV = nil
			case "profile":
				f.b.preparationInventory = nil
			case "cancelled":
				f.owner.cancel()
			case "claim-role":
				ranges = f.claims
			}
			f.b.archiveLife.mu.Lock()
			originalRange, originalClaim := f.ranges.bucket, f.claims.bucket
			f.b.archiveLife.mu.Unlock()
			state, err := f.b.newColdQuotaWorkerPrepare(f.owner, history, f.session, f.part, ranges)
			require.Error(t, err)
			require.Nil(t, state)
			require.Empty(t, f.requests())
			f.requireCredit(t, originalRange, originalClaim)
		})
	}
}

// The cached pointer cannot reopen a journal or continuation that was already
// attempted, nor may done bypass its original context, members or frozen data.
func TestColdQuotaWorkerPrepareDoneRejectsConsumedAndRebound(t *testing.T) {
	for _, fault := range []string{"consumed", "journal-attempted", "context", "finished", "history-birth", "reference", "mutation"} {
		t.Run(fault, func(t *testing.T) {
			f := newColdQuotaWorkerPrepareFixture(t)
			prepared, err := f.step()
			require.NoError(t, err)
			require.Nil(t, prepared)
			f.fundRange()
			prepared, err = f.step()
			require.NoError(t, err)
			require.NotNil(t, prepared)
			original := prepared.reference
			switch fault {
			case "consumed":
				atomic.StoreUint32(&prepared.consumed, 1)
			case "journal-attempted":
				atomic.StoreUint32(&prepared.journalAttempted, 1)
			case "context":
				f.owner.context = context.Background()
			case "finished":
				f.owner.finish()
			case "history-birth":
				f.prepared.history.release.record.TaskBirth++
			case "reference":
				prepared.reference.Digest = strings.Repeat("0", 64)
			case "mutation":
				prepared.mutation.Writes[0].Value = []byte("modified frozen mutation")
			}
			again, err := f.step()
			require.Error(t, err)
			require.Nil(t, again)
			require.Same(t, prepared, f.state.prepared, "refusal cannot allocate or replace the retained original")
			if fault != "reference" {
				require.Equal(t, original, prepared.reference)
			}
			require.Len(t, f.requests(), 2)
			if fault == "consumed" {
				require.Equal(t, uint32(1), atomic.LoadUint32(&prepared.consumed))
			}
			if fault == "journal-attempted" {
				require.Equal(t, uint32(1), atomic.LoadUint32(&prepared.journalAttempted))
			}
		})
	}
}

func (f *coldQuotaWorkerPrepareFixture) metadataView() *coldQuotaAdmissionFixture {
	return &coldQuotaAdmissionFixture{b: f.b, server: f.server, owner: f.owner, session: f.session, part: f.part, ranges: f.ranges, claims: f.claims}
}

// The real client has already returned a typed valid ACK to the interceptor.
// Replacing the enrolled context there must not publish either partial ready or
// a prepared pointer; the post-read original-binding check owns that boundary.
func TestColdQuotaWorkerPreparePostReturnBindingBeforePublication(t *testing.T) {
	for _, afterP1 := range []bool{false, true} {
		t.Run(fmt.Sprintf("afterP1-%t", afterP1), func(t *testing.T) {
			f := newColdQuotaWorkerPrepareFixture(t)
			if afterP1 {
				prepared, err := f.step()
				require.NoError(t, err)
				require.Nil(t, prepared)
				f.fundRange()
			}
			phase, ready := f.state.phase, f.state.ready
			var hits atomic.Int32
			coldQuotaMetadataIntercept(t, f.metadataView(), func(_ context.Context, _ *pb.TxnResponse, err error) error {
				hits.Add(1)
				f.owner.context = context.Background()
				return err
			})
			prepared, err := f.step()
			require.Error(t, err)
			require.Nil(t, prepared)
			require.Nil(t, f.state.prepared)
			require.Equal(t, phase, f.state.phase)
			require.Same(t, ready, f.state.ready)
			require.Equal(t, int32(1), hits.Load())
		})
	}
}

// Close cancels the original read but must join its actual native return. Busy
// ownership also refuses duplicates until that same return, with no new read.
func TestColdQuotaWorkerPrepareCloseJoinsActualReturn(t *testing.T) {
	f := newColdQuotaWorkerPrepareFixture(t)
	entered, allow := make(chan struct{}), make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(allow) })
	var hits atomic.Int32
	coldQuotaMetadataIntercept(t, f.metadataView(), func(_ context.Context, _ *pb.TxnResponse, err error) error {
		hits.Add(1)
		close(entered)
		<-allow
		return err
	})
	finished := make(chan error, 1)
	go func() {
		_, err := f.step()
		f.owner.finish()
		finished <- err
	}()
	coldQuotaJournalAwait(t, entered)
	f.b.archiveLife.mu.Lock()
	call, claims := f.session.operation, f.claims.bucket
	f.b.archiveLife.mu.Unlock()
	require.NotNil(t, call)
	stopped := make(chan error, 1)
	go func() { stopped <- f.b.Close() }()
	coldQuotaJournalAwait(t, call.ctx.Done())
	select {
	case <-call.done:
		t.Fatal("cancellation cannot release the native read's ownership")
	default:
	}
	select {
	case err := <-stopped:
		t.Fatalf("Close returned before the original native read: %v", err)
	default:
	}
	f.b.archiveLife.mu.Lock()
	operation, ranges, actualClaim := f.session.operation, f.ranges.bucket, f.claims.bucket
	_, registered := f.b.archiveLife.enrolled[f.owner]
	f.b.archiveLife.mu.Unlock()
	require.Same(t, call, operation)
	require.Equal(t, claims, actualClaim)
	require.Zero(t, ranges.tokens)
	require.True(t, registered)
	prepared, err := f.step()
	require.Error(t, err)
	require.Nil(t, prepared)
	require.Equal(t, int32(1), hits.Load())
	release.Do(func() { close(allow) })
	require.ErrorIs(t, workerTestReceive(t, finished), ErrOutcomeUnknown)
	require.NoError(t, workerTestReceive(t, stopped))
	coldQuotaJournalAwait(t, call.done)
	require.Nil(t, f.state.ready)
	require.Nil(t, f.state.prepared)
}
