//go:build linux || darwin

package controltarget

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
)

func TestTaskQuiesceTerminalNoPhysicalObservation(t *testing.T) {
	f := quiesceJournalSetup(t)
	a, err := f.j.AcceptUserQuiescence(context.Background(), f.e, f.receipt)
	require.NoError(t, err)
	path := filepath.Join(f.o.Directory, "users-quiesce.json")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	wire, err := f.j.CompleteUserQuiescence(context.Background(), a, nil, nil, f.runtimeKey)
	require.Error(t, err)
	require.Empty(t, wire)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after)
	wire, err = f.j.SignUserQuiescenceReceipt(context.Background(), f.e.Context(), f.e.Digest(), nil, f.runtimeKey)
	require.Error(t, err)
	require.Empty(t, wire)
}
func TestTaskQuiesceTerminalOriginalExecutions(t *testing.T) {
	f := liveSetup(t)
	j := f.create(t)
	ctx := context.Background()
	require.NoError(t, j.InstallActivation(ctx, f.activation))
	a, err := j.AcceptExecution(ctx, f.e)
	require.NoError(t, err)
	require.NoError(t, j.ConsumeStart(ctx, a))
	j.mu.Lock()
	entries, never, terminal, err := j.quiescenceExecutionsLocked(ctx, []QuiescedExecution{{Accepted: a, Disposition: "never_spawned"}})
	j.mu.Unlock()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.EqualValues(t, 1, never)
	require.Zero(t, terminal)
	r, err := j.RecordLocalTerminal(ctx, a, LocalExecutionResult{RootPID: 42, RootWaitStatus: 9, DrainConfirmed: true, Reason: "cancelled"})
	require.NoError(t, err)
	wire, err := p.SignLocalExecReceipt(f.runtimeKey, p.LocalExecReceiptClaims{Version: 1, State: r.State, Context: r.Context, DescriptorDigest: r.DescriptorDigest, TicketDigest: r.TicketDigest, NotBefore: r.NotBefore, NotAfter: r.NotAfter, AuthorityDeadline: r.AuthorityDeadline, RootPID: r.RootPID, RootWaitStatus: r.RootWaitStatus, DrainConfirmed: r.DrainConfirmed, Reason: r.Reason})
	require.NoError(t, err)
	valid := QuiescedExecution{Accepted: a, Disposition: "local_terminal", TerminalReceipt: wire}
	copyHandle := *a
	for name, input := range map[string][]QuiescedExecution{
		"valid": {valid}, "copy": {{Accepted: &copyHandle, Disposition: "local_terminal", TerminalReceipt: wire}}, "duplicate": {valid, valid}, "missing": {{Accepted: a, Disposition: "local_terminal"}}, "never-terminal": {{Accepted: a, Disposition: "never_spawned"}}, "wrong-signature": {{Accepted: a, Disposition: "local_terminal", TerminalReceipt: append([]byte("!"), wire...)}},
	} {
		t.Run(name, func(t *testing.T) {
			j.mu.Lock()
			defer j.mu.Unlock()
			entries, n, l, err := j.quiescenceExecutionsLocked(ctx, input)
			if name == "valid" {
				require.NoError(t, err)
				require.Zero(t, n)
				require.EqualValues(t, 1, l)
				require.Equal(t, digestJournalBytes(wire), entries[0].TerminalReceiptDigest)
			} else {
				require.Error(t, err)
				require.Nil(t, entries)
			}
		})
	}
}
