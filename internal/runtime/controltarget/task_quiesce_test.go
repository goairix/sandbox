//go:build linux || darwin

package controltarget

import (
	"context"
	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
)

type quiesceFixture struct {
	liveFixture
	j       *Journal
	e       controlprotocol.TaskUserQuiescenceEvidence
	receipt []byte
}

func quiesceJournalSetup(t *testing.T) quiesceFixture {
	t.Helper()
	f := liveSetup(t)
	j := f.create(t)
	ctx := context.Background()
	require.NoError(t, j.InstallActivation(ctx, f.activation))
	// An already accepted execution must remain unchanged by the USERS barrier.
	_, err := j.AcceptExecution(ctx, f.e)
	require.NoError(t, err)
	old := taskCloseEvidence(t, f, taskCloseClaims(f))
	_, err = j.CloseData(ctx, old)
	require.NoError(t, err)
	receipt, err := j.SignDataCloseReceipt(ctx, old.Context(), old.Digest(), f.runtimeKey)
	require.NoError(t, err)
	current := old.Context()
	current.CommandID = "b1111111-1111-4111-8111-111111111111"
	current.ClaimCreateRevision++
	current.ClaimID = "c1111111-1111-4111-8111-111111111111"
	current.WorkerID = "worker-2"
	current.LeaseID++
	c := controlprotocol.TaskUserQuiescenceTicketClaims{Version: 1, Purpose: "task_quiesce_users", Context: controlprotocol.TaskUserQuiescenceContext{Current: current, CloseDataContext: old.Context(), CloseDataTicketDigest: old.Digest(), CloseDataReceiptDigest: digestJournalBytes(receipt), CloseDataIntentRevision: 12}, NotBefore: old.NotBefore(), NotAfter: old.NotAfter()}
	issuer, err := f.o.Verifier.VerifyCommandIssuerCertificate(f.issuerWire, f.clock.now)
	require.NoError(t, err)
	wire, err := controlprotocol.SignTaskUserQuiescenceTicket(f.issuerKey, issuer, c)
	require.NoError(t, err)
	e, err := f.o.Verifier.VerifyTaskUserQuiescenceTicket(wire, f.issuerWire, c.Context, f.clock.now)
	require.NoError(t, err)
	return quiesceFixture{f, j, e, receipt}
}
func TestTaskQuiesceJournal(t *testing.T) {
	f := quiesceJournalSetup(t)
	j := f.j
	ctx := context.Background()
	before, err := j.Lookup(ctx, f.claims.Context.CommandID)
	require.NoError(t, err)
	gateBefore, err := os.ReadFile(filepath.Join(f.o.Directory, "gate.json"))
	require.NoError(t, err)
	a, err := j.AcceptUserQuiescence(ctx, f.e, f.receipt)
	require.NoError(t, err)
	require.NotNil(t, a)
	same, err := j.AcceptUserQuiescence(ctx, f.e, f.receipt)
	require.NoError(t, err)
	require.Same(t, a, same)
	r, err := j.LookupUserQuiescence(ctx, f.e.Context(), f.e.Digest())
	require.NoError(t, err)
	require.Equal(t, "pending", r.State)
	r.Context.Current.WorkerID = "alias"
	r, err = j.LookupUserQuiescence(ctx, f.e.Context(), f.e.Digest())
	require.NoError(t, err)
	require.Equal(t, f.e.Context(), r.Context)
	w, err := j.SignUserQuiescenceAccepted(ctx, a, f.runtimeKey)
	require.NoError(t, err)
	accepted, err := f.o.Verifier.VerifyTaskUserQuiescenceAccepted(w, f.activation.RuntimeCertificate(), f.e.Context(), f.e.Digest(), f.e.NotBefore(), f.e.NotAfter(), f.clock.now)
	require.NoError(t, err)
	require.Equal(t, "quiescence_accepted", accepted.State())
	after, err := j.Lookup(ctx, f.claims.Context.CommandID)
	require.NoError(t, err)
	require.Equal(t, before, after)
	gateAfter, err := os.ReadFile(filepath.Join(f.o.Directory, "gate.json"))
	require.NoError(t, err)
	require.Equal(t, gateBefore, gateAfter)
	copyHandle := *a
	_, err = j.SignUserQuiescenceAccepted(ctx, &copyHandle, f.runtimeKey)
	require.Error(t, err)
	require.NoError(t, j.Close())
	cold, err := OpenClosedJournal(ctx, f.o)
	require.NoError(t, err)
	defer cold.Close()
	r, err = cold.LookupUserQuiescence(ctx, f.e.Context(), f.e.Digest())
	require.NoError(t, err)
	require.Equal(t, "pending", r.State)
	_, err = cold.AcceptUserQuiescence(ctx, f.e, f.receipt)
	require.Error(t, err)
	_, err = cold.SignUserQuiescenceAccepted(ctx, a, f.runtimeKey)
	require.Error(t, err)
}
