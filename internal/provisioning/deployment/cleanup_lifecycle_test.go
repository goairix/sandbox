package deployment

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLocalCleanupLifecycleUnopened(t *testing.T) {
	// An absent deployment must never advertise a completion signal or make
	// callers mistake an unstarted worker for successful physical cleanup.
	for name, d := range map[string]*LocalCleanupDeployment{
		"nil":      nil,
		"unopened": {},
	} {
		t.Run(name, func(t *testing.T) {
			require.Nil(t, d.Done())
			require.ErrorIs(t, d.Wait(context.Background()), errConfiguration)
			require.ErrorIs(t, d.Wait(nil), errConfiguration)
		})
	}
}

func TestLocalCleanupLifecycleCanceledObservation(t *testing.T) {
	// Caller cancellation is an observation result, never an instruction to
	// stop or close the deployment. An unopened value has no worker to invent.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := &LocalCleanupDeployment{}
	require.ErrorIs(t, d.Wait(ctx), context.Canceled)
	require.Nil(t, d.Done())
	require.False(t, d.closed)
}
