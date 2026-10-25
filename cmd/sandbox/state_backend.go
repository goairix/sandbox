package main

import (
	"context"
	"fmt"

	"github.com/goairix/sandbox/internal/config"
	"github.com/goairix/sandbox/internal/storage/state"
	etcdstate "github.com/goairix/sandbox/internal/storage/state/etcd"
	redisstate "github.com/goairix/sandbox/internal/storage/state/redis"
	"github.com/goairix/sandbox/internal/telemetry/metrics"
)

// stateBackend keeps every production state consumer on one backend. An etcd
// deployment must never use Redis for a secondary owner, pool, or session key.
type stateBackend struct {
	store            state.AtomicStore
	activeRepository func(string) (state.ActiveSandboxRepository, error)
	fuseRepository   state.FUSEPoolRepository
	close            func() error
	kind             string
}

func etcdStateBackend(store *etcdstate.Store) stateBackend {
	return stateBackend{
		store: store,
		activeRepository: func(scope string) (state.ActiveSandboxRepository, error) {
			return etcdstate.NewActiveSandboxRepository(store, scope)
		},
		fuseRepository: etcdstate.NewFUSEPoolRepository(store),
		close:          store.Close,
		kind:           "etcd",
	}
}

func redisStateBackend(store *redisstate.Store) stateBackend {
	return stateBackend{
		store: store,
		activeRepository: func(scope string) (state.ActiveSandboxRepository, error) {
			return redisstate.NewActiveSandboxRepository(store, scope)
		},
		fuseRepository: redisstate.NewFUSEPoolRepository(store),
		close:          store.Close,
		kind:           "redis",
	}
}

// openStateBackend creates exactly one backend client after configuration and
// any Redis-specific initialization gate have been checked by the caller.
func openStateBackend(ctx context.Context, cfg *config.Config) (stateBackend, error) {
	switch cfg.Storage.State.Backend {
	case "etcd":
		options, err := etcdOptionsFromConfig(cfg.Storage.State.Etcd)
		if err != nil {
			return stateBackend{}, err
		}
		options.ControllerStageObserver = metrics.RecordControllerStage
		store, err := etcdstate.New(ctx, options)
		if err != nil {
			return stateBackend{}, fmt.Errorf("open etcd state: %w", err)
		}
		return etcdStateBackend(store), nil
	case "", "redis":
		store, err := redisstate.New(ctx, redisOptionsFromConfig(cfg.Storage.State.Redis))
		if err != nil {
			return stateBackend{}, fmt.Errorf("open Redis state: %w", err)
		}
		return redisStateBackend(store), nil
	default:
		return stateBackend{}, fmt.Errorf("unsupported state backend %q", cfg.Storage.State.Backend)
	}
}
