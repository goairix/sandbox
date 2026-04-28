package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"time"

	"github.com/goairix/sandbox/internal/api"
	"github.com/goairix/sandbox/internal/api/handler"
	"github.com/goairix/sandbox/internal/config"
	"github.com/goairix/sandbox/internal/logger"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/runtime/docker"
	k8sruntime "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/goairix/sandbox/internal/sandbox"
	"github.com/goairix/sandbox/internal/storage"
	redisstate "github.com/goairix/sandbox/internal/storage/state/redis"
	"github.com/goairix/sandbox/internal/telemetry"
	telemetrylog "github.com/goairix/sandbox/internal/telemetry/log"
	"github.com/goairix/sandbox/internal/telemetry/metrics"
	"github.com/goairix/sandbox/internal/telemetry/trace"
)

func main() {
	defer func() {
		if r := recover(); r != nil {
			if logger.ZapLogger() != nil {
				logger.Error(context.Background(), "panic: process crashed",
					logger.AddField("panic", r),
				)
			} else {
				log.Printf("panic: process crashed: %v", r)
			}
		}
	}()

	configPath := flag.String("config", "", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	// Initialize telemetry
	if err = telemetry.Init(cfg); err != nil {
		log.Fatalf("failed to init telemetry resource: %v", err)
	}
	if err = trace.Init(cfg); err != nil {
		log.Fatalf("failed to init tracer: %v", err)
	}
	if err = metrics.Init(cfg); err != nil {
		log.Fatalf("failed to init metrics: %v", err)
	}
	if err = telemetrylog.Init(cfg); err != nil {
		log.Fatalf("failed to init log exporter: %v", err)
	}
	if err = logger.Init(cfg); err != nil {
		log.Fatalf("failed to init logger: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize runtime
	var rt runtime.Runtime
	switch cfg.Runtime.Type {
	case "docker":
		rt, err = docker.New(ctx, cfg.Runtime.Docker.Host, cfg.Images.Gateway)
		if err != nil {
			log.Fatalf("failed to create docker runtime: %v", err)
		}
	case "kubernetes":
		rt, err = k8sruntime.New(cfg.Runtime.Kubernetes.Kubeconfig, cfg.Runtime.Kubernetes.Namespace)
		if err != nil {
			log.Fatalf("failed to create kubernetes runtime: %v", err)
		}
	default:
		log.Fatalf("unknown runtime type: %s", cfg.Runtime.Type)
	}

	// Initialize filesystem
	fsys, fsMeta, err := storage.NewFileSystem(cfg.Storage.FileSystem)
	if err != nil {
		log.Fatalf("failed to create filesystem: %v", err)
	}

	// Build pool config
	sandboxImage := cfg.Images.Sandbox
	if sandboxImage == "" {
		sandboxImage = "sandbox:latest"
	}

	mgr := sandbox.NewManager(rt, fsys, fsMeta, sandbox.ManagerConfig{
		PoolConfig: sandbox.PoolConfig{
			MinSize: cfg.Pool.MinSize,
			MaxSize: cfg.Pool.MaxSize,
			Image:   sandboxImage,
		},
		DefaultTimeout:          cfg.Security.SandboxTimeoutSeconds,
		AutoSyncIntervalSeconds: cfg.Workspace.AutoSyncIntervalSeconds,
	})

	// Initialize session store for persistent sandbox state
	if cfg.Storage.State.Redis.Addr != "" {
		store, storeErr := redisstate.New(ctx, redisstate.Options{
			Addr:     cfg.Storage.State.Redis.Addr,
			Password: cfg.Storage.State.Redis.Password,
			DB:       cfg.Storage.State.Redis.DB,
		})
		if storeErr != nil {
			log.Fatalf("failed to create redis state store: %v", storeErr)
		}
		ttl := time.Duration(cfg.Security.SandboxTimeoutSeconds) * time.Second
		mgr.SetSessionStore(sandbox.NewSessionStore(store, ttl))
		log.Printf("session store connected to redis at %s", cfg.Storage.State.Redis.Addr)
	}

	mgr.Start(ctx)

	h := handler.NewHandler(mgr)
	router := api.SetupRouter(h, cfg.Security.APIKey, cfg.Security.RateLimit, cfg.Telemetry.ServiceName)
	server := api.NewServer(router, cfg.Server.Host, cfg.Server.Port)

	// Graceful shutdown
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("shutting down...")
		cancel()
		mgr.Stop(context.Background())
		if shutdownErr := server.Stop(context.Background()); shutdownErr != nil {
			log.Printf("server shutdown error: %v", shutdownErr)
		}
		shutdownCtx := context.Background()
		if p := trace.TracerProvider(); p != nil {
			_ = p.Shutdown(shutdownCtx)
		}
		if p := telemetrylog.Provider(); p != nil {
			_ = p.Shutdown(shutdownCtx)
		}
	}()

	log.Printf("starting sandbox API server on %s:%d", cfg.Server.Host, cfg.Server.Port)
	if err = server.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("server error: %v", err)
		// Cancel context to trigger cleanup in the shutdown goroutine, then
		// exit immediately — do not wait for a signal that will never arrive.
		cancel()
		mgr.Stop(context.Background())
		log.Println("shutdown complete")
		return
	}

	// Wait for graceful shutdown to complete (signal-triggered path)
	<-shutdownDone
	log.Println("shutdown complete")
}
