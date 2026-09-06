package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/goairix/fs"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/goairix/sandbox/internal/fuseprotocol"
	"github.com/goairix/sandbox/internal/logger"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage"
	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/goairix/sandbox/internal/telemetry/metrics"
	telemetry "github.com/goairix/sandbox/internal/telemetry/trace"
)

// validDepRegexp validates dependency names and versions to prevent command injection.
var validDepRegexp = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

const randSuffixLen = 10
const multipartKeyPrefix = "sandbox:multipart:"
const multipartTTL = 24 * time.Hour

type MultipartUploadState struct {
	UploadID       string    `json:"upload_id"`
	SandboxID      string    `json:"sandbox_id"`
	DestPath       string    `json:"dest_path"`
	TotalChunks    int       `json:"total_chunks"`
	ReceivedChunks int       `json:"received_chunks"`
	CreatedAt      time.Time `json:"created_at"`
}

type fuseBindingClaim struct {
	record          state.FUSEPoolRecord
	returning       bool
	lost            bool
	published       bool
	lifecycle       *fuseSandboxLifecycle
	cleanupRecord   *state.FUSEPoolRecord
	sessionIdentity *Sandbox
}

type fuseSandboxLifecycle struct {
	sandboxID        string
	gate             *operationGate
	lease            *WorkspaceLease
	renewal          *WorkspaceLeaseRenewal
	record           state.FUSEPoolRecord
	cancel           context.CancelFunc
	teardownMu       sync.Mutex
	teardownRunning  bool
	teardownDone     bool
	lastFailureStage string
	lastFailureAt    time.Time
	gateClosed       bool
	renewalStopped   bool
	quiesceAttempted bool
	quiesced         bool
	flushAttempted   bool
	poolRemoved      bool
	claimed          *state.FUSEPoolRecord
	runtimeRemoved   bool
	evidence         runtime.TerminationEvidence
	probeRemoved     bool
	unmountRecorded  bool
	leaseReleased    bool
	sessionRemoved   bool
}

// randSuffix generates a random lowercase alphanumeric string of length n.
func randSuffix(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = alphabet[b[i]%byte(len(alphabet))]
	}
	return string(b)
}

func multipartKey(sandboxID, uploadID string) string {
	return multipartKeyPrefix + sandboxID + ":" + uploadID
}

// ManagerConfig configures the SandboxManager.
type ManagerConfig struct {
	PoolConfig              PoolConfig
	WorkspaceMode           string
	FUSEPool                *FUSEPool
	WorkspaceCoordinator    *WorkspaceCoordinator
	WorkspaceObjectClient   storage.WorkspaceObjectClient
	WorkspaceMarkerProfile  storage.RootMarkerProfile
	FUSEHealthInterval      time.Duration
	DefaultTimeout          int // seconds
	ExecTimeoutSeconds      int // per-execution timeout; 0 = no limit
	MaxExecTimeoutSeconds   int // maximum request timeout; 0 = no additional maximum
	AutoSyncIntervalSeconds int // 0 = disabled
}

// Manager orchestrates sandbox lifecycle: creation, execution, destruction.
type Manager struct {
	runtime        runtime.Runtime
	filesystem     fs.FileSystem
	fsMeta         *storage.FileSystemMeta
	config         ManagerConfig
	sessions       *SessionStore // optional, for persistent sandboxes
	multipartStore state.Store   // optional, for multipart upload state

	pool           *Pool
	fusePool       *FUSEPool
	sandboxes      map[string]*Sandbox
	workspaces     map[string]storage.ScopedFS // sandbox ID -> ScopedFS
	operationGates map[string]*operationGate
	fuseLifecycles map[string]*fuseSandboxLifecycle
	fuseInFlight   map[string]*fuseBindingClaim
	mu             sync.RWMutex

	stopCh        chan struct{}
	stopOnce      sync.Once
	shutdownDone  chan struct{}
	wg            sync.WaitGroup
	createWG      sync.WaitGroup
	lifecycleMu   sync.Mutex
	stopping      bool
	controlCtx    context.Context
	cancelControl context.CancelFunc
}

// NewManager creates a new SandboxManager.
func NewManager(rt runtime.Runtime, fsys fs.FileSystem, fsMeta *storage.FileSystemMeta, cfg ManagerConfig) *Manager {
	controlCtx, cancelControl := context.WithCancel(context.Background())
	m := &Manager{
		runtime:        rt,
		filesystem:     fsys,
		fsMeta:         fsMeta,
		config:         cfg,
		pool:           NewPool(rt, cfg.PoolConfig),
		fusePool:       cfg.FUSEPool,
		sandboxes:      make(map[string]*Sandbox),
		workspaces:     make(map[string]storage.ScopedFS),
		operationGates: make(map[string]*operationGate),
		fuseLifecycles: make(map[string]*fuseSandboxLifecycle),
		fuseInFlight:   make(map[string]*fuseBindingClaim),
		stopCh:         make(chan struct{}),
		shutdownDone:   make(chan struct{}),
		controlCtx:     controlCtx,
		cancelControl:  cancelControl,
	}
	if m.fusePool != nil {
		m.fusePool.config.PristineGuard = m.guardFUSEPoolRecord
	}
	return m
}

func (m *Manager) resolveExecTimeout(requested int) (int, error) {
	if requested < 0 {
		return 0, fmt.Errorf("%w: timeout must not be negative: %d", ErrInvalidExecTimeout, requested)
	}
	if m.config.MaxExecTimeoutSeconds > 0 && requested > m.config.MaxExecTimeoutSeconds {
		return 0, fmt.Errorf("%w: requested %d seconds exceeds maximum %d seconds", ErrInvalidExecTimeout, requested, m.config.MaxExecTimeoutSeconds)
	}
	if requested == 0 {
		return m.config.ExecTimeoutSeconds, nil
	}
	return requested, nil
}

func execTimeoutSource(requested int) string {
	if requested == 0 {
		return "default"
	}
	return "request"
}

func newExecContext(parent context.Context, effectiveTimeout int) (context.Context, context.CancelFunc) {
	if effectiveTimeout > 0 {
		return context.WithTimeoutCause(parent, time.Duration(effectiveTimeout)*time.Second, ErrExecTimeout)
	}
	return context.WithCancel(parent)
}

func isExecTimeout(parentCtx, execCtx context.Context, err error, effectiveTimeout int) bool {
	if errors.Is(context.Cause(execCtx), ErrExecTimeout) {
		return true
	}
	return effectiveTimeout > 0 && errors.Is(err, context.DeadlineExceeded) && parentCtx.Err() == nil
}

func execFailureStatus(parentCtx, execCtx context.Context, err error, effectiveTimeout int, timeoutSource string) string {
	if isExecTimeout(parentCtx, execCtx, err, effectiveTimeout) {
		return "timeout_" + timeoutSource
	}
	if parentCtx.Err() != nil || context.Cause(execCtx) != nil {
		return "caller_cancelled"
	}
	return "error"
}

func sendStreamTerminal(outCh chan runtime.StreamEvent, event runtime.StreamEvent) {
	select {
	case outCh <- event:
		return
	default:
	}

	// The buffer is full. Drop the oldest ordinary event to reserve room for
	// the terminal event without waiting for a slow or disconnected consumer.
	select {
	case <-outCh:
	default:
	}
	select {
	case outCh <- event:
	default:
	}
}

// SetSessionStore sets an optional SessionStore for persistent sandbox state.
func (m *Manager) SetSessionStore(ss *SessionStore) {
	m.sessions = ss
}

// SetMultipartStore sets the state.Store used for multipart upload state.
func (m *Manager) SetMultipartStore(s state.Store) {
	m.multipartStore = s
}

// Start initializes the manager and warms up the pool.
// It first removes any orphaned pool containers left over from a previous
// process that exited without cleanup (e.g. crash, SIGKILL).
func (m *Manager) Start(ctx context.Context) error {
	spanCtx, span := telemetry.Tracer().Start(ctx, "sandbox.Manager.Start")
	defer span.End()

	if m.config.WorkspaceMode == "fuse" {
		if m.fusePool == nil {
			return errors.Join(ErrInvalidFUSEPoolConfig, errors.New("FUSE mode requires a FUSE pool"))
		}
		if err := m.restorePersistentSandboxes(spanCtx); err != nil {
			return fmt.Errorf("restore persistent sandboxes for FUSE mode: %w", err)
		}
		if err := m.reconcileFUSEOrphans(spanCtx); err != nil {
			return fmt.Errorf("reconcile FUSE runtime orphans: %w", err)
		}
		if err := m.fusePool.Start(spanCtx); err != nil {
			return fmt.Errorf("start FUSE pool: %w", err)
		}
	} else if m.runtime.IsStateful() {
		// Stateful runtimes (e.g. Kubernetes): pods survive process restarts, so
		// we must restore persistent sandboxes synchronously first. This registers
		// their RuntimeIDs in m.sandboxes before cleanupOrphanedPoolContainers
		// runs, preventing live pods from being mistakenly deleted as orphans.
		if err := m.restorePersistentSandboxes(spanCtx); err != nil {
			logger.Error(spanCtx, "failed to restore persistent sandboxes", logger.ErrorField(err))
		}
		m.cleanupOrphanedPoolContainers(spanCtx)
		m.pool.WarmUp(spanCtx)
	} else {
		// Non-stateful runtimes (e.g. Docker): containers are gone after restart,
		// so cleanup first, then warm up, then restore in background to avoid
		// blocking API startup.
		m.cleanupOrphanedPoolContainers(spanCtx)
		m.pool.WarmUp(spanCtx)
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			if err := m.restorePersistentSandboxes(spanCtx); err != nil {
				logger.Error(spanCtx, "failed to restore persistent sandboxes", logger.ErrorField(err))
			}
		}()
	}

	m.wg.Add(1)
	go m.reapExpiredSandboxes()

	if m.config.AutoSyncIntervalSeconds > 0 {
		m.wg.Add(1)
		go m.autoSyncWorkspaces()
	}
	return nil
}

// reconcileFUSEOrphans is deliberately invoked only after session recovery
// and before pool reconciliation. It combines every durable source of runtime
// ownership; any unreadable source aborts cleanup rather than supplying an
// incomplete allow-delete set.
func (m *Manager) reconcileFUSEOrphans(ctx context.Context) error {
	reconciler, ok := m.runtime.(runtime.OrphanReconciler)
	if !ok {
		return nil
	}
	if m.sessions == nil || m.fusePool == nil || m.config.WorkspaceCoordinator == nil {
		return ErrInvalidFUSEPoolConfig
	}
	protected := make(map[string]struct{})
	ids, err := m.sessions.List(ctx)
	if err != nil {
		return fmt.Errorf("list protected FUSE sessions: %w", err)
	}
	for _, id := range ids {
		sb, err := m.sessions.Load(ctx, id)
		if err != nil {
			return fmt.Errorf("load protected FUSE session %q: %w", id, err)
		}
		if sb.ID != id || (sb.RuntimeUID != "" && validateOpaqueText(sb.RuntimeUID, false) != nil) {
			return ErrSandboxNotReady
		}
		if sb.RuntimeUID != "" {
			protected[sb.RuntimeUID] = struct{}{}
		}
	}
	ownerUIDs, err := m.config.WorkspaceCoordinator.ProtectedRuntimeUIDs(ctx)
	if err != nil {
		return err
	}
	for uid := range ownerUIDs {
		protected[uid] = struct{}{}
	}
	records, err := m.fusePool.repo.ListByPoolKey(ctx, m.fusePool.poolKey)
	if err != nil {
		return fmt.Errorf("list protected FUSE pool records: %w", err)
	}
	for _, record := range records {
		if record.RuntimeUID != "" {
			if validateOpaqueText(record.RuntimeUID, false) != nil {
				return state.ErrFUSEPoolCorrupt
			}
			protected[record.RuntimeUID] = struct{}{}
		}
	}
	return reconciler.ReconcileOrphanedResources(ctx, protected)
}

// Stop drains the pool and cleans up.
func (m *Manager) Stop(ctx context.Context) {
	_, span := telemetry.Tracer().Start(ctx, "sandbox.Manager.Stop")
	defer span.End()

	m.stopOnce.Do(func() {
		m.lifecycleMu.Lock()
		m.stopping = true
		m.cancelControl()
		close(m.stopCh)
		m.lifecycleMu.Unlock()
		go m.finishShutdown()
	})
	select {
	case <-m.shutdownDone:
	case <-ctx.Done():
	}
}

func (m *Manager) finishShutdown() {
	defer close(m.shutdownDone)
	m.createWG.Wait()
	m.mu.RLock()
	lifecycles := make([]*fuseSandboxLifecycle, 0, len(m.fuseLifecycles))
	for _, lifecycle := range m.fuseLifecycles {
		lifecycles = append(lifecycles, lifecycle)
	}
	m.mu.RUnlock()
	for _, lifecycle := range lifecycles {
		m.runFUSETeardown(context.Background(), lifecycle, ErrSandboxNotReady)
	}
	m.wg.Wait()
	if m.fusePool != nil {
		_ = m.fusePool.Stop(context.Background())
	}
	m.pool.Drain(context.Background())
}

// Create creates a new sandbox.
func (m *Manager) Create(ctx context.Context, cfg SandboxConfig) (*Sandbox, error) {
	m.lifecycleMu.Lock()
	if m.stopping {
		m.lifecycleMu.Unlock()
		return nil, ErrSandboxNotReady
	}
	m.createWG.Add(1)
	m.lifecycleMu.Unlock()
	defer m.createWG.Done()
	cfg = cloneSandboxConfig(cfg)
	createCtx, cancelCreate := context.WithCancel(ctx)
	stopCreate := context.AfterFunc(m.controlCtx, cancelCreate)
	defer stopCreate()
	defer cancelCreate()
	spanCtx, span := telemetry.Tracer().Start(createCtx, "sandbox.Manager.Create")
	defer span.End()
	if m.config.WorkspaceMode == "fuse" && cfg.WorkspacePath != "" {
		return m.createFUSESandbox(spanCtx, cfg)
	}

	createStart := time.Now()

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = m.config.DefaultTimeout
	}

	var timeoutDuration time.Duration
	if timeout < 0 {
		// -1 means never expire
		timeoutDuration = -1
	} else {
		timeoutDuration = time.Duration(timeout) * time.Second
	}

	m.mu.Lock()
	id := fmt.Sprintf("sandbox-%s", randSuffix(randSuffixLen))
	m.mu.Unlock()

	var info *runtime.SandboxInfo
	var err error
	var bindMounted bool
	var source string

	useBindMount := cfg.WorkspacePath != "" && m.fsMeta != nil && m.fsMeta.Provider == storage.ProviderLocal

	if cfg.Network.Enabled || useBindMount || cfg.Resources.TmpDisk != "" {
		source = "direct"
		spec := m.buildSpec(id, cfg)
		if cfg.Resources.TmpDisk != "" {
			if spec.Memory == "" {
				spec.Memory = m.config.PoolConfig.Memory
				spec.MemoryRequest = m.config.PoolConfig.MemoryRequest
			}
			if spec.CPU == "" {
				spec.CPU = m.config.PoolConfig.CPU
				spec.CPURequest = m.config.PoolConfig.CPURequest
			}
			if spec.Disk == "" {
				spec.Disk = m.config.PoolConfig.Disk
			}
		}
		if useBindMount {
			hostPath := m.resolveLocalWorkspacePath(cfg.WorkspacePath)
			spec.Mounts = append(spec.Mounts, runtime.Mount{
				HostPath:      hostPath,
				ContainerPath: "/workspace",
			})
			bindMounted = true
		}
		info, err = m.runtime.CreateSandbox(spanCtx, spec)
		if err != nil {
			telemetry.Error(err, span)
			metrics.RecordSandboxCreate(spanCtx, source, "error", 0)
			metrics.RecordError(spanCtx, "create_failed")
			return nil, fmt.Errorf("create sandbox: %w", err)
		}
	} else {
		source = "pool"
		info, err = m.pool.Acquire(spanCtx)
		if err != nil {
			telemetry.Error(err, span)
			metrics.RecordSandboxCreate(spanCtx, source, "error", 0)
			metrics.RecordError(spanCtx, "create_failed")
			return nil, fmt.Errorf("acquire container: %w", err)
		}
		// Remove pool label and update sandbox.id so the pod is correctly
		// identified after being taken from the pool (effective on K8s).
		nilVal := (*string)(nil)
		_ = m.runtime.UpdateLabels(spanCtx, info.RuntimeID, map[string]*string{
			"sandbox.pool": nilVal,
			"sandbox.id":   &id,
		})
	}
	if err := spanCtx.Err(); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), workspaceCleanupTimeout)
		defer cancel()
		_ = m.runtime.RemoveSandbox(cleanupCtx, info.RuntimeID)
		if source == "pool" {
			m.pool.NotifyRemoved()
		}
		return nil, errors.Join(ErrSandboxNotReady, err)
	}

	// Rename container for easier identification (best-effort)
	_ = m.runtime.RenameSandbox(spanCtx, info.RuntimeID, id)

	sb := &Sandbox{
		ID:        id,
		Config:    cfg,
		State:     StateReady,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		RuntimeID: info.RuntimeID,
		Timeout:   timeoutDuration,
	}

	// Install dependencies if requested
	if len(cfg.Dependencies) > 0 {
		installCmd := buildInstallCommand(cfg.Dependencies)
		if installCmd != "" {
			depStart := time.Now()
			if _, err := m.runtime.Exec(spanCtx, info.RuntimeID, runtime.ExecRequest{
				Command: installCmd,
				WorkDir: "/workspace",
				Timeout: 120, // 2 min for dependency install
			}); err != nil {
				logger.Error(spanCtx, "Create: install dependencies failed",
					logger.AddField("sandbox_id", id),
					logger.AddField("runtime_id", info.RuntimeID),
					logger.ErrorField(err),
				)
				metrics.RecordDependencyInstall(spanCtx, "error", time.Since(depStart).Seconds())
				// Cleanup on failure
				_ = m.runtime.RemoveSandbox(spanCtx, info.RuntimeID)
				metrics.RecordSandboxCreate(spanCtx, source, "error", 0)
				metrics.RecordError(spanCtx, "install_dependencies_failed")
				return nil, fmt.Errorf("install dependencies: %w", err)
			}
			metrics.RecordDependencyInstall(spanCtx, "success", time.Since(depStart).Seconds())
		}
	}

	m.mu.Lock()
	m.sandboxes[id] = sb
	m.mu.Unlock()

	// Persist persistent sandboxes to session store
	if cfg.Mode == ModePersistent && m.sessions != nil {
		m.mu.RLock()
		sessionSnapshot := cloneSandbox(sb)
		m.mu.RUnlock()
		if err := m.sessions.Save(spanCtx, &sessionSnapshot); err != nil {
			logger.Error(spanCtx, "Create: persist sandbox to session store failed",
				logger.AddField("sandbox_id", id),
				logger.ErrorField(err),
			)
		}
	}

	// Auto-mount workspace if specified
	if cfg.WorkspacePath != "" {
		if bindMounted {
			// Bind mount: just register the scoped FS, no file copy needed.
			if err := m.registerWorkspace(spanCtx, id, cfg.WorkspacePath); err != nil {
				_ = m.runtime.RemoveSandbox(spanCtx, info.RuntimeID)
				m.mu.Lock()
				delete(m.sandboxes, id)
				m.mu.Unlock()
				metrics.RecordSandboxCreate(spanCtx, source, "error", 0)
				metrics.RecordError(spanCtx, "register_workspace_failed")
				return nil, fmt.Errorf("register workspace: %w", err)
			}
		} else {
			if err := m.MountWorkspace(spanCtx, id, cfg.WorkspacePath, cfg.WorkspaceSyncExclude); err != nil {
				_ = m.runtime.RemoveSandbox(spanCtx, info.RuntimeID)
				m.mu.Lock()
				delete(m.sandboxes, id)
				m.mu.Unlock()
				metrics.RecordSandboxCreate(spanCtx, source, "error", 0)
				metrics.RecordError(spanCtx, "mount_workspace_failed")
				return nil, fmt.Errorf("mount workspace: %w", err)
			}
		}
	}

	span.SetAttributes(attribute.String("sandbox.id", id))
	metrics.SandboxActiveGauge.Add(spanCtx, 1)
	metrics.RecordSandboxCreate(spanCtx, source, "success", time.Since(createStart).Seconds())
	m.mu.RLock()
	result := cloneSandbox(sb)
	m.mu.RUnlock()
	return &result, nil
}

func (m *Manager) createFUSESandbox(ctx context.Context, cfg SandboxConfig) (_ *Sandbox, returnErr error) {
	if m.fusePool == nil || m.config.WorkspaceCoordinator == nil || m.config.WorkspaceObjectClient == nil || m.fsMeta == nil || m.sessions == nil || m.fusePool.spec.WorkspaceFUSE == nil {
		return nil, ErrInvalidFUSEPoolConfig
	}
	mountStarted := time.Now()
	defer func() {
		result := "success"
		if returnErr != nil {
			result = "error"
		}
		spec := m.fusePool.spec.WorkspaceFUSE
		metrics.RecordWorkspaceMount(ctx, spec.RuntimeType, spec.Provider, result, time.Since(mountStarted).Seconds())
	}()
	if !fuseResourcesCompatible(cfg.Resources, m.fusePool.spec) {
		return nil, errors.Join(ErrInvalidFUSEPoolConfig, errors.New("requested resources do not match the prepared FUSE pool"))
	}
	prefix, err := storage.BuildWorkspacePrefix(m.fsMeta.SubPath, cfg.WorkspacePath)
	if err != nil {
		return nil, err
	}
	id := fmt.Sprintf("sandbox-%s", randSuffix(randSuffixLen))
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = m.config.DefaultTimeout
	}
	timeoutDuration := time.Duration(timeout) * time.Second
	if timeout < 0 {
		timeoutDuration = -1
	}

	txnCtx, cancelTxn := context.WithCancel(ctx)
	stopTxn := context.AfterFunc(m.controlCtx, cancelTxn)
	defer stopTxn()
	defer cancelTxn()
	record, err := m.fusePool.Acquire(txnCtx, m.fusePool.poolKey)
	if err != nil {
		return nil, fmt.Errorf("acquire FUSE sandbox: %w", err)
	}
	claim := &fuseBindingClaim{record: *record}
	m.mu.Lock()
	m.fuseInFlight[record.PreparationID] = claim
	m.mu.Unlock()

	var lease *WorkspaceLease
	var renewal *WorkspaceLeaseRenewal
	bindingStarted := false
	var ambiguousAfter *state.FUSEPoolRecord
	published := false
	defer func() {
		if published {
			return
		}
		m.mu.RLock()
		cleanupRecord := claim.record
		m.mu.RUnlock()
		cleanupErr := m.cleanupFailedFUSECreate(cleanupRecord, ambiguousAfter, lease, renewal, claim, bindingStarted)
		if cleanupErr != nil {
			m.scheduleFailedFUSECreateCleanup(cleanupRecord, ambiguousAfter, lease, renewal, claim, bindingStarted)
		} else {
			m.mu.Lock()
			delete(m.fuseInFlight, record.PreparationID)
			m.mu.Unlock()
		}
		returnErr = errors.Join(returnErr, cleanupErr)
	}()

	lease, err = m.config.WorkspaceCoordinator.Acquire(txnCtx, WorkspaceLeaseRequest{
		Provider:        m.fusePool.spec.WorkspaceFUSE.Provider,
		StorageIdentity: m.fusePool.spec.WorkspaceFUSE.StorageIdentity,
		Bucket:          m.fusePool.spec.WorkspaceFUSE.Bucket,
		Prefix:          prefix,
		SandboxID:       id,
		Runtime:         m.fusePool.spec.WorkspaceFUSE.RuntimeType,
		RuntimeID:       record.RuntimeID,
	})
	if err != nil {
		bindingStarted = errors.Is(err, ErrWorkspaceAcquireCleanupUnconfirmed)
		return nil, fmt.Errorf("acquire workspace lease: %w", err)
	}
	var lifecycle *fuseSandboxLifecycle
	renewal, err = m.config.WorkspaceCoordinator.StartRenewal(context.Background(), lease, func(lost error) {
		m.mu.Lock()
		claim.lost = true
		published := claim.published
		publishedLifecycle := claim.lifecycle
		m.mu.Unlock()
		cancelTxn()
		if published && publishedLifecycle != nil {
			m.scheduleFUSETeardown(publishedLifecycle, lost)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("start workspace lease renewal: %w", err)
	}
	if err = storage.PrepareWorkspacePrefix(txnCtx, m.config.WorkspaceObjectClient, prefix, m.config.WorkspaceMarkerProfile); err != nil {
		return nil, err
	}
	err = m.config.WorkspaceCoordinator.BindRuntime(txnCtx, lease, record.RuntimeUID)
	bindingStarted = lease.RuntimeBindingMatches(record.RuntimeUID)
	if err != nil {
		return nil, fmt.Errorf("bind workspace runtime: %w", err)
	}
	auth, err := m.config.WorkspaceCoordinator.ConsumeMountAttempt(txnCtx, lease, record.PoolKey)
	if err != nil {
		return nil, fmt.Errorf("consume workspace mount attempt: %w", err)
	}
	nextRecord, err := m.fusePool.repo.Transition(txnCtx, record.PreparationID, state.FUSEPoolReserved, state.FUSEPoolBinding, record.ReservationToken, record.Revision)
	if err != nil {
		possibleAfter := expectedPublication(claim.record, state.FUSEPoolBinding, claim.record.ReservationToken)
		ambiguousAfter = &possibleAfter
		return nil, fmt.Errorf("publish FUSE binding: %w", err)
	}
	record = nextRecord
	m.mu.Lock()
	claim.record = *record
	m.mu.Unlock()
	runtimeRef, refErr := runtime.NewRuntimeRef(record.RuntimeID, record.RuntimeUID)
	if refErr != nil {
		return nil, fmt.Errorf("construct exact FUSE runtime reference: %w", refErr)
	}
	if err = m.runtime.AuthorizeWorkspaceMount(txnCtx, runtimeRef, auth); err != nil {
		return nil, fmt.Errorf("authorize workspace mount: %w", err)
	}
	if err = m.runtime.UpdateFUSENetwork(txnCtx, runtimeRef, cfg.Network.Enabled, cfg.Network.Whitelist, cfg.Network.BlockPrivate); err != nil {
		return nil, fmt.Errorf("update sandbox network: %w", err)
	}
	readyInfo, err := m.runtime.WaitSandboxReady(txnCtx, runtimeRef, auth.LeaseGeneration)
	if err != nil {
		return nil, fmt.Errorf("wait FUSE sandbox ready: %w", err)
	}
	if readyInfo == nil || readyInfo.RuntimeID != record.RuntimeID || readyInfo.RuntimeUID != record.RuntimeUID {
		return nil, fmt.Errorf("wait FUSE sandbox ready: runtime identity changed")
	}
	nextRecord, err = m.fusePool.repo.Transition(txnCtx, record.PreparationID, state.FUSEPoolBinding, state.FUSEPoolConsumed, record.ReservationToken, record.Revision)
	if err != nil {
		possibleAfter := expectedPublication(claim.record, state.FUSEPoolConsumed, claim.record.ReservationToken)
		ambiguousAfter = &possibleAfter
		return nil, fmt.Errorf("consume FUSE sandbox: %w", err)
	}
	record = nextRecord
	m.mu.Lock()
	claim.record = *record
	m.mu.Unlock()
	if len(cfg.Dependencies) > 0 {
		if command := buildInstallCommand(cfg.Dependencies); command != "" {
			if _, err = m.runtime.Exec(txnCtx, record.RuntimeID, runtime.ExecRequest{Command: command, WorkDir: "/workspace", Timeout: 120}); err != nil {
				return nil, fmt.Errorf("install dependencies: %w", err)
			}
		}
	}
	now := time.Now()
	owner := lease.OwnerSnapshot()
	sb := &Sandbox{
		ID: id, Config: cfg, State: StateReady, CreatedAt: now, UpdatedAt: now,
		RuntimeID: record.RuntimeID, RuntimeUID: record.RuntimeUID, Timeout: timeoutDuration,
		Workspace: &WorkspaceInfo{
			RootPath: cfg.WorkspacePath, MountedAt: now, LastHealthyAt: now,
			MountType: WorkspaceMountFUSE, MountState: WorkspaceMountReady, Driver: m.fusePool.spec.WorkspaceFUSE.Driver,
			Owner: owner, LeaseGeneration: owner.Generation,
			FUSEPreparationID: record.PreparationID, FUSEPoolKey: record.PoolKey,
			FUSEReservationToken: record.ReservationToken, FUSERecordRevision: record.Revision,
		},
	}
	gate := newOperationGate(true)
	lifecycleCtx, cancelLifecycle := context.WithCancel(context.Background())
	lifecycle = &fuseSandboxLifecycle{sandboxID: id, gate: gate, lease: lease, renewal: renewal, record: *record, cancel: cancelLifecycle}
	_ = lifecycleCtx
	m.mu.Lock()
	sessionIdentity := cloneSandbox(sb)
	claim.sessionIdentity = &sessionIdentity
	m.mu.Unlock()
	if err = m.publishSandboxAndSession(txnCtx, sb, gate, lifecycle, claim); err != nil {
		cancelLifecycle()
		lifecycle = nil
		return nil, err
	}
	published = true
	m.mu.RLock()
	result := cloneSandbox(sb)
	m.mu.RUnlock()
	m.startFUSEWatcher(lifecycleCtx, lifecycle)
	return &result, nil
}

func fuseResourcesCompatible(requested ResourceLimits, prepared runtime.SandboxSpec) bool {
	return (requested.Memory == "" || requested.Memory == prepared.Memory) &&
		(requested.CPU == "" || requested.CPU == prepared.CPU) &&
		(requested.Disk == "" || requested.Disk == prepared.Disk) &&
		(requested.TmpDisk == "" || requested.TmpDisk == prepared.TmpDisk)
}

func (m *Manager) publishSandboxAndSession(ctx context.Context, sb *Sandbox, gate *operationGate, lifecycle *fuseSandboxLifecycle, claim *fuseBindingClaim) error {
	if err := m.sessions.Save(ctx, sb); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workspaceCleanupTimeout)
		defer cancel()
		return errors.Join(fmt.Errorf("save FUSE sandbox session: %w", err), m.sessions.RemoveExact(cleanupCtx, sb))
	}
	m.mu.Lock()
	if ctx.Err() != nil || claim.lost {
		m.mu.Unlock()
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workspaceCleanupTimeout)
		defer cancel()
		return errors.Join(ErrWorkspaceLeaseLost, m.sessions.RemoveExact(cleanupCtx, sb))
	}
	m.lifecycleMu.Lock()
	stopping := m.stopping
	m.lifecycleMu.Unlock()
	if stopping {
		m.mu.Unlock()
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workspaceCleanupTimeout)
		defer cancel()
		return errors.Join(ErrSandboxNotReady, m.sessions.RemoveExact(cleanupCtx, sb))
	}
	m.sandboxes[sb.ID] = sb
	m.operationGates[sb.ID] = gate
	m.fuseLifecycles[sb.ID] = lifecycle
	delete(m.fuseInFlight, lifecycle.record.PreparationID)
	claim.published = true
	claim.lifecycle = lifecycle
	m.mu.Unlock()
	return nil
}

func (m *Manager) cleanupFailedFUSECreate(record state.FUSEPoolRecord, ambiguousAfter *state.FUSEPoolRecord, lease *WorkspaceLease, renewal *WorkspaceLeaseRenewal, claim *fuseBindingClaim, bindingStarted bool) error {
	timeout := workspaceCleanupTimeout
	if bindingStarted {
		timeout = m.fuseTeardownTimeout()
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if !bindingStarted {
		if lease != nil {
			if err := m.config.WorkspaceCoordinator.Release(ctx, lease, runtime.TerminationEvidence{}); err != nil {
				return err
			}
		}
		if renewal != nil {
			renewal.Stop()
		}
		m.mu.Lock()
		claim.returning = true
		m.mu.Unlock()
		if err := m.fusePool.ReturnPrepared(ctx, record); errors.Is(err, ErrFUSEPoolReturnFenced) {
			return nil
		} else {
			return err
		}
	}
	var claimed *state.FUSEPoolRecord
	var err error
	m.mu.RLock()
	if claim.cleanupRecord != nil {
		copy := *claim.cleanupRecord
		claimed = &copy
	}
	m.mu.RUnlock()
	if claimed == nil {
		if ambiguousAfter != nil {
			claimed, err = m.fusePool.claimAmbiguousCleanup(record, *ambiguousAfter)
		} else {
			claimed, err = m.fusePool.ClaimSingleUseCleanup(ctx, record)
		}
		if err == nil {
			m.mu.Lock()
			copy := *claimed
			claim.cleanupRecord = &copy
			m.mu.Unlock()
		}
	}
	if err != nil {
		return err
	}
	if err := m.fusePool.RemoveClaimedRuntime(ctx, *claimed); err != nil {
		return err
	}
	fencer, ok := m.runtime.(runtime.RuntimeFencer)
	if !ok {
		return ErrRuntimeExitUnconfirmed
	}
	evidence, err := fencer.ConfirmTerminated(ctx, record.RuntimeID, record.RuntimeUID)
	if err != nil {
		return err
	}
	if lease != nil {
		if err := m.config.WorkspaceCoordinator.Release(ctx, lease, evidence); err != nil {
			return err
		}
	}
	if renewal != nil {
		renewal.Stop()
	}
	m.mu.RLock()
	sessionIdentity := claim.sessionIdentity
	m.mu.RUnlock()
	if sessionIdentity != nil && m.sessions != nil {
		workspace := sessionIdentity.Workspace
		if workspace == nil {
			return ErrSessionPublicationConflict
		}
		if err := m.sessions.RemoveMatchingFUSESession(ctx, sessionIdentity.ID, sessionIdentity.RuntimeID, sessionIdentity.RuntimeUID, workspace.FUSEPreparationID, workspace.LeaseGeneration); err != nil {
			return err
		}
	}
	return m.fusePool.CompleteClaimedCleanup(ctx, *claimed)
}

func (m *Manager) scheduleFailedFUSECreateCleanup(record state.FUSEPoolRecord, ambiguousAfter *state.FUSEPoolRecord, lease *WorkspaceLease, renewal *WorkspaceLeaseRenewal, claim *fuseBindingClaim, bindingStarted bool) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for {
			if err := m.cleanupFailedFUSECreate(record, ambiguousAfter, lease, renewal, claim, bindingStarted); err == nil {
				m.mu.Lock()
				delete(m.fuseInFlight, record.PreparationID)
				m.mu.Unlock()
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
}

func (m *Manager) guardFUSEPoolRecord(ctx context.Context, record state.FUSEPoolRecord) (FUSEPoolDisposition, error) {
	m.mu.RLock()
	claim := m.fuseInFlight[record.PreparationID]
	if claim != nil {
		returning := claim.returning
		m.mu.RUnlock()
		if returning {
			return FUSEPoolPristine, nil
		}
		return FUSEPoolProtected, nil
	}
	for id, lifecycle := range m.fuseLifecycles {
		if lifecycle.record.RuntimeUID == record.RuntimeUID || lifecycle.record.PreparationID == record.PreparationID {
			_ = id
			m.mu.RUnlock()
			return FUSEPoolProtected, nil
		}
	}
	m.mu.RUnlock()
	if m.sessions != nil {
		ids, err := m.sessions.List(ctx)
		if err != nil {
			return FUSEPoolDispositionUnknown, err
		}
		for _, id := range ids {
			sb, err := m.sessions.Load(ctx, id)
			if err != nil {
				return FUSEPoolDispositionUnknown, err
			}
			if sb.RuntimeUID == record.RuntimeUID || (sb.Workspace != nil && sb.Workspace.FUSEPreparationID == record.PreparationID) {
				return FUSEPoolProtected, nil
			}
		}
	}
	if m.config.WorkspaceCoordinator != nil {
		protected, err := m.config.WorkspaceCoordinator.HasRuntimeOwner(ctx, record.RuntimeID, record.RuntimeUID)
		if err != nil {
			return FUSEPoolDispositionUnknown, err
		}
		if protected {
			return FUSEPoolProtected, nil
		}
	}
	if record.State == state.FUSEPoolBinding || record.State == state.FUSEPoolConsumed {
		return FUSEPoolAbandoned, nil
	}
	return FUSEPoolPristine, nil
}

func (m *Manager) startFUSEWatcher(ctx context.Context, lifecycle *fuseSandboxLifecycle) {
	interval := m.config.FUSEHealthInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-m.stopCh:
				m.runFUSETeardown(context.Background(), lifecycle, ErrSandboxNotReady)
				return
			case <-ticker.C:
				if err := m.checkFUSELifecycle(ctx, lifecycle); err != nil {
					metrics.RecordWorkspaceUnavailable(ctx, "health_check")
					metrics.RecordWorkspaceFUSEError(ctx, "health")
					m.runFUSETeardown(context.Background(), lifecycle, err)
					return
				}
			}
		}
	}()
}

func (m *Manager) checkFUSELifecycle(ctx context.Context, lifecycle *fuseSandboxLifecycle) error {
	info, err := m.runtime.GetSandbox(ctx, lifecycle.record.RuntimeID)
	if err != nil {
		return err
	}
	if info == nil || info.RuntimeUID != lifecycle.record.RuntimeUID || info.State != "running" {
		return ErrSandboxNotReady
	}
	health, err := m.runtime.WorkspaceHealth(ctx, runtime.RuntimeRef{ID: lifecycle.record.RuntimeID, UID: lifecycle.record.RuntimeUID})
	if err != nil {
		return err
	}
	if health != nil && m.fusePool != nil && m.fusePool.spec.WorkspaceFUSE != nil {
		spec := m.fusePool.spec.WorkspaceFUSE
		metrics.RecordWorkspaceFUSECache(ctx, spec.RuntimeType, spec.Provider, health.CacheBytes)
		if health.CacheExceeded || (health.CacheLimitBytes > 0 && health.CacheBytes >= health.CacheLimitBytes) {
			metrics.RecordWorkspaceFUSEError(ctx, "cache_threshold")
		}
	}
	if health == nil || !health.Ready || health.MountType != "fuse" || health.RuntimeUID != lifecycle.record.RuntimeUID || health.Generation != lifecycle.lease.OwnerSnapshot().Generation || health.RestartCount != 0 || health.RestartDetected {
		return ErrSandboxNotReady
	}
	m.mu.Lock()
	if sb := m.sandboxes[lifecycle.sandboxID]; sb != nil && sb.Workspace != nil {
		sb.Workspace.LastHealthyAt = time.Now()
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) teardownFUSESandbox(lifecycle *fuseSandboxLifecycle, cause error) {
	if lifecycle == nil {
		return
	}
	lifecycle.teardownMu.Lock()
	if lifecycle.teardownRunning || lifecycle.teardownDone {
		lifecycle.teardownMu.Unlock()
		return
	}
	lifecycle.teardownRunning = true
	lifecycle.teardownMu.Unlock()
	defer func() {
		lifecycle.teardownMu.Lock()
		lifecycle.teardownRunning = false
		lifecycle.teardownMu.Unlock()
	}()

	lifecycle.cancel()
	_ = cause
	if !lifecycle.gateClosed {
		if err := lifecycle.gate.CloseAndWait(context.Background()); err != nil {
			m.logFUSETeardownFailure(lifecycle, "close-operation-gate", err)
			return
		}
		lifecycle.gateClosed = true
	}
	if !lifecycle.renewalStopped {
		lifecycle.renewal.Stop()
		lifecycle.renewalStopped = true
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.fuseTeardownTimeout())
	defer cancel()
	ref := runtime.RuntimeRef{ID: lifecycle.record.RuntimeID, UID: lifecycle.record.RuntimeUID}
	generation := lifecycle.lease.OwnerSnapshot().Generation
	if !lifecycle.quiesceAttempted {
		token, err := m.runtime.QuiesceWorkspace(ctx, ref, generation)
		lifecycle.quiesceAttempted = true
		if err == nil && token.RuntimeUID == ref.UID && token.Generation == generation && token.Opaque != "" {
			lifecycle.quiesced = true
		} else if err != nil {
			logger.Warn(context.Background(), "FUSE sandbox quiesce failed; continuing with fail-closed runtime removal",
				logger.AddField("sandbox_id", lifecycle.sandboxID),
				logger.AddField("runtime_id", lifecycle.record.RuntimeID),
				logger.ErrorField(err),
			)
		}
	}
	if lifecycle.quiesced && !lifecycle.flushAttempted {
		// Destruction must remain possible when a best-effort flush fails. The
		// immutable runtime is removed next and the owner is retained until that
		// removal plus remote probe cleanup are both proven.
		started := time.Now()
		flushErr := m.runtime.FlushWorkspace(ctx, ref, generation)
		result := "success"
		if flushErr != nil {
			result = "error"
			metrics.RecordWorkspaceFUSEError(ctx, "flush")
			logger.Warn(context.Background(), "FUSE sandbox pre-removal flush failed; strong shutdown must still prove durability",
				logger.AddField("sandbox_id", lifecycle.sandboxID),
				logger.AddField("runtime_id", lifecycle.record.RuntimeID),
				logger.ErrorField(flushErr),
			)
		}
		if m.fusePool.spec.WorkspaceFUSE != nil {
			spec := m.fusePool.spec.WorkspaceFUSE
			metrics.RecordWorkspaceFlush(ctx, spec.RuntimeType, spec.Provider, result, time.Since(started).Seconds())
		}
		lifecycle.flushAttempted = true
	}
	if lifecycle.claimed == nil {
		claimed, err := m.fusePool.ClaimSingleUseCleanup(ctx, lifecycle.record)
		if err != nil {
			m.logFUSETeardownFailure(lifecycle, "claim-pool-cleanup", err)
			return
		}
		lifecycle.claimed = claimed
	}
	if !lifecycle.runtimeRemoved {
		if err := m.fusePool.RemoveClaimedRuntime(ctx, *lifecycle.claimed); err != nil {
			m.logFUSETeardownFailure(lifecycle, "remove-runtime", err)
			return
		}
		lifecycle.runtimeRemoved = true
	}
	if lifecycle.evidence.RuntimeUID == "" {
		fencer, ok := m.runtime.(runtime.RuntimeFencer)
		if !ok {
			m.logFUSETeardownFailure(lifecycle, "confirm-runtime-termination", errors.New("runtime has no termination fencer"))
			return
		}
		evidence, err := fencer.ConfirmTerminated(ctx, lifecycle.record.RuntimeID, lifecycle.record.RuntimeUID)
		if err != nil {
			m.logFUSETeardownFailure(lifecycle, "confirm-runtime-termination", err)
			return
		}
		lifecycle.evidence = evidence
	}
	if !lifecycle.unmountRecorded && m.fusePool.spec.WorkspaceFUSE != nil {
		metrics.RecordWorkspaceUnmount(ctx, m.fusePool.spec.WorkspaceFUSE.RuntimeType, "success")
		lifecycle.unmountRecorded = true
	}
	if !lifecycle.probeRemoved {
		probeName, err := fuseprotocol.DeriveProbeObjectName(lifecycle.record.RuntimeUID, generation)
		if err != nil {
			m.logFUSETeardownFailure(lifecycle, "derive-probe-object", err)
			return
		}
		probeKey := lifecycle.lease.OwnerSnapshot().Prefix + probeName
		if err := m.config.WorkspaceObjectClient.DeleteObject(ctx, probeKey); err != nil {
			m.logFUSETeardownFailure(lifecycle, "delete-probe-object", err)
			return
		}
		exists, err := m.config.WorkspaceObjectClient.HeadObject(ctx, probeKey)
		if err != nil || exists {
			if err == nil {
				err = errors.New("probe object still exists after deletion")
			}
			m.logFUSETeardownFailure(lifecycle, "verify-probe-object-deletion", err)
			return
		}
		lifecycle.probeRemoved = true
	}
	if !lifecycle.leaseReleased {
		if err := m.config.WorkspaceCoordinator.Release(ctx, lifecycle.lease, lifecycle.evidence); err != nil {
			m.logFUSETeardownFailure(lifecycle, "release-workspace-lease", err)
			return
		}
		lifecycle.leaseReleased = true
	}
	if !lifecycle.sessionRemoved && m.sessions != nil {
		if err := m.sessions.RemoveMatchingFUSESession(ctx, lifecycle.sandboxID, lifecycle.record.RuntimeID, lifecycle.record.RuntimeUID, lifecycle.record.PreparationID, lifecycle.lease.OwnerSnapshot().Generation); err != nil {
			m.logFUSETeardownFailure(lifecycle, "remove-session", err)
			return
		}
		lifecycle.sessionRemoved = true
	}
	if err := m.fusePool.CompleteClaimedCleanup(ctx, *lifecycle.claimed); err != nil {
		m.logFUSETeardownFailure(lifecycle, "complete-pool-cleanup", err)
		return
	}
	lifecycle.poolRemoved = true
	m.mu.Lock()
	delete(m.sandboxes, lifecycle.sandboxID)
	delete(m.operationGates, lifecycle.sandboxID)
	delete(m.fuseLifecycles, lifecycle.sandboxID)
	m.mu.Unlock()
	lifecycle.teardownMu.Lock()
	lifecycle.teardownDone = true
	lifecycle.teardownMu.Unlock()
}

func (m *Manager) logFUSETeardownFailure(lifecycle *fuseSandboxLifecycle, stage string, err error) {
	if lifecycle == nil || err == nil {
		return
	}
	now := time.Now()
	lifecycle.teardownMu.Lock()
	if lifecycle.lastFailureStage == stage && now.Sub(lifecycle.lastFailureAt) < 10*time.Second {
		lifecycle.teardownMu.Unlock()
		return
	}
	lifecycle.lastFailureStage = stage
	lifecycle.lastFailureAt = now
	lifecycle.teardownMu.Unlock()
	logger.Error(context.Background(), "FUSE sandbox teardown stage failed",
		logger.AddField("sandbox_id", lifecycle.sandboxID),
		logger.AddField("runtime_id", lifecycle.record.RuntimeID),
		logger.AddField("stage", stage),
		logger.ErrorField(err),
	)
}

func (m *Manager) fuseTeardownTimeout() time.Duration {
	const controlMargin = 30 * time.Second
	timeout := controlMargin
	if m == nil || m.fusePool == nil || m.fusePool.spec.WorkspaceFUSE == nil {
		return workspaceCleanupTimeout
	}
	// Destruction flushes once after quiescing and the strong supervisor
	// shutdown flushes again before unmounting. Leave a bounded control-plane
	// margin for quiesce, acknowledgements, and exact runtime termination proof.
	for _, duration := range []time.Duration{
		m.fusePool.spec.WorkspaceFUSE.FlushTimeout,
		m.fusePool.spec.WorkspaceFUSE.FlushTimeout,
		m.fusePool.spec.WorkspaceFUSE.UnmountTimeout,
	} {
		if duration <= 0 {
			continue
		}
		if duration > time.Duration(1<<63-1)-timeout {
			return time.Duration(1<<63 - 1)
		}
		timeout += duration
	}
	if timeout < workspaceCleanupTimeout {
		return workspaceCleanupTimeout
	}
	return timeout
}

func (m *Manager) scheduleFUSETeardown(lifecycle *fuseSandboxLifecycle, cause error) {
	m.lifecycleMu.Lock()
	if m.stopping {
		m.lifecycleMu.Unlock()
		return
	}
	m.wg.Add(1)
	m.lifecycleMu.Unlock()
	go func() {
		defer m.wg.Done()
		m.runFUSETeardown(context.Background(), lifecycle, cause)
	}()
}

func (m *Manager) runFUSETeardown(ctx context.Context, lifecycle *fuseSandboxLifecycle, cause error) {
	for {
		m.teardownFUSESandbox(lifecycle, cause)
		lifecycle.teardownMu.Lock()
		done := lifecycle.teardownDone
		lifecycle.teardownMu.Unlock()
		if done {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Get retrieves a sandbox by ID. Returns a copy to prevent external data races.
func (m *Manager) Get(ctx context.Context, id string) (Sandbox, error) {
	spanCtx, span := telemetry.Tracer().Start(ctx, "sandbox.Manager.Get")
	defer span.End()

	sb, release, err := m.acquireSandboxOperation(spanCtx, id)
	if err != nil {
		return Sandbox{}, err
	}
	defer release()
	m.mu.RLock()
	clone := cloneSandbox(sb)
	m.mu.RUnlock()
	return clone, nil
}

func cloneSandbox(sb *Sandbox) Sandbox {
	if sb == nil {
		return Sandbox{}
	}
	clone := *sb
	clone.Config = cloneSandboxConfig(sb.Config)
	if sb.Workspace != nil {
		workspace := *sb.Workspace
		workspace.SyncExclude = append([]string(nil), sb.Workspace.SyncExclude...)
		if sb.Workspace.LastFlushedAt != nil {
			lastFlushedAt := *sb.Workspace.LastFlushedAt
			workspace.LastFlushedAt = &lastFlushedAt
		}
		clone.Workspace = &workspace
	}
	return clone
}

func cloneSandboxConfig(config SandboxConfig) SandboxConfig {
	clone := config
	clone.Network.Whitelist = append([]string(nil), config.Network.Whitelist...)
	clone.Dependencies = append([]Dependency(nil), config.Dependencies...)
	clone.WorkspaceSyncExclude = append([]string(nil), config.WorkspaceSyncExclude...)
	return clone
}

func (m *Manager) sandboxRuntimeID(sb *Sandbox) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if sb == nil {
		return ""
	}
	return sb.RuntimeID
}

func (m *Manager) acquireSandboxOperation(ctx context.Context, id string) (*Sandbox, func(), error) {
	m.mu.RLock()
	sb, ok := m.sandboxes[id]
	gate := m.operationGates[id]
	if ok {
		if sb.Workspace != nil && sb.Workspace.MountType == WorkspaceMountFUSE {
			if gate == nil {
				m.mu.RUnlock()
				return nil, nil, fmt.Errorf("%w: %s", ErrSandboxNotReady, id)
			}
			m.mu.RUnlock()
			release, err := gate.Acquire()
			if err != nil {
				return nil, nil, fmt.Errorf("%w: %s", err, id)
			}
			m.mu.RLock()
			current := m.sandboxes[id]
			if current == nil || current != sb || m.operationGates[id] != gate || current.Workspace == nil || current.Workspace.MountType != WorkspaceMountFUSE {
				m.mu.RUnlock()
				release()
				return nil, nil, fmt.Errorf("%w: %s", ErrSandboxNotReady, id)
			}
			m.mu.RUnlock()
			return current, release, nil
		}
		m.mu.RUnlock()
		return sb, func() {}, nil
	}
	m.mu.RUnlock()

	if m.sessions != nil {
		loaded, err := m.sessions.Load(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		if loaded.Workspace != nil && loaded.Workspace.MountType == WorkspaceMountFUSE {
			return nil, nil, fmt.Errorf("%w: %s", ErrSandboxNotReady, id)
		}
		m.mu.Lock()
		if current := m.sandboxes[id]; current != nil {
			loaded = current
		} else {
			m.sandboxes[id] = loaded
		}
		m.mu.Unlock()
		return loaded, func() {}, nil
	}
	return nil, nil, fmt.Errorf("%w: %s", ErrSandboxNotFound, id)
}

// resolve looks up a sandbox by ID: first in memory, then in the session store.
// Returns a pointer to the in-memory Sandbox (caller must hold no lock).
func (m *Manager) resolve(ctx context.Context, id string) (*Sandbox, error) {
	m.mu.RLock()
	sb, ok := m.sandboxes[id]
	m.mu.RUnlock()
	if ok {
		return sb, nil
	}

	// Fall back to session store for persistent sandboxes
	if m.sessions != nil {
		sbPtr, err := m.sessions.Load(ctx, id)
		if err != nil {
			return nil, err
		}
		if sbPtr.Workspace != nil && sbPtr.Workspace.MountType == WorkspaceMountFUSE {
			return nil, fmt.Errorf("%w: %s", ErrSandboxNotReady, id)
		}
		logger.Info(ctx, "sandbox restored from session store",
			logger.AddField("sandbox_id", id),
			logger.AddField("runtime_id", sbPtr.RuntimeID),
		)
		// Register into in-memory map so subsequent lookups find it directly
		m.mu.Lock()
		m.sandboxes[id] = sbPtr
		m.mu.Unlock()

		// Restore workspace ScopedFS if workspace was mounted
		if sbPtr.Workspace != nil && sbPtr.Workspace.RootPath != "" {
			scoped, fsErr := storage.NewScopedFS(m.filesystem, sbPtr.Workspace.RootPath)
			if fsErr == nil {
				m.mu.Lock()
				m.workspaces[id] = scoped
				m.mu.Unlock()
			} else {
				logger.Error(ctx, "resolve: restore workspace scoped fs failed",
					logger.AddField("sandbox_id", id),
					logger.AddField("root_path", sbPtr.Workspace.RootPath),
					logger.ErrorField(fsErr),
				)
			}
		}

		return sbPtr, nil
	}

	return nil, fmt.Errorf("%w: %s", ErrSandboxNotFound, id)
}

// Destroy removes a sandbox.
func (m *Manager) Destroy(ctx context.Context, id string) error {
	return m.destroyWithReason(ctx, id, "manual")
}

func (m *Manager) destroyWithReason(ctx context.Context, id, reason string) error {
	ctx, span := telemetry.Tracer().Start(ctx, "sandbox.Destroy",
		trace.WithAttributes(attribute.String("sandbox.id", id)),
		trace.WithAttributes(attribute.String("reason", reason)),
	)
	defer span.End()
	m.mu.RLock()
	fuseLifecycle := m.fuseLifecycles[id]
	m.mu.RUnlock()
	if fuseLifecycle != nil {
		m.teardownFUSESandbox(fuseLifecycle, fmt.Errorf("sandbox destroy: %s", reason))
		fuseLifecycle.teardownMu.Lock()
		done := fuseLifecycle.teardownDone
		fuseLifecycle.teardownMu.Unlock()
		if !done {
			m.scheduleFUSETeardown(fuseLifecycle, fmt.Errorf("sandbox destroy retry: %s", reason))
			return fmt.Errorf("destroy FUSE sandbox cleanup is pending")
		}
		metrics.RecordSandboxDestroy(ctx, reason)
		return nil
	}

	sb, err := m.resolve(ctx, id)
	if err != nil {
		telemetry.Error(err, span)
		return err
	}

	m.mu.Lock()
	sb.State = StateDestroying
	m.mu.Unlock()

	// Clean up session store
	if m.sessions != nil {
		_ = m.sessions.Remove(ctx, id)
	}

	// Best-effort sync workspace back before destroying.
	// We do this before removing from the sandboxes map so that concurrent
	// autoSync goroutines see StateDestroying and skip this sandbox.
	m.mu.RLock()
	_, hasWS := m.workspaces[id]
	m.mu.RUnlock()
	if hasWS {
		_ = m.syncFromContainer(ctx, id, sb.RuntimeID, nil)
		m.mu.Lock()
		delete(m.workspaces, id)
		m.mu.Unlock()
	}

	m.mu.Lock()
	delete(m.sandboxes, id)
	m.mu.Unlock()

	// Remove the container
	if err := m.runtime.RemoveSandbox(ctx, sb.RuntimeID); err != nil {
		telemetry.Error(err, span)
		metrics.RecordError(ctx, "destroy_failed")
		return fmt.Errorf("remove sandbox: %w", err)
	}

	// Notify pool so it can refill
	m.pool.NotifyRemoved()
	metrics.SandboxActiveGauge.Add(ctx, -1)
	metrics.RecordSandboxDestroy(ctx, reason)

	return nil
}

// Exec executes a command in a sandbox synchronously.
func (m *Manager) Exec(ctx context.Context, id string, req runtime.ExecRequest) (*runtime.ExecResult, error) {
	parentCtx := ctx
	ctx, span := telemetry.Tracer().Start(ctx, "sandbox.Manager.Exec",
		trace.WithAttributes(
			attribute.String("sandbox.id", id),
			attribute.String("exec.language", req.Command[:min(len(req.Command), 20)]),
		),
	)
	defer span.End()

	execStart := time.Now()
	requestedTimeout := req.Timeout
	timeoutSource := execTimeoutSource(requestedTimeout)
	effectiveTimeout, err := m.resolveExecTimeout(requestedTimeout)
	span.SetAttributes(
		attribute.Int("exec.timeout.requested_seconds", requestedTimeout),
		attribute.String("exec.timeout.source", timeoutSource),
	)
	if err != nil {
		telemetry.Error(err, span)
		return nil, err
	}
	span.SetAttributes(attribute.Int("exec.timeout.effective_seconds", effectiveTimeout))
	req.Timeout = effectiveTimeout
	execCtx, cancel := newExecContext(ctx, effectiveTimeout)
	defer cancel()

	sb, release, err := m.acquireSandboxOperation(execCtx, id)
	if err != nil {
		telemetry.Error(err, span)
		metrics.RecordError(execCtx, "sandbox_not_found")
		return nil, err
	}
	defer release()

	m.mu.Lock()
	networkEnabled := sb.Config.Network.Enabled
	runtimeID := sb.RuntimeID
	networkDenied := req.RequiresNetwork && !networkEnabled
	if !networkDenied {
		sb.State = StateRunning
		sb.UpdatedAt = time.Now()
	}
	m.mu.Unlock()

	if networkDenied {
		telemetry.Error(ErrNetworkRequired, span)
		metrics.RecordError(execCtx, "network_required")
		return nil, ErrNetworkRequired
	}

	result, err := m.runtime.Exec(execCtx, runtimeID, req)

	m.mu.Lock()
	if err != nil {
		sb.State = StateError
	} else {
		sb.State = StateIdle
	}
	sb.UpdatedAt = time.Now()
	m.mu.Unlock()

	if err != nil {
		telemetry.Error(err, span)
		status := execFailureStatus(parentCtx, execCtx, err, effectiveTimeout, timeoutSource)
		metrics.RecordExec(execCtx, "sync", status, time.Since(execStart).Seconds())
		metrics.RecordError(execCtx, "exec_failed")
		if isExecTimeout(parentCtx, execCtx, err, effectiveTimeout) {
			return nil, fmt.Errorf("execute sandbox: %w", ErrExecTimeout)
		}
		return nil, err
	}

	status := "success"
	if result.ExitCode != 0 {
		status = "non_zero_exit"
	}
	metrics.RecordExec(execCtx, "sync", status, result.Duration.Seconds())
	span.SetAttributes(
		attribute.Int("exec.exit_code", result.ExitCode),
		attribute.Float64("exec.duration_s", result.Duration.Seconds()),
	)
	return result, nil
}

// ExecStream executes a command in a sandbox with streaming output.
func (m *Manager) ExecStream(ctx context.Context, id string, req runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
	parentCtx := ctx
	ctx, span := telemetry.Tracer().Start(ctx, "sandbox.Manager.ExecStream",
		trace.WithAttributes(attribute.String("sandbox.id", id)),
	)
	requestedTimeout := req.Timeout
	timeoutSource := execTimeoutSource(requestedTimeout)
	effectiveTimeout, err := m.resolveExecTimeout(requestedTimeout)
	span.SetAttributes(
		attribute.Int("exec.timeout.requested_seconds", requestedTimeout),
		attribute.String("exec.timeout.source", timeoutSource),
	)
	if err != nil {
		telemetry.Error(err, span)
		span.End()
		return nil, err
	}
	span.SetAttributes(attribute.Int("exec.timeout.effective_seconds", effectiveTimeout))
	req.Timeout = effectiveTimeout
	execCtx, cancel := newExecContext(ctx, effectiveTimeout)

	sb, release, err := m.acquireSandboxOperation(execCtx, id)
	if err != nil {
		telemetry.Error(err, span)
		metrics.RecordError(execCtx, "sandbox_not_found")
		cancel()
		span.End()
		return nil, err
	}

	m.mu.Lock()
	networkEnabled := sb.Config.Network.Enabled
	runtimeID := sb.RuntimeID
	networkDenied := req.RequiresNetwork && !networkEnabled
	if !networkDenied {
		sb.State = StateRunning
		sb.UpdatedAt = time.Now()
	}
	m.mu.Unlock()

	if networkDenied {
		telemetry.Error(ErrNetworkRequired, span)
		metrics.RecordError(execCtx, "network_required")
		cancel()
		span.End()
		release()
		return nil, ErrNetworkRequired
	}

	streamStart := time.Now()
	ch, err := m.runtime.ExecStream(execCtx, runtimeID, req)
	if err != nil {
		m.mu.Lock()
		sb.State = StateError
		sb.UpdatedAt = time.Now()
		m.mu.Unlock()
		telemetry.Error(err, span)
		status := execFailureStatus(parentCtx, execCtx, err, effectiveTimeout, timeoutSource)
		metrics.RecordExec(execCtx, "stream", status, time.Since(streamStart).Seconds())
		metrics.RecordError(execCtx, "exec_failed")
		timedOut := isExecTimeout(parentCtx, execCtx, err, effectiveTimeout)
		cancel()
		span.End()
		release()
		if timedOut {
			return nil, fmt.Errorf("execute sandbox stream: %w", ErrExecTimeout)
		}
		return nil, err
	}

	// Wrap channel to update state on completion
	outCh := make(chan runtime.StreamEvent, 64)
	go func() {
		defer span.End()
		defer cancel()
		defer release()
		defer close(outCh)

		finalized := false
		finalize := func(state State, status string, terminal *runtime.StreamEvent) {
			if finalized {
				return
			}
			finalized = true
			m.mu.Lock()
			sb.State = state
			sb.UpdatedAt = time.Now()
			m.mu.Unlock()
			if terminal != nil {
				sendStreamTerminal(outCh, *terminal)
			}
			metrics.RecordExec(execCtx, "stream", status, time.Since(streamStart).Seconds())
		}

		finishCancelled := func() {
			cause := context.Cause(execCtx)
			var terminal *runtime.StreamEvent
			if errors.Is(cause, ErrExecTimeout) {
				telemetry.Error(ErrExecTimeout, span)
				event := runtime.StreamEvent{Type: runtime.StreamError, Content: ErrExecTimeout.Error()}
				terminal = &event
			}
			status := execFailureStatus(parentCtx, execCtx, cause, effectiveTimeout, timeoutSource)
			finalize(StateError, status, terminal)
		}

		finishEvent := func(event runtime.StreamEvent) bool {
			switch event.Type {
			case runtime.StreamDone:
				finalize(StateIdle, "success", &event)
				return true
			case runtime.StreamError:
				finalize(StateError, "error", &event)
				return true
			default:
				return false
			}
		}

		finishReadyTerminal := func() bool {
			select {
			case event, ok := <-ch:
				return ok && finishEvent(event)
			default:
				return false
			}
		}

		for {
			select {
			case <-execCtx.Done():
				if finishReadyTerminal() {
					return
				}
				finishCancelled()
				return
			case event, ok := <-ch:
				if !ok {
					if context.Cause(execCtx) != nil {
						finishCancelled()
					} else {
						finalize(StateIdle, "success", nil)
					}
					return
				}
				if finishEvent(event) {
					return
				}
				select {
				case outCh <- event:
				case <-execCtx.Done():
					if finishReadyTerminal() {
						return
					}
					finishCancelled()
					return
				}
			}
		}
	}()

	return outCh, nil
}

// UploadFile uploads a file into a sandbox.
func (m *Manager) UploadFile(ctx context.Context, id, destPath string, size int64, reader io.Reader) error {
	ctx, span := telemetry.Tracer().Start(ctx, "sandbox.Manager.UploadFile",
		trace.WithAttributes(attribute.String("sandbox.id", id)),
	)
	defer span.End()

	sb, release, err := m.acquireSandboxOperation(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	if err := validatePublicFUSEFilePath(sb, destPath); err != nil {
		return err
	}

	runtimeID := m.sandboxRuntimeID(sb)
	if err := m.runtime.UploadFile(ctx, runtimeID, destPath, size, reader); err != nil {
		metrics.RecordFileOp(ctx, "upload", "error")
		return err
	}
	metrics.RecordFileOp(ctx, "upload", "success")
	return nil
}

// DownloadFile downloads a file from a sandbox.
func (m *Manager) DownloadFile(ctx context.Context, id string, srcPath string) (io.ReadCloser, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "sandbox.Manager.DownloadFile",
		trace.WithAttributes(attribute.String("sandbox.id", id)),
	)
	defer span.End()

	sb, release, err := m.acquireSandboxOperation(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := validatePublicFUSEFilePath(sb, srcPath); err != nil {
		release()
		return nil, err
	}

	runtimeID := m.sandboxRuntimeID(sb)
	rc, err := m.runtime.DownloadFile(ctx, runtimeID, srcPath)
	if err != nil {
		release()
		metrics.RecordFileOp(ctx, "download", "error")
		return nil, err
	}
	if rc == nil {
		release()
		return nil, fmt.Errorf("download file returned no reader")
	}
	metrics.RecordFileOp(ctx, "download", "success")
	return &gatedReadCloser{reader: rc, release: release}, nil
}

// ReadFileContent streams the raw content of a file from the sandbox without tar wrapping.
func (m *Manager) ReadFileContent(ctx context.Context, id string, srcPath string) (io.ReadCloser, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "sandbox.Manager.ReadFileContent",
		trace.WithAttributes(attribute.String("sandbox.id", id)),
	)
	defer span.End()

	sb, release, err := m.acquireSandboxOperation(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := validatePublicFUSEFilePath(sb, srcPath); err != nil {
		release()
		return nil, err
	}

	runtimeID := m.sandboxRuntimeID(sb)
	rc, err := m.runtime.ReadFileContent(ctx, runtimeID, srcPath)
	if err != nil {
		release()
		metrics.RecordFileOp(ctx, "read", "error")
		return nil, err
	}
	if rc == nil {
		release()
		return nil, fmt.Errorf("read file returned no reader")
	}
	metrics.RecordFileOp(ctx, "read", "success")
	return &gatedReadCloser{reader: rc, release: release}, nil
}

// GlobInfo returns files matching the glob pattern with their content.
func (m *Manager) GlobInfo(ctx context.Context, id string, pattern string) ([]runtime.FileContent, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "sandbox.Manager.GlobInfo",
		trace.WithAttributes(attribute.String("sandbox.id", id)),
	)
	defer span.End()

	sb, release, err := m.acquireSandboxOperation(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := validatePublicFUSEFilePath(sb, pattern); err != nil {
		release()
		return nil, err
	}
	runtimeID := m.sandboxRuntimeID(sb)
	files, err := m.runtime.GlobInfo(ctx, runtimeID, pattern)
	if err != nil {
		for i := range files {
			if files[i].Content != nil {
				_ = files[i].Content.Close()
			}
		}
		release()
		return files, err
	}
	if sb.Workspace != nil && sb.Workspace.MountType == WorkspaceMountFUSE {
		filtered := files[:0]
		for _, file := range files {
			if fuseprotocol.IsReservedProbeObjectName(filepath.Base(filepath.Clean(file.Path))) {
				if file.Content != nil {
					_ = file.Content.Close()
				}
				continue
			}
			filtered = append(filtered, file)
		}
		files = filtered
	}
	return holdGateForFileContents(files, release), nil
}

// DownloadFiles downloads multiple files in parallel.
func (m *Manager) DownloadFiles(ctx context.Context, id string, paths []string) ([]runtime.FileContent, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "sandbox.Manager.DownloadFiles",
		trace.WithAttributes(attribute.String("sandbox.id", id)),
	)
	defer span.End()

	sb, release, err := m.acquireSandboxOperation(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		if err := validatePublicFUSEFilePath(sb, path); err != nil {
			release()
			return nil, err
		}
	}

	runtimeID := m.sandboxRuntimeID(sb)
	files, err := m.runtime.DownloadFiles(ctx, runtimeID, paths)
	if err != nil {
		for i := range files {
			if files[i].Content != nil {
				_ = files[i].Content.Close()
			}
		}
		release()
		return files, err
	}
	return holdGateForFileContents(files, release), nil
}

// ListFiles lists files in a sandbox directory.
func (m *Manager) ListFiles(ctx context.Context, id string, dirPath string) ([]runtime.FileInfo, error) {
	sb, release, err := m.acquireSandboxOperation(ctx, id)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := validatePublicFUSEFilePath(sb, dirPath); err != nil {
		return nil, err
	}
	files, err := m.runtime.ListFiles(ctx, m.sandboxRuntimeID(sb), dirPath)
	return hideReservedFUSEFileInfos(sb, files), err
}

// ListFilesRecursive lists files recursively in a sandbox directory.
func (m *Manager) ListFilesRecursive(ctx context.Context, id string, dirPath string, maxDepth int, page int, pageSize int) (*runtime.FileListResult, error) {
	sb, release, err := m.acquireSandboxOperation(ctx, id)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := validatePublicFUSEFilePath(sb, dirPath); err != nil {
		return nil, err
	}
	runtimeID := m.sandboxRuntimeID(sb)
	if sb.Workspace != nil && sb.Workspace.MountType == WorkspaceMountFUSE {
		hiddenCount, probeErr := m.countReservedFUSEFiles(ctx, runtimeID, dirPath, maxDepth, "")
		if probeErr != nil {
			return nil, probeErr
		}
		return collectAndPaginateVisibleFUSEFiles(page, pageSize, hiddenCount, func(fetchPage, fetchPageSize int) (*runtime.FileListResult, error) {
			return m.runtime.ListFilesRecursive(ctx, runtimeID, dirPath, maxDepth, fetchPage, fetchPageSize)
		})
	}
	return m.runtime.ListFilesRecursive(ctx, runtimeID, dirPath, maxDepth, page, pageSize)
}

// GlobFiles finds files matching a glob pattern in a sandbox directory.
func (m *Manager) GlobFiles(ctx context.Context, id string, baseDir string, pattern string, page int, pageSize int) (*runtime.FileListResult, error) {
	sb, release, err := m.acquireSandboxOperation(ctx, id)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := validatePublicFUSEFilePath(sb, baseDir); err != nil {
		return nil, err
	}
	runtimeID := m.sandboxRuntimeID(sb)
	if sb.Workspace != nil && sb.Workspace.MountType == WorkspaceMountFUSE {
		hiddenCount, probeErr := m.countReservedFUSEFiles(ctx, runtimeID, baseDir, 0, pattern)
		if probeErr != nil {
			return nil, probeErr
		}
		return collectAndPaginateVisibleFUSEFiles(page, pageSize, hiddenCount, func(fetchPage, fetchPageSize int) (*runtime.FileListResult, error) {
			return m.runtime.GlobFiles(ctx, runtimeID, baseDir, pattern, fetchPage, fetchPageSize)
		})
	}
	return m.runtime.GlobFiles(ctx, runtimeID, baseDir, pattern, page, pageSize)
}

func collectAndPaginateVisibleFUSEFiles(page, pageSize, expectedHidden int, fetch func(int, int) (*runtime.FileListResult, error)) (*runtime.FileListResult, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 100
	}
	const minScanPageSize = 128
	scanPageSize := pageSize
	if scanPageSize < minScanPageSize {
		scanPageSize = minScanPageSize
	}
	start := (page - 1) * pageSize
	end := start + pageSize
	visibleIndex := 0
	rawScanned := 0
	rawTotal := -1
	visible := make([]runtime.FileInfo, 0, pageSize)
	for fetchPage := 1; ; fetchPage++ {
		result, err := fetch(fetchPage, scanPageSize)
		if err != nil {
			return nil, err
		}
		if result == nil {
			return nil, fmt.Errorf("runtime returned no file list result")
		}
		if rawTotal < 0 {
			rawTotal = result.TotalCount
		}
		for _, file := range result.Files {
			rawScanned++
			if !fuseprotocol.IsReservedProbeObjectName(filepath.Base(filepath.Clean(file.Name))) &&
				!fuseprotocol.IsReservedProbeObjectName(filepath.Base(filepath.Clean(file.Path))) {
				if visibleIndex >= start && visibleIndex < end {
					visible = append(visible, file)
				}
				visibleIndex++
			}
		}
		if len(visible) == pageSize || len(result.Files) == 0 || len(result.Files) < scanPageSize || (rawTotal >= 0 && rawScanned >= rawTotal) {
			break
		}
	}
	visibleTotal := rawTotal - expectedHidden
	if visibleTotal < 0 {
		visibleTotal = 0
	}
	return &runtime.FileListResult{
		Files:      visible,
		TotalCount: visibleTotal,
		Page:       page,
		PageSize:   pageSize,
	}, nil
}

func (m *Manager) countReservedFUSEFiles(ctx context.Context, runtimeID, baseDir string, maxDepth int, globPattern string) (int, error) {
	counter, ok := m.runtime.(runtime.ReservedFileCounter)
	if !ok {
		return 0, fmt.Errorf("count reserved FUSE files: runtime does not support bounded reserved-file counting")
	}
	count, err := counter.CountReservedFiles(ctx, runtimeID, baseDir, maxDepth, globPattern)
	if err != nil {
		return 0, fmt.Errorf("count reserved FUSE files: %w", err)
	}
	if count < 0 {
		return 0, fmt.Errorf("count reserved FUSE files: runtime returned negative count")
	}
	return count, nil
}

// FileExists reports whether a regular file exists at the given path inside the sandbox.
// Returns runtime.ErrFileNotFound if the file does not exist.
func (m *Manager) FileExists(ctx context.Context, id string, filePath string) error {
	sb, release, err := m.acquireSandboxOperation(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	if err := validatePublicFUSEFilePath(sb, filePath); err != nil {
		return err
	}
	return m.runtime.FileExists(ctx, m.sandboxRuntimeID(sb), filePath)
}

// ReadFileLines reads a range of lines from a file in a sandbox.
func (m *Manager) ReadFileLines(ctx context.Context, id string, filePath string, startLine int, endLine int) (*runtime.FileLineResult, error) {
	sb, release, err := m.acquireSandboxOperation(ctx, id)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := validatePublicFUSEFilePath(sb, filePath); err != nil {
		return nil, err
	}
	result, err := m.runtime.ReadFileLines(ctx, m.sandboxRuntimeID(sb), filePath, startLine, endLine)
	if err != nil {
		metrics.RecordFileOp(ctx, "read_lines", "error")
		return nil, err
	}
	metrics.RecordFileOp(ctx, "read_lines", "success")
	return result, nil
}

// EditFile performs a string replacement in a file in a sandbox.
func (m *Manager) EditFile(ctx context.Context, id string, filePath string, oldStr string, newStr string, replaceAll bool) error {
	ctx, span := telemetry.Tracer().Start(ctx, "sandbox.Manager.EditFile",
		trace.WithAttributes(attribute.String("sandbox.id", id)),
		trace.WithAttributes(attribute.String("file_path", filePath)),
	)
	defer span.End()

	sb, release, err := m.acquireSandboxOperation(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	if err := validatePublicFUSEFilePath(sb, filePath); err != nil {
		return err
	}
	if err := m.runtime.EditFile(ctx, m.sandboxRuntimeID(sb), filePath, oldStr, newStr, replaceAll); err != nil {
		metrics.RecordFileOp(ctx, "edit", "error")
		return err
	}
	metrics.RecordFileOp(ctx, "edit", "success")
	return nil
}

// EditFileLines replaces a range of lines in a file in a sandbox.
func (m *Manager) EditFileLines(ctx context.Context, id string, filePath string, startLine int, endLine int, newContent string) error {
	ctx, span := telemetry.Tracer().Start(ctx, "sandbox.Manager.EditFileLines",
		trace.WithAttributes(attribute.String("sandbox.id", id)),
		trace.WithAttributes(attribute.String("file_path", filePath)),
	)
	defer span.End()

	sb, release, err := m.acquireSandboxOperation(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	if err := validatePublicFUSEFilePath(sb, filePath); err != nil {
		return err
	}
	if err := m.runtime.EditFileLines(ctx, m.sandboxRuntimeID(sb), filePath, startLine, endLine, newContent); err != nil {
		metrics.RecordFileOp(ctx, "edit_lines", "error")
		return err
	}
	metrics.RecordFileOp(ctx, "edit_lines", "success")
	return nil
}

// UpdateNetwork dynamically updates network access for a running sandbox.
func (m *Manager) UpdateNetwork(ctx context.Context, id string, enabled bool, whitelist []string, blockPrivate bool) error {
	whitelist = append([]string(nil), whitelist...)
	ctx, span := telemetry.Tracer().Start(ctx, "sandbox.Manager.UpdateNetwork",
		trace.WithAttributes(attribute.String("sandbox.id", id)),
		trace.WithAttributes(attribute.String("white_list", strings.Join(whitelist, ","))),
	)
	defer span.End()

	sb, release, err := m.acquireSandboxOperation(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	m.mu.RLock()
	runtimeID := sb.RuntimeID
	runtimeUID := sb.RuntimeUID
	isFUSE := sb.Workspace != nil && sb.Workspace.MountType == WorkspaceMountFUSE
	lifecycle := m.fuseLifecycles[id]
	m.mu.RUnlock()

	if isFUSE {
		ref, refErr := runtime.NewRuntimeRef(runtimeID, runtimeUID)
		if refErr != nil {
			return refErr
		}
		err = m.runtime.UpdateFUSENetwork(ctx, ref, enabled, whitelist, blockPrivate)
	} else {
		err = m.runtime.UpdateNetwork(ctx, runtimeID, enabled, whitelist, blockPrivate)
	}
	if err != nil {
		if isFUSE && lifecycle != nil && errors.Is(err, runtime.ErrFUSENetworkStateUncertain) {
			lifecycle.gate.closeAdmission()
			m.scheduleFUSETeardown(lifecycle, err)
		}
		return err
	}

	m.mu.Lock()
	sb.Config.Network.Enabled = enabled
	sb.Config.Network.Whitelist = append([]string(nil), whitelist...)
	sb.Config.Network.BlockPrivate = blockPrivate
	sb.UpdatedAt = time.Now()
	m.mu.Unlock()

	return nil
}

// UpdateTTL dynamically updates the TTL for a running sandbox.
// The new timeout (in seconds) must be > 0; setting to never-expire (-1) after
// creation is not allowed.
func (m *Manager) UpdateTTL(ctx context.Context, id string, timeoutSeconds int) (*Sandbox, error) {
	if timeoutSeconds <= 0 {
		return nil, fmt.Errorf("timeout must be greater than 0")
	}

	sb, release, err := m.acquireSandboxOperation(ctx, id)
	if err != nil {
		return nil, err
	}
	defer release()

	now := time.Now()
	newTimeout := time.Duration(timeoutSeconds) * time.Second

	m.mu.Lock()
	sb.Timeout = newTimeout
	sb.CreatedAt = now // reset so reaper uses new baseline
	sb.UpdatedAt = now
	result := cloneSandbox(sb)
	m.mu.Unlock()

	// Persist to session store if applicable
	if result.Config.Mode == ModePersistent && m.sessions != nil {
		if err := m.sessions.Save(ctx, &result); err != nil {
			_ = err
		}
	}

	return &result, nil
}

// reapExpiredSandboxes periodically checks for sandboxes that have exceeded
// their timeout and destroys them.
func (m *Manager) reapExpiredSandboxes() {
	defer m.wg.Done()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.reapOnce()
		}
	}
}

func (m *Manager) reapOnce() {
	now := time.Now()
	var expired []string

	m.mu.RLock()
	for id, sb := range m.sandboxes {
		if sb.Timeout > 0 && now.Sub(sb.CreatedAt) > sb.Timeout {
			expired = append(expired, id)
		}
	}
	m.mu.RUnlock()

	for _, id := range expired {
		_ = m.destroyWithReason(context.Background(), id, "ttl_expired")
	}
}

// autoSyncWorkspaces periodically syncs changed files from all mounted
// workspaces back to storage. This reduces data loss if a container crashes
// between manual syncs.
func (m *Manager) autoSyncWorkspaces() {
	defer m.wg.Done()
	interval := time.Duration(m.config.AutoSyncIntervalSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.autoSyncOnce()
		}
	}
}

func (m *Manager) autoSyncOnce() {
	ctx := context.Background()

	// Restore persistent sandboxes not yet in local memory (handles multi-replica scenario)
	if m.sessions != nil {
		ids, err := m.sessions.List(ctx)
		if err != nil {
			logger.Error(ctx, "autoSyncOnce: list persistent sandboxes failed", logger.ErrorField(err))
		} else {
			for _, id := range ids {
				m.mu.RLock()
				_, inMemory := m.sandboxes[id]
				m.mu.RUnlock()
				if inMemory {
					continue
				}
				// Restore from session store so this replica can sync it
				sbPtr, err := m.sessions.Load(ctx, id)
				if err != nil {
					logger.Error(ctx, "autoSyncOnce: load sandbox from session store failed",
						logger.AddField("sandbox_id", id),
						logger.ErrorField(err),
					)
					continue
				}
				// Skip sandboxes that are being destroyed — the session entry may
				// be a stale record from a concurrent Destroy that removed the
				// session key after our List call but before we loaded it.
				// (StateDestroyed is never written to the session store because
				// Destroy deletes the key before the sandbox reaches that state.)
				if sbPtr.State == StateDestroying || (sbPtr.Workspace != nil && sbPtr.Workspace.MountType == WorkspaceMountFUSE) {
					continue
				}
				m.mu.Lock()
				m.sandboxes[id] = sbPtr
				m.mu.Unlock()
				metrics.RecordSessionRestore(ctx, "success")
				metrics.SandboxActiveGauge.Add(ctx, 1)

				if sbPtr.Workspace != nil && sbPtr.Workspace.RootPath != "" {
					scoped, fsErr := storage.NewScopedFS(m.filesystem, sbPtr.Workspace.RootPath)
					if fsErr != nil {
						logger.Error(ctx, "autoSyncOnce: restore workspace scoped fs failed",
							logger.AddField("sandbox_id", id),
							logger.AddField("root_path", sbPtr.Workspace.RootPath),
							logger.ErrorField(fsErr),
						)
					} else {
						m.mu.Lock()
						m.workspaces[id] = scoped
						m.mu.Unlock()
					}
				}
			}
		}
	}

	m.mu.RLock()
	type syncTarget struct {
		sandboxID   string
		runtimeID   string
		syncExclude []string
	}
	var targets []syncTarget
	for id := range m.workspaces {
		if sb, ok := m.sandboxes[id]; ok && sb.State != StateDestroying {
			var exclude []string
			if sb.Workspace != nil {
				exclude = append([]string(nil), sb.Workspace.SyncExclude...)
			}
			targets = append(targets, syncTarget{sandboxID: id, runtimeID: sb.RuntimeID, syncExclude: exclude})
		}
	}
	m.mu.RUnlock()

	for _, t := range targets {
		sb, release, acquireErr := m.acquireSandboxOperation(ctx, t.sandboxID)
		if acquireErr != nil {
			logger.Debug(ctx, "autoSync skipping sandbox",
				logger.AddField("sandbox_id", t.sandboxID),
				logger.ErrorField(acquireErr),
			)
			continue
		}
		m.mu.RLock()
		skip := sb.State == StateDestroying || (sb.Workspace != nil && sb.Workspace.MountType == WorkspaceMountFUSE)
		runtimeID := sb.RuntimeID
		m.mu.RUnlock()
		if skip {
			release()
			continue
		}
		syncErr := m.syncFromContainer(ctx, t.sandboxID, runtimeID, t.syncExclude)
		release()
		if syncErr != nil {
			if errors.Is(syncErr, runtime.ErrNotFound) {
				// Pod is gone — clean up both in-memory state and the session store
				// so we stop retrying on every tick and don't restore it again.
				m.mu.Lock()
				delete(m.workspaces, t.sandboxID)
				delete(m.sandboxes, t.sandboxID)
				m.mu.Unlock()
				if m.sessions != nil {
					_ = m.sessions.Remove(ctx, t.sandboxID)
				}
				continue
			}
			logger.Error(ctx, "auto-sync failed",
				logger.AddField("sandbox_id", t.sandboxID),
				logger.ErrorField(syncErr),
			)
		}
	}
}

// restorePersistentSandboxes reloads all persistent sandboxes from the session
// store at startup. If a container is gone, it recreates the container and
// re-syncs the workspace.
func (m *Manager) restorePersistentSandboxes(ctx context.Context) error {
	if m.sessions == nil {
		return nil
	}

	ids, err := m.sessions.List(ctx)
	if err != nil {
		return fmt.Errorf("list persistent sandboxes: %w", err)
	}

	var restored, recreated, failed int
	var restoreErr error
	for _, id := range ids {
		sbPtr, err := m.sessions.Load(ctx, id)
		if err != nil {
			logger.Error(ctx, "failed to load sandbox from session store",
				logger.AddField("sandbox_id", id),
				logger.ErrorField(err),
			)
			failed++
			restoreErr = errors.Join(restoreErr, fmt.Errorf("load persistent sandbox %q: %w", id, err))
			metrics.RecordSessionRestore(ctx, "error")
			continue
		}
		if sbPtr.Workspace != nil && sbPtr.Workspace.MountType == WorkspaceMountFUSE {
			if err := m.restoreFUSESandbox(ctx, sbPtr); err != nil {
				failed++
				restoreErr = errors.Join(restoreErr, fmt.Errorf("restore FUSE sandbox %q: %w", id, err))
				metrics.RecordSessionRestore(ctx, "error")
				continue
			}
			restored++
			metrics.RecordSessionRestore(ctx, "success")
			metrics.SandboxActiveGauge.Add(ctx, 1)
			continue
		}

		// Check if the container still exists and is running
		existingInfo, rtErr := m.runtime.GetSandbox(ctx, sbPtr.RuntimeID)
		if rtErr != nil || existingInfo == nil || existingInfo.State != "running" {
			// Container gone or not healthy — recreate it
			logger.Warn(ctx, "sandbox container gone, recreating",
				logger.AddField("sandbox_id", id),
				logger.AddField("runtime_id", sbPtr.RuntimeID),
			)
			newInfo, recreateErr := m.recreateSandbox(ctx, sbPtr)
			if recreateErr != nil {
				logger.Error(ctx, "failed to recreate sandbox",
					logger.AddField("sandbox_id", id),
					logger.ErrorField(recreateErr),
				)
				if removeErr := m.sessions.Remove(ctx, id); removeErr != nil {
					restoreErr = errors.Join(restoreErr, fmt.Errorf("remove unrecoverable persistent sandbox %q: %w", id, removeErr))
				}
				failed++
				restoreErr = errors.Join(restoreErr, fmt.Errorf("recreate persistent sandbox %q: %w", id, recreateErr))
				metrics.RecordSessionRestore(ctx, "error")
				continue
			}
			sbPtr.RuntimeID = newInfo.RuntimeID
			sbPtr.State = StateReady
			sbPtr.UpdatedAt = time.Now()
			if saveErr := m.sessions.Save(ctx, sbPtr); saveErr != nil {
				restoreErr = errors.Join(restoreErr, fmt.Errorf("save recreated persistent sandbox %q: %w", id, saveErr))
			}
			recreated++
		}

		// Register into in-memory map
		m.sandboxes[id] = sbPtr

		// Restore workspace ScopedFS and sync if needed
		if sbPtr.Workspace != nil && sbPtr.Workspace.RootPath != "" {
			scoped, fsErr := storage.NewScopedFS(m.filesystem, sbPtr.Workspace.RootPath)
			if fsErr == nil {
				m.workspaces[id] = scoped
				// Re-sync files to the new container (skip for bind mount)
				if !sbPtr.Workspace.BindMounted {
					if syncErr := m.syncToContainer(ctx, scoped, sbPtr.RuntimeID); syncErr != nil {
						logger.Error(ctx, "workspace re-sync failed",
							logger.AddField("sandbox_id", id),
							logger.AddField("runtime_id", sbPtr.RuntimeID),
							logger.ErrorField(syncErr),
						)
						restoreErr = errors.Join(restoreErr, fmt.Errorf("restore workspace for sandbox %q: %w", id, syncErr))
					}
				}
			} else {
				logger.Error(ctx, "failed to restore workspace scoped fs",
					logger.AddField("sandbox_id", id),
					logger.ErrorField(fsErr),
				)
				restoreErr = errors.Join(restoreErr, fmt.Errorf("restore scoped workspace for sandbox %q: %w", id, fsErr))
			}
		}

		restored++
		metrics.RecordSessionRestore(ctx, "success")
		metrics.SandboxActiveGauge.Add(ctx, 1)
	}

	if restored > 0 || failed > 0 {
		logger.Info(ctx, "persistent sandboxes restore complete",
			logger.AddField("restored", restored),
			logger.AddField("recreated", recreated),
			logger.AddField("failed", failed),
		)
	}
	return restoreErr
}

// restoreFUSESandbox reinstalls only the in-process gate, renewal and health
// watcher for an already-mounted runtime. The persisted mount attempt must be
// consumed; recovery never sends another authorization to the mounter.
func (m *Manager) restoreFUSESandbox(ctx context.Context, sb *Sandbox) error {
	if sb == nil || sb.Workspace == nil || m.fusePool == nil || m.sessions == nil ||
		m.config.WorkspaceCoordinator == nil || m.config.WorkspaceObjectClient == nil {
		return ErrInvalidFUSEPoolConfig
	}
	workspace := sb.Workspace
	owner := workspace.Owner
	spec := m.fusePool.spec.WorkspaceFUSE
	if spec == nil || m.fsMeta == nil {
		return ErrInvalidFUSEPoolConfig
	}
	recoveryResult := "error"
	defer func() {
		metrics.RecordWorkspaceRecovery(ctx, spec.RuntimeType, spec.Provider, recoveryResult)
	}()
	expectedPrefix, err := storage.BuildWorkspacePrefix(m.fsMeta.SubPath, workspace.RootPath)
	if err != nil {
		return ErrSandboxNotReady
	}
	if sb.ID == "" || sb.Config.Mode != ModePersistent || sb.State != StateReady ||
		sb.RuntimeID == "" || sb.RuntimeUID == "" || sb.RuntimeID != owner.RuntimeID || sb.RuntimeUID != owner.RuntimeUID ||
		owner.SandboxID != sb.ID || owner.Generation != workspace.LeaseGeneration || owner.MountAttempt != 1 ||
		owner.Provider != spec.Provider || owner.StorageIdentityHash != storageIdentityHash(spec.StorageIdentity) ||
		owner.Bucket != spec.Bucket || owner.Prefix != expectedPrefix || owner.Runtime != spec.RuntimeType ||
		workspace.MountState != WorkspaceMountReady || workspace.FUSEPreparationID == "" ||
		workspace.FUSEPoolKey != m.fusePool.poolKey || workspace.FUSEReservationToken == "" || workspace.FUSERecordRevision == 0 {
		return ErrSandboxNotReady
	}

	records, err := m.fusePool.repo.ListByPoolKey(ctx, workspace.FUSEPoolKey)
	if err != nil {
		return fmt.Errorf("list FUSE pool records: %w", err)
	}
	var record *state.FUSEPoolRecord
	for i := range records {
		candidate := records[i]
		if candidate.PreparationID == workspace.FUSEPreparationID {
			record = &candidate
			break
		}
	}
	if record == nil || record.State != state.FUSEPoolConsumed || record.RuntimeID != sb.RuntimeID ||
		record.RuntimeUID != sb.RuntimeUID || record.PoolKey != workspace.FUSEPoolKey ||
		record.ReservationToken != workspace.FUSEReservationToken || record.Revision != workspace.FUSERecordRevision {
		return ErrSandboxNotReady
	}

	info, err := m.runtime.GetSandbox(ctx, sb.RuntimeID)
	if err != nil || info == nil || info.State != "running" || info.RuntimeID != sb.RuntimeID || info.RuntimeUID != sb.RuntimeUID {
		return ErrSandboxNotReady
	}
	ref, err := runtime.NewRuntimeRef(sb.RuntimeID, sb.RuntimeUID)
	if err != nil {
		return ErrSandboxNotReady
	}
	health, err := m.runtime.WorkspaceHealth(ctx, ref)
	if err != nil || health == nil || !health.Ready || health.MountType != "fuse" ||
		health.RuntimeUID != sb.RuntimeUID || health.Generation != owner.Generation ||
		health.RestartCount != 0 || health.RestartDetected {
		return ErrSandboxNotReady
	}

	lease, err := m.config.WorkspaceCoordinator.Restore(ctx, owner)
	if err != nil {
		return fmt.Errorf("restore workspace lease: %w", err)
	}
	gate := newOperationGate(true)
	lifecycleCtx, cancelLifecycle := context.WithCancel(context.Background())
	var lifecycle *fuseSandboxLifecycle
	var restoreMu sync.Mutex
	var renewalLost error
	published := false
	renewal, err := m.config.WorkspaceCoordinator.StartRenewal(context.Background(), lease, func(lost error) {
		restoreMu.Lock()
		current := lifecycle
		if !published {
			renewalLost = lost
			current = nil
		}
		restoreMu.Unlock()
		if current != nil {
			m.scheduleFUSETeardown(current, lost)
		}
	})
	if err != nil {
		cancelLifecycle()
		return fmt.Errorf("restart workspace lease renewal: %w", err)
	}
	restoreMu.Lock()
	lifecycle = &fuseSandboxLifecycle{
		sandboxID: sb.ID,
		gate:      gate,
		lease:     lease,
		renewal:   renewal,
		record:    *record,
		cancel:    cancelLifecycle,
	}
	restoreMu.Unlock()

	m.mu.Lock()
	restoreMu.Lock()
	if renewalLost != nil {
		lost := renewalLost
		restoreMu.Unlock()
		m.mu.Unlock()
		cancelLifecycle()
		renewal.Stop()
		return errors.Join(ErrWorkspaceLeaseLost, lost)
	}
	if m.sandboxes[sb.ID] != nil || m.operationGates[sb.ID] != nil || m.fuseLifecycles[sb.ID] != nil {
		restoreMu.Unlock()
		m.mu.Unlock()
		cancelLifecycle()
		renewal.Stop()
		return ErrSandboxNotReady
	}
	sb.Workspace.LastHealthyAt = health.LastSuccessful
	if sb.Workspace.LastHealthyAt.IsZero() {
		sb.Workspace.LastHealthyAt = time.Now()
	}
	m.sandboxes[sb.ID] = sb
	m.operationGates[sb.ID] = gate
	m.fuseLifecycles[sb.ID] = lifecycle
	published = true
	restoreMu.Unlock()
	m.mu.Unlock()
	m.startFUSEWatcher(lifecycleCtx, lifecycle)
	recoveryResult = "success"
	return nil
}

// recreateSandbox creates a new container for a persistent sandbox whose
// container was lost (e.g. docker-compose restart).
func (m *Manager) recreateSandbox(ctx context.Context, sb *Sandbox) (*runtime.SandboxInfo, error) {
	// Use the original RuntimeID as the pod/container name so the identity is
	// consistent across restarts. On Docker, RenameSandbox will rename it to
	// sb.ID afterwards; on Kubernetes, pod names are immutable so RuntimeID is
	// the permanent name.
	spec := m.buildSpec(sb.RuntimeID, sb.Config)
	spec.Labels["sandbox.id"] = sb.ID

	// Restore bind mount if applicable
	if sb.Workspace != nil && sb.Workspace.BindMounted && m.fsMeta != nil && m.fsMeta.Provider == storage.ProviderLocal {
		hostPath := m.resolveLocalWorkspacePath(sb.Workspace.RootPath)
		spec.Mounts = append(spec.Mounts, runtime.Mount{
			HostPath:      hostPath,
			ContainerPath: "/workspace",
		})
	}

	info, err := m.runtime.CreateSandbox(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("create container: %w", err)
	}

	// Rename for identification
	_ = m.runtime.RenameSandbox(ctx, info.RuntimeID, sb.ID)

	return info, nil
}

// cleanupOrphanedPoolContainers removes pool containers left over from a
// previous process that exited without graceful shutdown.
// Must be called AFTER restorePersistentSandboxes so that active containers
// are already registered in m.sandboxes.
func (m *Manager) cleanupOrphanedPoolContainers(ctx context.Context) {
	containers, err := m.runtime.ListSandboxes(ctx, map[string]string{
		"sandbox.pool": "true",
	})
	if err != nil {
		logger.Error(ctx, "failed to list orphaned pool containers", logger.ErrorField(err))
		return
	}

	// Build set of runtime IDs belonging to restored persistent sandboxes
	activeRuntimeIDs := make(map[string]struct{})
	m.mu.RLock()
	for _, sb := range m.sandboxes {
		activeRuntimeIDs[sb.RuntimeID] = struct{}{}
	}
	m.mu.RUnlock()

	var removed int
	for _, c := range containers {
		if _, active := activeRuntimeIDs[c.RuntimeID]; active {
			continue // in use by a persistent sandbox
		}
		logger.Info(ctx, "removing orphaned pool container",
			logger.AddField("runtime_id", c.RuntimeID),
		)
		_ = m.runtime.RemoveSandbox(ctx, c.RuntimeID)
		removed++
	}
	if removed > 0 {
		logger.Info(ctx, "cleaned up orphaned pool containers",
			logger.AddField("count", removed),
		)
	}
}

// buildInstallCommand generates the shell command to install dependencies,
// grouping by package manager (pip/npm).
func buildInstallCommand(deps []Dependency) string {
	if len(deps) == 0 {
		return ""
	}
	var pipPkgs, npmPkgs []string
	for _, d := range deps {
		if !validDepRegexp.MatchString(d.Name) {
			logger.Warn(context.Background(), "skipping dependency with invalid name",
				logger.AddField("dep_name", d.Name),
			)
			continue
		}
		if d.Version != "" && !validDepRegexp.MatchString(d.Version) {
			logger.Warn(context.Background(), "skipping dependency with invalid version",
				logger.AddField("dep_name", d.Name),
				logger.AddField("dep_version", d.Version),
			)
			continue
		}
		pkg := d.Name
		switch d.Manager {
		case "pip":
			if d.Version != "" {
				pkg += "==" + d.Version
			}
			pipPkgs = append(pipPkgs, pkg)
		case "npm":
			if d.Version != "" {
				pkg += "@" + d.Version
			}
			npmPkgs = append(npmPkgs, pkg)
		default:
			logger.Warn(context.Background(), "skipping dependency with unknown manager",
				logger.AddField("dep_name", d.Name),
				logger.AddField("manager", d.Manager),
			)
		}
	}
	var cmds []string
	if len(pipPkgs) > 0 {
		cmds = append(cmds, "PIP_CACHE_DIR=/tmp/pip-cache pip install --no-cache-dir "+strings.Join(pipPkgs, " "))
	}
	if len(npmPkgs) > 0 {
		cmds = append(cmds, "npm_config_cache=/tmp/npm-cache npm install --no-save "+strings.Join(npmPkgs, " "))
	}
	return strings.Join(cmds, " && ")
}

// buildSpec constructs a runtime.SandboxSpec from sandbox config.
// Used for network-enabled sandboxes that bypass the pool.
func (m *Manager) buildSpec(id string, cfg SandboxConfig) runtime.SandboxSpec {
	tmpDisk := cfg.Resources.TmpDisk
	if tmpDisk == "" {
		tmpDisk = m.config.PoolConfig.TmpDisk
	}
	return runtime.SandboxSpec{
		ID:                  id,
		Image:               m.config.PoolConfig.Image,
		Memory:              cfg.Resources.Memory,
		CPU:                 cfg.Resources.CPU,
		Disk:                cfg.Resources.Disk,
		TmpDisk:             tmpDisk,
		NetworkEnabled:      cfg.Network.Enabled,
		NetworkWhitelist:    cfg.Network.Whitelist,
		NetworkBlockPrivate: cfg.Network.BlockPrivate,
		ReadOnlyRootFS:      false,
		RunAsUser:           1000,
		PidLimit:            100,
		Labels: map[string]string{
			"sandbox.id": id,
		},
	}
}

// resolveLocalWorkspacePath returns the absolute host path for a workspace.
func (m *Manager) resolveLocalWorkspacePath(workspacePath string) string {
	if m.fsMeta == nil {
		return workspacePath
	}
	return filepath.Join(m.fsMeta.LocalPath, workspacePath)
}

// registerWorkspace registers a ScopedFS for a bind-mounted workspace without
// copying files (they are already visible via the mount).
func (m *Manager) registerWorkspace(ctx context.Context, sandboxID, rootPath string) error {
	scoped, err := storage.NewScopedFS(m.filesystem, rootPath)
	if err != nil {
		return fmt.Errorf("create scoped filesystem: %w", err)
	}

	now := time.Now()
	m.mu.Lock()
	sb := m.sandboxes[sandboxID]
	m.workspaces[sandboxID] = scoped
	sb.Workspace = &WorkspaceInfo{
		RootPath:     rootPath,
		MountedAt:    now,
		LastSyncedAt: now,
		BindMounted:  true,
	}
	sb.UpdatedAt = now
	sessionSnapshot := cloneSandbox(sb)
	m.mu.Unlock()

	if m.sessions != nil {
		_ = m.sessions.Save(ctx, &sessionSnapshot)
	}
	return nil
}

// InitMultipartUpload initialises a multipart upload session.
// It creates the staging directory in the container and persists state to Redis.
func (m *Manager) InitMultipartUpload(ctx context.Context, sandboxID, destPath string, totalChunks int) (string, error) {
	if m.multipartStore == nil {
		return "", fmt.Errorf("multipart store not configured")
	}

	sb, release, err := m.acquireSandboxOperation(ctx, sandboxID)
	if err != nil {
		return "", err
	}
	defer release()
	if err := validatePublicFUSEFilePath(sb, destPath); err != nil {
		return "", err
	}
	runtimeID := m.sandboxRuntimeID(sb)

	uploadID := uuid.New().String()

	if _, err := m.runtime.Exec(ctx, runtimeID, runtime.ExecRequest{
		Command: fmt.Sprintf("mkdir -p '/tmp/.uploads/%s'", uploadID),
		Timeout: 10,
	}); err != nil {
		return "", fmt.Errorf("create staging dir: %w", err)
	}

	st := MultipartUploadState{
		UploadID:    uploadID,
		SandboxID:   sandboxID,
		DestPath:    destPath,
		TotalChunks: totalChunks,
		CreatedAt:   time.Now(),
	}
	data, err := json.Marshal(st)
	if err != nil {
		return "", fmt.Errorf("marshal multipart state: %w", err)
	}
	if err := m.multipartStore.Set(ctx, multipartKey(sandboxID, uploadID), data, multipartTTL); err != nil {
		return "", fmt.Errorf("save multipart state: %w", err)
	}
	return uploadID, nil
}

func (m *Manager) loadMultipartState(ctx context.Context, sandboxID, uploadID string) (*MultipartUploadState, error) {
	if m.multipartStore == nil {
		return nil, fmt.Errorf("multipart store not configured")
	}
	data, err := m.multipartStore.Get(ctx, multipartKey(sandboxID, uploadID))
	if err != nil {
		return nil, fmt.Errorf("get multipart state: %w", err)
	}
	if data == nil {
		return nil, fmt.Errorf("%w: %s", ErrUploadNotFound, uploadID)
	}
	var st MultipartUploadState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("unmarshal multipart state: %w", err)
	}
	if st.SandboxID != sandboxID {
		return nil, fmt.Errorf("%w: %s", ErrUploadNotFound, uploadID)
	}
	return &st, nil
}

func (m *Manager) saveMultipartState(ctx context.Context, sandboxID, uploadID string, st *MultipartUploadState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("marshal multipart state: %w", err)
	}
	ttl := multipartTTL - time.Since(st.CreatedAt)
	if ttl <= 0 {
		ttl = time.Second
	}
	return m.multipartStore.Set(ctx, multipartKey(sandboxID, uploadID), data, ttl)
}

// UploadChunk writes a single chunk to the container staging directory.
// Chunks must be uploaded in order: chunk_index must equal ReceivedChunks.
// Concurrent uploads for the same uploadID are not supported; callers must serialize chunk requests.
func (m *Manager) UploadChunk(ctx context.Context, sandboxID, uploadID string, chunkIndex int, size int64, reader io.Reader) (received int, total int, err error) {
	sb, release, err := m.acquireSandboxOperation(ctx, sandboxID)
	if err != nil {
		return 0, 0, err
	}
	defer release()
	runtimeID := m.sandboxRuntimeID(sb)
	st, err := m.loadMultipartState(ctx, sandboxID, uploadID)
	if err != nil {
		return 0, 0, err
	}
	if chunkIndex != st.ReceivedChunks {
		return 0, 0, fmt.Errorf("%w: expected %d, got %d", ErrUnexpectedChunkIndex, st.ReceivedChunks, chunkIndex)
	}

	chunkPath := fmt.Sprintf("/tmp/.uploads/%s/%d", uploadID, chunkIndex)
	if err := m.runtime.UploadFile(ctx, runtimeID, chunkPath, size, reader); err != nil {
		return 0, 0, fmt.Errorf("upload chunk: %w", err)
	}

	st.ReceivedChunks++
	if err := m.saveMultipartState(ctx, sandboxID, uploadID, st); err != nil {
		return 0, 0, fmt.Errorf("save multipart state: %w", err)
	}
	return st.ReceivedChunks, st.TotalChunks, nil
}

// GetMultipartStatus returns the current state of a multipart upload.
func (m *Manager) GetMultipartStatus(ctx context.Context, sandboxID, uploadID string) (*MultipartUploadState, error) {
	_, release, err := m.acquireSandboxOperation(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	defer release()
	return m.loadMultipartState(ctx, sandboxID, uploadID)
}

// CompleteMultipartUpload merges all chunks into the destination path.
// Returns the final file size in bytes.
func (m *Manager) CompleteMultipartUpload(ctx context.Context, sandboxID, uploadID string) (destPath string, size int64, err error) {
	sb, release, err := m.acquireSandboxOperation(ctx, sandboxID)
	if err != nil {
		return "", 0, err
	}
	defer release()
	runtimeID := m.sandboxRuntimeID(sb)
	st, err := m.loadMultipartState(ctx, sandboxID, uploadID)
	if err != nil {
		return "", 0, err
	}
	if err := validatePublicFUSEFilePath(sb, st.DestPath); err != nil {
		return "", 0, err
	}
	if st.ReceivedChunks != st.TotalChunks {
		return "", 0, fmt.Errorf("%w: received %d of %d chunks", ErrIncompleteUpload, st.ReceivedChunks, st.TotalChunks)
	}

	// Build: cat /tmp/.uploads/{id}/0 /tmp/.uploads/{id}/1 ... > destPath
	parts := make([]string, st.TotalChunks)
	for i := 0; i < st.TotalChunks; i++ {
		parts[i] = fmt.Sprintf("'/tmp/.uploads/%s/%d'", uploadID, i)
	}
	escapedDest := "'" + strings.ReplaceAll(st.DestPath, "'", "'\\''") + "'"
	escapedParent := "'" + strings.ReplaceAll(filepath.Dir(st.DestPath), "'", "'\\''") + "'"

	if mkRes, mkErr := m.runtime.Exec(ctx, runtimeID, runtime.ExecRequest{
		Command: "mkdir -p " + escapedParent,
		Timeout: 10,
	}); mkErr != nil || mkRes.ExitCode != 0 {
		if mkErr != nil {
			return "", 0, fmt.Errorf("create dest dir: %w", mkErr)
		}
		return "", 0, fmt.Errorf("create dest dir: exit %d: %s", mkRes.ExitCode, mkRes.Stderr)
	}

	catCmd := "cat " + strings.Join(parts, " ") + " > " + escapedDest
	catRes, err := m.runtime.Exec(ctx, runtimeID, runtime.ExecRequest{
		Command: catCmd,
		Timeout: 120,
	})
	if err != nil {
		return "", 0, fmt.Errorf("merge chunks: %w", err)
	}
	if catRes.ExitCode != 0 {
		return "", 0, fmt.Errorf("merge chunks: exit %d: %s", catRes.ExitCode, catRes.Stderr)
	}

	// Get file size via wc -c
	statResult, statErr := m.runtime.Exec(ctx, runtimeID, runtime.ExecRequest{
		Command: "wc -c < " + escapedDest,
		Timeout: 10,
	})
	if statErr == nil {
		_, _ = fmt.Sscanf(strings.TrimSpace(statResult.Stdout), "%d", &size)
	} else {
		var stderr string
		if statResult != nil {
			stderr = statResult.Stderr
		}
		logger.Warn(ctx, "CompleteMultipartUpload: stat failed",
			logger.AddField("upload_id", uploadID),
			logger.AddField("stderr", stderr),
			logger.ErrorField(statErr),
		)
	}

	// Cleanup staging dir
	if _, rmErr := m.runtime.Exec(ctx, runtimeID, runtime.ExecRequest{
		Command: fmt.Sprintf("rm -rf '/tmp/.uploads/%s'", uploadID),
		Timeout: 10,
	}); rmErr != nil {
		logger.Warn(ctx, "CompleteMultipartUpload: cleanup staging dir failed",
			logger.AddField("upload_id", uploadID),
			logger.ErrorField(rmErr),
		)
	}
	if delErr := m.multipartStore.Delete(ctx, multipartKey(sandboxID, uploadID)); delErr != nil {
		logger.Warn(ctx, "CompleteMultipartUpload: delete multipart state failed",
			logger.AddField("upload_id", uploadID),
			logger.ErrorField(delErr),
		)
	}

	return st.DestPath, size, nil
}

// CancelMultipartUpload removes staging files and Redis state.
func (m *Manager) CancelMultipartUpload(ctx context.Context, sandboxID, uploadID string) error {
	sb, release, err := m.acquireSandboxOperation(ctx, sandboxID)
	if err != nil {
		return err
	}
	defer release()
	runtimeID := m.sandboxRuntimeID(sb)
	if _, err := m.loadMultipartState(ctx, sandboxID, uploadID); err != nil {
		return err
	}

	if _, rmErr := m.runtime.Exec(ctx, runtimeID, runtime.ExecRequest{
		Command: fmt.Sprintf("rm -rf '/tmp/.uploads/%s'", uploadID),
		Timeout: 10,
	}); rmErr != nil {
		logger.Warn(ctx, "CancelMultipartUpload: cleanup staging dir failed",
			logger.AddField("upload_id", uploadID),
			logger.ErrorField(rmErr),
		)
	}
	return m.multipartStore.Delete(ctx, multipartKey(sandboxID, uploadID))
}
