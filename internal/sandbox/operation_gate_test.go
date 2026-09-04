package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOperationGateCloseRejectsNewOperationsAndDrainsReferences(t *testing.T) {
	gate := newOperationGate(true)
	release, err := gate.Acquire()
	require.NoError(t, err)

	drained := make(chan error, 1)
	go func() { drained <- gate.CloseAndWait(context.Background()) }()

	require.Eventually(t, func() bool {
		probeRelease, acquireErr := gate.Acquire()
		if acquireErr == nil {
			probeRelease()
		}
		return errors.Is(acquireErr, ErrSandboxNotReady)
	}, time.Second, time.Millisecond)
	select {
	case err := <-drained:
		t.Fatalf("gate drained while a reference was live: %v", err)
	default:
	}

	release()
	require.NoError(t, <-drained)
	_, err = gate.Acquire()
	require.ErrorIs(t, err, ErrSandboxNotReady)
}

func TestOperationGateExclusiveCanReopenOnlyItsGeneration(t *testing.T) {
	gate := newOperationGate(true)
	token, err := gate.BeginExclusive(context.Background())
	require.NoError(t, err)
	_, err = gate.Acquire()
	require.ErrorIs(t, err, ErrSandboxNotReady)
	require.NoError(t, token.Reopen())

	release, err := gate.Acquire()
	require.NoError(t, err)
	release()
	require.ErrorIs(t, token.Reopen(), ErrSandboxNotReady)

	token, err = gate.BeginExclusive(context.Background())
	require.NoError(t, err)
	token.Close()
	_, err = gate.Acquire()
	require.ErrorIs(t, err, ErrSandboxNotReady)
}

func TestOperationGateCancelledExclusiveReopensAdmission(t *testing.T) {
	gate := newOperationGate(true)
	release, err := gate.Acquire()
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = gate.BeginExclusive(ctx)
	require.ErrorIs(t, err, context.Canceled)
	release()

	nextRelease, err := gate.Acquire()
	require.NoError(t, err)
	nextRelease()
}

func TestOperationGateExclusiveCannotSucceedAfterPermanentCloseWinsDrain(t *testing.T) {
	gate := newOperationGate(true)
	release, err := gate.Acquire()
	require.NoError(t, err)

	exclusiveResult := make(chan error, 1)
	go func() {
		_, beginErr := gate.BeginExclusive(context.Background())
		exclusiveResult <- beginErr
	}()
	require.Eventually(t, func() bool {
		gate.mu.Lock()
		defer gate.mu.Unlock()
		return gate.exclusive
	}, time.Second, time.Millisecond)
	closeResult := make(chan error, 1)
	go func() { closeResult <- gate.CloseAndWait(context.Background()) }()
	require.Eventually(t, func() bool {
		gate.mu.Lock()
		defer gate.mu.Unlock()
		return gate.permanent
	}, time.Second, time.Millisecond)
	release()
	require.NoError(t, <-closeResult)
	require.ErrorIs(t, <-exclusiveResult, ErrSandboxNotReady)
}
