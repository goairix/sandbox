package sandbox

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/runtime"
)

type saturatedReadinessRuntime struct {
	runtime.Runtime
	pingCalls int
}

func (r *saturatedReadinessRuntime) Ping(context.Context) error {
	r.pingCalls++
	return errors.New("kubernetes client rate limiter is saturated")
}

func TestManagerReadyKeepsServingWhenRuntimeControlPlaneIsSaturated(t *testing.T) {
	rt := &saturatedReadinessRuntime{}
	mgr := NewManager(rt, nil, nil, ManagerConfig{})

	// A Kubernetes list delay must not withdraw every API endpoint. Runtime
	// operations still check Kubernetes and report their own bounded errors.
	require.NoError(t, mgr.Ready(context.Background()))
	require.Zero(t, rt.pingCalls)
}
