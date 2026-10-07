package etcd

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func taskClaimFixture(t *testing.T) (*Backend, *clientv3.Client, TaskReference, context.Context) {
	t.Helper()
	b, raw, in, _, ctx := taskDestroyFixture(t)
	s, r := taskDestroyPrepare(t, b, ctx, in)
	out, e := b.CommitStage(ctx, s)
	require.NoError(t, e)
	require.Equal(t, OutcomeCommitted, out)
	return b, raw, r, ctx
}

func TestTaskClaimCopies(t *testing.T) {
	t.Run("seal-and-owned-reference", func(t *testing.T) {
		b, _, ref, ctx := taskClaimFixture(t)
		c, e := b.ClaimTask(ctx, ref, "worker", 30*time.Second)
		require.NoError(t, e)
		defer taskReleaseClaim(t, b, c)
		copy := new(TaskClaim)
		reflect.ValueOf(copy).Elem().Set(reflect.ValueOf(c).Elem())
		for _, bad := range []*TaskClaim{nil, {}, copy} {
			require.ErrorIs(t, b.RenewTaskClaim(ctx, bad), ErrInvalidRecord)
			require.ErrorIs(t, b.ReleaseTaskClaim(ctx, bad), ErrInvalidRecord)
			stage, e := b.PrepareTaskCheckpoint(ctx, bad, TaskCheckpointInput{}, time.Second)
			require.ErrorIs(t, e, ErrInvalidRecord)
			require.Nil(t, stage)
		}
		foreign := new(Backend)
		require.ErrorIs(t, foreign.RenewTaskClaim(ctx, c), ErrInvalidRecord)
		require.ErrorIs(t, foreign.ReleaseTaskClaim(ctx, c), ErrInvalidRecord)
		original := c.Reference()
		diagnostic := original
		diagnostic.LeaseID++
		diagnostic.Task.TaskID = "changed"
		require.Equal(t, original, c.Reference())
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				_ = c.Reference()
			}
		}()
		e = b.RenewTaskClaim(ctx, c)
		wg.Wait()
		require.NoError(t, e)
	})
	t.Run("complete-budget", func(t *testing.T) {
		b, _, ref, ctx := taskClaimFixture(t)
		c, e := b.ClaimTask(ctx, ref, "worker", 30*time.Second)
		require.NoError(t, e)
		defer taskReleaseClaim(t, b, c)
		bundleCheckpoint, e := b.readDomain(ctx, c.checkpointKey)
		require.NoError(t, e)
		before := ownTaskFence(bundleCheckpoint[0])
		in := TaskCheckpointInput{StageID: "checkpoint", ExpectedRevision: before.mod, State: TaskCheckpointPending, DetailDigest: strings.Repeat("a", 64)}
		attempt := StageAttemptLocator{Namespace: ref.Namespace, RestoreEpoch: ref.RestoreEpoch, Partition: ref.Partition, RequestID: c.Reference().ClaimID, StageID: in.StageID, AttemptID: c.Reference().ClaimID}
		m, e := c.checkpointMutation(before, in, attempt)
		require.NoError(t, e)
		require.Equal(t, 59, len(m.Comparisons)+len(m.Writes)+14)
		require.NoError(t, b.preflightTaskClaim(c, before))
		// A private pure builder copy is never passed to any capability API.
		oversized := &TaskClaim{reference: c.reference, claimKey: c.claimKey, guardKey: c.guardKey, checkpointKey: c.checkpointKey, value: c.value, fences: append([]taskFence(nil), c.fences...)}
		oversized.fences = append(oversized.fences, c.fences[0], c.fences[0])
		require.Error(t, b.preflightTaskClaim(oversized, before))
		oversized.fences = append([]taskFence(nil), c.fences...)
		oversized.fences[0].value = strings.Repeat("x", 256<<10)
		require.Error(t, b.preflightTaskClaim(oversized, before))
	})

	t.Run("strict-record", func(t *testing.T) {
		task, _, _, _ := taskTestRecords()
		record := taskClaimRecord{Version: 1, Task: task.Reference, ClaimID: taskClaimTestID, WorkerID: "worker", LeaseID: 7}
		wire, e := encodeTaskClaimRecord(record)
		require.NoError(t, e)
		kv := &mvccpb.KeyValue{Value: []byte(wire), Lease: 7, CreateRevision: 11, ModRevision: 11}
		var got taskClaimRecord
		require.NoError(t, decodeTaskClaimRecord(kv, &got))
		require.Equal(t, record, got)
		reject := func(wire []byte) {
			bad := *kv
			bad.Value = wire
			dst := record
			require.ErrorIs(t, decodeTaskClaimRecord(&bad, &dst), ErrCorruptRecord)
			require.Equal(t, record, dst)
		}
		taskBadFields(t, kv.Value, reject, "")
		reject(append(bytes.Clone(kv.Value), 0xff))
		reject(append(bytes.Clone(kv.Value), []byte(" {}")...))
		for _, modify := range []func(*mvccpb.KeyValue){func(k *mvccpb.KeyValue) { k.Lease = 0 }, func(k *mvccpb.KeyValue) { k.Lease = 8 }, func(k *mvccpb.KeyValue) { k.ModRevision++ }, func(k *mvccpb.KeyValue) { k.CreateRevision = 0 }} {
			bad := *kv
			modify(&bad)
			dst := record
			require.ErrorIs(t, decodeTaskClaimRecord(&bad, &dst), ErrCorruptRecord)
			require.Equal(t, record, dst)
		}
		padded := *kv
		padded.Value = append(bytes.Clone(kv.Value), bytes.Repeat([]byte(" "), 4096-len(kv.Value))...)
		require.NoError(t, decodeTaskClaimRecord(&padded, &got))
		reject(append(padded.Value, ' '))
		clear(kv.Value)
		require.Equal(t, record, got)
	})
}

func TestTaskClaimLifecycle(t *testing.T) {
	t.Run("original-pair-renew-release", func(t *testing.T) {
		b, raw, ref, ctx := taskClaimFixture(t)
		lease := &creationFaultLease{Lease: b.client.Lease}
		b.client.Lease = lease
		sent := time.Now()
		c, e := b.ClaimTask(ctx, ref, "worker", 1500*time.Millisecond)
		require.NoError(t, e)
		defer taskReleaseClaim(t, b, c)
		require.EqualValues(t, 2, lease.requestedTTL.Load())
		require.True(t, c.deadline.Before(time.Now().Add(2*time.Second)))
		require.False(t, c.deadline.Before(sent.Add(2*time.Second)))
		original := c.Reference()
		for _, key := range []string{c.claimKey, c.guardKey} {
			v, e := raw.Get(ctx, key)
			require.NoError(t, e)
			require.Len(t, v.Kvs, 1)
			require.Equal(t, original.LeaseID, v.Kvs[0].Lease)
			require.Equal(t, original.CreateRevision, v.Kvs[0].CreateRevision)
			require.Equal(t, v.Kvs[0].CreateRevision, v.Kvs[0].ModRevision)
			require.Equal(t, c.value, string(v.Kvs[0].Value))
		}
		require.NoError(t, b.RenewTaskClaim(ctx, c))
		require.EqualValues(t, 1, lease.grants.Load())
		require.EqualValues(t, 1, lease.keeps.Load())
		require.Equal(t, original, c.Reference())
		require.NoError(t, b.ReleaseTaskClaim(ctx, c))
		require.Error(t, b.RenewTaskClaim(ctx, c))
		for _, f := range c.fences {
			v, e := raw.Get(ctx, f.key)
			require.NoError(t, e)
			require.Len(t, v.Kvs, 1)
			require.True(t, f.matches(v.Kvs[0]))
		}
		cp, e := b.LoadTaskCheckpoint(ctx, ref)
		require.NoError(t, e)
		require.NotNil(t, cp)
	})
	t.Run("concurrent-one-winner", func(t *testing.T) {
		b, _, ref, ctx := taskClaimFixture(t)
		type result struct {
			c *TaskClaim
			e error
		}
		results := make(chan result, 2)
		start := make(chan struct{})
		for _, worker := range []string{"first", "second"} {
			go func(worker string) {
				<-start
				c, e := b.ClaimTask(ctx, ref, worker, 30*time.Second)
				results <- result{c, e}
			}(worker)
		}
		close(start)
		a, z := <-results, <-results
		winners := 0
		for _, r := range []result{a, z} {
			if r.e == nil {
				winners++
				require.NotNil(t, r.c)
				taskReleaseClaim(t, b, r.c)
			} else {
				require.Nil(t, r.c)
				require.ErrorIs(t, r.e, ErrConflict)
			}
		}
		require.Equal(t, 1, winners)
	})

	t.Run("coherent-bundle", func(t *testing.T) {
		b, raw, r, ctx := taskClaimFixture(t)
		bundle, e := b.loadTaskClaimBundle(ctx, r)
		require.NoError(t, e)
		require.Len(t, bundle.fences, 8)
		require.Equal(t, r, bundle.task.Reference)
		require.Equal(t, bundle.birth, bundle.checkpoint.CreateRevision)
		require.Equal(t, bundle.birth, bundle.fences[4].mod)
		require.Greater(t, bundle.birth, bundle.task.ControlRevision)
		// New claim setup must reject an existing fixed claim without adopting it.
		key, e := b.namespace.taskClaimKey(r)
		require.NoError(t, e)
		_, e = raw.Put(ctx, key, "existing")
		require.NoError(t, e)
		denied, e := b.loadTaskClaimBundle(ctx, r)
		require.ErrorIs(t, e, ErrConflict)
		require.Nil(t, denied)
	})
}

func taskReleaseClaim(t *testing.T, b *Backend, c *TaskClaim) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, b.ReleaseTaskClaim(ctx, c))
}
