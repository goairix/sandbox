//go:build linux

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

func recoveryReviewGuardRead(b *Backend, ops []clientv3.Op) bool {
	return len(ops) == 1 && ops[0].IsGet() && string(ops[0].KeyBytes()) == b.identityKey
}

func TestTaskWorkspaceSyncRecoveryNativeReviewMonitor(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	f, wire, slot := recoveryExecutionCold(t)
	s, err := f.b.prepareTaskWorkspaceSyncRecovery(f.ctx, f.c, slot, wire.destination, workspaceSyncLocalNativeWorker)
	require.NoError(t, err)
	require.NoError(t, <-wire.done)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	original := f.b.client.KV
	pauseMonitor := func() (<-chan struct{}, chan struct{}) {
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		recoveryExecutionSetKV(s, &faultKV{KV: original, match: func(ops []clientv3.Op) bool { return recoveryReviewGuardRead(f.b, ops) }, before: func() { once.Do(func() { close(entered); <-release }) }})
		return entered, release
	}
	entered, release := pauseMonitor()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("idle monitor did not enter actual native guard")
	}
	foreground := make(chan error, 1)
	go func() { _, err := s.next(f.ctx); foreground <- err }()
	early := false
	var foregroundErr error
	select {
	case foregroundErr = <-foreground:
		early = true
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if !early {
		foregroundErr = <-foreground
	}
	require.False(t, early, "a live foreground stage must wait for an admitted monitor, not fail because its busy bit is held")
	require.NoError(t, foregroundErr)
	entered, release = pauseMonitor()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("second monitor guard did not enter")
	}
	go func() { _, err := s.next(f.ctx); foreground <- err }()
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		close(release)
		t.Fatalf("Close crossed registered monitor IO: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	select {
	case err := <-foreground:
		require.Error(t, err, "waiting foreground cancels when owner seals")
	case <-time.After(time.Second):
		close(release)
		t.Fatal("foreground cancellation did not return")
	}
	close(release)
	require.NoError(t, <-closed)
}

func TestTaskWorkspaceSyncRecoveryNativeReviewTerminalPostguard(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	f, wire, slot := recoveryExecutionCold(t)
	s, err := f.b.prepareTaskWorkspaceSyncRecovery(f.ctx, f.c, slot, wire.destination, workspaceSyncLocalNativeWorker)
	require.NoError(t, err)
	require.NoError(t, <-wire.done)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	body, charge, q := recoveryExecutionFirst(t, s)
	original := f.b.client.KV
	var written atomic.Bool
	var writes atomic.Int32
	terminalKV := &faultKV{KV: original, match: recyclingTerminalMatch, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		require.NoError(t, err)
		require.True(t, r.Succeeded)
		writes.Add(1)
		written.Store(true)
		return r, nil
	}}
	var failed atomic.Bool
	recoveryExecutionSetKV(s, &faultKV{KV: terminalKV, match: func(ops []clientv3.Op) bool { return recoveryReviewGuardRead(f.b, ops) }, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		if written.Load() && failed.CompareAndSwap(false, true) {
			require.NoError(t, err)
			require.True(t, r.Succeeded)
			return nil, context.DeadlineExceeded
		}
		return r, err
	}})
	require.ErrorIs(t, s.settleLocal(f.ctx, charge, q), ErrOutcomeUnknown, "only the successful write's final native postguard is lost")
	require.True(t, failed.Load())
	require.False(t, q.known)
	require.EqualValues(t, 1, writes.Load())
	require.NoError(t, s.settleLocal(f.ctx, charge, q), "same fresh attempt must recognize its exact terminal through a read")
	require.True(t, q.known)
	require.NoError(t, s.settleLocal(f.ctx, charge, q))
	require.EqualValues(t, 1, writes.Load(), "postguard retry never retransmits terminal")
	recoveryExecutionSetKV(s, original)
	outcome, err := s.recycleLocal(f.ctx, charge, q)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	require.NoError(t, body.Close())
}
