//go:build linux && (amd64 || arm64)

package controltarget

import (
	"context"
	"errors"
	"github.com/goairix/sandbox/internal/runtime/launcher"
	"github.com/stretchr/testify/require"
	"os"
	"testing"
)

// No execution is accepted in this fixture: the physical original census and
// CompleteUserQuiescence empty registration set therefore omit no accepted exec.
func archiveNativeEmptySetup(t *testing.T, o *launcher.UserNamespaceQuiescenceObservation) archiveFixture {
	t.Helper()
	f, q := archiveEmptyQuiesceSetup(t, liveSetup(t))
	j := f.j
	ctx := context.Background()
	completed, err := j.CompleteUserQuiescence(ctx, q, o, nil, f.runtimeKey)
	require.NoError(t, err)
	require.NotEmpty(t, completed)
	require.Zero(t, q.terminal.RegisteredCount)
	require.Zero(t, j.Status().Records)
	return archiveEvidenceSetup(t, f, q, completed)
}

func TestTaskArchiveAdmissionNative(t *testing.T) {
	if os.Getenv("SANDBOX_TASK_ARCHIVE_NATIVE") != "1" {
		t.Skip("requires restricted original Linux PID1 fixture")
	}
	require.Equal(t, 1, os.Getpid())
	require.Equal(t, "/journal", os.Getenv("TMPDIR"))
	info, err := os.Stat("/journal")
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0700), info.Mode().Perm())
	kernel, err := launcher.BootstrapPID1()
	require.NoError(t, err)
	o, err := kernel.ObserveNoUserDescendants(context.Background())
	require.NoError(t, err)
	t.Logf("actual PID1 bootstrap + original census/ECHILD: %+v", kernel.Snapshot())
	for _, fault := range []string{"none", "file-sync", "dir-sync", "post-durable-clock"} {
		t.Run(fault, func(t *testing.T) {
			f := archiveNativeEmptySetup(t, o)
			hit := false
			if fault != "none" {
				f.j.files.hook = func(op, name string, after bool) error {
					if fault == "post-durable-clock" && op == "dir-sync" && after {
						hit = true
						f.clock.err = errors.New("lost trusted clock after actual archive pending persistence")
						return nil
					}
					if fault == op && !after {
						hit = true
						return errors.New("actual archive admission persistence fault")
					}
					return nil
				}
			}
			a, err := f.j.AcceptWorkspaceArchive(context.Background(), f.archive, f.q, f.quiescedReceipt)
			f.j.files.hook = nil
			f.clock.err = nil
			if fault != "none" {
				require.True(t, hit)
				require.Error(t, err)
				require.Nil(t, a)
				require.True(t, f.j.Status().Poisoned)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, a)
			r, err := f.j.LookupWorkspaceArchive(context.Background(), f.archive.Context(), f.archive.Digest())
			require.NoError(t, err)
			require.Equal(t, "pending", r.State)
			require.NoError(t, o.RevalidateCurrent(context.Background()))
			require.NoError(t, f.j.Close())
			cold, err := OpenClosedJournal(context.Background(), f.o)
			require.NoError(t, err)
			defer cold.Close()
			_, err = cold.AcceptWorkspaceArchive(context.Background(), f.archive, f.q, f.quiescedReceipt)
			require.Error(t, err)
			t.Log("actual completed original USERS quiescence admitted durable pending; cold authority refused")
		})
	}
}
