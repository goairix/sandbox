//go:build linux && (amd64 || arm64)

package controltarget

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime/launcher"
	"github.com/stretchr/testify/require"
)

func TestTaskQuiesceTerminalNative(t *testing.T) {
	if os.Getenv("SANDBOX_TASK3_NATIVE") != "1" {
		t.Skip("requires Root-owned actual protected PID1")
	}
	require.Equal(t, 1, os.Getpid())
	require.Equal(t, "/journal", os.Getenv("TMPDIR"))
	info, err := os.Stat("/journal")
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0700), info.Mode().Perm())
	kernel, err := launcher.BootstrapPID1()
	require.NoError(t, err)
	observation, err := kernel.ObserveNoUserDescendants(context.Background())
	require.NoError(t, err)
	t.Logf("actual PID=%d kernel=%+v original empty census/ECHILD", os.Getpid(), kernel.Snapshot())
	for _, fault := range []string{"none", "open-temp", "write", "file-sync", "rename", "dir-sync", "post-durable-clock"} {
		t.Run(fault, func(t *testing.T) {
			f := quiesceJournalSetup(t)
			ctx := context.Background()
			a, err := f.j.AcceptUserQuiescence(ctx, f.e, f.receipt)
			require.NoError(t, err)
			hit := false
			if fault != "none" {
				f.j.files.hook = func(op, name string, after bool) error {
					if fault == "post-durable-clock" && op == "dir-sync" && after {
						hit = true
						f.clock.err = errors.New("lost clock after durable replacement")
						return nil
					}
					if op == fault && !after {
						hit = true
						return errors.New("actual protected terminal IO fault")
					}
					return nil
				}
			}
			wire, err := f.j.CompleteUserQuiescence(ctx, a, observation, nil, f.runtimeKey)
			f.j.files.hook = nil
			f.clock.err = nil
			if fault != "none" {
				require.True(t, hit)
				require.Error(t, err)
				require.Empty(t, wire)
				require.True(t, f.j.Status().Poisoned)
				query, err := f.j.SignUserQuiescenceReceipt(ctx, f.e.Context(), f.e.Digest(), observation, f.runtimeKey)
				require.Error(t, err)
				require.Nil(t, query)
				if fault == "dir-sync" || fault == "post-durable-clock" {
					r, err := f.j.LookupUserQuiescence(ctx, f.e.Context(), f.e.Digest())
					require.NoError(t, err)
					require.Equal(t, "users_quiesced", r.State)
					t.Log("readable terminal + freshly empty namespace remains unsignable")
				}
				return
			}
			require.NoError(t, err)
			require.NotEmpty(t, wire)
			f.clock.now = f.clock.now.Add(time.Minute)
			query, err := f.j.SignUserQuiescenceReceipt(ctx, f.e.Context(), f.e.Digest(), observation, f.runtimeKey)
			require.NoError(t, err)
			require.NotEmpty(t, query)
			require.NoError(t, observation.RevalidateCurrent(ctx))
			copyObservation := *observation
			query, err = f.j.SignUserQuiescenceReceipt(ctx, f.e.Context(), f.e.Digest(), &copyObservation, f.runtimeKey)
			require.Error(t, err)
			require.Nil(t, query)
			require.NoError(t, f.j.Close())
			cold, err := OpenClosedJournal(ctx, f.o)
			require.NoError(t, err)
			defer cold.Close()
			query, err = cold.SignUserQuiescenceReceipt(ctx, f.e.Context(), f.e.Digest(), observation, f.runtimeKey)
			require.Error(t, err)
			require.Nil(t, query)
		})
	}
}
