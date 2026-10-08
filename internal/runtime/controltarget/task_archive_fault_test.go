//go:build linux || darwin

package controltarget

import (
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTaskArchivePersistenceFaults(t *testing.T) {
	for _, op := range []string{"open-temp", "write", "file-sync", "rename", "dir-sync"} {
		for _, after := range []bool{false, true} {
			t.Run(op+map[bool]string{false: "/before", true: "/after"}[after], func(t *testing.T) {
				f := archiveHostSetup(t)
				hit := false
				f.j.files.hook = func(operation, name string, isAfter bool) error {
					if operation == op && isAfter == after && (op == "dir-sync" || name == "workspace-archive.json" || strings.HasPrefix(name, ".workspace-archive.")) {
						hit = true
						return errors.New("actual admission syscall boundary")
					}
					return nil
				}
				a, err := f.j.AcceptWorkspaceArchive(context.Background(), f.archive, f.q, f.quiescedReceipt)
				require.True(t, hit)
				require.Error(t, err)
				require.Nil(t, a)
				require.True(t, f.j.Status().Poisoned)
				f.j.files.hook = nil
				a, err = f.j.AcceptWorkspaceArchive(context.Background(), f.archive, f.q, f.quiescedReceipt)
				require.Error(t, err)
				require.Nil(t, a)
				entries, err := os.ReadDir(f.o.Directory)
				require.NoError(t, err)
				retained := false
				for _, entry := range entries {
					retained = retained || entry.Name() == "workspace-archive.json" || taskArchiveTempName(entry.Name())
				}
				if op != "open-temp" || after {
					require.True(t, retained, "uncertainty cannot auto-delete temp or pending")
				}
			})
		}
	}
}
func TestTaskArchiveLateAuthentication(t *testing.T) {
	for _, fault := range []string{"clock", "ticket-window", "retained-q"} {
		t.Run(fault, func(t *testing.T) {
			f := archiveHostSetup(t)
			hit := false
			f.j.files.hook = func(op, name string, after bool) error {
				if op == "dir-sync" && after {
					hit = true
					switch fault {
					case "clock":
						f.clock.err = errors.New("lost trusted clock")
					case "ticket-window":
						f.clock.now = f.clock.now.Add(time.Minute)
					case "retained-q":
						return os.Remove(filepath.Join(f.o.Directory, "users-quiesce.json"))
					}
				}
				return nil
			}
			a, err := f.j.AcceptWorkspaceArchive(context.Background(), f.archive, f.q, f.quiescedReceipt)
			require.True(t, hit)
			require.Error(t, err)
			require.Nil(t, a)
			require.True(t, f.j.Status().Poisoned)
			f.j.files.hook = nil
			f.clock.err = nil
			r, err := f.j.LookupWorkspaceArchive(context.Background(), f.archive.Context(), f.archive.Digest())
			require.NoError(t, err)
			require.Equal(t, "pending", r.State)
		})
	}
}

func TestTaskArchiveDeadlineCapturedBeforePendingIO(t *testing.T) {
	f := archiveHostSetup(t)
	var pendingIO time.Time
	f.j.files.hook = func(op, name string, after bool) error {
		if op == "open-temp" && !after && strings.HasPrefix(name, ".workspace-archive.") {
			pendingIO = time.Now()
			time.Sleep(30 * time.Millisecond)
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, err := f.j.AcceptWorkspaceArchive(ctx, f.archive, f.q, f.quiescedReceipt)
	require.NoError(t, err)
	require.False(t, pendingIO.IsZero())
	require.True(t, a.deadline.Before(pendingIO.Add(f.archive.SessionNotAfter().Sub(f.clock.now)-time.Second)))
	require.Greater(t, time.Until(a.deadline), 4*time.Minute)
}
