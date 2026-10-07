package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const taskTestTicketDomain = "sandbox-task-close-data-ticket:v1\x00"
const taskTestReceiptDomain = "sandbox-task-data-closed-receipt:v1\x00"

type taskCloseFixture struct {
	activationFixture
	issuer CommandIssuerIdentity
	ticket TaskCloseDataTicketClaims
}

func taskCloseSetup(t *testing.T) taskCloseFixture {
	t.Helper()
	f := newActivationFixture(t)
	issuer, e := f.verifier.VerifyCommandIssuerCertificate(f.issuerWire, f.now)
	require.NoError(t, e)
	b, i := f.claims.Binding, f.claims.Identity
	c := TaskCloseDataContext{Namespace: b.Namespace, AuthorityID: b.AuthorityID, Target: b.Target, RestoreEpoch: b.RestoreEpoch, IssuerCertificateID: issuer.CertificateID(), IssuerCertificateDigest: issuer.Digest(), CommandID: "8a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", TaskID: "9a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", TaskDigest: strings.Repeat("b", 64), ClaimID: "aa1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", WorkerID: "worker-1", SandboxID: i.SandboxID, WorkspaceHash: i.WorkspaceHash, Generation: i.Generation, DataGateEpoch: 3, ControlRevision: 10, ClaimCreateRevision: 11, LeaseID: 12, Runtime: i.Runtime}
	return taskCloseFixture{f, issuer, TaskCloseDataTicketClaims{Version: 1, Purpose: "task_close_data", Context: c, NotBefore: f.now.Add(-2 * time.Second), NotAfter: f.now.Add(20 * time.Second)}}
}

// Independent standard-library signatures bypass every production signer and
// schema validator, so invalid signed claims test the verifier itself.
func taskCloseRaw(t *testing.T, key ed25519.PrivateKey, c any, domain string) []byte {
	t.Helper()
	payload, e := json.Marshal(c)
	require.NoError(t, e)
	wire, e := json.Marshal(struct {
		Claims    json.RawMessage `json:"claims"`
		Signature []byte          `json:"signature"`
	}{payload, ed25519.Sign(key, append([]byte(domain), payload...))})
	require.NoError(t, e)
	return wire
}
func taskTicketReject(t *testing.T, v *ManagementVerifier, w, issuer []byte, c TaskCloseDataContext, now time.Time) {
	t.Helper()
	e, err := v.VerifyTaskCloseDataTicket(w, issuer, c, now)
	require.Error(t, err)
	require.Equal(t, TaskCloseDataEvidence{}, e)
}
func taskTicketSignReject(t *testing.T, key ed25519.PrivateKey, i CommandIssuerIdentity, c TaskCloseDataTicketClaims) {
	t.Helper()
	w, e := SignTaskCloseDataTicket(key, i, c)
	require.Error(t, e)
	require.Nil(t, w)
}

func TestTaskCloseDataTicket(t *testing.T) {
	f := taskCloseSetup(t)
	raw := taskCloseRaw(t, f.issuerKey, f.ticket, taskTestTicketDomain)
	e, err := f.verifier.VerifyTaskCloseDataTicket(raw, f.issuerWire, f.ticket.Context, f.now)
	require.NoError(t, err)
	wire, err := SignTaskCloseDataTicket(f.issuerKey, f.issuer, f.ticket)
	require.NoError(t, err)
	require.Equal(t, raw, wire)
	sum := sha256.Sum256(raw)
	require.Equal(t, hex.EncodeToString(sum[:]), e.Digest())
	require.Equal(t, f.ticket.Context, e.Context())
	require.Equal(t, f.ticket.NotBefore, e.NotBefore())
	require.Equal(t, f.ticket.NotAfter, e.NotAfter())
	for _, now := range []time.Time{f.ticket.NotBefore.Add(time.Second), f.ticket.NotAfter.Add(-time.Second - time.Nanosecond)} {
		_, err = f.verifier.VerifyTaskCloseDataTicket(raw, f.issuerWire, f.ticket.Context, now)
		require.NoError(t, err)
	}
	c := f.ticket
	c.NotAfter = c.NotBefore.Add(30 * time.Second)
	_, err = SignTaskCloseDataTicket(f.issuerKey, f.issuer, c)
	require.NoError(t, err)
}
func TestTaskCloseDataTicketRejects(t *testing.T) {
	f := taskCloseSetup(t)
	raw := taskCloseRaw(t, f.issuerKey, f.ticket, taskTestTicketDomain)
	for _, domain := range []string{"sandbox-task-close-data-ticket:v1", `sandbox-task-close-data-ticket:v1\x00`, taskTestReceiptDomain, "sandbox-exec-start-ticket:v1\x00"} {
		taskTicketReject(t, f.verifier, taskCloseRaw(t, f.issuerKey, f.ticket, domain), f.issuerWire, f.ticket.Context, f.now)
	}
	for _, now := range []time.Time{{}, f.now.In(time.FixedZone("offset", 3600)), f.ticket.NotBefore.Add(time.Second - time.Nanosecond), f.ticket.NotAfter.Add(-time.Second)} {
		taskTicketReject(t, f.verifier, raw, f.issuerWire, f.ticket.Context, now)
	}
	for _, v := range []*ManagementVerifier{nil, {}} {
		taskTicketReject(t, v, raw, f.issuerWire, f.ticket.Context, f.now)
	}
}
func TestTaskCloseDataTicketStrict(t *testing.T) {
	f := taskCloseSetup(t)
	raw := taskCloseRaw(t, f.issuerKey, f.ticket, taskTestTicketDomain)
	padded := append(bytes.Clone(raw), bytes.Repeat([]byte(" "), 4096-len(raw))...)
	e, err := f.verifier.VerifyTaskCloseDataTicket(padded, f.issuerWire, f.ticket.Context, f.now)
	require.NoError(t, err)
	require.Equal(t, raw, e.Wire())
	taskTicketReject(t, f.verifier, append(padded, ' '), f.issuerWire, f.ticket.Context, f.now)
}
func TestTaskCloseDataTicketCopies(t *testing.T) {
	f := taskCloseSetup(t)
	w, err := SignTaskCloseDataTicket(f.issuerKey, f.issuer, f.ticket)
	require.NoError(t, err)
	original := bytes.Clone(w)
	e, err := f.verifier.VerifyTaskCloseDataTicket(w, f.issuerWire, f.ticket.Context, f.now)
	require.NoError(t, err)
	clear(w)
	clear(f.issuerKey)
	clear(f.issuerWire)
	clear(f.issuer.PublicKey())
	clear(f.issuer.Wire())
	clear(e.Wire())
	c := e.Context()
	c.Runtime.BootID = "changed"
	require.Equal(t, original, e.Wire())
	require.Equal(t, f.ticket.Context, e.Context())
	var wg sync.WaitGroup
	for j := 0; j < 4; j++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 10; k++ {
				clear(e.Wire())
				if !bytes.Equal(original, e.Wire()) {
					t.Error("evidence wire aliased")
				}
			}
		}()
	}
	wg.Wait()
}
