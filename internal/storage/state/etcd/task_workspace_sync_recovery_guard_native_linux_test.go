//go:build linux

package etcd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestTaskWorkspaceSyncRecoveryNativeQueryRejects(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	f, wire, slot := recoveryExecutionCold(t)
	s, err := f.b.prepareTaskWorkspaceSyncRecovery(f.ctx, f.c, slot, wire.destination, workspaceSyncLocalNativeWorker)
	require.NoError(t, err)
	require.NoError(t, <-wire.done)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	for _, fault := range []string{"data", "double", "abrupt", "close"} {
		t.Run(fault, func(t *testing.T) {
			wire.mu.Lock()
			wire.queryFault, wire.queryCloseError = fault, fault == "close"
			wire.mu.Unlock()
			require.NoError(t, s.begin(f.ctx))
			receipt, err := s.authority.queryProducer(s.ctx, wire.destination)
			s.end()
			require.Error(t, err)
			require.Empty(t, receipt.Wire(), "failed EOF or physical Close never grants historical query evidence")
			<-wire.done
		})
	}
}

func TestTaskWorkspaceSyncRecoveryNativeHistoricalQuery(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	taskArchiveRecoveryFixtureDuration = 31 * time.Second
	t.Cleanup(func() { taskArchiveRecoveryFixtureDuration = 0 })
	f, wire, slot := recoveryExecutionCold(t)
	key, err := f.b.namespace.taskWorkspaceArchiveKey(f.c.reference.Task)
	require.NoError(t, err)
	got, err := f.raw.Get(f.ctx, key)
	require.NoError(t, err)
	require.Len(t, got.Kvs, 1)
	var command taskArchiveCommandRecord
	require.NoError(t, decodeTaskArchiveCommand(got.Kvs[0], &command))
	timer := time.NewTimer(time.Until(command.Claims.SessionNotAfter) + 20*time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	require.True(t, time.Now().After(command.Claims.NotAfter))
	require.True(t, time.Now().After(command.Claims.SessionNotAfter))
	require.NoError(t, f.b.RenewTaskClaim(f.ctx, f.c))
	r, err := f.b.RecoverTaskWorkspaceSyncLocal(f.ctx, f.c, slot, wire.destination, workspaceSyncLocalNativeWorker)
	require.NoError(t, err)
	require.True(t, r.RegularFilesComplete)
	require.EqualValues(t, 1, wire.sends.Load(), "expired original producer authority is never resent")
}

func TestTaskWorkspaceSyncRecoveryNativeFences(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	for _, fault := range []string{"claim", "archive", "history", "target"} {
		t.Run(fault, func(t *testing.T) {
			f, wire, slot := recoveryExecutionCold(t)
			s, err := f.b.prepareTaskWorkspaceSyncRecovery(f.ctx, f.c, slot, wire.destination, workspaceSyncLocalNativeWorker)
			require.NoError(t, err)
			require.NoError(t, <-wire.done)
			switch fault {
			case "claim":
				_, err = f.raw.Revoke(f.ctx, clientv3.LeaseID(f.c.reference.LeaseID))
				require.NoError(t, err)
			case "archive":
				file, err := os.OpenFile(filepath.Join(s.store.recovery.directory, "slot-00.tar"), os.O_WRONLY, 0)
				require.NoError(t, err)
				_, err = file.WriteAt([]byte{0}, 0)
				require.NoError(t, err)
				require.NoError(t, file.Close())
			case "history":
				key, err := f.b.namespace.taskWorkspaceArchiveKey(f.c.reference.Task)
				require.NoError(t, err)
				value, err := f.raw.Get(f.ctx, key)
				require.NoError(t, err)
				_, err = f.raw.Delete(f.ctx, key)
				require.NoError(t, err)
				_, err = f.raw.Put(f.ctx, key, string(value.Kvs[0].Value))
				require.NoError(t, err)
			case "target":
				root := s.target.pin.Root
				require.NoError(t, os.Rename(root, root+"-old"))
				require.NoError(t, os.Mkdir(root, 0700))
				t.Cleanup(func() { _ = os.Remove(root); _ = os.Rename(root+"-old", root) })
			}
			complete, err := s.run(s.ctx)
			require.Error(t, err)
			require.False(t, complete)
			require.Zero(t, s.completedFiles)
			require.NoError(t, s.Close())
		})
	}
}

func TestTaskWorkspaceSyncRecoveryNativeMetadataTamper(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	for _, fault := range []string{"boot", "namespace"} {
		t.Run(fault, func(t *testing.T) {
			f, wire, slot := recoveryExecutionCold(t)
			path := filepath.Join(f.b.archiveStore.recovery.directory, "slot-00.json")
			wireBytes, err := os.ReadFile(path)
			require.NoError(t, err)
			var out [taskArchiveRecoveryMetadataBytes]byte
			manifest, err := decodeTaskArchiveRecovery(wireBytes, out[:])
			require.NoError(t, err)
			if fault == "boot" {
				manifest.Binding.Local.Worker.BootID = uuid.NewString()
			} else {
				manifest.Binding.Local.Worker.MountNamespaceIno++
			}
			local, err := encodeOperationMetadata(manifest.Binding.Local, 16384)
			require.NoError(t, err)
			manifest.Binding.TargetDigest = workspaceSyncIntentDigest(local)
			encoded, err := encodeTaskArchiveRecovery(manifest, out[:])
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, encoded, 0600))
			r, err := f.b.RecoverTaskWorkspaceSyncLocal(f.ctx, f.c, slot, wire.destination, workspaceSyncLocalNativeWorker)
			require.Error(t, err)
			require.False(t, r.RegularFilesComplete)
			require.Zero(t, r.CompletedFiles)
			require.Nil(t, f.b.archiveStore.recovery.slots[slot].owner, "failed metadata owner physically joined")
		})
	}
}
