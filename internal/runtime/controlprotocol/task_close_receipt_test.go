package controlprotocol

import (
	"bytes"
	"github.com/stretchr/testify/require"
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
}
func TestTaskDataClosedReceiptRejects(t *testing.T) {
	f := taskCloseSetup(t)
	c := taskReceiptClaims(f)
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
}
