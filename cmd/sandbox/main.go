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

	"k8s.io/client-go/dynamic"
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
	resumeDeployment := flag.String("resume-kubernetes-deployment", "", "scale this in-cluster Deployment to a positive replica count and wait for availability")
	resumeNamespace := flag.String("resume-kubernetes-namespace", "", "namespace containing the Deployment to resume")
	resumeReplicas := flag.Int("resume-kubernetes-replicas", 0, "replica count used to resume an in-cluster Deployment")
	backendFingerprintNamespace := flag.String("kubernetes-backend-fingerprint-namespace", "", "namespace containing the Deployment backend fingerprint")
	backendFingerprintDeployment := flag.String("kubernetes-backend-fingerprint-deployment", "", "Deployment containing the installed backend fingerprint")
	backendFingerprint := flag.String("kubernetes-backend-fingerprint", "", "desired backend fingerprint used by upgrade and rollback guards")
	cleanupProtocol := flag.String("kubernetes-cleanup-protocol", "", "desired Kubernetes FUSE cleanup protocol used by upgrade guards")
	requiredDrainProtocol := flag.String("required-kubernetes-drain-protocol", "", "required installed drain protocol before a backend-changing upgrade")
	verifyBackendFingerprint := flag.Bool("verify-kubernetes-backend-fingerprint", false, "fail unless the installed backend fingerprint matches the desired fingerprint")
	drainTimeout := flag.Duration("drain-timeout", 10*time.Minute, "maximum time to wait for a Kubernetes Deployment drain or resume")
	drainRelease := flag.Bool("drain-release", false, "finalize all sandbox state and verify a release-wide zero-state drain")
	flag.Parse()
	modeCount := 0
	for _, enabled := range []bool{*drainDeployment != "", *resumeDeployment != "", *verifyBackendFingerprint} {
		if enabled {
			modeCount++
		}
	}
	if modeCount > 1 {
		log.Fatal("Kubernetes deployment drain, resume, and backend verification modes are mutually exclusive")
	}
	if *verifyBackendFingerprint {
		if *backendFingerprintNamespace == "" || *backendFingerprintDeployment == "" || *backendFingerprint == "" || *drainTimeout <= 0 {
			log.Fatal("backend fingerprint namespace, Deployment, desired value, and a positive drain-timeout are required")
		}
		restConfig, configErr := rest.InClusterConfig()
		if configErr != nil {
			log.Fatalf("failed to load in-cluster backend verification configuration: %v", configErr)
		}
		client, clientErr := kubernetes.NewForConfig(restConfig)
		if clientErr != nil {
			log.Fatalf("failed to create in-cluster backend verification client: %v", clientErr)
		}
		verifyCtx, verifyCancel := context.WithTimeout(context.Background(), *drainTimeout)
		defer verifyCancel()
		matches, matchErr := kubernetesBackendFingerprintMatches(verifyCtx, client, *backendFingerprintNamespace, *backendFingerprintDeployment, *backendFingerprint)
		if matchErr != nil {
			log.Fatalf("failed to verify Kubernetes backend fingerprint: %v", matchErr)
		}
		if !matches {
			log.Fatal("cross-backend Helm rollback is not supported; apply the target backend with helm upgrade so the current backend can be drained safely")
		}
		log.Printf("Kubernetes backend fingerprint matches; rollback does not require a release drain")
		return
	}
	if *resumeDeployment != "" {
		if *resumeNamespace == "" || *resumeReplicas < 0 || *drainTimeout <= 0 {
			log.Fatal("resume-kubernetes-namespace, non-negative resume-kubernetes-replicas, and a positive drain-timeout are required")
		}
		restConfig, configErr := rest.InClusterConfig()
		if configErr != nil {
			log.Fatalf("failed to load in-cluster resume configuration: %v", configErr)
		}
		client, clientErr := kubernetes.NewForConfig(restConfig)
		if clientErr != nil {
			log.Fatalf("failed to create in-cluster resume client: %v", clientErr)
		}
		resumeCtx, resumeCancel := context.WithTimeout(context.Background(), *drainTimeout)
		defer resumeCancel()
		resumed, resumeErr := resumeKubernetesDeployment(resumeCtx, client, *resumeNamespace, *resumeDeployment, int32(*resumeReplicas), time.Second)
		if resumeErr != nil {
			log.Fatalf("failed to resume Kubernetes API deployment: %v", resumeErr)
		}
		if resumed {
			log.Printf("Kubernetes API deployment %s/%s resumed at %d replicas", *resumeNamespace, *resumeDeployment, *resumeReplicas)
		} else {
			log.Printf("Kubernetes API deployment %s/%s already has positive replicas; resume skipped", *resumeNamespace, *resumeDeployment)
		}
		return
	}
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
		fingerprintArgs := []string{*backendFingerprintNamespace, *backendFingerprintDeployment, *backendFingerprint}
		fingerprintConfigured := fingerprintArgs[0] != "" || fingerprintArgs[1] != "" || fingerprintArgs[2] != ""
		if fingerprintConfigured {
			if fingerprintArgs[0] == "" || fingerprintArgs[1] == "" || fingerprintArgs[2] == "" {
				log.Fatal("backend fingerprint namespace, Deployment, and desired value must be configured together")
			}
			if *cleanupProtocol == "" {
				log.Fatal("desired Kubernetes cleanup protocol must be configured with backend fingerprint comparison")
			}
			checkCtx, checkCancel := context.WithTimeout(context.Background(), *drainTimeout)
			defer checkCancel()
			backendMatches, matchErr := kubernetesBackendFingerprintMatches(checkCtx, client, fingerprintArgs[0], fingerprintArgs[1], fingerprintArgs[2])
			if matchErr != nil {
				log.Fatalf("failed to compare Kubernetes backend fingerprint before drain: %v", matchErr)
			}
			cleanupMatches, matchErr := kubernetesCleanupProtocolMatches(checkCtx, client, fingerprintArgs[0], fingerprintArgs[1], *cleanupProtocol)
			if matchErr != nil {
				log.Fatalf("failed to compare Kubernetes cleanup protocol before drain: %v", matchErr)
			}
			if !shouldDrainKubernetesRelease(backendMatches, cleanupMatches) {
				log.Printf("Kubernetes backend fingerprint and cleanup protocol are unchanged; release drain skipped")
				return
			}
			if *requiredDrainProtocol == "" {
				log.Fatal("required Kubernetes drain protocol must be configured for an upgrade that requires release drain")
			}
			protocolMatches, protocolErr := kubernetesDrainProtocolMatches(checkCtx, client, fingerprintArgs[0], fingerprintArgs[1], *requiredDrainProtocol)
			if protocolErr != nil {
				log.Fatalf("failed to verify Kubernetes drain protocol before drain: %v", protocolErr)
			}
			if !protocolMatches {
				log.Fatal("release-draining upgrade requires a prior same-backend Chart and sandbox-api drain protocol upgrade")
			}
		}
		drainCtx, drainCancel := context.WithTimeout(context.Background(), *drainTimeout)
		defer drainCancel()
		if drainErr := drainKubernetesDeployment(drainCtx, client, *drainNamespace, *drainDeployment, *drainHPA, time.Second); drainErr != nil {
			log.Fatalf("failed to drain Kubernetes API deployment: %v", drainErr)
		}
		log.Printf("Kubernetes API deployment %s/%s drained", *drainNamespace, *drainDeployment)
		if !*drainRelease {
			return
		}
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
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
	fuseCredentials, err := loadRuntimeFUSECredentials(cfg)
	if err != nil {
		log.Fatalf("failed to load FUSE credentials: %v", err)
	}
	defer fuseCredentials.Zero()

	// Initialize the runtime with its own in-memory credential copy. Prepared
	// containers and Pods remain credential-free until one-shot authorization.
	var rt runtime.Runtime
	switch cfg.Runtime.Type {
	case "docker":
		if fuseEnabled {
			rt, err = docker.NewWithFUSECredentials(ctx, cfg.Runtime.Docker.Host, cfg.Images.Gateway, fuseCredentials)
		} else {
			rt, err = docker.New(ctx, cfg.Runtime.Docker.Host, cfg.Images.Gateway)
		}
		if err != nil {
			log.Fatalf("failed to create docker runtime: %v", err)
		}
	case "kubernetes":
		var options []k8sruntime.Option
		if fuseEnabled {
			options = append(options, k8sruntime.WithFUSECredentials(fuseCredentials))
		}
		rt, err = k8sruntime.New(cfg.Runtime.Kubernetes.Kubeconfig, cfg.Runtime.Kubernetes.Namespace, options...)
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
	if cfg.Storage.State.Redis.Addr != "" || len(cfg.Storage.State.Redis.Addrs) > 0 {
		redisStore, err = redisstate.New(ctx, redisstate.Options{
			Mode: redisstate.Mode(cfg.Storage.State.Redis.Mode), Addr: cfg.Storage.State.Redis.Addr,
			Addrs: cfg.Storage.State.Redis.Addrs, MasterName: cfg.Storage.State.Redis.MasterName,
			Username: cfg.Storage.State.Redis.Username, Password: cfg.Storage.State.Redis.Password,
			DB: cfg.Storage.State.Redis.DB, Durability: redisstate.DurabilityMode(cfg.Storage.State.Redis.Durability),
			AckReplicas: cfg.Storage.State.Redis.AckReplicas,
			AckTimeout:  time.Duration(cfg.Storage.State.Redis.AckTimeoutMS) * time.Millisecond,
			PoolSize:    cfg.Storage.State.Redis.PoolSize, MinIdleConns: cfg.Storage.State.Redis.MinIdleConns,
			DialTimeout:  time.Duration(cfg.Storage.State.Redis.DialTimeoutMS) * time.Millisecond,
			ReadTimeout:  time.Duration(cfg.Storage.State.Redis.ReadTimeoutMS) * time.Millisecond,
			WriteTimeout: time.Duration(cfg.Storage.State.Redis.WriteTimeoutMS) * time.Millisecond,
			MaxRetries:   cfg.Storage.State.Redis.MaxRetries,
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
		objectClient, err = storage.NewWorkspaceObjectClient(cfg.Storage.FileSystem, storage.FileSystemCredentials{
			AccessKey: fuseCredentials.AccessKey, SecretKey: fuseCredentials.SecretKey,
		})
		fuseCredentials.Zero()
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
			MinSize:        cfg.Pool.MinSize,
			MaxSize:        cfg.Pool.MaxSize,
			Image:          sandboxImage,
			Memory:         cfg.Security.MaxMemory,
			MemoryRequest:  cfg.Security.MaxMemoryRequest,
			CPU:            cfg.Security.MaxCPU,
			CPURequest:     cfg.Security.MaxCPURequest,
			Disk:           cfg.Security.MaxDisk,
			TmpDisk:        cfg.Security.MaxTmpDisk,
			PidLimit:       cfg.Security.MaxPids,
			SeccompProfile: cfg.Security.SeccompProfile,
		},
		DefaultTimeout:          cfg.Security.SandboxTimeoutSeconds,
		ExecTimeoutSeconds:      cfg.Security.ExecTimeoutSeconds,
		MaxExecTimeoutSeconds:   cfg.Security.MaxExecTimeoutSeconds,
		AutoSyncIntervalSeconds: cfg.Workspace.AutoSyncIntervalSeconds,
		DefaultMountMode:        sandbox.WorkspaceMountType(cfg.Workspace.DefaultMountMode),
		EnabledMountModes:       make(map[sandbox.WorkspaceMountType]bool, len(cfg.Workspace.EnabledMountModes)),
	}
	if cfg.Runtime.Type == "kubernetes" {
		if redisStore == nil {
			log.Fatal("Kubernetes runtime requires Redis for stateless multi-replica sandbox state")
		}
		stateScope := os.Getenv("SANDBOX_STATE_SCOPE")
		if stateScope == "" {
			stateScope = cfg.Runtime.Kubernetes.Namespace
		}
		activeRepository, repositoryErr := redisstate.NewActiveSandboxRepository(redisStore, stateScope)
		if repositoryErr != nil {
			log.Fatalf("failed to create active sandbox repository: %v", repositoryErr)
		}
		instanceID := os.Getenv("POD_UID")
		if instanceID == "" {
			instanceID = os.Getenv("HOSTNAME")
		}
		if instanceID == "" {
			instanceID, repositoryErr = newOwnershipToken()
			if repositoryErr != nil {
				log.Fatalf("failed to create API instance identity: %v", repositoryErr)
			}
		}
		managerConfig.ActiveSandboxes = activeRepository
		managerConfig.InstanceID = instanceID
	}
	if cfg.Runtime.Type == "kubernetes" && cfg.Pool.MinSize > 0 {
		if redisStore == nil {
			log.Fatal("Kubernetes ordinary pool requires Redis for shared multi-replica inventory")
		}
		managerConfig.PoolStateStore = redisStore
		managerConfig.PoolScope = os.Getenv("SANDBOX_STATE_SCOPE")
		if managerConfig.PoolScope == "" {
			managerConfig.PoolScope = cfg.Runtime.Kubernetes.Namespace
		}
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
		var fuseInventoryStore state.AtomicStore
		var fuseInventoryScope string
		if cfg.Runtime.Type == "kubernetes" {
			fuseInventoryStore = redisStore
			fuseInventoryScope = os.Getenv("SANDBOX_STATE_SCOPE")
			if fuseInventoryScope == "" {
				fuseInventoryScope = cfg.Runtime.Kubernetes.Namespace
			}
			if provider, ok := rt.(runtime.WarmPoolContractProvider); ok {
				fuseSpec.PoolContract = provider.WarmPoolContract() + ":scope:" + fuseInventoryScope
			}
			fuseSpec.WorkspaceFUSE.PoolKey, specErr = sandbox.ComputeFUSEPoolKey(fuseSpec)
			if specErr != nil {
				log.Fatalf("failed to compute FUSE runtime contract: %v", specErr)
			}
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
			InventoryStore:  fuseInventoryStore,
			InventoryScope:  fuseInventoryScope,
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
		log.Printf("session store connected to redis mode=%s endpoints=%d", cfg.Storage.State.Redis.Mode, max(1, len(cfg.Storage.State.Redis.Addrs)))
	}
	if *drainRelease {
		drainCtx, drainCancel := context.WithTimeout(ctx, *drainTimeout)
		defer drainCancel()
		if err := mgr.DrainRelease(drainCtx); err != nil {
			log.Fatalf("release drain failed: %v", err)
		}
		if cfg.Runtime.Type == "kubernetes" {
			restConfig, configErr := rest.InClusterConfig()
			if configErr != nil {
				log.Fatalf("load in-cluster drain audit configuration: %v", configErr)
			}
			client, clientErr := kubernetes.NewForConfig(restConfig)
			if clientErr != nil {
				log.Fatalf("create Kubernetes drain audit client: %v", clientErr)
			}
			dynamicClient, dynamicErr := dynamic.NewForConfig(restConfig)
			if dynamicErr != nil {
				log.Fatalf("create Kubernetes dynamic drain audit client: %v", dynamicErr)
			}
			if auditErr := auditKubernetesDrainedResources(drainCtx, client, dynamicClient, cfg.Runtime.Kubernetes.Namespace); auditErr != nil {
				log.Fatalf("Kubernetes release drain audit failed: %v", auditErr)
			}
		}
		log.Printf("release drain completed with zero managed state")
		return
	}

	if err = mgr.Start(ctx); err != nil {
		log.Fatalf("failed to start sandbox manager: %v", err)
	}

	h := handler.NewHandler(mgr, cfg.Security.MaxUploadBytes)
	router := api.SetupRouter(h, cfg.Security.APIKey, cfg.Security.RateLimit, cfg.Telemetry.ServiceName, mgr.Ready)
	server := api.NewServer(router, cfg.Server.Host, cfg.Server.Port)

	// Graceful shutdown
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("shutting down...")
		mgr.BeginShutdown()
		if shutdownErr := server.Stop(context.Background()); shutdownErr != nil {
			log.Printf("server shutdown error: %v", shutdownErr)
		}
		cancel()
		if stopErr := mgr.Stop(context.Background()); stopErr != nil {
			log.Printf("sandbox manager shutdown error: %v", stopErr)
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
		if stopErr := mgr.Stop(context.Background()); stopErr != nil {
			log.Printf("sandbox manager shutdown error: %v", stopErr)
		}
		log.Println("shutdown complete")
		return
	}

	// Wait for graceful shutdown to complete (signal-triggered path)
	<-shutdownDone
	log.Println("shutdown complete")
}

func loadRuntimeFUSECredentials(cfg *config.Config) (runtime.FUSECredentials, error) {
	if cfg == nil || !cfg.Workspace.MountModeEnabled("fuse") {
		return runtime.FUSECredentials{}, nil
	}
	loaded, err := storage.LoadFileSystemCredentials(cfg.Storage.FileSystem)
	if err != nil {
		return runtime.FUSECredentials{}, err
	}
	defer loaded.Zero()
	return runtime.FUSECredentials{AccessKey: append([]byte(nil), loaded.AccessKey...), SecretKey: append([]byte(nil), loaded.SecretKey...)}, nil
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
