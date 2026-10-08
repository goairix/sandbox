package controlprotocol

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func archiveReceipt(c TaskWorkspaceArchiveTicketClaims) TaskWorkspaceArchiveReceiptClaims {
	return TaskWorkspaceArchiveReceiptClaims{Version: 1, State: "archive_complete", Context: c.Context, TicketDigest: strings.Repeat("b", 64), NotBefore: c.NotBefore, NotAfter: c.NotAfter, SessionNotAfter: c.SessionNotAfter, ArchiveBytes: 1536, EntryCount: 1, SHA256: strings.Repeat("c", 64)}
}

func TestTaskWorkspaceArchiveReceiptIndependentHistorical(t *testing.T) {
	f, c := archiveSetup(t)
	r := archiveReceipt(c)
	w := taskCloseRaw(t, f.runtimeKey, r, "sandbox-task-workspace-archive-complete:v1\x00")
	for _, now := range []time.Time{f.now, c.NotAfter.Add(time.Second), c.SessionNotAfter.Add(time.Minute)} {
		e, err := f.verifier.VerifyTaskWorkspaceArchiveReceipt(w, f.runtimeWire, c.Context, r.TicketDigest, c.NotBefore, c.NotAfter, c.SessionNotAfter, now)
		require.NoError(t, err)
		require.Equal(t, c.Context, e.Context())
		require.Equal(t, c.NotBefore, e.NotBefore())
		require.Equal(t, c.NotAfter, e.NotAfter())
		require.Equal(t, c.SessionNotAfter, e.SessionNotAfter())
		require.Equal(t, r.TicketDigest, e.TicketDigest())
		require.Equal(t, r.State, e.State())
		require.Equal(t, r.ArchiveBytes, e.ArchiveBytes())
		require.Equal(t, r.EntryCount, e.EntryCount())
		require.Equal(t, r.SHA256, e.SHA256())
		clear(e.Wire())
		require.Equal(t, w, e.Wire())
	}
	signed, err := SignTaskWorkspaceArchiveReceipt(f.runtimeKey, r)
	require.NoError(t, err)
	require.Equal(t, w, signed)
	e, err := f.verifier.VerifyTaskWorkspaceArchiveReceipt(w, f.runtimeWire, c.Context, r.TicketDigest, c.NotBefore, c.NotAfter, c.SessionNotAfter, f.now)
	require.NoError(t, err)
	clear(w)
	require.Equal(t, signed, e.Wire())
	r.ArchiveBytes, r.EntryCount = 1<<40, 1000000
	w = taskCloseRaw(t, f.runtimeKey, r, "sandbox-task-workspace-archive-complete:v1\x00")
	_, err = f.verifier.VerifyTaskWorkspaceArchiveReceipt(w, f.runtimeWire, c.Context, r.TicketDigest, c.NotBefore, c.NotAfter, c.SessionNotAfter, f.now)
	require.NoError(t, err)
}

func TestTaskWorkspaceArchiveReceiptRejects(t *testing.T) {
	f, c := archiveSetup(t)
	r := archiveReceipt(c)
	verify := func(w []byte, expected TaskWorkspaceArchiveContext, digest string, before, after, session time.Time) error {
		e, err := f.verifier.VerifyTaskWorkspaceArchiveReceipt(w, f.runtimeWire, expected, digest, before, after, session, f.now)
		if err != nil {
			require.Equal(t, TaskWorkspaceArchiveReceiptEvidence{}, e)
		}
		return err
	}
	reject := func(w []byte) {
		require.Error(t, verify(w, c.Context, r.TicketDigest, c.NotBefore, c.NotAfter, c.SessionNotAfter))
	}
	valid := taskCloseRaw(t, f.runtimeKey, r, "sandbox-task-workspace-archive-complete:v1\x00")
	for name, mutate := range map[string]func(*TaskWorkspaceArchiveReceiptClaims){
		"version":          func(x *TaskWorkspaceArchiveReceiptClaims) { x.Version = 0 },
		"state":            func(x *TaskWorkspaceArchiveReceiptClaims) { x.State = "users_quiesced" },
		"ticket":           func(x *TaskWorkspaceArchiveReceiptClaims) { x.TicketDigest = "" },
		"hash":             func(x *TaskWorkspaceArchiveReceiptClaims) { x.SHA256 = strings.Repeat("A", 64) },
		"zero-bytes":       func(x *TaskWorkspaceArchiveReceiptClaims) { x.ArchiveBytes = 0 },
		"small-bytes":      func(x *TaskWorkspaceArchiveReceiptClaims) { x.ArchiveBytes = 1024 },
		"unaligned":        func(x *TaskWorkspaceArchiveReceiptClaims) { x.ArchiveBytes = 1537 },
		"over-tib":         func(x *TaskWorkspaceArchiveReceiptClaims) { x.ArchiveBytes = 1<<40 + 512 },
		"overflow-bytes":   func(x *TaskWorkspaceArchiveReceiptClaims) { x.ArchiveBytes = ^uint64(0) },
		"zero-entries":     func(x *TaskWorkspaceArchiveReceiptClaims) { x.EntryCount = 0 },
		"over-entries":     func(x *TaskWorkspaceArchiveReceiptClaims) { x.EntryCount = 1000001 },
		"overflow-entries": func(x *TaskWorkspaceArchiveReceiptClaims) { x.EntryCount = ^uint64(0) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := r
			mutate(&bad)
			wire, err := SignTaskWorkspaceArchiveReceipt(f.runtimeKey, bad)
			require.Error(t, err)
			require.Nil(t, wire)
			reject(taskCloseRaw(t, f.runtimeKey, bad, "sandbox-task-workspace-archive-complete:v1\x00"))
		})
	}
	for name, mutate := range archiveInvalidChanges() {
		if name == "purpose" {
			continue
		}
		t.Run("ticket-invariant-"+name, func(t *testing.T) {
			bad := c
			mutate(&bad)
			x := archiveReceipt(bad)
			x.Version = bad.Version
			wire, err := SignTaskWorkspaceArchiveReceipt(f.runtimeKey, x)
			require.Error(t, err)
			require.Nil(t, wire)
			reject(taskCloseRaw(t, f.runtimeKey, x, "sandbox-task-workspace-archive-complete:v1\x00"))
		})
	}
	for name, mutate := range archiveContextChanges() {
		t.Run("context-"+name, func(t *testing.T) {
			x := r
			mutate(&x.Context)
			require.NoError(t, validateTaskArchiveContext(x.Context))
			reject(taskCloseRaw(t, f.runtimeKey, x, "sandbox-task-workspace-archive-complete:v1\x00"))
			require.Error(t, verify(valid, x.Context, r.TicketDigest, c.NotBefore, c.NotAfter, c.SessionNotAfter))
		})
	}
	for _, field := range []string{"digest", "before", "after", "session"} {
		t.Run("original-"+field, func(t *testing.T) {
			x := r
			switch field {
			case "digest":
				x.TicketDigest = strings.Repeat("d", 64)
			case "before":
				x.NotBefore = x.NotBefore.Add(time.Nanosecond)
			case "after":
				x.NotAfter = x.NotAfter.Add(time.Nanosecond)
			case "session":
				x.SessionNotAfter = x.SessionNotAfter.Add(time.Nanosecond)
			}
			reject(taskCloseRaw(t, f.runtimeKey, x, "sandbox-task-workspace-archive-complete:v1\x00"))
			require.Error(t, verify(valid, c.Context, x.TicketDigest, x.NotBefore, x.NotAfter, x.SessionNotAfter))
		})
	}
	for _, domain := range []string{"sandbox-task-workspace-archive-complete:v1", "sandbox-task-workspace-archive-ticket:v1\x00", "sandbox-task-users-quiesced-receipt:v1\x00"} {
		reject(taskCloseRaw(t, f.runtimeKey, r, domain))
	}
	reject(taskCloseRaw(t, f.issuerKey, r, "sandbox-task-workspace-archive-complete:v1\x00"))
	for _, key := range []ed25519.PrivateKey{nil, f.runtimeKey[:32]} {
		_, err := SignTaskWorkspaceArchiveReceipt(key, r)
		require.Error(t, err)
	}
	var nilVerifier *ManagementVerifier
	_, err := nilVerifier.VerifyTaskWorkspaceArchiveReceipt(valid, f.runtimeWire, c.Context, r.TicketDigest, c.NotBefore, c.NotAfter, c.SessionNotAfter, f.now)
	require.Error(t, err)
}

func TestTaskWorkspaceArchiveReceiptRuntimeCoverage(t *testing.T) {
	f, c := archiveSetup(t)
	r := archiveReceipt(c)
	w := taskCloseRaw(t, f.runtimeKey, r, "sandbox-task-workspace-archive-complete:v1\x00")
	var cert runtimeIdentityEnvelope
	require.NoError(t, json.Unmarshal(f.runtimeWire, &cert))
	for name, mutate := range map[string]func(*RuntimeIdentityCertificateClaims){
		"before-late":   func(x *RuntimeIdentityCertificateClaims) { x.NotBefore = c.NotBefore.Add(time.Nanosecond) },
		"session-short": func(x *RuntimeIdentityCertificateClaims) { x.NotAfter = c.SessionNotAfter.Add(-time.Nanosecond) },
		"expired":       func(x *RuntimeIdentityCertificateClaims) { x.NotAfter = f.now.Add(time.Second) },
		"binding":       func(x *RuntimeIdentityCertificateClaims) { x.Target = "other" },
		"sandbox":       func(x *RuntimeIdentityCertificateClaims) { x.SandboxID = "other" },
		"workspace":     func(x *RuntimeIdentityCertificateClaims) { x.WorkspaceHash = strings.Repeat("d", 64) },
		"generation":    func(x *RuntimeIdentityCertificateClaims) { x.Generation++ },
		"runtime":       func(x *RuntimeIdentityCertificateClaims) { x.Runtime.BootID = "other" },
		"key":           func(x *RuntimeIdentityCertificateClaims) { x.PublicKey = f.issuerKey.Public().(ed25519.PublicKey) },
	} {
		t.Run(name, func(t *testing.T) {
			x := cert.Claims
			mutate(&x)
			cw, err := SignRuntimeIdentityCertificate(f.root, x)
			require.NoError(t, err)
			_, err = f.verifier.VerifyTaskWorkspaceArchiveReceipt(w, cw, c.Context, r.TicketDigest, c.NotBefore, c.NotAfter, c.SessionNotAfter, f.now)
			require.Error(t, err)
		})
	}
	for _, now := range []time.Time{{}, f.now.In(time.FixedZone("offset", 3600)), cert.Claims.NotAfter.Add(-time.Second)} {
		_, err := f.verifier.VerifyTaskWorkspaceArchiveReceipt(w, f.runtimeWire, c.Context, r.TicketDigest, c.NotBefore, c.NotAfter, c.SessionNotAfter, now)
		require.Error(t, err)
	}
	x := cert.Claims
	x.NotBefore, x.NotAfter = c.NotBefore, c.SessionNotAfter
	cw, err := SignRuntimeIdentityCertificate(f.root, x)
	require.NoError(t, err)
	_, err = f.verifier.VerifyTaskWorkspaceArchiveReceipt(w, cw, c.Context, r.TicketDigest, c.NotBefore, c.NotAfter, c.SessionNotAfter, f.now)
	require.NoError(t, err)
}
