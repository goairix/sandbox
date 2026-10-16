//go:build linux || darwin

package controltarget

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

// A blocking lock, persistence, or trusting copied handles breaks this query.
func TestJournalObserveGate(t *testing.T) {
	j, o := createJournalFixture(t)
	before, err := os.ReadFile(filepath.Join(o.Directory, "gate.json"))
	require.NoError(t, err)
	status := j.Status()
	gate, err := j.ObserveGate(context.Background())
	require.NoError(t, err)
	require.Equal(t, "closed", gate.GateState)
	require.Equal(t, int64(2), gate.DataGateEpoch)
	gate.Identity.SandboxID = "changed"
	again, err := j.ObserveGate(context.Background())
	require.NoError(t, err)
	require.Equal(t, o.Identity, again.Identity)
	j.mu.Lock()
	_, err = j.ObserveGate(context.Background())
	j.mu.Unlock()
	require.ErrorIs(t, err, ErrBusy)
	after, err := os.ReadFile(filepath.Join(o.Directory, "gate.json"))
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, status, j.Status())
	var copy Journal
	reflect.ValueOf(&copy).Elem().Set(reflect.ValueOf(j).Elem())
	for _, invalid := range []*Journal{nil, {}, &copy} {
		_, err = invalid.ObserveGate(context.Background())
		require.Error(t, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = j.ObserveGate(ctx)
	require.ErrorIs(t, err, context.Canceled)
	j.mu.Lock()
	j.poisoned = true
	j.mu.Unlock()
	_, err = j.ObserveGate(context.Background())
	require.ErrorIs(t, err, ErrJournalUnavailable)
	require.NoError(t, j.Close())
	_, err = j.ObserveGate(context.Background())
	require.ErrorIs(t, err, ErrClosed)
}
