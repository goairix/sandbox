package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"time"
)

func quiesceSetup(t *testing.T) (taskCloseFixture, TaskUserQuiescenceTicketClaims) {
	f := taskCloseSetup(t)
	old := f.ticket.Context
	current := old
	current.CommandID = "ba1c1595-cbfd-4d2e-9ef7-f4dd3c84b172"
	return f, TaskUserQuiescenceTicketClaims{Version: 1, Purpose: "task_quiesce_users", Context: TaskUserQuiescenceContext{Current: current, CloseDataContext: old, CloseDataTicketDigest: strings.Repeat("c", 64), CloseDataReceiptDigest: strings.Repeat("d", 64), CloseDataIntentRevision: 12}, NotBefore: f.ticket.NotBefore, NotAfter: f.ticket.NotAfter}
}
func TestTaskQuiesceTicket(t *testing.T) {
	f, c := quiesceSetup(t)
	raw := taskCloseRaw(t, f.issuerKey, c, "sandbox-task-quiesce-users-ticket:v1\x00")
	e, err := f.verifier.VerifyTaskUserQuiescenceTicket(raw, f.issuerWire, c.Context, f.now)
	require.NoError(t, err)
	w, err := SignTaskUserQuiescenceTicket(f.issuerKey, f.issuer, c)
	require.NoError(t, err)
	require.Equal(t, raw, w)
	require.Equal(t, c.Context, e.Context())
	require.Equal(t, c.NotBefore, e.NotBefore())
	require.Equal(t, c.NotAfter, e.NotAfter())
	require.Equal(t, wireDigest(raw), e.Digest())
	clear(raw)
	require.Equal(t, w, e.Wire())
	clear(e.Wire())
	require.Equal(t, w, e.Wire())
	for _, delta := range []time.Duration{-time.Nanosecond, 0} {
		now := c.NotAfter.Add(-time.Second).Add(delta)
		_, err = f.verifier.VerifyTaskUserQuiescenceTicket(w, f.issuerWire, c.Context, now)
		if delta == 0 {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
	}
}
func TestTaskQuiesceTicketRejects(t *testing.T) {
	f, original := quiesceSetup(t)
	changes := map[string]func(*TaskUserQuiescenceTicketClaims){
		"purpose": func(c *TaskUserQuiescenceTicketClaims) { c.Purpose = "task_close_data" }, "version": func(c *TaskUserQuiescenceTicketClaims) { c.Version = 2 },
		"task":               func(c *TaskUserQuiescenceTicketClaims) { c.Context.Current.TaskDigest = strings.Repeat("e", 64) },
		"earlier":            func(c *TaskUserQuiescenceTicketClaims) { c.Context.Current.ClaimCreateRevision-- },
		"equal-other-claim":  func(c *TaskUserQuiescenceTicketClaims) { c.Context.Current.ClaimID = c.Context.Current.CommandID },
		"equal-other-worker": func(c *TaskUserQuiescenceTicketClaims) { c.Context.Current.WorkerID = "other" },
		"equal-other-lease":  func(c *TaskUserQuiescenceTicketClaims) { c.Context.Current.LeaseID++ },
		"digest":             func(c *TaskUserQuiescenceTicketClaims) { c.Context.CloseDataReceiptDigest = "" },
		"intent":             func(c *TaskUserQuiescenceTicketClaims) { c.Context.CloseDataIntentRevision = 0 },
		"window":             func(c *TaskUserQuiescenceTicketClaims) { c.NotAfter = c.NotBefore.Add(31 * time.Second) },
		"binding": func(c *TaskUserQuiescenceTicketClaims) {
			c.Context.Current.Target = "other"
			c.Context.CloseDataContext.Target = "other"
		},
	}
	for n, m := range changes {
		t.Run(n, func(t *testing.T) {
			c := original
			m(&c)
			w, err := SignTaskUserQuiescenceTicket(f.issuerKey, f.issuer, c)
			require.Error(t, err)
			require.Nil(t, w)
			w = taskCloseRaw(t, f.issuerKey, c, "sandbox-task-quiesce-users-ticket:v1\x00")
			e, err := f.verifier.VerifyTaskUserQuiescenceTicket(w, f.issuerWire, c.Context, f.now)
			require.Error(t, err)
			require.Equal(t, TaskUserQuiescenceEvidence{}, e)
		})
	}
	for _, domain := range []string{"sandbox-task-quiesce-users-ticket:v1", "sandbox-task-close-data-ticket:v1\x00"} {
		w := taskCloseRaw(t, f.issuerKey, original, domain)
		e, err := f.verifier.VerifyTaskUserQuiescenceTicket(w, f.issuerWire, original.Context, f.now)
		require.Error(t, err)
		require.Empty(t, e.Wire())
	}
	for _, key := range []ed25519.PrivateKey{nil, f.runtimeKey} {
		_, err := SignTaskUserQuiescenceTicket(key, f.issuer, original)
		require.Error(t, err)
	}
	valid := taskCloseRaw(t, f.issuerKey, original, "sandbox-task-quiesce-users-ticket:v1\x00")
	for _, wire := range [][]byte{append(bytes.Clone(valid), 'x'), bytes.Replace(valid, []byte(`"current":`), []byte(`"current":null,"current":`), 1), bytes.Replace(valid, []byte(`"version":1`), []byte(`"version":1,"extra":0`), 1), bytes.Repeat([]byte(" "), 8193)} {
		_, err := f.verifier.VerifyTaskUserQuiescenceTicket(wire, f.issuerWire, original.Context, f.now)
		require.Error(t, err)
	}
	c := original
	c.Context.Current.ClaimCreateRevision++
	c.Context.Current.ClaimID = c.Context.Current.CommandID
	c.Context.Current.WorkerID = "new"
	c.Context.Current.LeaseID++
	w, err := SignTaskUserQuiescenceTicket(f.issuerKey, f.issuer, c)
	require.NoError(t, err)
	_, err = f.verifier.VerifyTaskUserQuiescenceTicket(w, f.issuerWire, c.Context, f.now)
	require.NoError(t, err)
}
