package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func archiveSetup(t *testing.T) (taskCloseFixture, TaskWorkspaceArchiveTicketClaims) {
	f, q := quiesceSetup(t)
	c := q.Context.Current
	c.CommandID = "ca1c1595-cbfd-4d2e-9ef7-f4dd3c84b172"
	return f, TaskWorkspaceArchiveTicketClaims{Version: 1, Purpose: "task_workspace_archive", Context: TaskWorkspaceArchiveContext{Current: c, QuiescenceContext: q.Context, QuiescenceTicketDigest: strings.Repeat("e", 64), QuiescenceReceiptDigest: strings.Repeat("f", 64), PlanDigest: strings.Repeat("a", 64), QuiescenceIntentRevision: 13, FinalSyncRevision: 14}, NotBefore: q.NotBefore, NotAfter: q.NotAfter, SessionNotAfter: f.now.Add(40 * time.Second)}
}

func TestTaskWorkspaceArchiveTicketIndependentPositive(t *testing.T) {
	f, c := archiveSetup(t)
	raw := taskCloseRaw(t, f.issuerKey, c, "sandbox-task-workspace-archive-ticket:v1\x00")
	e, err := f.verifier.VerifyTaskWorkspaceArchiveTicket(raw, f.issuerWire, c.Context, f.now)
	require.NoError(t, err, "independently signed valid archive ticket must authenticate")
	require.Equal(t, c.Context, e.Context())
	w, err := SignTaskWorkspaceArchiveTicket(f.issuerKey, f.issuer, c)
	require.NoError(t, err)
	require.Equal(t, raw, w)
	require.Equal(t, wireDigest(w), e.Digest())
	require.Equal(t, c.NotBefore, e.NotBefore())
	require.Equal(t, c.NotAfter, e.NotAfter())
	require.Equal(t, c.SessionNotAfter, e.SessionNotAfter())
	clear(raw)
	clear(e.Wire())
	require.Equal(t, w, e.Wire())
	contextCopy := e.Context()
	contextCopy.PlanDigest = ""
	require.Equal(t, c.Context, e.Context())
}

// Mutations retain syntactically valid lineage to exercise independent expected
// attribution rather than only invalid-field parsing.
func archiveContextChanges() map[string]func(*TaskWorkspaceArchiveContext) {
	changes := map[string]func(*TaskWorkspaceArchiveContext){}
	for name, mutate := range taskContextChanges() {
		if name == "CommandID" {
			continue
		}
		changes[name] = func(c *TaskWorkspaceArchiveContext) {
			mutate(&c.Current)
			mutate(&c.QuiescenceContext.Current)
			mutate(&c.QuiescenceContext.CloseDataContext)
		}
	}
	for name, mutate := range map[string]func(*TaskWorkspaceArchiveContext){
		"archive-command": func(c *TaskWorkspaceArchiveContext) { c.Current.CommandID = "da1c1595-cbfd-4d2e-9ef7-f4dd3c84b172" },
		"quiesce-command": func(c *TaskWorkspaceArchiveContext) {
			c.QuiescenceContext.Current.CommandID = "da1c1595-cbfd-4d2e-9ef7-f4dd3c84b172"
		},
		"close-command": func(c *TaskWorkspaceArchiveContext) {
			c.QuiescenceContext.CloseDataContext.CommandID = "da1c1595-cbfd-4d2e-9ef7-f4dd3c84b172"
		},
		"quiesce-ticket":  func(c *TaskWorkspaceArchiveContext) { c.QuiescenceTicketDigest = strings.Repeat("b", 64) },
		"quiesce-receipt": func(c *TaskWorkspaceArchiveContext) { c.QuiescenceReceiptDigest = strings.Repeat("b", 64) },
		"plan":            func(c *TaskWorkspaceArchiveContext) { c.PlanDigest = strings.Repeat("b", 64) },
		"close-ticket": func(c *TaskWorkspaceArchiveContext) {
			c.QuiescenceContext.CloseDataTicketDigest = strings.Repeat("b", 64)
		},
		"close-receipt": func(c *TaskWorkspaceArchiveContext) {
			c.QuiescenceContext.CloseDataReceiptDigest = strings.Repeat("b", 64)
		},
		"close-intent": func(c *TaskWorkspaceArchiveContext) {
			c.QuiescenceContext.CloseDataIntentRevision++
			c.QuiescenceIntentRevision++
			c.FinalSyncRevision++
		},
		"quiesce-intent": func(c *TaskWorkspaceArchiveContext) { c.QuiescenceIntentRevision++; c.FinalSyncRevision++ },
		"final-sync":     func(c *TaskWorkspaceArchiveContext) { c.FinalSyncRevision++ },
	} {
		changes[name] = mutate
	}
	return changes
}

func archiveInvalidChanges() map[string]func(*TaskWorkspaceArchiveTicketClaims) {
	changes := map[string]func(*TaskWorkspaceArchiveTicketClaims){
		"version":            func(c *TaskWorkspaceArchiveTicketClaims) { c.Version = 2 },
		"purpose":            func(c *TaskWorkspaceArchiveTicketClaims) { c.Purpose = "task_quiesce_users" },
		"earlier-claim":      func(c *TaskWorkspaceArchiveTicketClaims) { c.Context.Current.ClaimCreateRevision-- },
		"equal-other-claim":  func(c *TaskWorkspaceArchiveTicketClaims) { c.Context.Current.ClaimID = c.Context.Current.CommandID },
		"equal-other-worker": func(c *TaskWorkspaceArchiveTicketClaims) { c.Context.Current.WorkerID = "other" },
		"equal-other-lease":  func(c *TaskWorkspaceArchiveTicketClaims) { c.Context.Current.LeaseID++ },
		"close-command-collision": func(c *TaskWorkspaceArchiveTicketClaims) {
			c.Context.Current.CommandID = c.Context.QuiescenceContext.CloseDataContext.CommandID
		},
		"quiesce-command-collision": func(c *TaskWorkspaceArchiveTicketClaims) {
			c.Context.Current.CommandID = c.Context.QuiescenceContext.Current.CommandID
		},
		"quiesce-intent-claim": func(c *TaskWorkspaceArchiveTicketClaims) {
			c.Context.QuiescenceIntentRevision = c.Context.QuiescenceContext.Current.ClaimCreateRevision
		},
		"quiesce-intent-close": func(c *TaskWorkspaceArchiveTicketClaims) {
			c.Context.QuiescenceIntentRevision = c.Context.QuiescenceContext.CloseDataIntentRevision
		},
		"final-sync-claim": func(c *TaskWorkspaceArchiveTicketClaims) {
			c.Context.FinalSyncRevision = c.Context.Current.ClaimCreateRevision
		},
		"final-sync-quiesce": func(c *TaskWorkspaceArchiveTicketClaims) {
			c.Context.FinalSyncRevision = c.Context.QuiescenceIntentRevision
		},
		"empty-plan":            func(c *TaskWorkspaceArchiveTicketClaims) { c.Context.PlanDigest = "" },
		"upper-quiesce-ticket":  func(c *TaskWorkspaceArchiveTicketClaims) { c.Context.QuiescenceTicketDigest = strings.Repeat("A", 64) },
		"short-quiesce-receipt": func(c *TaskWorkspaceArchiveTicketClaims) { c.Context.QuiescenceReceiptDigest = "a" },
		"session-equal":         func(c *TaskWorkspaceArchiveTicketClaims) { c.SessionNotAfter = c.NotAfter },
		"session-earlier":       func(c *TaskWorkspaceArchiveTicketClaims) { c.SessionNotAfter = c.NotAfter.Add(-time.Nanosecond) },
		"session-over-hour": func(c *TaskWorkspaceArchiveTicketClaims) {
			c.SessionNotAfter = c.NotBefore.Add(time.Hour + time.Nanosecond)
		},
		"session-zero": func(c *TaskWorkspaceArchiveTicketClaims) { c.SessionNotAfter = time.Time{} },
		"session-offset": func(c *TaskWorkspaceArchiveTicketClaims) {
			c.SessionNotAfter = c.SessionNotAfter.In(time.FixedZone("offset", 3600))
		},
	}
	return changes
}

func TestTaskWorkspaceArchiveTicketAttributionAndRejects(t *testing.T) {
	f, original := archiveSetup(t)
	valid := taskCloseRaw(t, f.issuerKey, original, "sandbox-task-workspace-archive-ticket:v1\x00")
	reject := func(t *testing.T, wire []byte, expected TaskWorkspaceArchiveContext) {
		e, err := f.verifier.VerifyTaskWorkspaceArchiveTicket(wire, f.issuerWire, expected, f.now)
		require.Error(t, err)
		require.Equal(t, TaskWorkspaceArchiveEvidence{}, e)
	}
	for name, mutate := range archiveContextChanges() {
		t.Run("other-"+name, func(t *testing.T) {
			c := original
			mutate(&c.Context)
			require.NoError(t, validateTaskArchiveContext(c.Context))
			require.NotEqual(t, original.Context, c.Context)
			reject(t, valid, c.Context)
			reject(t, taskCloseRaw(t, f.issuerKey, c, "sandbox-task-workspace-archive-ticket:v1\x00"), original.Context)
		})
	}
	for name, mutate := range archiveInvalidChanges() {
		t.Run(name, func(t *testing.T) {
			c := original
			mutate(&c)
			wire, err := SignTaskWorkspaceArchiveTicket(f.issuerKey, f.issuer, c)
			require.Error(t, err)
			require.Nil(t, wire)
			reject(t, taskCloseRaw(t, f.issuerKey, c, "sandbox-task-workspace-archive-ticket:v1\x00"), original.Context)
		})
	}
	for _, domain := range []string{"sandbox-task-workspace-archive-ticket:v1", "sandbox-task-quiesce-users-ticket:v1\x00", "sandbox-task-workspace-archive-complete:v1\x00"} {
		reject(t, taskCloseRaw(t, f.issuerKey, original, domain), original.Context)
	}
	reject(t, taskCloseRaw(t, f.runtimeKey, original, "sandbox-task-workspace-archive-ticket:v1\x00"), original.Context)
	for _, key := range []ed25519.PrivateKey{nil, f.runtimeKey, f.issuerKey[:32]} {
		_, err := SignTaskWorkspaceArchiveTicket(key, f.issuer, original)
		require.Error(t, err)
	}
	_, err := SignTaskWorkspaceArchiveTicket(f.issuerKey, CommandIssuerIdentity{}, original)
	require.Error(t, err)
	var nilVerifier *ManagementVerifier
	_, err = nilVerifier.VerifyTaskWorkspaceArchiveTicket(valid, f.issuerWire, original.Context, f.now)
	require.Error(t, err)
	// A strictly newer claim may have a different owner; equality may not.
	c := original
	c.Context.Current.ClaimCreateRevision++
	c.Context.Current.ClaimID = c.Context.Current.CommandID
	c.Context.Current.WorkerID = "new"
	c.Context.Current.LeaseID++
	w, err := SignTaskWorkspaceArchiveTicket(f.issuerKey, f.issuer, c)
	require.NoError(t, err)
	_, err = f.verifier.VerifyTaskWorkspaceArchiveTicket(w, f.issuerWire, c.Context, f.now)
	require.NoError(t, err)
}

func TestTaskWorkspaceArchiveTicketWindowsAndIssuer(t *testing.T) {
	f, c := archiveSetup(t)
	w := taskCloseRaw(t, f.issuerKey, c, "sandbox-task-workspace-archive-ticket:v1\x00")
	for _, tc := range []struct {
		now time.Time
		ok  bool
	}{
		{c.NotBefore.Add(time.Second - time.Nanosecond), false}, {c.NotBefore.Add(time.Second), true},
		{c.NotAfter.Add(-time.Second - time.Nanosecond), true}, {c.NotAfter.Add(-time.Second), false},
		{c.SessionNotAfter, false}, {time.Time{}, false}, {f.now.In(time.FixedZone("offset", 3600)), false},
	} {
		_, err := f.verifier.VerifyTaskWorkspaceArchiveTicket(w, f.issuerWire, c.Context, tc.now)
		if tc.ok {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
	}
	var cert commandIssuerEnvelope
	require.NoError(t, json.Unmarshal(f.issuerWire, &cert))
	for name, mutate := range map[string]func(*CommandIssuerCertificateClaims){
		"session-short": func(x *CommandIssuerCertificateClaims) { x.NotAfter = c.SessionNotAfter.Add(-time.Nanosecond) },
		"before-late":   func(x *CommandIssuerCertificateClaims) { x.NotBefore = c.NotBefore.Add(time.Nanosecond) },
		"expired":       func(x *CommandIssuerCertificateClaims) { x.NotAfter = f.now.Add(time.Second) },
		"binding":       func(x *CommandIssuerCertificateClaims) { x.Target = "other" },
		"id":            func(x *CommandIssuerCertificateClaims) { x.CertificateID = c.Context.Current.CommandID },
		"key":           func(x *CommandIssuerCertificateClaims) { x.PublicKey = f.runtimeKey.Public().(ed25519.PublicKey) },
	} {
		t.Run(name, func(t *testing.T) {
			x := cert.Claims
			mutate(&x)
			iw, err := SignCommandIssuerCertificate(f.root, x)
			require.NoError(t, err)
			bad := c
			// Bind to the new certificate so interval tests cannot pass merely by
			// failing the old digest match.
			for _, ctx := range []*TaskCloseDataContext{&bad.Context.Current, &bad.Context.QuiescenceContext.Current, &bad.Context.QuiescenceContext.CloseDataContext} {
				ctx.IssuerCertificateID = x.CertificateID
				ctx.IssuerCertificateDigest = wireDigest(iw)
			}
			wire := taskCloseRaw(t, f.issuerKey, bad, "sandbox-task-workspace-archive-ticket:v1\x00")
			_, err = f.verifier.VerifyTaskWorkspaceArchiveTicket(wire, iw, bad.Context, f.now)
			if name == "id" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			_, err = f.verifier.VerifyTaskWorkspaceArchiveTicket(w, iw, c.Context, f.now)
			require.Error(t, err)
			if issuer, err := f.verifier.VerifyCommandIssuerCertificate(iw, f.now); err == nil && name != "id" {
				_, err = SignTaskWorkspaceArchiveTicket(f.issuerKey, issuer, bad)
				require.Error(t, err)
			}
		})
	}
	// Exact certificate edges and maximum protocol windows are inclusive.
	c.NotBefore = cert.Claims.NotBefore
	c.NotAfter = c.NotBefore.Add(30 * time.Second)
	c.SessionNotAfter = c.NotBefore.Add(time.Hour)
	if c.SessionNotAfter.After(cert.Claims.NotAfter) {
		c.SessionNotAfter = cert.Claims.NotAfter
	}
	w, err := SignTaskWorkspaceArchiveTicket(f.issuerKey, f.issuer, c)
	require.NoError(t, err)
	_, err = f.verifier.VerifyTaskWorkspaceArchiveTicket(w, f.issuerWire, c.Context, c.NotAfter.Add(-2*time.Second))
	require.NoError(t, err)
	pretty := new(bytes.Buffer)
	require.NoError(t, json.Indent(pretty, w, "", " "))
	e, err := f.verifier.VerifyTaskWorkspaceArchiveTicket(pretty.Bytes(), f.issuerWire, c.Context, c.NotAfter.Add(-2*time.Second))
	require.NoError(t, err)
	require.Equal(t, w, e.Wire())
}
