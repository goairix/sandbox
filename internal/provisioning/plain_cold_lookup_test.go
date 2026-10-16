package provisioning

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPlainColdLookupAdmission(t *testing.T) {
	var missing *PlainColdService
	result, err := missing.Lookup(context.Background(), "sandbox")
	require.ErrorIs(t, err, ErrPlainColdProfile)
	require.Nil(t, result)
	s, _, _ := coldBusinessFixture(t)
	s.closed = true
	result, err = s.Lookup(context.Background(), "sandbox")
	require.Error(t, err)
	require.Nil(t, result)
	require.Empty(t, s.active)
	s.closed = false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err = s.Lookup(ctx, "sandbox")
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, result)
	require.Empty(t, s.active)
}

// Uncertainty is a fixed worst-case second, including after a successful query.
func TestPlainColdLookupTTLBoundary(t *testing.T) {
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	require.ErrorIs(t, checkPlainColdLookupTTL(now, now.Add(time.Second)), ErrPlainColdUnavailable)
	require.ErrorIs(t, checkPlainColdLookupTTL(now, now), ErrPlainColdUnavailable)
	require.NoError(t, checkPlainColdLookupTTL(now, now.Add(time.Second+time.Nanosecond)))
}
