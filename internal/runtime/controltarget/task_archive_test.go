//go:build linux || darwin

package controltarget

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type archiveFixture struct {
	quiesceFixture
	q               *AcceptedUserQuiescence
	archive         p.TaskWorkspaceArchiveEvidence
	quiescedReceipt []byte
}

// This host-only structural terminal is NOT physical namespace-drain proof.
func archiveHostSetup(t *testing.T) archiveFixture {
	t.Helper()
	f := quiesceJournalSetup(t)
	q, err := f.j.AcceptUserQuiescence(context.Background(), f.e, f.receipt)
	require.NoError(t, err)
	r := q.record
	r.State = "users_quiesced"
	r.ExecutionSetDigest = strings.Repeat("e", 64)
	r.RegisteredCount = 1
	r.NeverSpawnedCount = 1
	b, err := encodeTaskQuiescence(r)
	require.NoError(t, err)
	require.NoError(t, f.j.persistTaskQuiescenceTerminalLocked(context.Background(), b))
	q.terminal = &r
	w, err := p.SignTaskUserQuiescenceReceipt(f.runtimeKey, p.TaskUserQuiescenceReceiptClaims{Version: 1, State: r.State, Context: r.Context, TicketDigest: r.TicketDigest, NotBefore: r.NotBefore, NotAfter: r.NotAfter, ExecutionSetDigest: r.ExecutionSetDigest, RegisteredCount: r.RegisteredCount, NeverSpawnedCount: r.NeverSpawnedCount})
	require.NoError(t, err)
	return archiveEvidenceSetup(t, f, q, w)
}

func archiveEvidenceSetup(t *testing.T, f quiesceFixture, q *AcceptedUserQuiescence, w []byte) archiveFixture {
	t.Helper()
	c := q.record.Context.Current
	c.CommandID = "d1111111-1111-4111-8111-111111111111"
	c.ClaimCreateRevision++
	claims := p.TaskWorkspaceArchiveTicketClaims{Version: 1, Purpose: "task_workspace_archive", Context: p.TaskWorkspaceArchiveContext{Current: c, QuiescenceContext: q.record.Context, QuiescenceTicketDigest: q.record.TicketDigest, QuiescenceReceiptDigest: digestJournalBytes(w), PlanDigest: strings.Repeat("a", 64), QuiescenceIntentRevision: 20, FinalSyncRevision: 30}, NotBefore: f.clock.now.Add(-2 * time.Second), NotAfter: f.clock.now.Add(20 * time.Second), SessionNotAfter: f.clock.now.Add(5 * time.Minute)}
	i, err := f.o.Verifier.VerifyCommandIssuerCertificate(f.issuerWire, f.clock.now)
	require.NoError(t, err)
	wire, err := p.SignTaskWorkspaceArchiveTicket(f.issuerKey, i, claims)
	require.NoError(t, err)
	e, err := f.o.Verifier.VerifyTaskWorkspaceArchiveTicket(wire, f.issuerWire, claims.Context, f.clock.now)
	require.NoError(t, err)
	return archiveFixture{f, q, e, w}
}

func archiveEmptyQuiesceSetup(t *testing.T, f liveFixture) (quiesceFixture, *AcceptedUserQuiescence) {
	t.Helper()
	j := f.create(t)
	ctx := context.Background()
	require.NoError(t, j.InstallActivation(ctx, f.activation))
	old := taskCloseEvidence(t, f, taskCloseClaims(f))
	_, err := j.CloseData(ctx, old)
	require.NoError(t, err)
	w, err := j.SignDataCloseReceipt(ctx, old.Context(), old.Digest(), f.runtimeKey)
	require.NoError(t, err)
	c := old.Context()
	c.CommandID = "b1111111-1111-4111-8111-111111111111"
	c.ClaimCreateRevision++
	claims := p.TaskUserQuiescenceTicketClaims{Version: 1, Purpose: "task_quiesce_users", Context: p.TaskUserQuiescenceContext{Current: c, CloseDataContext: old.Context(), CloseDataTicketDigest: old.Digest(), CloseDataReceiptDigest: digestJournalBytes(w), CloseDataIntentRevision: 12}, NotBefore: old.NotBefore(), NotAfter: old.NotAfter()}
	i, err := f.o.Verifier.VerifyCommandIssuerCertificate(f.issuerWire, f.clock.now)
	require.NoError(t, err)
	wire, err := p.SignTaskUserQuiescenceTicket(f.issuerKey, i, claims)
	require.NoError(t, err)
	e, err := f.o.Verifier.VerifyTaskUserQuiescenceTicket(wire, f.issuerWire, claims.Context, f.clock.now)
	require.NoError(t, err)
	q, err := j.AcceptUserQuiescence(ctx, e, w)
	require.NoError(t, err)
	return quiesceFixture{f, j, e, w}, q
}

func TestTaskArchiveOriginalCertificateCoverage(t *testing.T) {
	for _, mode := range []string{"runtime-session", "runtime-expired", "issuer-expired", "activation-expired"} {
		t.Run(mode, func(t *testing.T) {
			f := liveSetup(t)
			root, key, err := ed25519.GenerateKey(rand.Reader)
			require.NoError(t, err)
			binding := f.activation.Binding()
			identity := f.activation.Identity()
			birth := f.activation.Birth()
			v, err := p.NewManagementVerifier(binding, []ed25519.PublicKey{root})
			require.NoError(t, err)
			before := f.clock.now.Add(-time.Minute)
			runtimeAfter, issuerAfter := f.clock.now.Add(time.Hour), f.clock.now.Add(time.Hour)
			if mode == "runtime-session" || mode == "runtime-expired" {
				runtimeAfter = f.clock.now.Add(2 * time.Minute)
			}
			if mode == "issuer-expired" {
				issuerAfter = f.clock.now.Add(2 * time.Minute)
			}
			iw, err := p.SignCommandIssuerCertificate(key, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: f.claims.Context.IssuerCertificateID, Role: "command_issuer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: f.issuerKey.Public().(ed25519.PublicKey), NotBefore: before, NotAfter: issuerAfter})
			require.NoError(t, err)
			rw, err := p.SignRuntimeIdentityCertificate(key, p.RuntimeIdentityCertificateClaims{Version: 1, CertificateID: "51111111-1111-4111-8111-111111111111", Role: "runtime_receipt", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: birth.RuntimePublicKey, NotBefore: before, NotAfter: runtimeAfter, SandboxID: identity.SandboxID, WorkspaceHash: identity.WorkspaceHash, Generation: identity.Generation, Runtime: identity.Runtime})
			require.NoError(t, err)
			aw, err := p.SignTargetActivation(key, p.TargetActivationClaims{Version: 1, Role: "target_activation", ActivationID: f.activation.ActivationID(), Binding: binding, Identity: identity, DataGateEpoch: f.o.DataGateEpoch, UID: birth.UID, GID: birth.GID, ContractDigest: birth.ContractDigest, RuntimeCertificateDigest: digestJournalBytes(rw), IssuerCertificateDigest: digestJournalBytes(iw), NotBefore: before, NotAfter: f.clock.now.Add(30 * time.Second)})
			require.NoError(t, err)
			f.activation, err = v.VerifyTargetActivation(aw, rw, iw, birth, f.clock.now)
			require.NoError(t, err)
			f.o.Verifier = v
			f.issuerWire = iw
			f.claims.Context.IssuerCertificateDigest = digestJournalBytes(iw)
			qf, q := archiveEmptyQuiesceSetup(t, f)
			// Explicit test-only structural empty terminal; no Darwin physical proof.
			r := q.record
			r.State = "users_quiesced"
			r.ExecutionSetDigest = strings.Repeat("e", 64)
			b, err := encodeTaskQuiescence(r)
			require.NoError(t, err)
			require.NoError(t, qf.j.persistTaskQuiescenceTerminalLocked(context.Background(), b))
			q.terminal = &r
			w, err := p.SignTaskUserQuiescenceReceipt(f.runtimeKey, p.TaskUserQuiescenceReceiptClaims{Version: 1, State: r.State, Context: r.Context, TicketDigest: r.TicketDigest, NotBefore: r.NotBefore, NotAfter: r.NotAfter, ExecutionSetDigest: r.ExecutionSetDigest})
			require.NoError(t, err)
			if mode != "runtime-session" {
				f.clock.now = f.clock.now.Add(3 * time.Minute)
			}
			// For expired issuer, obtain genuine evidence while the original cert
			// remains fresh, then advance only trusted UTC before admission.
			if mode == "issuer-expired" {
				f.clock.now = f.clock.now.Add(-3 * time.Minute)
			}
			current := q.record.Context.Current
			current.CommandID = "d1111111-1111-4111-8111-111111111111"
			current.ClaimCreateRevision++
			claims := p.TaskWorkspaceArchiveTicketClaims{Version: 1, Purpose: "task_workspace_archive", Context: p.TaskWorkspaceArchiveContext{Current: current, QuiescenceContext: r.Context, QuiescenceTicketDigest: r.TicketDigest, QuiescenceReceiptDigest: digestJournalBytes(w), PlanDigest: strings.Repeat("a", 64), QuiescenceIntentRevision: 20, FinalSyncRevision: 30}, NotBefore: f.clock.now.Add(-2 * time.Second), NotAfter: f.clock.now.Add(20 * time.Second), SessionNotAfter: f.clock.now.Add(time.Minute)}
			if mode == "runtime-session" {
				claims.SessionNotAfter = f.clock.now.Add(5 * time.Minute)
			}
			i, err := v.VerifyCommandIssuerCertificate(iw, f.clock.now)
			require.NoError(t, err)
			ticket, err := p.SignTaskWorkspaceArchiveTicket(f.issuerKey, i, claims)
			require.NoError(t, err)
			e, err := v.VerifyTaskWorkspaceArchiveTicket(ticket, iw, claims.Context, f.clock.now)
			require.NoError(t, err)
			if mode == "issuer-expired" {
				f.clock.now = f.clock.now.Add(3 * time.Minute)
			}
			a, err := qf.j.AcceptWorkspaceArchive(context.Background(), e, q, w)
			if mode == "activation-expired" {
				require.NoError(t, err)
				require.NotNil(t, a)
			} else {
				require.Error(t, err)
				require.Nil(t, a)
			}
		})
	}
}

func TestTaskArchiveOriginalAdmission(t *testing.T) {
	f := archiveHostSetup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, err := f.j.AcceptWorkspaceArchive(ctx, f.archive, f.q, f.quiescedReceipt)
	require.NoError(t, err, "authenticated ticket + labeled structural prerequisite must admit")
	require.NotNil(t, a)
	deadline := a.deadline
	pointIO := 0
	f.j.files.observe = func(string) { pointIO++ }
	duplicate, err := f.j.AcceptWorkspaceArchive(ctx, f.archive, f.q, f.quiescedReceipt)
	require.NoError(t, err)
	require.Same(t, a, duplicate)
	require.Zero(t, pointIO)
	f.j.files.observe = nil
	require.Equal(t, deadline, a.deadline)
	require.Greater(t, time.Until(deadline), 4*time.Minute)
	r, err := f.j.LookupWorkspaceArchive(context.Background(), f.archive.Context(), f.archive.Digest())
	require.NoError(t, err)
	require.Equal(t, "pending", r.State)
	r.Context.PlanDigest = strings.Repeat("b", 64)
	require.NoError(t, f.j.Close())
	cold, err := OpenClosedJournal(context.Background(), f.o)
	require.NoError(t, err)
	defer cold.Close()
	r, err = cold.LookupWorkspaceArchive(context.Background(), f.archive.Context(), f.archive.Digest())
	require.NoError(t, err)
	require.Equal(t, f.archive.Context(), r.Context)
	_, err = cold.AcceptWorkspaceArchive(context.Background(), f.archive, f.q, f.quiescedReceipt)
	require.Error(t, err)
}

func TestTaskArchiveAdmissionRefusals(t *testing.T) {
	for _, fault := range []string{"copy-q", "foreign-q", "pending-q", "changed-terminal", "missing-retained-q", "corrupt-receipt", "wrong-receipt-time", "wrong-receipt-summary", "birth-key", "expired-cert", "capacity-bytes", "capacity-files", "expired-ticket", "oversize-receipt"} {
		t.Run(fault, func(t *testing.T) {
			f := archiveHostSetup(t)
			q, w := f.q, f.quiescedReceipt
			switch fault {
			case "copy-q":
				c := *q
				q = &c
			case "foreign-q":
				q = archiveHostSetup(t).q
			case "pending-q":
				q.terminal = nil
			case "changed-terminal":
				q.terminal.Context.Current.WorkerID = "changed"
			case "missing-retained-q":
				require.NoError(t, os.Remove(filepath.Join(f.o.Directory, "users-quiesce.json")))
			case "corrupt-receipt":
				w = append([]byte("!"), w...)
			case "wrong-receipt-time", "wrong-receipt-summary":
				r := *q.terminal
				if fault == "wrong-receipt-time" {
					r.NotBefore = r.NotBefore.Add(time.Second)
				} else {
					r.ExecutionSetDigest = strings.Repeat("b", 64)
				}
				var err error
				w, err = p.SignTaskUserQuiescenceReceipt(f.runtimeKey, p.TaskUserQuiescenceReceiptClaims{Version: 1, State: r.State, Context: r.Context, TicketDigest: r.TicketDigest, NotBefore: r.NotBefore, NotAfter: r.NotAfter, ExecutionSetDigest: r.ExecutionSetDigest, RegisteredCount: r.RegisteredCount, NeverSpawnedCount: r.NeverSpawnedCount})
				require.NoError(t, err)
			case "birth-key":
				f.j.birth.RuntimePublicKey[0] ^= 1
			case "expired-cert":
				f.clock.now = f.clock.now.Add(25 * time.Hour)
			case "capacity-bytes":
				f.j.logicalBytes = f.j.maxBytes*85/100 - 1
			case "capacity-files":
				f.j.records += maxJournalContentFiles - f.j.contentFilesLocked() - 1
			case "expired-ticket":
				f.clock.now = f.clock.now.Add(time.Minute)
			case "oversize-receipt":
				w = make([]byte, 12289)
			}
			a, err := f.j.AcceptWorkspaceArchive(context.Background(), f.archive, q, w)
			require.Error(t, err)
			require.Nil(t, a)
			_, err = os.Stat(filepath.Join(f.o.Directory, "workspace-archive.json"))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestTaskArchiveRecordRecovery(t *testing.T) {
	for _, fault := range []string{"same-temp", "terminal-history", "conflicting-temp", "different-terminal-summaries", "missing-q", "pending-q", "missing-activation", "temp-only", "corrupt", "summary-in-pending"} {
		t.Run(fault, func(t *testing.T) {
			f := archiveHostSetup(t)
			a, err := f.j.AcceptWorkspaceArchive(context.Background(), f.archive, f.q, f.quiescedReceipt)
			require.NoError(t, err)
			r := a.record
			path := filepath.Join(f.o.Directory, "workspace-archive.json")
			before := f.j.Status()
			temp := filepath.Join(f.o.Directory, ".workspace-archive.e1111111-1111-4111-8111-111111111111.tmp")
			switch fault {
			case "terminal-history":
				r.State = "archive_complete"
				r.ArchiveBytes = 1536
				r.EntryCount = 1
				r.SHA256 = strings.Repeat("b", 64)
				b, err := encodeTaskArchive(r)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, b, 0600))
			case "same-temp", "conflicting-temp", "different-terminal-summaries", "temp-only":
				if fault == "conflicting-temp" {
					r.Context.PlanDigest = strings.Repeat("b", 64)
				}
				if fault == "different-terminal-summaries" {
					r.State = "archive_complete"
					r.ArchiveBytes = 1536
					r.EntryCount = 1
					r.SHA256 = strings.Repeat("b", 64)
					b, err := encodeTaskArchive(r)
					require.NoError(t, err)
					require.NoError(t, os.WriteFile(path, b, 0600))
					r.SHA256 = strings.Repeat("c", 64)
				}
				b, err := encodeTaskArchive(r)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(temp, b, 0600))
				if fault == "temp-only" {
					require.NoError(t, os.Remove(path))
				}
			case "missing-q":
				require.NoError(t, os.Remove(filepath.Join(f.o.Directory, "users-quiesce.json")))
			case "pending-q":
				b, err := encodeTaskQuiescence(f.q.record)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(f.o.Directory, "users-quiesce.json"), b, 0600))
			case "missing-activation":
				require.NoError(t, os.Remove(filepath.Join(f.o.Directory, "activation.json")))
			case "corrupt":
				require.NoError(t, os.WriteFile(path, []byte("{}"), 0600))
			case "summary-in-pending":
				b, err := encodeTaskArchive(r)
				require.NoError(t, err)
				b = append(b[:len(b)-1], []byte(",\"archive_bytes\":0}")...)
				require.NoError(t, os.WriteFile(path, b, 0600))
			}
			require.NoError(t, f.j.Close())
			cold, err := OpenClosedJournal(context.Background(), f.o)
			if fault == "terminal-history" {
				require.NoError(t, err)
				defer cold.Close()
				r, err := cold.LookupWorkspaceArchive(context.Background(), f.archive.Context(), f.archive.Digest())
				require.NoError(t, err)
				require.Equal(t, "archive_complete", r.State)
				_, err = cold.AcceptWorkspaceArchive(context.Background(), f.archive, f.q, f.quiescedReceipt)
				require.Error(t, err)
				return
			}
			if fault != "same-temp" {
				require.Error(t, err)
				require.Nil(t, cold)
				return
			}
			require.NoError(t, err)
			defer cold.Close()
			b, err := os.ReadFile(temp)
			require.NoError(t, err)
			require.Equal(t, before.LogicalBytes+int64(len(b)), cold.Status().LogicalBytes)
			require.EqualValues(t, 1, cold.Status().TemporaryFiles)
		})
	}
}

func TestTaskArchiveChangedDuplicate(t *testing.T) {
	for _, mode := range []string{"attribution", "receipt", "copy-archive", "expired-session"} {
		t.Run(mode, func(t *testing.T) {
			f := archiveHostSetup(t)
			a, err := f.j.AcceptWorkspaceArchive(context.Background(), f.archive, f.q, f.quiescedReceipt)
			require.NoError(t, err)
			e, w := f.archive, f.quiescedReceipt
			switch mode {
			case "attribution":
				c := p.TaskWorkspaceArchiveTicketClaims{Version: 1, Purpose: "task_workspace_archive", Context: e.Context(), NotBefore: e.NotBefore(), NotAfter: e.NotAfter(), SessionNotAfter: e.SessionNotAfter()}
				c.Context.PlanDigest = strings.Repeat("b", 64)
				i, err := f.o.Verifier.VerifyCommandIssuerCertificate(f.issuerWire, f.clock.now)
				require.NoError(t, err)
				wire, err := p.SignTaskWorkspaceArchiveTicket(f.issuerKey, i, c)
				require.NoError(t, err)
				e, err = f.o.Verifier.VerifyTaskWorkspaceArchiveTicket(wire, f.issuerWire, c.Context, f.clock.now)
				require.NoError(t, err)
			case "receipt":
				w = append([]byte("!"), w...)
			case "copy-archive":
				copied := *a
				f.j.workspaceArchive = &copied
			case "expired-session":
				a.deadline = time.Now().Add(-time.Second)
			}
			io := 0
			f.j.files.observe = func(string) { io++ }
			duplicate, err := f.j.AcceptWorkspaceArchive(context.Background(), e, f.q, w)
			require.Error(t, err)
			require.Nil(t, duplicate)
			require.Zero(t, io)
		})
	}
}
