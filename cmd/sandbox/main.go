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

	"github.com/goairix/sandbox/internal/api"
	"github.com/goairix/sandbox/internal/api/handler"
	"github.com/goairix/sandbox/internal/config"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/runtime/docker"
	k8sruntime "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/goairix/sandbox/internal/sandbox"
	"github.com/goairix/sandbox/internal/storage"
)

func main() {
	configPath := flag.String("config", "", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
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
	fsys, err := storage.NewFileSystem(cfg.Storage.FileSystem)
	if err != nil {
		log.Fatalf("failed to create filesystem: %v", err)
	}

	// Build pool config
	sandboxImage := cfg.Images.Sandbox
	if sandboxImage == "" {
		sandboxImage = "sandbox:latest"
	}

	mgr := sandbox.NewManager(rt, fsys, sandbox.ManagerConfig{
		PoolConfig: sandbox.PoolConfig{
			MinSize: cfg.Pool.MinSize,
			MaxSize: cfg.Pool.MaxSize,
			Image:   sandboxImage,
		},
		DefaultTimeout: cfg.Security.SandboxTimeoutSeconds,
	})
	mgr.Start(ctx)

	h := handler.NewHandler(mgr)
	router := api.SetupRouter(h, cfg.Security.APIKey, cfg.Security.RateLimit)
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
