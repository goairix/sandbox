package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/goairix/fs"
	"github.com/goairix/sandbox/internal/api"
	"github.com/goairix/sandbox/internal/api/handler"
	"github.com/goairix/sandbox/internal/config"
	"github.com/goairix/sandbox/internal/logger"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/runtime/docker"
	k8sruntime "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/goairix/sandbox/internal/sandbox"
	"github.com/goairix/sandbox/internal/storage"
	"github.com/goairix/sandbox/internal/storage/state"
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
	drainDeployment := flag.String("drain-kubernetes-deployment", "", "scale this in-cluster Deployment to zero and wait for its Pods")
	drainNamespace := flag.String("drain-kubernetes-namespace", "", "namespace containing the Deployment to drain")
	drainHPA := flag.String("drain-kubernetes-hpa", "", "optional HPA to delete before draining the Deployment")
	drainTimeout := flag.Duration("drain-timeout", 10*time.Minute, "maximum time to wait for a Kubernetes Deployment drain")
	flag.Parse()
	if *drainDeployment != "" {
		if *drainNamespace == "" || *drainTimeout <= 0 {
			log.Fatal("drain-kubernetes-namespace and a positive drain-timeout are required")
		}
		restConfig, configErr := rest.InClusterConfig()
		if configErr != nil {
			log.Fatalf("failed to load in-cluster drain configuration: %v", configErr)
		}
		client, clientErr := kubernetes.NewForConfig(restConfig)
		if clientErr != nil {
			log.Fatalf("failed to create in-cluster drain client: %v", clientErr)
		}
		drainCtx, drainCancel := context.WithTimeout(context.Background(), *drainTimeout)
		defer drainCancel()
		if drainErr := drainKubernetesDeployment(drainCtx, client, *drainNamespace, *drainDeployment, *drainHPA, time.Second); drainErr != nil {
			log.Fatalf("failed to drain Kubernetes API deployment: %v", drainErr)
		}
		log.Printf("Kubernetes API deployment %s/%s drained", *drainNamespace, *drainDeployment)
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	if cfg.Workspace.AllowUnverifiedDurableFlush {
		log.Printf("WARNING: local Docker MinIO validation is allowing an unverified durable-flush profile; do not use this mode for production or durability claims")
	}
	if cfg.Workspace.AllowMissingLSMForKind {
		log.Printf("WARNING: local kind validation is running without AppArmor/SELinux enforcement; production must disable workspace.allow_missing_lsm_for_kind")
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

	fuseEnabled := cfg.Workspace.MountModeEnabled("fuse")

	// Initialize runtime. Docker FUSE uses a root-owned staging directory and
	// copies only the operator-selected credential files into each special
	// container; ordinary containers never receive /dev/fuse or these files.
	var rt runtime.Runtime
	switch cfg.Runtime.Type {
	case "docker":
		if fuseEnabled {
			rt, err = docker.NewWithFUSESecretsAtRoot(ctx, cfg.Runtime.Docker.Host, cfg.Images.Gateway, cfg.Runtime.Docker.WorkspaceSecretRoot, &docker.FileSecretMaterializer{
				Root:          cfg.Runtime.Docker.WorkspaceSecretRoot,
				AccessKeyFile: cfg.Storage.FileSystem.CredentialFiles.AccessKeyFile,
				SecretKeyFile: cfg.Storage.FileSystem.CredentialFiles.SecretKeyFile,
				CAFile:        cfg.Storage.FileSystem.CAFile,
			})
		} else {
			rt, err = docker.New(ctx, cfg.Runtime.Docker.Host, cfg.Images.Gateway)
		}
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

	// Keep the Redis Store concrete so exactly one production FUSE repository
	// wrapper owns its Lua state machine. The compile-time assertion documents
	// the only broader capability the coordinator/session code relies on.
	var redisStore *redisstate.Store
	if cfg.Storage.State.Redis.Addr != "" {
		redisStore, err = redisstate.New(ctx, redisstate.Options{
			Addr: cfg.Storage.State.Redis.Addr, Password: cfg.Storage.State.Redis.Password, DB: cfg.Storage.State.Redis.DB,
		})
		if err != nil {
			log.Fatalf("failed to create redis state store: %v", err)
		}
		defer redisStore.Close()
		var _ state.AtomicStore = redisStore
	}

	var fsys fs.FileSystem
	var fsMeta *storage.FileSystemMeta
	var objectClient storage.WorkspaceObjectClient
	selected := cfg.Workspace.Backend
	fsys, fsMeta, err = storage.NewFileSystemFromConfiguredCredentials(cfg.Storage.FileSystem, selected.StorageIdentity)
	if err != nil {
		log.Fatalf("failed to create filesystem: %v", err)
	}
	if fuseEnabled {
		credentials, credentialErr := storage.LoadFileSystemCredentials(cfg.Storage.FileSystem)
		if credentialErr != nil {
			log.Fatalf("failed to load FUSE control-plane credentials: %v", credentialErr)
		}
		objectClient, err = storage.NewWorkspaceObjectClient(cfg.Storage.FileSystem, credentials)
		credentials.Zero()
		if err != nil {
			log.Fatalf("failed to create FUSE workspace object client: %v", err)
		}
	}

	// Build pool config
	sandboxImage := cfg.Images.Sandbox
	if sandboxImage == "" {
		sandboxImage = "sandbox:latest"
	}

	managerConfig := sandbox.ManagerConfig{
		RuntimeType: cfg.Runtime.Type,
		PoolConfig: sandbox.PoolConfig{
			MinSize:       cfg.Pool.MinSize,
			MaxSize:       cfg.Pool.MaxSize,
			Image:         sandboxImage,
			Memory:        cfg.Security.MaxMemory,
			MemoryRequest: cfg.Security.MaxMemoryRequest,
			CPU:           cfg.Security.MaxCPU,
			CPURequest:    cfg.Security.MaxCPURequest,
			Disk:          cfg.Security.MaxDisk,
			TmpDisk:       cfg.Security.MaxTmpDisk,
		},
		DefaultTimeout:          cfg.Security.SandboxTimeoutSeconds,
		ExecTimeoutSeconds:      cfg.Security.ExecTimeoutSeconds,
		MaxExecTimeoutSeconds:   cfg.Security.MaxExecTimeoutSeconds,
		AutoSyncIntervalSeconds: cfg.Workspace.AutoSyncIntervalSeconds,
		DefaultMountMode:        sandbox.WorkspaceMountType(cfg.Workspace.DefaultMountMode),
		EnabledMountModes:       make(map[sandbox.WorkspaceMountType]bool, len(cfg.Workspace.EnabledMountModes)),
	}
	for _, mode := range cfg.Workspace.EnabledMountModes {
		managerConfig.EnabledMountModes[sandbox.WorkspaceMountType(mode)] = true
	}
	if fsMeta != nil && fsMeta.Provider != storage.ProviderLocal {
		if redisStore == nil {
			log.Fatal("remote workspace modes require Redis for cross-mode ownership")
		}
		managerConfig.WorkspaceCoordinator = sandbox.NewWorkspaceCoordinator(redisStore,
			time.Duration(cfg.Workspace.LeaseTTLSeconds)*time.Second,
			time.Duration(cfg.Workspace.LeaseRenewIntervalSeconds)*time.Second,
		)
	}
	if fuseEnabled {
		if redisStore == nil {
			log.Fatal("FUSE mode requires Redis")
		}
		fuseSpec, specErr := buildFUSESpec(cfg, sandboxImage)
		if specErr != nil {
			log.Fatalf("failed to build FUSE pool spec: %v", specErr)
		}
		ownershipToken, tokenErr := newOwnershipToken()
		if tokenErr != nil {
			log.Fatalf("failed to create FUSE pool ownership token: %v", tokenErr)
		}
		poolRepo := redisstate.NewFUSEPoolRepository(redisStore)
		managerConfig.FUSEPool = sandbox.NewFUSEPool(rt, poolRepo, sandbox.FUSEPoolConfig{
			MinSize: cfg.Workspace.FUSEPool.MinSize, MaxSize: cfg.Workspace.FUSEPool.MaxSize,
			RefillInterval:  time.Duration(cfg.Workspace.FUSEPool.RefillIntervalSeconds) * time.Second,
			PrepareTimeout:  time.Duration(cfg.Workspace.FUSEPool.PrepareTimeoutSeconds) * time.Second,
			ReservationTTL:  time.Duration(cfg.Workspace.LeaseTTLSeconds) * time.Second,
			MaintainerToken: ownershipToken,
		}, fuseSpec)
		managerConfig.WorkspaceObjectClient = objectClient
		profile, profileErr := storage.RootMarkerProfileByID(cfg.Workspace.Backend.Profile)
		if profileErr != nil {
			log.Fatalf("failed to select FUSE marker profile: %v", profileErr)
		}
		managerConfig.WorkspaceMarkerProfile = profile
		managerConfig.FUSEHealthInterval = 5 * time.Second
	}
	mgr := sandbox.NewManager(rt, fsys, fsMeta, managerConfig)

	// Initialize session store for persistent sandbox state
	if redisStore != nil {
		ttl := time.Duration(cfg.Security.SandboxTimeoutSeconds) * time.Second
		mgr.SetSessionStore(sandbox.NewSessionStore(redisStore, ttl))
		mgr.SetEphemeralLifecycleStore(sandbox.NewEphemeralLifecycleStore(redisStore))
		mgr.SetMultipartStore(redisStore)
		log.Printf("session store connected to redis at %s", cfg.Storage.State.Redis.Addr)
	}

	if err = mgr.Start(ctx); err != nil {
		log.Fatalf("failed to start sandbox manager: %v", err)
	}

	h := handler.NewHandler(mgr, cfg.Security.MaxUploadBytes)
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

func newOwnershipToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	defer clear(raw)
	return hex.EncodeToString(raw), nil
}

func buildFUSESpec(cfg *config.Config, sandboxImage string) (runtime.SandboxSpec, error) {
	if cfg == nil {
		return runtime.SandboxSpec{}, sandbox.ErrInvalidFUSEPoolConfig
	}
	provider := cfg.Workspace.Backend
	if provider.Preset == "" {
		return runtime.SandboxSpec{}, sandbox.ErrInvalidFUSEPoolConfig
	}
	image := sandboxImage
	if cfg.Runtime.Type == "docker" {
		image = provider.DockerImage
	}
	fuse := &runtime.WorkspaceFUSESpec{
		RuntimeType: cfg.Runtime.Type, Provider: cfg.Storage.FileSystem.Provider, Driver: provider.Driver,
		Profile: provider.Profile, StorageIdentity: provider.StorageIdentity,
		CredentialGeneration: provider.CredentialGeneration, MounterImage: provider.MounterImage, DockerImage: provider.DockerImage,
		SecretName: cfg.Workspace.SecretName, CASecretKey: provider.CASecretKey, EndpointHostIPs: append([]string(nil), provider.EndpointHostIPs...),
		Bucket: cfg.Storage.FileSystem.Bucket, Endpoint: cfg.Storage.FileSystem.Endpoint, Region: cfg.Storage.FileSystem.Region,
		UseSSL: cfg.Storage.FileSystem.UseSSL, CacheSize: cfg.Workspace.CacheSize, CacheMedium: cfg.Workspace.CacheMedium,
		MountTimeout:           time.Duration(cfg.Workspace.MountTimeoutSeconds) * time.Second,
		FlushTimeout:           time.Duration(cfg.Workspace.FlushTimeoutSeconds) * time.Second,
		UnmountTimeout:         time.Duration(cfg.Workspace.UnmountTimeoutSeconds) * time.Second,
		LSMProfile:             provider.LSMProfile,
		AllowMissingLSMForKind: cfg.Workspace.AllowMissingLSMForKind,
		MounterResources: runtime.WorkspaceFUSEResources{
			CPURequest: cfg.Workspace.MounterResources.CPURequest, CPULimit: cfg.Workspace.MounterResources.CPULimit,
			MemoryRequest: cfg.Workspace.MounterResources.MemoryRequest, MemoryLimit: cfg.Workspace.MounterResources.MemoryLimit,
			EphemeralStorageRequest: cfg.Workspace.MounterResources.EphemeralStorageRequest,
			EphemeralStorageLimit:   cfg.Workspace.MounterResources.EphemeralStorageLimit,
		},
		SystemEgress: runtime.SystemEgressSpec{
			Mode: runtime.SystemEgressMode(provider.SystemEgressMode), DNSCIDRs: append([]string(nil), provider.DNSCIDRs...),
			DNSPorts: []int32{53}, EndpointCIDRs: append([]string(nil), provider.SystemEgressCIDRs...),
			EndpointFQDNs: append([]string(nil), provider.SystemEgressFQDNs...), EndpointPorts: append([]int32(nil), provider.EndpointPorts...),
			ProxyURL: provider.ProxyURL,
		},
	}
	spec := runtime.SandboxSpec{
		ID: "sandbox-fuse-pool-template", Image: image,
		Memory: cfg.Security.MaxMemory, MemoryRequest: cfg.Security.MaxMemoryRequest,
		CPU: cfg.Security.MaxCPU, CPURequest: cfg.Security.MaxCPURequest,
		Disk: cfg.Security.MaxDisk, TmpDisk: cfg.Security.MaxTmpDisk, PidLimit: cfg.Security.MaxPids,
		ReadOnlyRootFS: true, RunAsUser: 1000, SeccompProfile: cfg.Security.SeccompProfile,
		Labels: map[string]string{"sandbox.pool": "true"}, WorkspaceFUSE: fuse,
	}
	poolKey, err := sandbox.ComputeFUSEPoolKey(spec)
	if err != nil {
		return runtime.SandboxSpec{}, err
	}
	fuse.PoolKey = poolKey
	return spec, nil
}
