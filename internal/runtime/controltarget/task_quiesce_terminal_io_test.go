//go:build linux || darwin

package controltarget

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Direct private IO tests do not assert a physical terminal. The full producer
// with a real opaque observation is separately selected on actual Linux PID1.
func TestTaskQuiesceTerminalIO(t *testing.T) {
	for _, op := range []string{"open-temp", "write", "file-sync", "rename", "dir-sync"} {
		for _, after := range []bool{false, true} {
			t.Run(op+map[bool]string{false: "-before", true: "-after"}[after], func(t *testing.T) {
				f := quiesceJournalSetup(t)
				ctx := context.Background()
				a, err := f.j.AcceptUserQuiescence(ctx, f.e, f.receipt)
				require.NoError(t, err)
				r := a.record
				r.State = "users_quiesced"
				r.ExecutionSetDigest = strings.Repeat("a", 64)
				data, err := encodeTaskQuiescence(r)
				require.NoError(t, err)
				hit := false
				f.j.files.hook = func(got, name string, post bool) error {
					if got == op && after == post {
						hit = true
						return errors.New("terminal IO fault")
					}
					return nil
				}
				before := f.j.logicalBytes
				f.j.mu.Lock()
				err = f.j.persistTaskQuiescenceTerminalLocked(ctx, data)
				if err != nil {
					f.j.poison(err)
				}
				f.j.mu.Unlock()
				f.j.files.hook = nil
				require.True(t, hit)
				require.Error(t, err)
				require.Equal(t, before, f.j.logicalBytes)
				require.True(t, f.j.Status().Poisoned)
				wire, err := f.j.SignUserQuiescenceReceipt(ctx, f.e.Context(), f.e.Digest(), nil, f.runtimeKey)
				require.Error(t, err)
				require.Nil(t, wire)
				if op == "dir-sync" || op == "rename" && after {
					record, err := f.j.LookupUserQuiescence(ctx, f.e.Context(), f.e.Digest())
					require.NoError(t, err)
					require.Equal(t, "users_quiesced", record.State)
				}
			})
		}
	}
}
func TestTaskQuiesceTerminalCapacity(t *testing.T) {
	f := quiesceJournalSetup(t)
	ctx := context.Background()
	a, err := f.j.AcceptUserQuiescence(ctx, f.e, f.receipt)
	require.NoError(t, err)
	r := a.record
	r.State = "users_quiesced"
	r.ExecutionSetDigest = strings.Repeat("b", 64)
	r.RegisteredCount = 64
	r.LocalTerminalCount = 32
	r.NeverSpawnedCount = 32
	wire, err := encodeTaskQuiescence(r)
	require.NoError(t, err)
	f.j.mu.Lock()
	defer f.j.mu.Unlock()
	actual := f.j.logicalBytes + int64(len(wire))
	f.j.maxBytes = actual
	err = f.j.persistTaskQuiescenceTerminalLocked(ctx, wire)
	require.ErrorIs(t, err, ErrCapacity)
	f.j.maxBytes = 256 * 1024 * 1024
	before := f.j.logicalBytes
	old := f.j.taskQuiescenceBytes
	require.NoError(t, f.j.persistTaskQuiescenceTerminalLocked(ctx, wire))
	require.Equal(t, before-old+int64(len(wire)), f.j.logicalBytes)
}
