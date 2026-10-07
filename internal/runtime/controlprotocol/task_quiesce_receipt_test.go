package controlprotocol

import (
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"time"
)

func TestTaskQuiesceReceipt(t *testing.T) {
	f, c := quiesceSetup(t)
	d := strings.Repeat("e", 64)
	a := TaskUserQuiescenceAcceptedClaims{Version: 1, State: "quiescence_accepted", Context: c.Context, TicketDigest: d, NotBefore: c.NotBefore, NotAfter: c.NotAfter}
	aw := taskCloseRaw(t, f.runtimeKey, a, "sandbox-task-quiesce-users-accepted:v1\x00")
	signed, err := SignTaskUserQuiescenceAccepted(f.runtimeKey, a)
	require.NoError(t, err)
	require.Equal(t, aw, signed)
	ae, err := f.verifier.VerifyTaskUserQuiescenceAccepted(aw, f.runtimeWire, c.Context, d, c.NotBefore, c.NotAfter, f.now)
	require.NoError(t, err)
	require.Equal(t, "quiescence_accepted", ae.State())
	require.Equal(t, c.Context, ae.Context())
	r := TaskUserQuiescenceReceiptClaims{Version: 1, State: "users_quiesced", Context: c.Context, TicketDigest: d, NotBefore: c.NotBefore, NotAfter: c.NotAfter, ExecutionSetDigest: strings.Repeat("f", 64), RegisteredCount: 2, NeverSpawnedCount: 1, LocalTerminalCount: 1}
	rw := taskCloseRaw(t, f.runtimeKey, r, "sandbox-task-users-quiesced-receipt:v1\x00")
	signed, err = SignTaskUserQuiescenceReceipt(f.runtimeKey, r)
	require.NoError(t, err)
	require.Equal(t, rw, signed)
	re, err := f.verifier.VerifyTaskUserQuiescenceReceipt(rw, f.runtimeWire, c.Context, d, c.NotBefore, c.NotAfter, f.now.Add(25*time.Second))
	require.NoError(t, err)
	require.Equal(t, "users_quiesced", re.State())
	require.Equal(t, uint32(2), re.RegisteredCount())
	require.Equal(t, uint32(1), re.NeverSpawnedCount())
	require.Equal(t, uint32(1), re.LocalTerminalCount())
	clear(re.Wire())
	require.Equal(t, rw, re.Wire())
	clear(ae.Wire())
	require.Equal(t, aw, ae.Wire())
	_, err = f.verifier.VerifyTaskUserQuiescenceReceipt(aw, f.runtimeWire, c.Context, d, c.NotBefore, c.NotAfter, f.now)
	require.Error(t, err)
	_, err = f.verifier.VerifyTaskUserQuiescenceAccepted(rw, f.runtimeWire, c.Context, d, c.NotBefore, c.NotAfter, f.now)
	require.Error(t, err)
	for _, count := range []uint32{3, 65, ^uint32(0)} {
		r.RegisteredCount = count
		_, err = SignTaskUserQuiescenceReceipt(f.runtimeKey, r)
		require.Error(t, err)
		w := taskCloseRaw(t, f.runtimeKey, r, "sandbox-task-users-quiesced-receipt:v1\x00")
		e, err := f.verifier.VerifyTaskUserQuiescenceReceipt(w, f.runtimeWire, c.Context, d, c.NotBefore, c.NotAfter, f.now)
		require.Error(t, err)
		require.Equal(t, TaskUserQuiescenceReceiptEvidence{}, e)
	}
	for _, bad := range []string{"", strings.Repeat("0", 64)} {
		_, err = f.verifier.VerifyTaskUserQuiescenceAccepted(aw, f.runtimeWire, c.Context, bad, c.NotBefore, c.NotAfter, f.now)
		require.Error(t, err)
	}
}
