package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"strings"
	"sync"
	"testing"
	"time"
)

func taskReceiptClaims(f taskCloseFixture) TaskDataClosedReceiptClaims {
	return TaskDataClosedReceiptClaims{Version: 1, State: "data_closed", Context: f.ticket.Context, TicketDigest: f.ticket.Context.TaskDigest, NotBefore: f.ticket.NotBefore, NotAfter: f.ticket.NotAfter}
}
func taskReceiptVerify(f taskCloseFixture, w []byte, c TaskDataClosedReceiptClaims, now time.Time) (TaskDataClosedReceiptEvidence, error) {
	return f.verifier.VerifyTaskDataClosedReceipt(w, f.runtimeWire, c.Context, c.TicketDigest, c.NotBefore, c.NotAfter, now)
}
func taskReceiptReject(t *testing.T, f taskCloseFixture, w []byte, c TaskDataClosedReceiptClaims, now time.Time) {
	t.Helper()
	e, err := taskReceiptVerify(f, w, c, now)
	require.Error(t, err)
	require.Equal(t, TaskDataClosedReceiptEvidence{}, e)
}
func TestTaskDataClosedReceipt(t *testing.T) {
	f := taskCloseSetup(t)
	c := taskReceiptClaims(f)
	raw := taskCloseRaw(t, f.runtimeKey, c, taskTestReceiptDomain)
	w, err := SignTaskDataClosedReceipt(f.runtimeKey, c)
	require.NoError(t, err)
	require.Equal(t, raw, w)
	e, err := taskReceiptVerify(f, w, c, f.now.Add(40*time.Second))
	require.NoError(t, err)
	require.Equal(t, c.Context, e.Context())
	require.Equal(t, c.TicketDigest, e.TicketDigest())
	require.Equal(t, "data_closed", e.State())
	require.Equal(t, c.NotBefore, e.NotBefore())
	require.Equal(t, c.NotAfter, e.NotAfter())
	pretty := new(bytes.Buffer)
	require.NoError(t, json.Indent(pretty, raw, "", " "))
	normalized, err := taskReceiptVerify(f, pretty.Bytes(), c, f.now)
	require.NoError(t, err)
	require.Equal(t, raw, normalized.Wire())
	c.NotAfter = c.NotBefore.Add(30 * time.Second)
	w, err = SignTaskDataClosedReceipt(f.runtimeKey, c)
	require.NoError(t, err)
	_, err = taskReceiptVerify(f, w, c, f.now)
	require.NoError(t, err)
}
func TestTaskDataClosedReceiptRejects(t *testing.T) {
	f := taskCloseSetup(t)
	c := taskReceiptClaims(f)
	valid := taskCloseRaw(t, f.runtimeKey, c, taskTestReceiptDomain)
	for name, mutate := range taskContextChanges() {
		t.Run("context-"+name, func(t *testing.T) {
			bad := c
			mutate(&bad.Context)
			taskReceiptReject(t, f, valid, bad, f.now)
			taskReceiptReject(t, f, taskCloseRaw(t, f.runtimeKey, bad, taskTestReceiptDomain), c, f.now)
		})
	}
	for name, mutate := range taskInvalidContexts() {
		t.Run("invalid-"+name, func(t *testing.T) {
			bad := c
			mutate(&bad.Context)
			w, e := SignTaskDataClosedReceipt(f.runtimeKey, bad)
			require.Error(t, e)
			require.Nil(t, w)
			taskReceiptReject(t, f, taskCloseRaw(t, f.runtimeKey, bad, taskTestReceiptDomain), bad, f.now)
		})
	}
	for name, times := range taskBadTimes(c.NotBefore, c.NotAfter) {
		t.Run(name, func(t *testing.T) {
			bad := c
			bad.NotBefore, bad.NotAfter = times[0], times[1]
			w, e := SignTaskDataClosedReceipt(f.runtimeKey, bad)
			require.Error(t, e)
			require.Nil(t, w)
			taskReceiptReject(t, f, taskCloseRaw(t, f.runtimeKey, bad, taskTestReceiptDomain), bad, f.now)
		})
	}
	for _, mutate := range []func(*TaskDataClosedReceiptClaims){func(c *TaskDataClosedReceiptClaims) { c.Version = 0 }, func(c *TaskDataClosedReceiptClaims) { c.State = "pending" }, func(c *TaskDataClosedReceiptClaims) { c.State = "drained" }, func(c *TaskDataClosedReceiptClaims) { c.TicketDigest = "" }, func(c *TaskDataClosedReceiptClaims) { c.TicketDigest = strings.Repeat("A", 64) }} {
		bad := c
		mutate(&bad)
		w, e := SignTaskDataClosedReceipt(f.runtimeKey, bad)
		require.Error(t, e)
		require.Nil(t, w)
		taskReceiptReject(t, f, taskCloseRaw(t, f.runtimeKey, bad, taskTestReceiptDomain), bad, f.now)
	}
	for _, mutate := range []func(*TaskDataClosedReceiptClaims){func(c *TaskDataClosedReceiptClaims) { c.TicketDigest = strings.Repeat("f", 64) }, func(c *TaskDataClosedReceiptClaims) { c.NotBefore = c.NotBefore.Add(time.Nanosecond) }, func(c *TaskDataClosedReceiptClaims) { c.NotAfter = c.NotAfter.Add(-time.Nanosecond) }} {
		bad := c
		mutate(&bad)
		taskReceiptReject(t, f, valid, bad, f.now)
		taskReceiptReject(t, f, taskCloseRaw(t, f.runtimeKey, bad, taskTestReceiptDomain), c, f.now)
	}
	for _, domain := range []string{"sandbox-task-data-closed-receipt:v1", `sandbox-task-data-closed-receipt:v1\x00`, taskTestTicketDomain, "sandbox-local-exec-receipt:v1\x00"} {
		taskReceiptReject(t, f, taskCloseRaw(t, f.runtimeKey, c, domain), c, f.now)
	}
	var cert runtimeIdentityEnvelope
	require.NoError(t, json.Unmarshal(f.runtimeWire, &cert))
	for name, mutate := range map[string]func(*RuntimeIdentityCertificateClaims){"expired": func(c *RuntimeIdentityCertificateClaims) { c.NotAfter = f.now.Add(time.Second) }, "before-ticket": func(x *RuntimeIdentityCertificateClaims) { x.NotBefore = c.NotBefore.Add(time.Nanosecond) }, "after-ticket": func(x *RuntimeIdentityCertificateClaims) { x.NotAfter = c.NotAfter.Add(-time.Nanosecond) }, "wrong-boot": func(x *RuntimeIdentityCertificateClaims) { x.Runtime.BootID = "other" }} {
		t.Run("runtime-"+name, func(t *testing.T) {
			x := cert.Claims
			mutate(&x)
			g := f
			w, e := SignRuntimeIdentityCertificate(f.root, x)
			require.NoError(t, e)
			g.runtimeWire = w
			taskReceiptReject(t, g, valid, c, f.now)
		})
	}
	x := cert.Claims
	x.Role = "command_issuer"
	g := f
	g.runtimeWire = runtimeIdentityRawWire(t, f.root, x, "sandbox-runtime-identity-certificate:v1\x00")
	taskReceiptReject(t, g, valid, c, f.now)
	g = f
	g.runtimeWire = f.issuerWire
	taskReceiptReject(t, g, valid, c, f.now)
	otherRoot, _ := managementTestKey(t)
	g = f
	var err error
	g.verifier, err = NewManagementVerifier(f.claims.Binding, []ed25519.PublicKey{otherRoot})
	require.NoError(t, err)
	taskReceiptReject(t, g, valid, c, f.now)
	for _, v := range []*ManagementVerifier{nil, {}} {
		g = f
		g.verifier = v
		taskReceiptReject(t, g, valid, c, f.now)
	}
	for _, now := range []time.Time{{}, f.now.In(time.FixedZone("offset", 3600)), cert.Claims.NotBefore.Add(time.Second - time.Nanosecond), cert.Claims.NotAfter.Add(-time.Second)} {
		taskReceiptReject(t, f, valid, c, now)
	}
	malformed := bytes.Clone(f.runtimeKey)
	malformed[63] ^= 1
	for _, key := range []ed25519.PrivateKey{nil, f.runtimeKey[:32], append(bytes.Clone(f.runtimeKey), 0), malformed} {
		w, e := SignTaskDataClosedReceipt(key, c)
		require.Error(t, e)
		require.Nil(t, w)
	}
	f = taskCloseSetup(t)
	c = taskReceiptClaims(f)
	raw := taskCloseRaw(t, f.runtimeKey, c, taskTestReceiptDomain)
	taskReceiptReject(t, f, raw, c, f.now.Add(2*time.Hour))
	bad := c
	bad.Context.Runtime.BootID = "wrong"
	taskReceiptReject(t, f, raw, bad, f.now)
	taskReceiptReject(t, f, taskCloseRaw(t, f.issuerKey, c, taskTestReceiptDomain), c, f.now)
	taskReceiptReject(t, f, taskCloseRaw(t, f.runtimeKey, c, taskTestTicketDomain), c, f.now)
}
func TestTaskDataClosedReceiptStrict(t *testing.T) {
	f := taskCloseSetup(t)
	c := taskReceiptClaims(f)
	valid := taskCloseRaw(t, f.runtimeKey, c, taskTestReceiptDomain)
	taskStrictCases(t, valid, func(t *testing.T, w []byte) { taskReceiptReject(t, f, w, c, f.now) })
	for _, field := range []string{"drain_confirmed", "remote_settled", "owner_released", "terminal"} {
		taskReceiptReject(t, f, runtimeIdentityMutate(t, valid, []string{"claims", field}, json.RawMessage("true"), false), c, f.now)
	}
	var cert runtimeIdentityEnvelope
	require.NoError(t, json.Unmarshal(f.runtimeWire, &cert))
	c.Context.Runtime.UID = "replacement-�"
	cert.Claims.Runtime.UID = c.Context.Runtime.UID
	var err error
	f.runtimeWire, err = SignRuntimeIdentityCertificate(f.root, cert.Claims)
	require.NoError(t, err)
	w := taskCloseRaw(t, f.runtimeKey, c, taskTestReceiptDomain)
	_, err = taskReceiptVerify(f, w, c, f.now)
	require.NoError(t, err)
	for _, bad := range [][]byte{[]byte(`\ud800`), []byte(`\udc00`), {255}} {
		taskReceiptReject(t, f, bytes.Replace(w, []byte("�"), bad, 1), c, f.now)
	}
	f = taskCloseSetup(t)
	c = taskReceiptClaims(f)
	raw := taskCloseRaw(t, f.runtimeKey, c, taskTestReceiptDomain)
	padded := append(bytes.Clone(raw), bytes.Repeat([]byte(" "), 8192-len(raw))...)
	e, err := taskReceiptVerify(f, padded, c, f.now)
	require.NoError(t, err)
	require.Equal(t, raw, e.Wire())
	taskReceiptReject(t, f, append(padded, ' '), c, f.now)
}
func TestTaskDataClosedReceiptCopies(t *testing.T) {
	f := taskCloseSetup(t)
	c := taskReceiptClaims(f)
	raw := taskCloseRaw(t, f.runtimeKey, c, taskTestReceiptDomain)
	e, err := taskReceiptVerify(f, raw, c, f.now)
	require.NoError(t, err)
	original := bytes.Clone(raw)
	clear(raw)
	clear(e.Wire())
	clear(f.runtimeWire)
	clear(f.runtimeKey)
	x := e.Context()
	x.WorkerID = "changed"
	require.Equal(t, original, e.Wire())
	require.Equal(t, c.Context, e.Context())
	var wg sync.WaitGroup
	for j := 0; j < 4; j++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 10; k++ {
				clear(e.Wire())
				if !bytes.Equal(original, e.Wire()) {
					t.Error("receipt evidence aliased")
				}
			}
		}()
	}
	wg.Wait()
}
