package etcd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestCleanupLocalProfileRefusal(t *testing.T) {
	f := newCleanupActorFixture(t)
	for name, ctx := range map[string]context.Context{"live": f.ctx, "nil": nil} {
		t.Run(name, func(t *testing.T) {
			result, err := f.w.ExecuteCleanupLocal(ctx, f.ref)
			require.Error(t, err)
			require.NotEqual(t, OutcomeCommitted, result.Outcome)
			require.Nil(t, result.Release)
		})
	}
	f.assertNoPhysicalGrant(t)
	select {
	case <-f.probe.entered:
		t.Fatal("public execution must refuse incomplete physical configuration before metadata admission")
	default:
	}

	for _, ttl := range []time.Duration{15 * time.Second, 59 * time.Second} {
		options := integrationOptions(f.b, f.raw)
		options.RequestTimeout = time.Second
		options.WorkerControl = &WorkerControlOptions{SessionTTL: ttl}
		options.LocalCleanup = &LocalCleanupOptions{MaxActive: 32}
		require.ErrorIs(t, validateLocalCleanupOptions(options), ErrInvalidConfiguration)
	}
	for _, timeout := range []time.Duration{0, 3*time.Second + time.Nanosecond} {
		options := integrationOptions(f.b, f.raw)
		options.RequestTimeout = timeout
		options.WorkerControl = &WorkerControlOptions{SessionTTL: 60 * time.Second}
		options.LocalCleanup = &LocalCleanupOptions{MaxActive: 32}
		require.ErrorIs(t, validateLocalCleanupOptions(options), ErrInvalidConfiguration)
	}
}

// This creates retained diagnostic rows, not a runnable KernelBoundary, release
// seal or physical completion. The original Task/intent/receipt birth is real.
// Negative cold-recovery tests must preserve these rows without making effects.
func cleanupLocalRetainedRows(t *testing.T, f *cleanupActorFixture, purpose string) (string, string, cleanupExecutionRecord) {
	t.Helper()
	_, input := cleanupLocalWireFixture(t)
	r := f.evidence.record
	r.Phase, r.Confirmed = "occupied", "claimed"
	r.Run = &cleanupExecutionRun{ID: uuid.NewString(), Worker: input.state.record.Run.Worker}
	r.Pending = &cleanupExecutionPending{Purpose: purpose, ClaimID: uuid.NewString()}
	if purpose != "claim" {
		r.Pending.StageID, r.Pending.RequestID = "task_close_data", r.Pending.ClaimID
		r.Pending.AttemptID, r.Pending.Digest = uuid.NewString(), strings.Repeat("a", 64)
	}
	e, err := f.b.namespace.cleanupExecutionKey(f.ref)
	require.NoError(t, err)
	s, err := f.b.namespace.cleanupExecutionSlotKey(r.Slot)
	require.NoError(t, err)
	ew, err := encodeCleanupExecution(r)
	require.NoError(t, err)
	sw, err := encodeCleanupExecutionSlot(cleanupSlotRecordFromExecution(r))
	require.NoError(t, err)
	// Execution selection is born before occupation. The later atomic reserve
	// preserves E's birth while creating S at the occupied E modification.
	selecting := r
	selecting.Phase, selecting.Confirmed = "selecting", "queued"
	selecting.Run, selecting.Pending = nil, nil
	initial, err := encodeCleanupExecution(selecting)
	require.NoError(t, err)
	birth, err := f.raw.Txn(f.ctx).If(clientv3.Compare(clientv3.Version(e), "=", 0), clientv3.Compare(clientv3.Version(s), "=", 0)).Then(clientv3.OpPut(e, initial)).Commit()
	require.NoError(t, err)
	require.True(t, birth.Succeeded)
	response, err := f.raw.Txn(f.ctx).If(
		clientv3.Compare(clientv3.CreateRevision(e), "=", birth.Header.Revision),
		clientv3.Compare(clientv3.ModRevision(e), "=", birth.Header.Revision),
		clientv3.Compare(clientv3.Value(e), "=", initial),
		clientv3.Compare(clientv3.Version(s), "=", 0),
	).Then(clientv3.OpPut(s, sw), clientv3.OpPut(e, ew)).Commit()
	require.NoError(t, err)
	require.True(t, response.Succeeded)
	require.Greater(t, response.Header.Revision, birth.Header.Revision)

	observed, err := f.raw.Txn(f.ctx).Then(clientv3.OpGet(e), clientv3.OpGet(s)).Commit()
	require.NoError(t, err)
	require.Len(t, observed.Responses, 2)
	execution, slot := observed.Responses[0].GetResponseRange(), observed.Responses[1].GetResponseRange()
	require.Len(t, execution.Kvs, 1)
	require.Len(t, slot.Kvs, 1)
	require.Equal(t, birth.Header.Revision, execution.Kvs[0].CreateRevision)
	require.Equal(t, response.Header.Revision, execution.Kvs[0].ModRevision)
	require.Equal(t, response.Header.Revision, slot.Kvs[0].CreateRevision)
	require.Equal(t, slot.Kvs[0].CreateRevision, slot.Kvs[0].ModRevision)
	require.Greater(t, slot.Kvs[0].CreateRevision, execution.Kvs[0].CreateRevision)
	require.LessOrEqual(t, slot.Kvs[0].CreateRevision, execution.Kvs[0].ModRevision)
	return e, s, r
}

func TestCleanupLocalRecoveryRetains(t *testing.T) {
	for _, purpose := range []string{"claim", "close", "archive", "termination"} {
		t.Run(purpose, func(t *testing.T) {
			f := newCleanupActorFixture(t)
			e, s, record := cleanupLocalRetainedRows(t, f, purpose)
			before, err := f.raw.Get(f.ctx, e)
			require.NoError(t, err)
			slotBefore, err := f.raw.Get(f.ctx, s)
			require.NoError(t, err)
			job := f.queue(t, f.ctx, f.ref)
			_ = workerTestReceive(t, f.probe.entered)
			f.probe.unblock()
			workerTestReceive(t, job.done)
			require.True(t, job.result.Retained)
			require.NotNil(t, job.result.Slot)
			require.NotNil(t, job.result.Pending)
			require.Equal(t, record.Pending.Purpose, job.result.Pending.Purpose)
			require.NotEqual(t, OutcomeCommitted, job.result.Outcome)
			require.Nil(t, job.result.Release)
			after, err := f.raw.Get(f.ctx, e)
			require.NoError(t, err)
			slotAfter, err := f.raw.Get(f.ctx, s)
			require.NoError(t, err)
			require.Equal(t, before.Kvs, after.Kvs, "cold unjoined run cannot rotate, replace pending, or invent joined")
			require.Equal(t, slotBefore.Kvs, slotAfter.Kvs)
			f.assertNoPhysicalGrant(t)
		})
	}
}

func TestCleanupLocalReleaseSettlement(t *testing.T) {
	t.Run("archive-store-coordinate", func(t *testing.T) {
		// Independent format oracle for newTaskArchiveRecoveryStoreContext's
		// random32-byte hex ID. This is codec attribution only: Darwin cannot
		// open the real Linux recovery store or mint a physical Bind witness.
		var random [32]byte
		_, err := rand.Read(random[:])
		require.NoError(t, err)
		storeID := hex.EncodeToString(random[:])
		_, input := cleanupLocalWireFixture(t)
		record := input.state.record
		record.Confirmed = "archive-bound"
		record.Archive = &cleanupExecutionArchive{StoreID: storeID, Slot: 0, BindingDigest: strings.Repeat("a", 64)}
		wire, err := encodeCleanupExecution(record)
		require.NoError(t, err)
		key := fmt.Sprintf("%sp/%02x/tasks/%s/execution", record.Task.Namespace, record.Task.Partition, record.Task.TaskID)
		kv := &mvccpb.KeyValue{Key: []byte(key), Value: []byte(wire), CreateRevision: 12, ModRevision: 13, Version: 2}
		var decoded cleanupExecutionRecord
		require.NoError(t, decodeCleanupExecution(kv, 13, record.Task, &decoded))
		require.Equal(t, record, decoded)
		for _, invalid := range []string{uuid.NewString(), strings.Repeat("A", 64), strings.Repeat("g", 64), storeID[:63]} {
			bad := record
			bad.Archive = &cleanupExecutionArchive{StoreID: invalid, Slot: 0, BindingDigest: record.Archive.BindingDigest}
			_, err := encodeCleanupExecution(bad)
			require.Error(t, err)
		}
	})

	f := newCleanupActorFixture(t)
	e, s, _ := cleanupLocalRetainedRows(t, f, "claim")
	job := f.queue(t, f.ctx, f.ref)
	_ = workerTestReceive(t, f.probe.entered)
	f.probe.unblock()
	workerTestReceive(t, job.done)
	before, err := f.raw.Get(f.ctx, e)
	require.NoError(t, err)
	slotBefore, err := f.raw.Get(f.ctx, s)
	require.NoError(t, err)
	proof, err := f.b.readLocalOwnerReleaseProof(f.ctx, f.ref)
	require.NoError(t, err)
	require.Nil(t, proof)
	require.Error(t, f.b.settleCleanupRun(f.ctx, job, proof), "missing genuine release proof cannot return a slot")
	require.Error(t, f.b.joinCleanupRun(f.ctx, job), "a new observer job cannot join a persisted foreign run")
	after, err := f.raw.Get(f.ctx, e)
	require.NoError(t, err)
	slotAfter, err := f.raw.Get(f.ctx, s)
	require.NoError(t, err)
	require.Equal(t, before.Kvs, after.Kvs)
	require.Equal(t, slotBefore.Kvs, slotAfter.Kvs)
	f.assertNoPhysicalGrant(t)
}
