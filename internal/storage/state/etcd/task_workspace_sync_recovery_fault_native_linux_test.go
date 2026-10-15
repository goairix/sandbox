//go:build linux

package etcd

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func recoveryExecutionSetKV(s *taskWorkspaceSyncRecoverySource, kv clientv3.KV) {
	s.authority.nativeMu.Lock()
	s.authority.backend.client.KV = kv
	s.authority.nativeMu.Unlock()
}

func TestTaskWorkspaceSyncRecoveryNativeUnknown(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	for _, stage := range []string{"terminal", "gc"} {
		t.Run(stage, func(t *testing.T) {
			f, wire, slot := recoveryExecutionCold(t)
			s, err := f.b.prepareTaskWorkspaceSyncRecovery(f.ctx, f.c, slot, wire.destination, workspaceSyncLocalNativeWorker)
			require.NoError(t, err)
			require.NoError(t, <-wire.done)
			body, charge, q := recoveryExecutionFirst(t, s)
			copy := *q
			copy.self = &copy
			require.Error(t, s.settleLocal(f.ctx, charge, &copy), "copied physical diagnostics cannot become a completion owner")
			if stage == "gc" {
				require.NoError(t, s.settleLocal(f.ctx, charge, q))
			}
			original := f.b.client.KV
			var writes atomic.Int32
			match := recyclingTerminalMatch
			if stage == "gc" {
				match = recyclingGCMatch
			}
			recoveryExecutionSetKV(s, &faultKV{KV: original, match: match, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				require.NoError(t, err)
				require.True(t, r.Succeeded)
				writes.Add(1)
				return nil, context.DeadlineExceeded
			}})
			call := func() error {
				if stage == "terminal" {
					return s.settleLocal(f.ctx, charge, q)
				}
				_, err := s.recycleLocal(f.ctx, charge, q)
				return err
			}
			require.ErrorIs(t, call(), ErrOutcomeUnknown)
			require.NoError(t, call(), "only exact read arbitration resolves this fresh attempt")
			require.NoError(t, call(), "known completion does not repeat its native write")
			require.EqualValues(t, 1, writes.Load())
			recoveryExecutionSetKV(s, original)
			if stage == "terminal" {
				_, err = s.recycleLocal(f.ctx, charge, q)
				require.NoError(t, err)
			}
			actual, err := os.ReadFile(filepath.Join(s.target.pin.Root, body.entry.path))
			require.NoError(t, err)
			require.Equal(t, "hello", string(actual))
			require.NoError(t, body.Close())
			require.NoError(t, s.Close())
		})
	}
}

func TestTaskWorkspaceSyncRecoveryNativeCloseJoin(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	for _, stage := range []string{"write", "fsync", "close", "terminal", "gc"} {
		t.Run(stage, func(t *testing.T) {
			f, wire, slot := recoveryExecutionCold(t)
			s, err := f.b.prepareTaskWorkspaceSyncRecovery(f.ctx, f.c, slot, wire.destination, workspaceSyncLocalNativeWorker)
			require.NoError(t, err)
			require.NoError(t, <-wire.done)
			entered, release := make(chan struct{}), make(chan struct{})
			var reached atomic.Bool
			pause := func() {
				if reached.CompareAndSwap(false, true) {
					close(entered)
					<-release
				}
			}
			var call func() error
			if stage == "terminal" || stage == "gc" {
				_, charge, q := recoveryExecutionFirst(t, s)
				if stage == "gc" {
					require.NoError(t, s.settleLocal(f.ctx, charge, q))
				}
				match := recyclingTerminalMatch
				if stage == "gc" {
					match = recyclingGCMatch
				}
				recoveryExecutionSetKV(s, &faultKV{KV: f.b.client.KV, match: match, before: pause})
				call = func() error {
					if stage == "terminal" {
						return s.settleLocal(f.ctx, charge, q)
					}
					_, err := s.recycleLocal(f.ctx, charge, q)
					return err
				}
			} else {
				s.localIOComplete = func(at string) {
					if at == stage {
						pause()
					}
				}
				call = func() error { _, err := s.run(s.ctx); return err }
			}
			running := make(chan error, 1)
			go func() { running <- call() }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("actual registered IO window was not reached")
			}
			closed := make(chan error, 1)
			go func() { closed <- f.b.Close() }()
			select {
			case <-closed:
				t.Fatal("Backend.Close crossed a registered physical/native owner")
			case <-time.After(25 * time.Millisecond):
			}
			options := recoveryTestOptions(s.store.recovery.directory, s.store.limits.Jobs)
			l := s.store.limits
			options.TaskWorkspaceArchiveSpoolOptions = TaskWorkspaceArchiveSpoolOptions{s.store.recovery.directory, l.Jobs, l.JobBytes, l.TotalBytes, l.Entries, l.JobIndexBytes, l.TotalIndexBytes}
			blocked, err := newTaskArchiveRecoveryStore(options)
			require.Error(t, err, "same inode root remains exclusively owned until actual writer/native/descriptor joins")
			require.Nil(t, blocked)
			close(release)
			require.Error(t, <-running)
			require.NoError(t, <-closed)
			cold, err := newTaskArchiveRecoveryStore(options)
			require.NoError(t, err, "root lock is released last after real joins")
			require.NoError(t, cold.close())
		})
	}
}
