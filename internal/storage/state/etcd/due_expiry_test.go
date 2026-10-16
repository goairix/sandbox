package etcd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type dueExpiryFixture struct {
	*duePageFixture
	claimShare *WorkerBudgetShare
	candidate  DueCandidateRecord
	keys       []string
	control    SandboxControlRecord
}

// Installs canonical active metadata into the real bounded gRPC model. This
// isolates admission behavior; the native test supplies the full publication.
func newDueExpiryFixture(t *testing.T) *dueExpiryFixture {
	t.Helper()
	f := newDuePageFixture(t)
	share, err := f.b.AcquireWorkerBudget(context.Background(), f.session, WorkerBudgetClaim, 0)
	require.NoError(t, err)
	runtime := RuntimeReference{ID: "pod", UID: uuid.NewString(), BootID: uuid.NewString()}
	c := SandboxControlRecord{Version: 1, SandboxID: "expired-sandbox", WorkspaceHash: strings.Repeat("a", 64), IntentID: "expired-intent", RestoreEpoch: f.b.restoreEpoch, Generation: 1, DataGateEpoch: 1, Phase: PhaseActive, Snapshot: SnapshotReference{Version: "snapshot", Digest: strings.Repeat("b", 64)}, Runtime: &runtime, ExpiresAt: f.now.Add(-time.Second)}
	placement := SandboxPlacementRecord{Version: 1, SandboxID: c.SandboxID, WorkspaceHash: c.WorkspaceHash, IntentID: c.IntentID, RestoreEpoch: c.RestoreEpoch, Generation: c.Generation, Partition: 0xaa}
	owner := WorkspaceOwnerRecord{Version: 1, WorkspaceHash: c.WorkspaceHash, SandboxID: c.SandboxID, IntentID: c.IntentID, RestoreEpoch: c.RestoreEpoch, Generation: c.Generation, Runtime: &runtime}
	fence := WorkspaceFenceRecord{Version: 1, WorkspaceHash: c.WorkspaceHash, RestoreEpoch: c.RestoreEpoch, Generation: c.Generation}
	index := RuntimeIndexRecord{Version: 1, IntentID: c.IntentID, SandboxID: c.SandboxID, WorkspaceHash: c.WorkspaceHash, RestoreEpoch: c.RestoreEpoch, Generation: c.Generation, Runtime: runtime}
	placementKey, err := f.b.namespace.placementKey(c.SandboxID)
	require.NoError(t, err)
	controlKey, _, err := f.b.namespace.sandboxKeys(0xaa, c.SandboxID, "read")
	require.NoError(t, err)
	ownerKey, err := f.b.namespace.Key("p", "aa", "workspaces", c.WorkspaceHash, "owner")
	require.NoError(t, err)
	fenceKey, err := f.b.namespace.Key("p", "aa", "workspaces", c.WorkspaceHash, "fence")
	require.NoError(t, err)
	indexKey, err := f.b.namespace.runtimeIndexKey(runtime.UID)
	require.NoError(t, err)
	keys := []string{placementKey, controlKey, ownerKey, fenceKey, indexKey}
	for i, r := range []interface{ Validate() error }{placement, c, owner, fence, index} {
		wire, err := encodeDomainRecord(r)
		require.NoError(t, err)
		f.server.put(keys[i], wire, 0)
	}
	candidate := dueCandidateForControl(0xaa, c)
	key, err := f.b.namespace.dueCandidateKey(candidate)
	require.NoError(t, err)
	wire, err := encodeDueCandidate(candidate)
	require.NoError(t, err)
	f.server.put(key, wire, 0)
	return &dueExpiryFixture{duePageFixture: f, claimShare: share, candidate: candidate, keys: keys, control: c}
}

func (f *dueExpiryFixture) earnClaim() {
	f.b.archiveLife.mu.Lock()
	f.claimShare.bucket.last = time.Now().Add(-2 * time.Second)
	f.b.archiveLife.mu.Unlock()
}

func (f *dueExpiryFixture) admit() (ExpiredTaskResult, error) {
	return f.b.AdmitExpiredTask(context.Background(), f.partition, f.claimShare, f.candidate, uuid.NewString(), 30*time.Second)
}

func TestDueExpiryAtomic(t *testing.T) {
	f := newDueExpiryFixture(t)
	f.earnClaim()
	before := f.server.point(f.keys[1])
	owner := f.server.point(f.keys[2])
	result, err := f.admit()
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	require.NotEmpty(t, result.Reference.AttemptID)
	require.Equal(t, "destroy", result.Reference.StageID)
	dueKey, _ := f.b.namespace.dueCandidateKey(f.candidate)
	require.Nil(t, f.server.point(dueKey), "exact candidate consumed with task birth")
	control := f.server.point(f.keys[1])
	require.Greater(t, control.ModRevision, before.ModRevision)
	for _, key := range taskDestroyKeys(t, f.b, result.Task) {
		kv := f.server.point(key)
		require.NotNil(t, kv)
		require.Equal(t, control.ModRevision, kv.CreateRevision)
		require.Zero(t, kv.Lease)
	}
	require.Equal(t, owner, f.server.point(f.keys[2]), "metadata admission never releases owner/quota")
	var actual *pb.TxnRequest
	f.server.mu.Lock()
	for _, r := range f.server.requests {
		for _, op := range r.Success {
			if d := op.GetRequestDeleteRange(); d != nil && string(d.Key) == dueKey {
				actual = r
			}
		}
	}
	f.server.mu.Unlock()
	require.NotNil(t, actual)
	require.Len(t, actual.Compare, 52)
	require.Len(t, actual.Success, 7)
	require.Len(t, actual.Failure, 4)
	require.Equal(t, 63, len(actual.Compare)+len(actual.Success)+len(actual.Failure))
	require.LessOrEqual(t, actual.Size(), 262144)
	f.b.archiveLife.mu.Lock()
	tokens := f.claimShare.bucket.tokens
	f.b.archiveLife.mu.Unlock()
	require.Zero(t, tokens)
	// The original task remains usable by the unchanged task metadata reader.
	task, err := f.b.LoadTask(context.Background(), result.Task)
	require.NoError(t, err)
	require.Equal(t, f.control.ExpiresAt, task.ExpiresAt)
}

func TestDueExpiryClockAdmission(t *testing.T) {
	for _, scenario := range []string{"early", "equality", "uncertain", "failure", "invalid-uncertainty", "backwards", "precommit-failure"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDueExpiryFixture(t)
			f.earnClaim()
			calls := 0
			f.b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
				calls++
				now := f.candidate.ExpiresAt.Add(time.Second)
				uncertainty := time.Duration(0)
				switch scenario {
				case "early":
					now = f.candidate.ExpiresAt.Add(-time.Nanosecond)
				case "equality":
					now = f.candidate.ExpiresAt
				case "uncertain":
					now = f.candidate.ExpiresAt.Add(100 * time.Millisecond)
					uncertainty = 200 * time.Millisecond
				case "failure":
					return ClockObservation{}, errors.New("clock unavailable")
				case "invalid-uncertainty":
					uncertainty = 2 * time.Second
				case "backwards":
					if calls > 1 {
						now = now.Add(-time.Nanosecond)
					}
				case "precommit-failure":
					if calls > 1 {
						return ClockObservation{}, errors.New("clock lost before commit")
					}
				}
				return ClockObservation{UTC: now, Uncertainty: uncertainty}, nil
			})
			beforeGrants, _, _ := f.server.counts()
			before := f.server.point(f.keys[1])
			result, err := f.admit()
			if scenario == "equality" {
				require.NoError(t, err)
				require.Equal(t, OutcomeCommitted, result.Outcome)
				require.Equal(t, 2, calls)
			} else {
				require.Error(t, err)
				require.Equal(t, OutcomeUnknown, result.Outcome)
				require.Equal(t, before, f.server.point(f.keys[1]))
				if scenario != "backwards" && scenario != "precommit-failure" {
					after, _, _ := f.server.counts()
					require.Equal(t, beforeGrants, after, "early/untrusted clock refuses before Stage Grant")
				}
			}
		})
	}
}

func TestDueExpiryCandidateAndWorkerFences(t *testing.T) {
	for _, scenario := range []string{"stale-expiry", "stale-generation", "stale-intent", "control", "due", "profile", "session", "partition", "claim-share"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDueExpiryFixture(t)
			f.earnClaim()
			before := f.server.point(f.keys[1])
			if strings.HasPrefix(scenario, "stale-") {
				switch scenario {
				case "stale-expiry":
					f.candidate.ExpiresAt = f.candidate.ExpiresAt.Add(-time.Second)
				case "stale-generation":
					f.candidate.Generation++
				case "stale-intent":
					f.candidate.IntentID = "other"
				}
			} else {
				key := f.keys[1]
				switch scenario {
				case "due":
					key, _ = f.b.namespace.dueCandidateKey(f.candidate)
				case "profile":
					key = f.b.workerControl.profile.key
				case "session":
					key = f.session.guard.key
				case "partition":
					key = f.partition.fence.key
				case "claim-share":
					key = f.claimShare.fence.key
				}
				f.b.client.KV = &faultKV{KV: f.b.client.KV, match: putsKey(f.keys[1]), before: func() { kv := f.server.point(key); f.server.put(key, string(kv.Value), kv.Lease) }}
			}
			result, err := f.admit()
			require.Error(t, err)
			require.Equal(t, OutcomeUnknown, result.Outcome)
			current := f.server.point(f.keys[1])
			require.Equal(t, before.Value, current.Value, "failed real Txn preserves active control")
			if scenario == "profile" || scenario == "session" || scenario == "partition" || scenario == "claim-share" {
				f.b.archiveLife.mu.Lock()
				lost := f.session.lost
				f.b.archiveLife.mu.Unlock()
				require.True(t, lost, "an observed original worker fence loss is terminal")
			}
			if result.Task.TaskID != "" {
				for _, key := range taskDestroyKeys(t, f.b, result.Task) {
					require.Nil(t, f.server.point(key))
				}
			}
		})
	}
}

func TestDueExpiryBudgetAndUnknown(t *testing.T) {
	t.Run("empty-original-claim-bucket", func(t *testing.T) {
		f := newDueExpiryFixture(t)
		before, _, _ := f.server.counts()
		_, err := f.admit()
		require.ErrorIs(t, err, ErrConflict)
		after, _, _ := f.server.counts()
		require.Equal(t, before, after)
		f.earnClaim()
		r, err := f.admit()
		require.NoError(t, err)
		require.Equal(t, OutcomeCommitted, r.Outcome)
	})
	for _, phase := range []string{"Grant", "commit"} {
		t.Run(phase, func(t *testing.T) {
			f := newDueExpiryFixture(t)
			f.earnClaim()
			if phase == "Grant" {
				original := f.b.client.Lease
				f.b.client.Lease = &workerTestLease{Lease: original, grant: func(context.Context, int64) (*clientv3.LeaseGrantResponse, error) {
					return nil, context.DeadlineExceeded
				}}
			} else {
				f.b.client.KV = &faultKV{KV: f.b.client.KV, match: putsKey(f.keys[1]), after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
					require.NoError(t, err)
					require.True(t, r.Succeeded)
					return nil, context.DeadlineExceeded
				}}
			}
			result, err := f.admit()
			require.ErrorIs(t, err, ErrOutcomeUnknown)
			require.Equal(t, OutcomeUnknown, result.Outcome)
			require.NotEmpty(t, result.Reference.AttemptID)
			require.NotEmpty(t, result.Reference.Digest)
			f.b.archiveLife.mu.Lock()
			tokens := f.claimShare.bucket.tokens
			f.b.archiveLife.mu.Unlock()
			require.Zero(t, tokens, "unknown cannot refund real claim credit")
			if phase == "commit" {
				outcome, err := f.b.ResolveStage(context.Background(), result.Reference)
				require.NoError(t, err)
				require.Equal(t, OutcomeCommitted, outcome)
				dueKey, _ := f.b.namespace.dueCandidateKey(f.candidate)
				require.Nil(t, f.server.point(dueKey))
				require.NotNil(t, f.server.point(f.keys[2]))
			}
		})
	}
}

func TestDueExpiryCompetition(t *testing.T) {
	f := newDueExpiryFixture(t)
	f.earnClaim()
	start := make(chan struct{})
	done := make(chan ExpiredTaskResult, 2)
	for i := 0; i < 2; i++ {
		go func() { <-start; r, _ := f.admit(); done <- r }()
	}
	close(start)
	committed := 0
	for i := 0; i < 2; i++ {
		r := workerTestReceive(t, done)
		if r.Outcome == OutcomeCommitted {
			committed++
		}
	}
	require.Equal(t, 1, committed)
}

func TestDueExpiryOriginalDeadlineAndCloseJoin(t *testing.T) {
	f := newDueExpiryFixture(t)
	f.earnClaim()
	// A short original lease deadline must bound generic preparatory calls as
	// well as the worker-specific methods. No time is advanced by the caller.
	f.b.archiveLife.mu.Lock()
	deadline := time.Now().Add(500 * time.Millisecond)
	f.session.deadline = deadline
	f.b.archiveLife.mu.Unlock()
	f.b.authorityClock = testAuthorityClock(func(ctx context.Context) (ClockObservation, error) {
		d, ok := ctx.Deadline()
		require.True(t, ok)
		require.False(t, d.After(deadline))
		return ClockObservation{UTC: f.now}, nil
	})
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	original := f.b.client.Lease
	f.b.client.Lease = &workerTestLease{Lease: original, grant: func(ctx context.Context, ttl int64) (*clientv3.LeaseGrantResponse, error) {
		d, ok := ctx.Deadline()
		require.True(t, ok)
		require.False(t, d.After(deadline))
		r, err := original.Grant(ctx, ttl)
		close(entered)
		<-release
		return r, err
	}}
	done := make(chan ExpiredTaskResult, 1)
	go func() { r, _ := f.admit(); done <- r }()
	workerTestReceive(t, entered)
	closed := make(chan error, 1)
	go func() { closed <- f.b.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned before actual Grant: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	result := workerTestReceive(t, done)
	require.Equal(t, OutcomeUnknown, result.Outcome)
	require.NotEmpty(t, result.Reference.AttemptID)
	require.NoError(t, workerTestReceive(t, closed))
}

func TestDueExpiryManualDestroyUnchanged(t *testing.T) {
	f := newDueExpiryFixture(t)
	f.b.authorityClock = nil
	s, task, err := f.b.PrepareDestroy(context.Background(), BeginDestroyInput{SandboxID: f.control.SandboxID, RequestID: fmt.Sprintf("manual-%s", uuid.NewString()), ExpectedControlRevision: f.server.point(f.keys[1]).ModRevision}, time.Second)
	require.NoError(t, err)
	outcome, err := f.b.CommitStage(context.Background(), s)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	require.NoError(t, f.b.ReleaseStage(context.Background(), s))
	require.NotEmpty(t, task.TaskID)
}

func TestDueExpiryExpectedRefusalsPreserveSession(t *testing.T) {
	for _, scenario := range []string{"stale-generation", "stale-expiry", "already-destroying", "control-CAS-conflict", "precommit-clock-error"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDueExpiryFixture(t)
			f.earnClaim()
			originalSession := f.session.Reference()
			originalCandidate := f.candidate
			switch scenario {
			case "stale-generation":
				f.candidate.Generation++
			case "stale-expiry":
				f.candidate.ExpiresAt = f.candidate.ExpiresAt.Add(-time.Second)
			case "already-destroying":
				control := f.control
				control.Phase = PhaseDestroying
				wire, err := encodeDomainRecord(control)
				require.NoError(t, err)
				f.server.put(f.keys[1], wire, 0)
			case "control-CAS-conflict":
				f.b.client.KV = &faultKV{KV: f.b.client.KV, match: putsKey(f.keys[1]), before: func() {
					current := f.server.point(f.keys[1])
					f.server.put(f.keys[1], string(current.Value), current.Lease)
				}}
			case "precommit-clock-error":
				calls := 0
				f.b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
					calls++
					if calls == 2 {
						return ClockObservation{}, errors.New("clock temporarily unavailable")
					}
					return ClockObservation{UTC: f.now}, nil
				})
			}
			result, err := f.admit()
			require.Error(t, err)
			require.Equal(t, OutcomeUnknown, result.Outcome)
			if result.Task.TaskID != "" {
				for _, key := range taskDestroyKeys(t, f.b, result.Task) {
					require.Nil(t, f.server.point(key))
				}
			}
			dueKey, err := f.b.namespace.dueCandidateKey(originalCandidate)
			require.NoError(t, err)
			require.NotNil(t, f.server.point(dueKey), "expected refusal cannot consume discovery evidence")
			grants, _, _ := f.server.counts()
			require.NoError(t, f.b.RenewWorkerSession(context.Background(), f.session), "known stale metadata or clock refusal must preserve the original worker")
			require.Equal(t, originalSession, f.session.Reference())
			after, _, _ := f.server.counts()
			require.Equal(t, grants, after, "renewal cannot recreate the original session or its bucket")
		})
	}
}
