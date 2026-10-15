//go:build linux

package etcd

import (
	"archive/tar"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestTaskWorkspaceSyncLocalFinalizationNativeUnknown(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	for _, stage := range []string{"reserve", "progress", "terminal", "gc", "complete"} {
		t.Run(stage, func(t *testing.T) {
			data := archiveTarBytes(t, []*tar.Header{{Name: "workspace", Typeflag: tar.TypeDir, Mode: 0700}, {Name: "workspace/empty", Typeflag: tar.TypeDir, Mode: 0555}}, []string{"", ""})
			f, wire, _ := finalizationNativeCold(t, data, 2, nil, false, nil)
			s, err := f.b.prepareTaskWorkspaceSyncRecoveryMode(f.ctx, f.c, 0, wire.destination, workspaceSyncLocalNativeWorker, true)
			require.NoError(t, err)
			progress, complete, err := f.b.namespace.workspaceSyncLocalKeys(f.c.reference.Task)
			require.NoError(t, err)
			original := f.b.client.KV
			var writes atomic.Int32
			match := func(ops []clientv3.Op) bool {
				if writes.Load() > 0 {
					return false
				}
				switch stage {
				case "reserve":
					return len(ops) == 2 && ops[0].IsPut() && strings.Contains(string(ops[0].ValueBytes()), `"kind":"local_metadata_pass"`)
				case "progress":
					return len(ops) == 1 && ops[0].IsPut() && string(ops[0].KeyBytes()) == progress && strings.Contains(string(ops[0].ValueBytes()), `"phase":"metadata"`)
				case "terminal":
					return recyclingTerminalMatch(ops)
				case "gc":
					return recyclingGCMatch(ops)
				case "complete":
					return putsKey(complete)(ops)
				}
				return false
			}
			recoveryExecutionSetKV(s, &faultKV{KV: original, match: match, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				require.NoError(t, err)
				require.True(t, r.Succeeded)
				writes.Add(1)
				return nil, context.DeadlineExceeded
			}})
			r, err := s.finalizeLocal(f.ctx)
			require.ErrorIs(t, err, ErrOutcomeUnknown)
			require.False(t, r.LocalComplete)
			require.EqualValues(t, 1, writes.Load())
			_, err = s.finalizeLocal(f.ctx)
			require.Error(t, err)
			require.EqualValues(t, 1, writes.Load(), "same attempt never retries writes")
			recoveryExecutionSetKV(s, original)
			require.NoError(t, s.Close())
			require.NoError(t, <-wire.done)
			recoveryExecutionReopen(t, f)
			r, err = f.b.FinalizeTaskWorkspaceSyncLocal(f.ctx, f.c, 0, wire.destination, workspaceSyncLocalNativeWorker)
			require.NoError(t, err)
			require.True(t, r.LocalComplete)
		})
	}
}

type finalizationDeferredKV struct {
	clientv3.KV
	match   func([]clientv3.Op) bool
	pending func() (*clientv3.TxnResponse, error)
}
type finalizationDeferredTxn struct {
	clientv3.Txn
	owner        *finalizationDeferredKV
	cmps         []clientv3.Cmp
	ops, failure []clientv3.Op
}

func (k *finalizationDeferredKV) Txn(ctx context.Context) clientv3.Txn {
	return &finalizationDeferredTxn{Txn: k.KV.Txn(ctx), owner: k}
}
func (t *finalizationDeferredTxn) If(c ...clientv3.Cmp) clientv3.Txn {
	t.cmps = c
	t.Txn = t.Txn.If(c...)
	return t
}
func (t *finalizationDeferredTxn) Then(o ...clientv3.Op) clientv3.Txn {
	t.ops = o
	t.Txn = t.Txn.Then(o...)
	return t
}
func (t *finalizationDeferredTxn) Else(o ...clientv3.Op) clientv3.Txn {
	t.failure = o
	t.Txn = t.Txn.Else(o...)
	return t
}
func (t *finalizationDeferredTxn) Commit() (*clientv3.TxnResponse, error) {
	if t.owner.pending == nil && t.owner.match(t.ops) {
		t.owner.pending = func() (*clientv3.TxnResponse, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return t.owner.KV.Txn(ctx).If(t.cmps...).Then(t.ops...).Else(t.failure...).Commit()
		}
		return nil, context.DeadlineExceeded
	}
	return t.Txn.Commit()
}

func TestTaskWorkspaceSyncLocalFinalizationNativeDeferredLedger(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	data := archiveTarBytes(t, []*tar.Header{{Name: "workspace", Typeflag: tar.TypeDir, Mode: 0700}}, []string{""})
	f, wire, root := finalizationNativeCold(t, data, 1, []string{"protected/keep"}, true, func(root string) {
		require.NoError(t, os.MkdirAll(filepath.Join(root, "protected/keep"), 0700))
		require.NoError(t, os.Chmod(filepath.Join(root, "protected"), 0000))
	})
	s, err := f.b.prepareTaskWorkspaceSyncRecoveryMode(f.ctx, f.c, 0, wire.destination, workspaceSyncLocalNativeWorker, true)
	require.NoError(t, err)
	progress, _, err := f.b.namespace.workspaceSyncLocalKeys(f.c.reference.Task)
	require.NoError(t, err)
	armed := false
	s.localIOComplete = func(stage string) {
		if stage == "directory-chmod" && len(s.finalization.progress.Ledger.Frames) > 0 {
			armed = true
		}
	}
	original := f.b.client.KV
	deferred := &finalizationDeferredKV{KV: original, match: func(ops []clientv3.Op) bool {
		if !armed || len(ops) != 1 || !ops[0].IsPut() || string(ops[0].KeyBytes()) != progress {
			return false
		}
		var p taskWorkspaceSyncLocalProgress
		return json.Unmarshal(ops[0].ValueBytes(), &p) == nil && p.Phase == "metadata" && len(p.Ledger.Frames) == 0
	}}
	recoveryExecutionSetKV(s, deferred)
	_, err = s.finalizeLocal(f.ctx)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.NotNil(t, deferred.pending)
	recoveryExecutionSetKV(s, original)
	require.NoError(t, s.Close())
	require.NoError(t, <-wire.done)
	next, err := f.b.prepareTaskWorkspaceSyncRecoveryMode(f.ctx, f.c, 0, wire.destination, workspaceSyncLocalNativeWorker, true)
	require.NoError(t, err)
	lateRan := false
	next.localIOComplete = func(stage string) {
		if stage == "directory-chmod" && !lateRan {
			lateRan = true
			r, err := deferred.pending()
			require.NoError(t, err)
			require.False(t, r.Succeeded, "new same-claim progress generation fences late ledger pop")
		}
	}
	r, err := next.finalizeLocal(f.ctx)
	require.NoError(t, err)
	require.True(t, r.LocalComplete)
	require.True(t, lateRan)
	require.NoError(t, next.Close())
	info, err := os.Stat(filepath.Join(root, "protected"))
	require.NoError(t, err)
	require.Zero(t, info.Mode().Perm())
}

func TestTaskWorkspaceSyncLocalFinalizationNativeDeferredRegularGC(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	f, wire, slot := recoveryExecutionCold(t)
	s, err := f.b.prepareTaskWorkspaceSyncRecoveryMode(f.ctx, f.c, slot, wire.destination, workspaceSyncLocalNativeWorker, true)
	require.NoError(t, err)
	progress, _, err := f.b.namespace.workspaceSyncLocalKeys(f.c.reference.Task)
	require.NoError(t, err)
	original := f.b.client.KV
	// Claim A durably creates regular progress, then loses that ACK before
	// regular effects. The original occupied S1 put is still present.
	recoveryExecutionSetKV(s, &faultKV{KV: original, match: putsKey(progress), after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		require.NoError(t, err)
		require.True(t, r.Succeeded)
		return nil, context.DeadlineExceeded
	}})
	_, err = s.finalizeLocal(f.ctx)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	recoveryExecutionSetKV(s, original)
	require.NoError(t, s.Close())
	require.NoError(t, <-wire.done)
	recoveryExecutionReopen(t, f) // genuine claim B
	bClaim := f.c.reference.ClaimID
	b, err := f.b.prepareTaskWorkspaceSyncRecoveryMode(f.ctx, f.c, slot, wire.destination, workspaceSyncLocalNativeWorker, true)
	require.NoError(t, err)
	original = f.b.client.KV
	deferred := &finalizationDeferredKV{KV: original, match: recyclingGCMatch}
	recoveryExecutionSetKV(b, deferred)
	_, err = b.finalizeLocal(f.ctx)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.NotNil(t, deferred.pending, "actual regular GC remains able to commit after ACK loss")
	recoveryExecutionSetKV(b, original)
	require.NoError(t, b.Close())
	require.NoError(t, <-wire.done)
	next, err := f.b.prepareTaskWorkspaceSyncRecoveryMode(f.ctx, f.c, slot, wire.destination, workspaceSyncLocalNativeWorker, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, next.Close()) })
	effects := 0
	next.localIOComplete = func(string) { effects++ }
	_, err = next.finalizeLocal(f.ctx)
	require.ErrorIs(t, err, ErrConflict, "claim B must already own durable regular progress before its uncertain GC")
	require.Zero(t, effects, "same-claim refusal precedes any repeated repair")
	require.NoError(t, next.Close())
	require.NoError(t, <-wire.done)
	point, err := f.raw.Get(f.ctx, progress)
	require.NoError(t, err)
	require.Len(t, point.Kvs, 1)
	p, err := decodeWorkspaceSyncLocalProgress(point.Kvs[0])
	require.NoError(t, err)
	require.Equal(t, bClaim, p.Claim.ClaimID)
	recoveryExecutionReopen(t, f) // genuine claim C fences B's full64-node GC
	// Reopening joins/closes B's client too. Submit the captured exact native
	// request through the still-live fixture connection, keeping every B pin.
	deferred.KV = f.raw.KV
	late, err := deferred.pending()
	require.NoError(t, err)
	require.False(t, late.Succeeded, "only genuine claim C fences B GC without weakening its existing pins")
	r, err := f.b.FinalizeTaskWorkspaceSyncLocal(f.ctx, f.c, slot, wire.destination, workspaceSyncLocalNativeWorker)
	require.NoError(t, err)
	require.True(t, r.LocalComplete)
}

func TestTaskWorkspaceSyncLocalFinalizationNativeFaults(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	for _, stage := range []string{"mkdir", "unlink", "rmdir", "directory-chmod", "metadata-fsync", "metadata-close", "target-close"} {
		t.Run(stage, func(t *testing.T) {
			data := archiveTarBytes(t, []*tar.Header{{Name: "workspace", Typeflag: tar.TypeDir, Mode: 0700}, {Name: "workspace/empty", Typeflag: tar.TypeDir, Mode: 0000}}, []string{"", ""})
			f, wire, _ := finalizationNativeCold(t, data, 2, nil, true, func(root string) {
				require.NoError(t, os.MkdirAll(filepath.Join(root, "extra/sub"), 0700))
				require.NoError(t, os.WriteFile(filepath.Join(root, "extra/sub/file"), []byte("extra"), 0600))
			})
			s, err := f.b.prepareTaskWorkspaceSyncRecoveryMode(f.ctx, f.c, 0, wire.destination, workspaceSyncLocalNativeWorker, true)
			require.NoError(t, err)
			reached := false
			s.localIOComplete = func(actual string) {
				if actual == stage && !reached {
					reached = true
					s.cancel()
				}
			}
			r, err := s.finalizeLocal(f.ctx)
			require.Error(t, err)
			require.False(t, r.LocalComplete)
			require.True(t, reached)
			require.NoError(t, s.Close())
			require.NoError(t, <-wire.done)
			_, complete, err := f.b.namespace.workspaceSyncLocalKeys(f.c.reference.Task)
			require.NoError(t, err)
			absent, err := f.raw.Get(f.ctx, complete)
			require.NoError(t, err)
			require.Empty(t, absent.Kvs)
			recoveryExecutionReopen(t, f)
			r, err = f.b.FinalizeTaskWorkspaceSyncLocal(f.ctx, f.c, 0, wire.destination, workspaceSyncLocalNativeWorker)
			require.NoError(t, err)
			require.True(t, r.LocalComplete)
			if stage == "target-close" {
				require.Zero(t, r.CreatedDirectories+r.RemovedEntries+r.UpdatedDirectoryModes, "effects-done recovery is readonly")
			}
		})
	}
}

func TestTaskWorkspaceSyncLocalFinalizationNativeJoin(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	for _, stage := range []string{"directory-chmod", "metadata-close"} {
		t.Run(stage, func(t *testing.T) {
			data := archiveTarBytes(t, []*tar.Header{{Name: "workspace", Typeflag: tar.TypeDir, Mode: 0700}, {Name: "workspace/empty", Typeflag: tar.TypeDir, Mode: 0555}}, []string{"", ""})
			f, wire, _ := finalizationNativeCold(t, data, 2, nil, false, nil)
			s, err := f.b.prepareTaskWorkspaceSyncRecoveryMode(f.ctx, f.c, 0, wire.destination, workspaceSyncLocalNativeWorker, true)
			require.NoError(t, err)
			entered, release := make(chan struct{}), make(chan struct{})
			var reached atomic.Bool
			s.localIOComplete = func(actual string) {
				if actual == stage && reached.CompareAndSwap(false, true) {
					close(entered)
					<-release
				}
			}
			ran := make(chan error, 1)
			go func() { _, err := s.finalizeLocal(f.ctx); ran <- err }()
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("actual metadata boundary was not reached")
			}
			_, err = f.b.archiveStore.registerRecovery(f.ctx, 0)
			require.ErrorIs(t, err, ErrConflict)
			seed := s.manifest.Seed
			spool, err := f.b.archiveStore.reserveRecovery(f.ctx, seed)
			require.ErrorIs(t, err, ErrConflict)
			require.Nil(t, spool)
			closed := make(chan error, 1)
			go func() { closed <- s.Close() }()
			select {
			case err := <-closed:
				t.Fatalf("Close crossed actual %s operation: %v", stage, err)
			case <-time.After(25 * time.Millisecond):
			}
			close(release)
			require.Error(t, <-ran)
			require.NoError(t, <-closed)
			require.NoError(t, <-wire.done)
		})
	}
}
