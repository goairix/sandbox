package etcd

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This finite metadata actor fixture tests original enqueue and workspace
// ownership only. It creates no publication/Kernel or managed Claim authority;
// the separate signed-source actor test exercises actual source authentication.
func managedSourceMetadataWorker(t *testing.T, f *dueExpiryFixture, scan *dueWorkerScan, task TaskReference) (*DueWorker, *cleanupLocalJob) {
	t.Helper()
	f.b.localCleanup = &LocalCleanupOptions{MaxActive: 32}
	f.b.detachedCleanup = &detachedCleanupConfig{}
	cache, err := newDueWorkerCache(f.b, f.session)
	require.NoError(t, err)
	w := &DueWorker{backend: f.b, session: f.session, ctx: f.session.ctx, scans: []*dueWorkerScan{scan}, cache: cache, cleanupWake: make(chan struct{}, 1)}
	w.self = w
	j, err := w.enqueueCleanupLocal(context.Background(), task)
	require.NoError(t, err)
	j.step = "snapshot" // Scheduling seam only; it grants no native source fact.
	t.Cleanup(func() {
		w.releaseCleanupManagedSource(j)
		f.b.finishCleanupJob(j, context.Canceled)
	})
	return w, j
}

func TestCleanupManagedSourceWorkspacePreservesPendingThenProgresses(t *testing.T) {
	f := newDueExpiryFixture(t)
	scan := dueCheckpointTestScan(t, f)
	f.earn()
	require.NoError(t, scan.step(context.Background()))
	native := f.b.client.KV
	f.b.client.KV = &faultKV{KV: native, match: dueCheckpointTestPendingPut, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		require.NoError(t, err)
		require.True(t, r.Succeeded)
		return nil, context.DeadlineExceeded
	}}
	f.earnClaim()
	require.ErrorIs(t, scan.step(context.Background()), ErrOutcomeUnknown)
	f.b.client.KV = native
	// A lost mutation ACK loses the old session as designed. Release it and
	// acquire genuine new partition/budget capabilities; never revive its flags
	// or adopt an old cursor. Unlike newDueWorkerScan, this finite scheduling
	// seam intentionally pauses before its constructor's automatic resolution,
	// so workspace admission must first yield to the real persisted Pending.
	oldSession := f.session
	dueCheckpointTestReopenExpiry(t, f)
	require.NotSame(t, oldSession, f.session)
	saved, err := f.b.readDueCheckpoint(context.Background(), f.partition)
	require.NoError(t, err)
	require.NotNil(t, saved.record.Pending)
	scan = &dueWorkerScan{backend: f.b, partition: f.partition, ranges: f.share, claims: f.claimShare, ttl: 30 * time.Second, checkpoint: saved}
	w, j := managedSourceMetadataWorker(t, f, scan, saved.record.Pending.Task)
	key, err := f.b.namespace.dueCandidateKey(f.candidate)
	require.NoError(t, err)
	wire, err := encodeDueCandidate(f.candidate)
	require.NoError(t, err)
	require.NoError(t, w.cache.put(key, wire, f.candidate))
	before := w.cache.snapshot()
	cursor, page, index := scan.cursor, scan.page, scan.index
	pendingKey := dueCheckpointTestKey(f.duePageFixture)
	pendingNative := f.server.point(pendingKey)

	ready, err := w.acquireCleanupManagedSource(j)
	require.NoError(t, err)
	require.False(t, ready)
	require.Nil(t, w.managedSource)
	require.False(t, w.cache.managedSource)
	require.Equal(t, before, w.cache.snapshot())
	require.Same(t, saved, scan.checkpoint)
	require.Equal(t, cursor, scan.cursor)
	require.Equal(t, page, scan.page)
	require.Equal(t, index, scan.index)
	require.Equal(t, pendingNative, f.server.point(pendingKey))

	// The original due lane resolves the same unstarted attempt. Source may
	// then progress; no replacement checkpoint or invented resolved DTO is used.
	outcome, err := scan.resolvePending(context.Background())
	require.NoError(t, err)
	require.Equal(t, OutcomeAborted, outcome)
	require.Nil(t, scan.checkpoint.record.Pending)
	ready, err = w.acquireCleanupManagedSource(j)
	require.NoError(t, err)
	require.True(t, ready)
	require.Equal(t, before.OwnedBytes+24<<20, w.cache.snapshot().OwnedBytes)
	first := w.managedSource
	ready, err = w.acquireCleanupManagedSource(j)
	require.NoError(t, err)
	require.True(t, ready)
	require.Same(t, first, w.managedSource)
	require.Equal(t, before.OwnedBytes+24<<20, w.cache.snapshot().OwnedBytes, "same job does not double-reserve")
	w.releaseCleanupManagedSource(j)
	require.Equal(t, before, w.cache.snapshot())
}

func TestCleanupManagedSourceWorkspaceRebuildsHighCacheCoverage(t *testing.T) {
	f := newDueExpiryFixture(t)
	scan := dueCheckpointTestScan(t, f)
	f.earn()
	require.NoError(t, scan.step(context.Background()))
	f.earnClaim()
	result, err := f.admit()
	require.NoError(t, err)
	w, j := managedSourceMetadataWorker(t, f, scan, result.Task)
	for n := 0; w.cache.bytes <= 17<<20; n++ {
		candidate := f.candidate
		candidate.SandboxID = fmt.Sprintf("cache-%05d-%s", n, strings.Repeat("x", 100))
		key, err := f.b.namespace.dueCandidateKey(candidate)
		require.NoError(t, err)
		wire, err := encodeDueCandidate(candidate)
		require.NoError(t, err)
		require.NoError(t, w.cache.put(key, wire, candidate))
	}
	checkpoint := scan.checkpoint
	ready, err := w.acquireCleanupManagedSource(j)
	require.NoError(t, err)
	require.True(t, ready, "large cache cannot indefinitely starve source")
	require.Zero(t, w.cache.count)
	require.Zero(t, w.cache.bytes)
	require.Same(t, checkpoint, scan.checkpoint, "rebuild retains the original CAS handle")
	require.Nil(t, scan.cursor)
	require.Nil(t, scan.page)
	require.True(t, scan.fresh)
	require.True(t, w.windowAt.IsZero())
	keys, bytes := w.cache.limitsLocked(false, false)
	require.Equal(t, dueCacheKeys-20, keys)
	require.Equal(t, uint64(16<<20), bytes)
	require.Equal(t, uint64(48<<20), w.Status().OwnedBytes)
	require.Zero(t, w.Status().Queued, "source reserve is not a fabricated cache row")
	w.releaseCleanupManagedSource(j)
	keys, bytes = w.cache.limitsLocked(false, false)
	require.Equal(t, dueCacheKeys, keys)
	require.Equal(t, uint64(40<<20), bytes)
	// A real fixed-R refill can now rebuild the invalidated view.
	f.earn()
	require.NoError(t, scan.refill(context.Background()))
	require.False(t, scan.fresh)
}

func TestCleanupManagedSourceMemorySchemaBounds(t *testing.T) {
	types := []reflect.Type{
		reflect.TypeOf(SandboxSnapshotRecord{}), reflect.TypeOf(CreationIntentRecord{}),
		reflect.TypeOf(RuntimeEnrolledPublicationRecord{}), reflect.TypeOf(RuntimeEnrolledPublicationProofRecord{}),
		reflect.TypeOf(RuntimeEnrollmentRecord{}), reflect.TypeOf(RuntimeEnrollmentActivationRecord{}),
		reflect.TypeOf(RuntimeDispatchRecord{}), reflect.TypeOf(RuntimeDispatchInputRecord{}),
		reflect.TypeOf(RuntimeBindingRecord{}), reflect.TypeOf(RuntimeBindingCertificateRecord{}),
		reflect.TypeOf(RuntimeIndexRecord{}), reflect.TypeOf(RuntimeMountIntentRecord{}),
	}
	var headers uintptr
	var walk func(reflect.Type, int)
	walk = func(typ reflect.Type, depth int) {
		if typ == reflect.TypeOf(json.RawMessage{}) || typ == reflect.TypeOf(time.Time{}) {
			return // opaque JSON is scanned, never decoded to recursive any.
		}
		switch typ.Kind() {
		case reflect.Pointer, reflect.Array, reflect.Slice:
			walk(typ.Elem(), depth)
		case reflect.Struct:
			require.LessOrEqual(t, depth, 8, typ.String())
			require.LessOrEqual(t, typ.NumField(), 64, typ.String())
			for i := 0; i < typ.NumField(); i++ {
				walk(typ.Field(i).Type, depth+1)
			}
		}
	}
	for _, typ := range types {
		headers += typ.Size()
		walk(typ, 1)
	}
	headers += reflect.TypeOf(cleanupManagedSourceWorkspace{}).Size()
	require.Less(t, headers, uintptr(32<<10), "fixed source headers must remain in the explicit model")
	require.Less(t, cleanupManagedSourceBound(), uint64(16<<20))
	require.Equal(t, 24<<20, cleanupManagedWorkspaceBytes)
	require.Equal(t, 20, cleanupManagedWorkspaceKeys)
}
