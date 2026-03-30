package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/goairix/sandbox/internal/api"
	"github.com/goairix/sandbox/internal/api/handler"
	"github.com/goairix/sandbox/internal/config"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/runtime/docker"
	"github.com/goairix/sandbox/internal/sandbox"
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
		rt, err = docker.New(ctx, cfg.Runtime.Docker.Host)
		if err != nil {
			log.Fatalf("failed to create docker runtime: %v", err)
		}
	case "kubernetes":
		log.Fatal("kubernetes runtime not yet implemented")
	default:
		log.Fatalf("unknown runtime type: %s", cfg.Runtime.Type)
	}

	// Build pool configs
	poolConfigs := map[sandbox.Language]sandbox.PoolConfig{
		sandbox.LangPython: {
			Language: sandbox.LangPython,
			MinSize:  cfg.Pool.MinSize,
			MaxSize:  cfg.Pool.MaxSize,
			Image:    "sandbox-python:latest",
		},
		sandbox.LangNodeJS: {
			Language: sandbox.LangNodeJS,
			MinSize:  cfg.Pool.MinSize,
			MaxSize:  cfg.Pool.MaxSize,
			Image:    "sandbox-nodejs:latest",
		},
		sandbox.LangBash: {
			Language: sandbox.LangBash,
			MinSize:  cfg.Pool.MinSize,
			MaxSize:  cfg.Pool.MaxSize,
			Image:    "sandbox-bash:latest",
		},
	}

	mgr := sandbox.NewManager(rt, sandbox.ManagerConfig{
		PoolConfigs:    poolConfigs,
		DefaultTimeout: cfg.Security.ExecTimeoutSeconds,
	})
	mgr.Start(ctx)

	h := handler.NewHandler(mgr)
	router := api.SetupRouter(h, "", 0)
	server := api.NewServer(router, cfg.Server.Host, cfg.Server.Port)

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("shutting down...")
		cancel()
		mgr.Stop(context.Background())
		if err := server.Stop(context.Background()); err != nil {
			log.Printf("server shutdown error: %v", err)
		}
	}()

	log.Printf("starting sandbox API server on %s:%d", cfg.Server.Host, cfg.Server.Port)
	if err := server.Start(); err != nil {
		log.Printf("server stopped: %v", err)
	}
}
