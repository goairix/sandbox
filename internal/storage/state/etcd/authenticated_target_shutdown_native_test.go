//go:build linux && (amd64 || arm64)

package etcd

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
)

// Root sends ordinary SIGTERM to the exact owned production target only after
// the readiness marker. The backend neither signals nor controls the target.
func TestAuthenticatedTargetNativeShutdown(t *testing.T) {
	f := nativeExecutionSetup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, prepared := f.prepare(t, nativeRequest("hold", 60))
	h, err := f.b.DeliverExecEffect(ctx, prepared, f.destination)
	require.NoError(t, err)
	h.mu.Lock()
	reader := h.reader
	h.mu.Unlock()
	require.NotNil(t, reader)
	output := &nativeReadyWriter{ready: make(chan struct{})}
	type result struct {
		receipt p.LocalExecReceiptEvidence
		err     error
	}
	done := make(chan result, 1)
	go func() {
		defer close(done)
		receipt, err := h.Wait(ctx, output, io.Discard)
		done <- result{receipt, err}
	}()
	defer func() { cancel(); h.Close(); <-done }()
	select {
	case <-output.ready:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	snapshot := f.inspect(t, "PRODUCTION_SHUTDOWN_ACTIVE_COST")
	require.Equal(t, float64(1), snapshot["active"])
	require.Equal(t, "open", snapshot["gate"])
	processes, ok := snapshot["processes"].([]any)
	require.True(t, ok)
	rootPID := 0
	for _, process := range processes {
		actual, ok := process.(map[string]any)
		require.True(t, ok)
		if actual["comm"] == "user" {
			require.Zero(t, rootPID, "more than one actual user process")
			pid, ok := actual["pid"].(float64)
			require.True(t, ok)
			rootPID = int(pid)
		}
	}
	require.Greater(t, rootPID, 1)
	started := time.Now()
	fmt.Printf("ACTUAL_ACTIVE_SHUTDOWN_READY command=%s root_pid=%d current_boot=%s runtime_uid=%s\n", prepared.draft.commandID, rootPID, f.identity.Runtime.BootID, f.identity.Runtime.UID)
	select {
	case r := <-done:
		require.ErrorIs(t, r.err, ErrDeliveryUnknown)
		require.Empty(t, r.receipt.Wire(), "shutdown stream fabricated terminal receipt")
		require.Less(t, time.Since(started), 20*time.Second, "ordinary stop was not observed before signed expiry")
		t.Logf("ACTUAL_PRODUCTION_SHUTDOWN_STREAM_UNKNOWN command=%s elapsed=%s error=%v", prepared.draft.commandID, time.Since(started), r.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-reader.done:
	default:
		t.Fatal("shutdown reader did not join")
	}
	receipt, err := h.Wait(ctx, io.Discard, io.Discard)
	require.Error(t, err)
	require.Empty(t, receipt.Wire())
	require.Error(t, f.b.RenewExecDelivery(ctx, h))
	require.NoError(t, h.Close())
	require.Equal(t, prepared.Reference(), h.Reference())
	fmt.Printf("ACTUAL_PRODUCTION_SHUTDOWN_JOINED_UNKNOWN command=%s reader_joined=true caller_joined=true terminal_receipt=false\n", prepared.draft.commandID)
}
