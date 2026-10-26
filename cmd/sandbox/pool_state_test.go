package main

import (
	"testing"

	"github.com/goairix/sandbox/internal/config"
	"github.com/goairix/sandbox/internal/sandbox"
	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/stretchr/testify/require"
)

// A zero warm-pool minimum must still expose existing shared inventory to
// release drain. It disables refill, not recovery or cleanup of old records.
func TestKubernetesPoolStateAvailableForDrainAtZeroMinimum(t *testing.T) {
	store := &poolStateStoreStub{}
	cfg := &config.Config{}
	cfg.Runtime.Type = "kubernetes"
	cfg.Runtime.Kubernetes.Namespace = "sandbox-fuse-etcd"
	cfg.Pool.MinSize = 0
	managerConfig := sandbox.ManagerConfig{}

	attachKubernetesPoolState(&managerConfig, cfg, store, "sandbox-fuse-etcd/sandbox")

	require.NotNil(t, managerConfig.PoolStateStore)
	require.Same(t, store, managerConfig.PoolStateStore)
	require.Equal(t, "sandbox-fuse-etcd/sandbox", managerConfig.PoolScope)
}

type poolStateStoreStub struct{ state.AtomicStore }
