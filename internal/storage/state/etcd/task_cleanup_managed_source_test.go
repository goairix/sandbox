package etcd

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// The original acquisition/publication/destroy producers create every positive
// source point. This queue-only host fixture exercises the real registry actor,
// not full-local startup, a Kernel, managed Claim admission or runtime effects.
// Source verification must finish before the still-unadmitted wire phase fails.
func TestCleanupManagedSourceActorAuthenticatesBeforeWireAdmission(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		name := "signed-source"
		if corrupt {
			name = "changed-native-companion"
		}
		t.Run(name, func(t *testing.T) {
			f := newDetachedCleanupSourceFixture(t, "detached", &DetachedCleanupOptions{PinnedPlainImage: detachedCleanupPlainImageForTest}, func(o *Options) {
				o.WorkerControl.SessionTTL = 60 * time.Second
			})
			b, ref := f.b, f.task.Reference
			publicationKey, proofKey, err := b.namespace.runtimePublicationKeys(ref.Partition, f.task.CreationIntentID)
			require.NoError(t, err)
			executionKey, err := b.namespace.cleanupExecutionKey(ref)
			require.NoError(t, err)
			pendingKey, err := b.namespace.cleanupPendingKey(ref)
			require.NoError(t, err)
			taskKey, err := b.namespace.taskKey(ref)
			require.NoError(t, err)
			taskBefore, pendingBefore := f.server.point(taskKey), f.server.point(pendingKey)
			require.NotNil(t, taskBefore)
			require.Nil(t, f.server.point(executionKey))
			if corrupt {
				// This is an actual foreign native update used only as a negative.
				// No copied signed DTO repairs its now-invalid immutable envelope.
				f.server.put(proofKey, "{}", 0)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			t.Cleanup(cancel)
			// Match the existing queue-only actor seam. The public physical entry
			// remains unusable; source reads cannot manufacture its missing Kernel.
			b.localCleanup = &LocalCleanupOptions{MaxActive: 32}
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			var w *DueWorker
			var actor atomic.Pointer[DueWorker]
			var publicationReads, fullReads, forbiddenWrites atomic.Int64
			var reservationObserved atomic.Bool
			native := b.client.KV
			b.client.KV = &faultKV{KV: native, match: func(ops []clientv3.Op) bool {
				selected := false
				for _, op := range ops {
					key := string(op.KeyBytes())
					if op.IsGet() && key == publicationKey {
						publicationReads.Add(1)
						selected = true
					}
					if (op.IsPut() || op.IsDelete()) && (key == executionKey || strings.HasPrefix(key, b.namespace.Root()+"cleanup-execution-slots/") || strings.HasPrefix(key, b.namespace.Root()+"command-issuers/")) {
						forbiddenWrites.Add(1)
					}
				}
				if selected {
					// This observation runs on the actual native path after workspace
					// admission, never on a test-only reserve/projection entry point.
					if current := actor.Load(); current != nil && current.Status().OwnedBytes >= uint64(dueCacheReservedBytes)+(24<<20) {
						reservationObserved.Store(true)
					}
					if len(ops) == 16 {
						fullReads.Add(1) // identity/restore plus the original 14 points
					}
				}
				return false
			}}
			w, err = b.StartDueWorker(ctx, DueWorkerOptions{WorkerID: "managed-source", Partitions: []uint8{ref.Partition}, RangeSlots: []uint16{0}, ClaimSlots: []uint16{0}, RelistSlot: 0, StageTTL: 30 * time.Second})
			require.NoError(t, err)
			actor.Store(w)
			require.Equal(t, int64(60), w.session.ttlSeconds, "actual constructor-owned session must satisfy cleanup owner profile")
			t.Cleanup(func() {
				cancel()
				_ = w.Stop()
				workerTestReceive(t, w.Done())
			})
			initialGrants := lease.grants.Load()
			job, err := w.enqueueCleanupLocal(ctx, ref)
			require.NoError(t, err)
			select {
			case <-job.done:
			case <-ctx.Done():
				t.Fatal("original source actor did not finish its native work")
			}

			if corrupt {
				require.ErrorIs(t, job.err, ErrCorruptRecord)
				require.NotEqual(t, "detached-source-verified", job.result.Reason)
			} else {
				require.ErrorIs(t, job.err, ErrInvalidConfiguration, "wire/physical phase remains unadmitted after real source verification")
				require.Equal(t, "detached-source-verified", job.result.Reason)
			}
			require.GreaterOrEqual(t, publicationReads.Load(), int64(2))
			require.Positive(t, fullReads.Load())
			require.True(t, reservationObserved.Load(), "source RPCs must already own the additional 24MiB workspace charge")
			require.Zero(t, forbiddenWrites.Load(), "no issuer registration, occupation or Run before wire admission")
			require.Equal(t, initialGrants, lease.grants.Load(), "source authentication grants no TaskClaim Lease")
			require.Nil(t, job.claim)
			require.Nil(t, f.server.point(executionKey))
			require.Equal(t, taskBefore, f.server.point(taskKey))
			require.Equal(t, pendingBefore, f.server.point(pendingKey))
			require.Equal(t, w.cache.snapshot().OwnedBytes, w.Status().OwnedBytes)
			require.Less(t, w.Status().OwnedBytes, uint64(dueCacheReservedBytes)+(24<<20), "actual native return and source retirement release the workspace charge")
		})
	}
}

// Cancellation cannot refund a native response still held by the original
// client call. This blocks only after the real server returned the full signed
// history; no fabricated response, source fact or physical admission is used.
func TestCleanupManagedSourceWorkspaceWaitsForActualNativeReturn(t *testing.T) {
	f := newDetachedCleanupSourceFixture(t, "detached", &DetachedCleanupOptions{PinnedPlainImage: detachedCleanupPlainImageForTest}, func(o *Options) {
		o.WorkerControl.SessionTTL = 60 * time.Second
	})
	b, task := f.b, f.task
	b.localCleanup = &LocalCleanupOptions{MaxActive: 32}
	publicationKey, _, err := b.namespace.runtimePublicationKeys(task.Reference.Partition, task.CreationIntentID)
	require.NoError(t, err)
	_, snapshotKey, err := b.namespace.sandboxKeys(task.Reference.Partition, task.Reference.SandboxID, task.Snapshot.Version)
	require.NoError(t, err)
	intentKey, err := b.namespace.intentKey(task.Reference.Partition, task.CreationIntentID)
	require.NoError(t, err)
	entered, returnNative := make(chan struct{}), make(chan struct{})
	var release sync.Once
	unblock := func() { release.Do(func() { close(returnNative) }) }
	t.Cleanup(unblock)
	var keysMu sync.Mutex
	seenKeys := make(map[string]bool)
	native := b.client.KV
	b.client.KV = &faultKV{KV: native, match: func(ops []clientv3.Op) bool {
		source, full := false, false
		for _, op := range ops {
			key := string(op.KeyBytes())
			if op.IsGet() && (key == publicationKey || key == snapshotKey || key == intentKey) {
				source = true
				full = full || key == publicationKey && len(ops) == 16
			}
		}
		if source {
			keysMu.Lock()
			for _, op := range ops {
				seenKeys[string(op.KeyBytes())] = true
			}
			keysMu.Unlock()
		}
		return full
	}, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		close(entered)
		<-returnNative
		return r, err
	}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w, err := b.StartDueWorker(ctx, DueWorkerOptions{WorkerID: "source-native-return", Partitions: []uint8{task.Reference.Partition}, RangeSlots: []uint16{0}, ClaimSlots: []uint16{0}, RelistSlot: 0, StageTTL: 30 * time.Second})
	require.NoError(t, err)
	require.Equal(t, int64(60), w.session.ttlSeconds, "actual constructor-owned session must satisfy cleanup owner profile")
	t.Cleanup(func() {
		unblock()
		cancel()
		_ = w.Stop()
		workerTestReceive(t, w.Done())
	})
	j, err := w.enqueueCleanupLocal(ctx, task.Reference)
	require.NoError(t, err)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("actual full source response was not reached")
	}
	keysMu.Lock()
	count := len(seenKeys)
	keysMu.Unlock()
	require.Equal(t, 19, count, "four locating points are a subset of full14; plus snapshot/placement/intent/identity/restore")
	require.LessOrEqual(t, count, 20)
	cancel()
	stopped := make(chan struct{})
	go func() {
		_ = w.Stop()
		close(stopped)
	}()
	require.GreaterOrEqual(t, w.Status().OwnedBytes, uint64(48<<20))
	select {
	case <-j.done:
		t.Fatal("cancellation detached the still-running original source call")
	default:
	}
	select {
	case <-w.Done():
		t.Fatal("Stop completed before the original call returned")
	default:
	}
	unblock()
	workerTestReceive(t, stopped)
	workerTestReceive(t, w.Done())
	select {
	case <-j.done:
	case <-time.After(3 * time.Second):
		t.Fatal("source registry job did not finish after actual native return")
	}
	require.Less(t, w.Status().OwnedBytes, uint64(48<<20))
	require.Nil(t, j.claim)
}
