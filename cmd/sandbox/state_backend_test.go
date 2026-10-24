package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	etcdstate "github.com/goairix/sandbox/internal/storage/state/etcd"
)

func TestEtcdStateBackendBindsAllRepositories(t *testing.T) {
	endpoint := os.Getenv("TEST_ETCD_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_ETCD_ENDPOINT is not set")
	}
	ctx := context.Background()
	store, err := etcdstate.New(ctx, etcdstate.Options{
		Endpoints: []string{endpoint}, Prefix: "/sandbox-backend-test/" + uuid.NewString() + "/",
		Timeout: 3 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	backend := etcdStateBackend(store)
	require.Same(t, store, backend.store)
	active, err := backend.activeRepository("namespace-a")
	require.NoError(t, err)
	require.IsType(t, &etcdstate.ActiveSandboxRepository{}, active)
	require.IsType(t, &etcdstate.FUSEPoolRepository{}, backend.fuseRepository)
	require.NoError(t, backend.store.Set(ctx, "session:one", []byte("present"), 0))
	stored, err := backend.store.Get(ctx, "session:one")
	require.NoError(t, err)
	require.Equal(t, []byte("present"), stored)
	keys, err := backend.fuseRepository.ListPoolKeys(ctx)
	require.NoError(t, err)
	require.Empty(t, keys)
}
