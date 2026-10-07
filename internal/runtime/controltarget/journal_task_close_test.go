//go:build linux || darwin

package controltarget

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
)

func taskCloseClaims(f liveFixture) controlprotocol.TaskCloseDataTicketClaims {
	c := f.claims.Context
	return controlprotocol.TaskCloseDataTicketClaims{Version: 1, Purpose: "task_close_data", Context: controlprotocol.TaskCloseDataContext{Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch, IssuerCertificateID: c.IssuerCertificateID, IssuerCertificateDigest: c.IssuerCertificateDigest, CommandID: "81111111-1111-4111-8111-111111111111", TaskID: "91111111-1111-4111-8111-111111111111", TaskDigest: strings.Repeat("b", 64), ClaimID: "a1111111-1111-4111-8111-111111111111", WorkerID: "worker-1", SandboxID: c.SandboxID, WorkspaceHash: c.WorkspaceHash, Generation: c.Generation, DataGateEpoch: c.DataGateEpoch, ControlRevision: 10, ClaimCreateRevision: 11, LeaseID: 12, Runtime: c.Runtime}, NotBefore: f.clock.now.Add(-2 * time.Second), NotAfter: f.clock.now.Add(20 * time.Second)}
}
func taskCloseEvidence(t *testing.T, f liveFixture, c controlprotocol.TaskCloseDataTicketClaims) controlprotocol.TaskCloseDataEvidence {
	t.Helper()
	i, err := f.o.Verifier.VerifyCommandIssuerCertificate(f.issuerWire, f.clock.now)
	require.NoError(t, err)
	w, err := controlprotocol.SignTaskCloseDataTicket(f.issuerKey, i, c)
	require.NoError(t, err)
	e, err := f.o.Verifier.VerifyTaskCloseDataTicket(w, f.issuerWire, c.Context, f.clock.now)
	require.NoError(t, err)
	return e
}

// A complete retained pending record must be diagnosed and counted on cold
// recovery, without becoming close proof or recovering activation authority.
func TestJournalTaskCloseColdPending(t *testing.T) {
	f := liveSetup(t)
	j := f.create(t)
	ctx := context.Background()
	require.NoError(t, j.InstallActivation(ctx, f.activation))
	_, err := j.CloseGate(ctx, f.o.DataGateEpoch)
	require.NoError(t, err)
	before := j.Status()
	require.NoError(t, j.Close())
	c := taskCloseClaims(f)
	e := taskCloseEvidence(t, f, c)
	wire, err := json.Marshal(map[string]any{"version": 1, "state": "pending", "context": c.Context, "ticket_digest": e.Digest(), "not_before": c.NotBefore, "not_after": c.NotAfter})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(f.o.Directory, "data-close.json"), wire, 0600))
	cold, err := OpenClosedJournal(ctx, f.o)
	require.NoError(t, err)
	require.NoError(t, cold.Close())
	status := cold.Status()
	require.Equal(t, "closed", status.Gate.GateState)
	require.Equal(t, before.LogicalBytes+int64(len(wire)), status.LogicalBytes)
	require.Equal(t, before.Records, status.Records)
}

func TestJournalTaskCloseLifecycle(t *testing.T) {
	f := liveSetup(t)
	j := f.create(t)
	ctx := context.Background()
	require.NoError(t, j.InstallActivation(ctx, f.activation))
	a, err := j.AcceptExecution(ctx, f.e)
	require.NoError(t, err)
	execBefore, err := j.Lookup(ctx, f.e.Context().CommandID)
	require.NoError(t, err)
	e := taskCloseEvidence(t, f, taskCloseClaims(f))
	var writes []string
	j.files.hook = func(op, name string, after bool) error {
		if op == "rename" && after {
			writes = append(writes, name)
		}
		return nil
	}
	r, err := j.CloseData(ctx, e)
	require.NoError(t, err)
	require.NotNil(t, r)
	require.Equal(t, []string{"data-close.json", "gate.json", "data-close.json"}, writes)
	require.Equal(t, "data_closed", r.State)
	require.Equal(t, e.Context(), r.Context)
	require.Equal(t, e.Digest(), r.TicketDigest)
	require.Equal(t, "closed", j.Status().Gate.GateState)
	execAfter, err := j.Lookup(ctx, f.e.Context().CommandID)
	require.NoError(t, err)
	require.Equal(t, execBefore, execAfter)
	_, err = a.Snapshot()
	require.NoError(t, err)
	require.Error(t, j.InstallActivation(ctx, f.activation))
	before := j.Status()
	r.Context.WorkerID = "caller-mutation"
	writes = nil
	retry, err := j.CloseData(ctx, e)
	require.NoError(t, err)
	require.Equal(t, e.Context(), retry.Context)
	require.Empty(t, writes)
	require.Equal(t, before, j.Status())
	c := taskCloseClaims(f)
	c.Context.ClaimID = "b1111111-1111-4111-8111-111111111111"
	got, err := j.CloseData(ctx, taskCloseEvidence(t, f, c))
	require.ErrorIs(t, err, ErrConflict)
	require.Nil(t, got)
	j.files.hook = nil
	require.NoError(t, j.Close())
	f.clock.now = f.clock.now.Add(2 * time.Hour)
	cold, err := OpenClosedJournal(ctx, f.o)
	require.NoError(t, err)
	defer func() { require.NoError(t, cold.Close()) }()
	calls := f.clock.calls
	history, err := cold.LookupDataClose(ctx, e.Context(), e.Digest())
	require.NoError(t, err)
	require.Equal(t, retry, history)
	require.Equal(t, calls, f.clock.calls)
	got, err = cold.CloseData(ctx, e)
	require.Error(t, err)
	require.Nil(t, got)
}

func TestJournalTaskCloseExpiredActivation(t *testing.T) {
	f := liveSetup(t)
	j := f.create(t)
	ctx := context.Background()
	require.NoError(t, j.InstallActivation(ctx, f.activation))
	// Both old business expiry (10m) and activation expiry (30m) have passed;
	// the original installed certificates remain fresh for one hour.
	f.clock.now = f.clock.now.Add(40 * time.Minute)
	e := taskCloseEvidence(t, f, taskCloseClaims(f))
	r, err := j.CloseData(ctx, e)
	require.NoError(t, err)
	require.Equal(t, "data_closed", r.State)
}
