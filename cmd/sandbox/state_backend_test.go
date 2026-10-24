package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/config"
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

func TestOpenEtcdStateBackendWithMutualTLS(t *testing.T) {
	endpoint := os.Getenv("TEST_ETCD_TLS_ENDPOINT")
	certificateDirectory := os.Getenv("TEST_ETCD_TLS_DIRECTORY")
	if endpoint == "" || certificateDirectory == "" {
		t.Skip("TEST_ETCD_TLS_ENDPOINT and TEST_ETCD_TLS_DIRECTORY are not set")
	}
	ctx := context.Background()
	configuration := &config.Config{}
	configuration.Storage.State.Backend = "etcd"
	configuration.Storage.State.Etcd = config.EtcdConfig{
		Endpoints: []string{endpoint}, Prefix: "/sandbox-mtls-test/" + uuid.NewString() + "/",
		TimeoutMS: 3000,
		CAFile:    certificateDirectory + "/ca.crt",
		CertFile:  certificateDirectory + "/client.crt",
		KeyFile:   certificateDirectory + "/client.key",
	}
	backend, err := openStateBackend(ctx, configuration)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backend.close()) })
	require.Equal(t, "etcd", backend.kind)
	require.NoError(t, backend.store.Set(ctx, "startup", []byte("ready"), 0))
	value, err := backend.store.Get(ctx, "startup")
	require.NoError(t, err)
	require.Equal(t, []byte("ready"), value)

	// The server must reject a client that trusts its CA but presents no
	// identity. This checks server enforcement, beyond parsing the PEM files.
	options, err := etcdOptionsFromConfig(configuration.Storage.State.Etcd)
	require.NoError(t, err)
	options.TLS.Certificates = nil
	unauthenticated, err := etcdstate.New(ctx, options)
	if unauthenticated != nil {
		t.Cleanup(func() { require.NoError(t, unauthenticated.Close()) })
	}
	require.Error(t, err)
}

func TestOpenEtcdStateBackendWithoutAuthentication(t *testing.T) {
	endpoint := os.Getenv("TEST_ETCD_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_ETCD_ENDPOINT is not set")
	}
	ctx := context.Background()
	configuration := &config.Config{}
	configuration.Storage.State.Backend = "etcd"
	configuration.Storage.State.Etcd = config.EtcdConfig{
		Endpoints: []string{endpoint}, Prefix: "/sandbox-plain-test/" + uuid.NewString() + "/",
		TimeoutMS: 3000,
	}
	backend, err := openStateBackend(ctx, configuration)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backend.close()) })
	require.NoError(t, backend.store.Set(ctx, "startup", []byte("ready"), 0))
	value, err := backend.store.Get(ctx, "startup")
	require.NoError(t, err)
	require.Equal(t, []byte("ready"), value)
}
