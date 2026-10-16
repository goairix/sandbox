package etcd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestSnapshotCacheOptions(t *testing.T) {
	for _, size := range []uint64{0, 256 * 1024, 32 * 1024 * 1024, 64 * 1024 * 1024} {
		o := validOptions(t)
		o.SnapshotCacheBytes = size
		require.NoError(t, validateOptions(o))
	}
	for _, size := range []uint64{1, 256*1024 - 1, 64*1024*1024 + 1, ^uint64(0)} {
		o := validOptions(t)
		o.SnapshotCacheBytes = size
		require.ErrorIs(t, validateOptions(o), ErrInvalidConfiguration)
	}
}

func snapshotCacheTestRecord(id, payload string) (snapshotCacheKey, *SandboxSnapshotRecord) {
	return snapshotCacheKey{partition: 1, sandboxID: id, version: "v1", digest: strings.Repeat("a", 64)}, &SandboxSnapshotRecord{Version: 1, SandboxID: id, Snapshot: SnapshotReference{Version: "v1", Digest: strings.Repeat("a", 64)}, Payload: []byte(payload)}
}

func TestSnapshotCacheWeightAndClone(t *testing.T) {
	b := &Backend{archiveLife: &taskArchiveBackend{}}
	c := newSnapshotReadCache(b, 256*1024)
	require.Zero(t, c.used)
	require.Nil(t, c.entries, "constructor must not preallocate cache entries")
	require.Nil(t, c.calls)
	require.Nil(t, c.misses)
	key, record := snapshotCacheTestRecord("original", `{"a":1}`)
	c.store(key, record)
	record.Payload[2] = 'z'
	got := c.lookup(key)
	require.Equal(t, `{"a":1}`, string(got.Payload))
	got.Payload[2] = 'x'
	got.SandboxID = "caller"
	require.Equal(t, "original", c.lookup(key).SandboxID)
	require.Equal(t, `{"a":1}`, string(c.lookup(key).Payload))
	// Hundreds of tiny entries cannot evade a byte budget through zero payload.
	for i := 0; i < 2000; i++ {
		k, r := snapshotCacheTestRecord(fmt.Sprintf("tiny-%d", i), "{}")
		c.store(k, r)
		require.LessOrEqual(t, c.used, uint64(256*1024))
	}
	require.Less(t, len(c.entries), 1000)
	require.Nil(t, c.lookup(key), "oldest tiny record must be evicted")
	// Recently used entries survive, older large entries make room first.
	c = newSnapshotReadCache(b, 256*1024)
	keys := make([]snapshotCacheKey, 5)
	for i := 0; i < 4; i++ {
		k, r := snapshotCacheTestRecord(fmt.Sprint(i), strings.Repeat("x", 60*1024))
		keys[i] = k
		c.store(k, r)
	}
	require.NotNil(t, c.lookup(keys[0]))
	keys[4], record = snapshotCacheTestRecord("4", strings.Repeat("y", 60*1024))
	c.store(keys[4], record)
	require.Nil(t, c.lookup(keys[1]))
	require.NotNil(t, c.lookup(keys[0]))
	require.LessOrEqual(t, c.used, uint64(256*1024))
}

func snapshotLifecycleBackend(t *testing.T) *Backend {
	t.Helper()
	b := &Backend{archiveLife: &taskArchiveBackend{}, requestTimeout: time.Second}
	b.snapshotReads = newSnapshotReadCache(b, 0)
	t.Cleanup(func() {
		err := b.Close()
		// NewCtxClient has no gRPC connection; upstream Close returns its
		// canceled context. Production clients close their real connection.
		require.True(t, err == nil || errors.Is(err, context.Canceled), "%v", err)
	})
	return b
}

func TestSnapshotReadAdmissionAndClose(t *testing.T) {
	b := snapshotLifecycleBackend(t)
	calls := make([]*snapshotReadCall, 256)
	t.Cleanup(func() {
		for _, call := range calls {
			if call != nil {
				call.finish()
			}
		}
	})
	for i := range calls {
		var err error
		calls[i], err = b.registerSnapshotRead(context.Background())
		require.NoError(t, err)
	}
	_, err := b.registerSnapshotRead(context.Background())
	require.ErrorIs(t, err, ErrSnapshotReadCapacity)
	copy := *b
	_, err = copy.registerSnapshotRead(context.Background())
	require.ErrorIs(t, err, ErrInvalidRecord, "diagnostic Backend copy cannot own read admission")
	calls[0].finish()
	replacement, err := b.registerSnapshotRead(context.Background())
	require.NoError(t, err)
	calls[0] = replacement
	done := make(chan error, 1)
	go func() { done <- b.Close() }()
	for _, call := range calls {
		select {
		case <-call.context.Done():
		case <-time.After(time.Second):
			t.Fatal("Close did not cancel bounded active caller")
		}
	}
	select {
	case <-done:
		t.Fatal("Close returned before original callers left")
	default:
	}
	_, err = b.registerSnapshotRead(context.Background())
	require.ErrorIs(t, err, context.Canceled)
	for _, call := range calls {
		call.finish()
	}
	require.NoError(t, <-done)
	require.Empty(t, b.snapshotReads.calls)
	require.Empty(t, b.snapshotReads.entries)
}

// Only the transport blocks; all cache admission, coalescing, cancellation and
// ownership behavior runs through the production Backend and worker.
type snapshotBlockingKV struct {
	clientv3.KV
	started chan context.Context
	release chan struct{}
}

func (k *snapshotBlockingKV) Txn(ctx context.Context) clientv3.Txn {
	return &snapshotBlockingTxn{context: ctx, kv: k}
}

type snapshotBlockingTxn struct {
	context context.Context
	kv      *snapshotBlockingKV
}

func (t *snapshotBlockingTxn) If(...clientv3.Cmp) clientv3.Txn  { return t }
func (t *snapshotBlockingTxn) Then(...clientv3.Op) clientv3.Txn { return t }
func (t *snapshotBlockingTxn) Else(...clientv3.Op) clientv3.Txn { return t }
func (t *snapshotBlockingTxn) Commit() (*clientv3.TxnResponse, error) {
	t.kv.started <- t.context
	select {
	case <-t.kv.release:
		return nil, ErrCorruptRecord
	case <-t.context.Done():
		return nil, t.context.Err()
	}
}

func TestSnapshotReadMissBoundsAndCancellation(t *testing.T) {
	b := snapshotLifecycleBackend(t)
	o := validOptions(t)
	b.namespace, b.clusterID, b.restoreEpoch = o.Namespace, 1, "epoch"
	b.identityKey, _ = b.namespace.Key("meta", "identity")
	b.restoreKey, _ = b.namespace.Key("meta", "restore_epoch")
	b.identityValue = "identity"
	client := clientv3.NewCtxClient(context.Background())
	b.client = client
	transport := &snapshotBlockingKV{started: make(chan context.Context, 32), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-transport.release:
		default:
			close(transport.release)
		}
	})
	client.KV = transport
	callers := make([]*snapshotReadCall, 17)
	t.Cleanup(func() {
		for _, call := range callers {
			if call != nil {
				call.finish()
			}
		}
	})
	results := make(chan error, 18)
	var err error
	for i := 0; i < 16; i++ {
		callers[i], err = b.registerSnapshotRead(context.Background())
		require.NoError(t, err)
		key, _ := snapshotCacheTestRecord(fmt.Sprintf("miss-%d", i), "{}")
		go func(call *snapshotReadCall, key snapshotCacheKey) {
			_, e := b.snapshotRecord(call, key, SandboxPlacementRecord{})
			call.finish()
			results <- e
		}(callers[i], key)
		select {
		case <-transport.started:
		case <-time.After(time.Second):
			t.Fatal("miss worker did not start")
		}
	}
	callers[16], err = b.registerSnapshotRead(context.Background())
	require.NoError(t, err)
	key, _ := snapshotCacheTestRecord("overflow", "{}")
	_, err = b.snapshotRecord(callers[16], key, SandboxPlacementRecord{})
	require.ErrorIs(t, err, ErrSnapshotReadCapacity)
	callers[16].finish()
	// A new waiter on an existing key still joins even with all 16 slots full.
	waiter, err := b.registerSnapshotRead(context.Background())
	require.NoError(t, err)
	t.Cleanup(waiter.finish)
	key, _ = snapshotCacheTestRecord("miss-0", "{}")
	_, shared, err := b.startSnapshotRead(waiter, key, SandboxPlacementRecord{})
	require.NoError(t, err)
	callers[0].cancel()
	require.ErrorIs(t, <-results, context.Canceled)
	b.archiveLife.mu.Lock()
	misses := len(b.snapshotReads.misses)
	b.archiveLife.mu.Unlock()
	require.Equal(t, 16, misses)
	close(transport.release)
	for i := 0; i < 15; i++ {
		require.Error(t, <-results)
	}
	<-shared.done
	require.Error(t, shared.err)
	require.NoError(t, waiter.context.Err(), "first caller cancellation cannot cancel shared worker or other waiter")
	waiter.finish()
	require.Empty(t, transport.started, "shared waiter must not start another worker")
	require.ErrorIs(t, b.Close(), context.Canceled, "NewCtxClient returns its canceled context when closed")
	require.Empty(t, b.snapshotReads.misses)
}

func TestSnapshotReadMissDeadline(t *testing.T) {
	b := snapshotLifecycleBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	call, err := b.registerSnapshotRead(ctx)
	require.NoError(t, err)
	t.Cleanup(call.finish)
	<-call.context.Done()
	key, _ := snapshotCacheTestRecord("expired", "{}")
	_, _, err = b.startSnapshotRead(call, key, SandboxPlacementRecord{})
	require.ErrorIs(t, err, context.DeadlineExceeded, "an expired caller preserves its timeout attribution")
	call.finish()

	o := validOptions(t)
	b.namespace, b.clusterID, b.restoreEpoch = o.Namespace, 1, "epoch"
	b.identityKey, _ = b.namespace.Key("meta", "identity")
	b.restoreKey, _ = b.namespace.Key("meta", "restore_epoch")
	b.identityValue = "identity"
	b.requestTimeout = 20 * time.Millisecond
	b.client = clientv3.NewCtxClient(context.Background())
	transport := &snapshotBlockingKV{started: make(chan context.Context, 1), release: make(chan struct{})}
	b.client.KV = transport
	call, err = b.registerSnapshotRead(context.Background())
	require.NoError(t, err)
	t.Cleanup(call.finish)
	key, _ = snapshotCacheTestRecord("miss-timeout", "{}")
	_, err = b.snapshotRecord(call, key, SandboxPlacementRecord{})
	require.ErrorIs(t, err, context.DeadlineExceeded, "the shared worker has its own fixed request timeout")
	require.NoError(t, call.context.Err(), "worker expiry cannot cancel the caller context")
	call.finish()
}
