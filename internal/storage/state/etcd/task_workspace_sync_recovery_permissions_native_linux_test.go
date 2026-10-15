//go:build linux

package etcd

import (
	"archive/tar"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTaskWorkspaceSyncRecoveryNativePermissions(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit actual Linux worker required")
	}
	for _, mode := range []int64{0000, 0444} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			text := "old acknowledged local write has no terminal evidence"
			data := archiveTarBytes(t, []*tar.Header{{Name: "workspace", Typeflag: tar.TypeDir, Mode: 0700}, {Name: "workspace/z", Typeflag: tar.TypeDir, Mode: 0700}, {Name: "workspace/z/readonly", Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(text))}}, []string{"", "", text})
			var original TaskWorkspaceSyncIntentReference
			var birth int64
			f, wire, slot := recoveryExecutionColdData(t, data, 3, func(s *PreparedTaskWorkspaceSyncSource, body *taskWorkspaceSyncBody, target *taskWorkspaceSyncLocalTarget) {
				original, birth = body.intent.record.Reference, body.intent.revision
				w, err := s.putLocal(s.ctx, body, target)
				require.NoError(t, err)
				require.True(t, w.original())
				info, err := os.Stat(filepath.Join(target.pin.Root, body.entry.path))
				require.NoError(t, err)
				require.EqualValues(t, mode, info.Mode().Perm(), "actual original put reached chmod/fsync/Close; terminal remains unknown")
			})
			s, err := f.b.prepareTaskWorkspaceSyncRecovery(f.ctx, f.c, slot, wire.destination, workspaceSyncLocalNativeWorker)
			require.NoError(t, err)
			require.NoError(t, <-wire.done)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			var entry *workspaceSyncArchiveEntry
			for {
				entry, err = s.next(s.ctx)
				require.NoError(t, err)
				if entry.kind == tar.TypeReg {
					break
				}
			}
			body, err := s.openBody(s.ctx, entry)
			require.NoError(t, err)
			charge, err := s.reserveNext(s.ctx, body)
			require.NoError(t, err)
			require.Equal(t, original, charge.record.Reference, "fresh repair retains exact old IntentID/slot")
			require.Equal(t, birth, charge.birth)
			writes, permissionChanges := 0, 0
			s.localIOComplete = func(stage string) {
				if stage == "write" {
					writes++
				}
				if stage == "permission-chmod" {
					permissionChanges++
				}
			}
			q, err := s.reconcileLocal(s.ctx, body, charge)
			require.NoError(t, err, "fresh guarded repair must reopen a regular inode left in a legal non-writable mode")
			require.Positive(t, writes)
			require.Equal(t, 1, permissionChanges)
			require.NoError(t, s.settleLocal(s.ctx, charge, q))
			outcome, err := s.recycleLocal(s.ctx, charge, q)
			require.NoError(t, err)
			require.Equal(t, OutcomeCommitted, outcome)
			require.Equal(t, original.IntentID, s.checkpoint.IntentID)
			require.Equal(t, birth, s.checkpoint.IntentBirth)
			info, err := os.Stat(filepath.Join(s.target.pin.Root, entry.path))
			require.NoError(t, err)
			require.EqualValues(t, mode, info.Mode().Perm(), "common executor restores exact planned mode")
			require.NoError(t, body.Close())
			complete, err := s.run(s.ctx)
			require.NoError(t, err)
			require.True(t, complete)
			require.EqualValues(t, 1, s.completedFiles)
		})
	}
}
