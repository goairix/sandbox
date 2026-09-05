package docker

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	dockerclient "github.com/docker/docker/client"

	"github.com/goairix/sandbox/internal/fuseprotocol"
	"github.com/goairix/sandbox/internal/runtime"
)

// Runtime implements runtime.Runtime using Docker.
type Runtime struct {
	cli                dockerAPI
	isolatedNetworkID  string
	openNetworkID      string
	gatewayImage       string
	secretRoot         string
	stateMu            sync.Mutex
	workspaceStates    map[string]*dockerWorkspaceState
	secretMaterializer FUSESecretMaterializer
	secretValidator    func(string, string) error
}

type dockerWorkspaceState struct {
	mu            sync.Mutex
	runtimeUID    string
	sandboxID     string
	preparationID string
	poolKey       string
	generation    int64
	authorized    bool
	authAttempted bool
	quiesceToken  *runtime.WorkspaceQuiesceToken
	poisoned      bool
	cacheVolume   string
	cacheBytes    int64
	secretRoot    string
	systemEgress  runtime.SystemEgressSpec
}

const (
	dockerPreparationLabel  = "sandbox.preparation.id"
	dockerLogicalIDLabel    = "sandbox.logical.id"
	dockerSystemEgressLabel = "sandbox.system-egress.v1"
	dockerCacheBytesLabel   = "sandbox.workspace.cache.bytes"
	dockerSecretRootLabel   = "sandbox.workspace.secret-root"
	dockerCleanupTimeout    = 30 * time.Second
)

// New creates a new Docker runtime.
func New(ctx context.Context, host, gatewayImage string) (*Runtime, error) {
	opts := []dockerclient.Opt{
		dockerclient.WithAPIVersionNegotiation(),
	}
	if host != "" {
		opts = append(opts, dockerclient.WithHost(host))
	}

	cli, err := dockerclient.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}

	isolatedID, openID, err := ensureNetworks(ctx, cli)
	if err != nil {
		return nil, err
	}

	if gatewayImage == "" {
		gatewayImage = defaultGatewayImage
	}

	return &Runtime{
		cli:               cli,
		isolatedNetworkID: isolatedID,
		openNetworkID:     openID,
		gatewayImage:      gatewayImage,
		secretRoot:        dockerWorkspaceSecretRoot,
		workspaceStates:   make(map[string]*dockerWorkspaceState),
	}, nil
}

// NewWithFUSESecrets enables Docker FUSE preparation with an explicit trusted
// credential materializer. Legacy callers of New remain unchanged and fail
// closed if they accidentally request a FUSE runtime.
func NewWithFUSESecrets(ctx context.Context, host, gatewayImage string, materializer FUSESecretMaterializer) (*Runtime, error) {
	return NewWithFUSESecretsAtRoot(ctx, host, gatewayImage, dockerWorkspaceSecretRoot, materializer)
}

// NewWithFUSESecretsAtRoot enables Docker FUSE preparation with a staging root
// that is visible at the identical absolute path to both sandbox-api and the
// Docker daemon.
func NewWithFUSESecretsAtRoot(ctx context.Context, host, gatewayImage, secretRoot string, materializer FUSESecretMaterializer) (*Runtime, error) {
	runtime, err := New(ctx, host, gatewayImage)
	if err != nil {
		return nil, err
	}
	if materializer == nil {
		_ = runtime.Close()
		return nil, fmt.Errorf("Docker workspace FUSE secret materializer is required")
	}
	if !validDockerWorkspaceSecretRoot(secretRoot) {
		_ = runtime.Close()
		return nil, fmt.Errorf("Docker workspace FUSE secret root is invalid")
	}
	runtime.secretRoot = secretRoot
	runtime.secretMaterializer = materializer
	return runtime, nil
}

// Close releases resources held by the Docker runtime.
func (r *Runtime) Close() error {
	return r.cli.Close()
}

func (r *Runtime) CreateSandbox(ctx context.Context, spec runtime.SandboxSpec) (*runtime.SandboxInfo, error) {
	if spec.WorkspaceFUSE != nil {
		return nil, fmt.Errorf("FUSE sandboxes require the prepare/authorize/ready lifecycle")
	}
	var networkID string
	var pairNetworkID, gatewayIP string

	if spec.NetworkEnabled {
		// Create a gateway sidecar pair for network-enabled sandboxes.
		// The sandbox connects to an isolated pair network, and the gateway
		// bridges it to the open network with optional whitelist filtering.
		var gatewayID string
		var err error
		pairNetworkID, gatewayID, gatewayIP, err = createSandboxPair(
			ctx, r.cli, spec.ID, r.openNetworkID, r.gatewayImage, spec.NetworkWhitelist, spec.NetworkBlockPrivate,
		)
		if err != nil {
			return nil, fmt.Errorf("create gateway pair: %w", err)
		}
		_ = gatewayID // tracked via labels, cleaned up in RemoveSandbox
		networkID = pairNetworkID
	} else {
		// No network access — use the shared isolated network
		networkID = r.isolatedNetworkID
	}

	containerID, err := createContainer(ctx, r.cli, spec, networkID, r.effectiveSecretRoot())
	if err != nil {
		if pairNetworkID != "" {
			_ = removeSandboxPair(ctx, r.cli, spec.ID)
		}
		return nil, err
	}

	// Start the container
	if err := r.cli.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		_ = r.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
		if pairNetworkID != "" {
			_ = removeSandboxPair(ctx, r.cli, spec.ID)
		}
		return nil, fmt.Errorf("start container: %w", err)
	}

	// Set the sandbox's default route to the gateway
	if gatewayIP != "" {
		if err := setupSandboxRoute(ctx, r.cli, containerID, gatewayIP); err != nil {
			_ = r.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
			_ = removeSandboxPair(ctx, r.cli, spec.ID)
			return nil, fmt.Errorf("setup sandbox route: %w", err)
		}
	}

	return &runtime.SandboxInfo{
		ID:         spec.ID,
		RuntimeID:  containerID,
		RuntimeUID: containerID,
		State:      "running",
		CreatedAt:  time.Now(),
	}, nil
}

func (r *Runtime) PrepareSandbox(ctx context.Context, spec runtime.SandboxSpec) (*runtime.SandboxInfo, error) {
	if spec.WorkspaceFUSE == nil {
		return nil, runtime.ErrWorkspaceFUSEUnsupported
	}
	if _, _, err := createContainerConfigWithSecretRoot(spec, r.effectiveSecretRoot()); err != nil {
		return nil, err
	}
	fuse := spec.WorkspaceFUSE
	if fuse.SystemEgress.Mode != runtime.SystemEgressCIDR {
		return nil, fmt.Errorf("Docker workspace FUSE supports only CIDR system egress")
	}
	if r.secretMaterializer == nil {
		return nil, fmt.Errorf("Docker workspace FUSE secret materializer is unavailable")
	}
	preparationID, err := newDockerPreparationID()
	if err != nil {
		return nil, fmt.Errorf("create workspace FUSE preparation identity: %w", err)
	}
	systemEgressLabel, systemEgress, err := encodeDockerSystemEgress(fuse.SystemEgress)
	if err != nil {
		return nil, err
	}
	cacheBytes, err := parseMemory(fuse.CacheSize)
	if err != nil {
		return nil, fmt.Errorf("parse workspace FUSE cache size: %w", err)
	}
	resourceSpec := spec
	resourceSpec.ID = preparationID
	resourceSpec.Labels = cloneLabels(spec.Labels)
	resourceSpec.Labels[dockerPreparationLabel] = preparationID
	resourceSpec.Labels[dockerLogicalIDLabel] = spec.ID
	resourceSpec.Labels[dockerSystemEgressLabel] = systemEgressLabel
	resourceSpec.Labels[dockerCacheBytesLabel] = fmt.Sprintf("%d", cacheBytes)
	resourceSpec.Labels[dockerSecretRootLabel] = r.effectiveSecretRoot()
	resourceState := &dockerWorkspaceState{
		sandboxID: spec.ID, preparationID: preparationID, poolKey: fuse.PoolKey,
		cacheVolume: fuseCacheVolumeName(preparationID), cacheBytes: cacheBytes, secretRoot: r.effectiveSecretRoot(), systemEgress: systemEgress,
	}
	tombstoneKey := dockerPreparationStateKey(preparationID)
	r.storeWorkspaceState(tombstoneKey, resourceState)
	secretTarget := filepath.Join(r.effectiveSecretRoot(), preparationID)
	if err := r.secretMaterializer.Materialize(ctx, resourceSpec, secretTarget); err != nil {
		return nil, r.rollbackPreparation(tombstoneKey, resourceState, fmt.Errorf("materialize workspace FUSE secret: %w", err))
	}
	validator := r.secretValidator
	if validator == nil {
		validator = validateRootSecretDirectory
	}
	if err := validator(secretTarget, fuse.CASecretKey); err != nil {
		return nil, r.rollbackPreparation(tombstoneKey, resourceState, fmt.Errorf("validate workspace FUSE secret: %w", err))
	}
	cacheVolume := resourceState.cacheVolume
	if _, err := r.cli.VolumeCreate(ctx, volume.CreateOptions{Name: cacheVolume, Labels: map[string]string{
		"sandbox.managed": "true", "sandbox.id": preparationID, "sandbox.role": "fuse-cache",
		dockerPreparationLabel: preparationID, dockerLogicalIDLabel: spec.ID, dockerCacheBytesLabel: fmt.Sprintf("%d", cacheBytes), dockerSecretRootLabel: r.effectiveSecretRoot(),
	}}); err != nil {
		createErr := fmt.Errorf("create workspace FUSE cache volume: %w", err)
		verifyCtx, cancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
		_, inspectErr := r.inspectExactPreparationVolume(verifyCtx, resourceState)
		cancel()
		if inspectErr != nil && !dockerclient.IsErrNotFound(inspectErr) {
			return nil, errors.Join(createErr, inspectErr, runtime.ErrTerminationUnconfirmed)
		}
		return nil, r.rollbackPreparation(tombstoneKey, resourceState, createErr)
	}
	if _, err := r.inspectExactPreparationVolume(ctx, resourceState); err != nil {
		return nil, r.rollbackPreparation(tombstoneKey, resourceState, fmt.Errorf("verify workspace FUSE cache volume identity: %w", err))
	}
	pairNetworkID, _, gatewayIP, err := createFUSESandboxPair(ctx, r.cli, preparationID, r.openNetworkID, r.gatewayImage, r.effectiveSecretRoot(), systemEgress)
	if err != nil {
		return nil, r.rollbackPreparation(tombstoneKey, resourceState, fmt.Errorf("create workspace system egress gateway: %w", err))
	}
	containerID, err := createContainer(ctx, r.cli, resourceSpec, pairNetworkID, r.effectiveSecretRoot())
	if err != nil {
		verifyCtx, cancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
		absent, verifyErr := r.confirmNoFUSERuntime(verifyCtx, preparationID)
		cancel()
		if verifyErr == nil && absent {
			return nil, r.rollbackPreparation(tombstoneKey, resourceState, err)
		} else {
			return nil, errors.Join(err, verifyErr, runtime.ErrTerminationUnconfirmed)
		}
	}
	resourceState.runtimeUID = containerID
	r.moveWorkspaceState(tombstoneKey, containerID, resourceState)
	cleanupContainer := func(cause error) error {
		return r.terminateAndRollbackPreparation(containerID, resourceState, cause)
	}
	if err := r.cli.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return nil, cleanupContainer(fmt.Errorf("start workspace FUSE container: %w", err))
	}
	if err := r.setupFUSERoute(ctx, containerID, pairNetworkID, gatewayIP); err != nil {
		return nil, cleanupContainer(fmt.Errorf("setup workspace FUSE system route: %w", err))
	}
	canonicalEndpoint, _, _, err := canonicalDockerFUSEEndpoint(fuse)
	if err != nil {
		return nil, cleanupContainer(err)
	}
	bootstrap := fuseprotocol.BootstrapConfig{
		Version: fuseprotocol.Version, RuntimeUID: containerID, Provider: fuse.Provider, Bucket: fuse.Bucket,
		Endpoint: canonicalEndpoint, Region: fuse.Region, Profile: fuse.Profile,
		AccessKeyFile: filepath.Join(dockerMounterSecretPath, "accessKey"), SecretKeyFile: filepath.Join(dockerMounterSecretPath, "secretKey"),
		PasswdFile: filepath.Join(dockerMounterRunPath, "passwd-s3fs"), CAFile: dockerSecretFilePath(fuse.CASecretKey),
		CacheDir: dockerMounterCachePath, MountPath: dockerWorkspacePath, PoolKey: fuse.PoolKey, CacheLimitBytes: cacheBytes,
		MountTimeoutSeconds: ceilDockerSeconds(fuse.MountTimeout), FlushTimeoutSeconds: ceilDockerSeconds(fuse.FlushTimeout), UnmountTimeoutSeconds: ceilDockerSeconds(fuse.UnmountTimeout),
	}
	raw, err := marshalControl(bootstrap)
	if err != nil {
		return nil, cleanupContainer(err)
	}
	response, err := r.execControl(ctx, containerID, []string{fuseprotocol.MounterBinary, "bootstrap"}, raw)
	if err != nil {
		return nil, cleanupContainer(err)
	}
	ack, err := decodeControlAck(response)
	if err != nil || !ack.Accepted || ack.RuntimeUID != containerID || ack.Generation != 0 {
		return nil, cleanupContainer(fmt.Errorf("workspace bootstrap acknowledgement is invalid"))
	}
	ref := runtime.RuntimeRef{ID: containerID, UID: containerID}
	if err := r.PreparedSandboxHealth(ctx, ref, fuse.PoolKey); err != nil {
		return nil, cleanupContainer(err)
	}
	return &runtime.SandboxInfo{ID: spec.ID, RuntimeID: containerID, RuntimeUID: containerID, State: "running", CreatedAt: time.Now()}, nil
}

func (r *Runtime) confirmNoFUSERuntime(ctx context.Context, sandboxID string) (bool, error) {
	items, err := r.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(
		filters.Arg("label", "sandbox.managed=true"), filters.Arg("label", "sandbox.role=fuse-runtime"), filters.Arg("label", "sandbox.id="+sandboxID),
	)})
	if err != nil {
		return false, err
	}
	return len(items) == 0, nil
}

func newDockerPreparationID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "prep-" + hex.EncodeToString(random[:]), nil
}

func encodeDockerSystemEgress(spec runtime.SystemEgressSpec) (string, runtime.SystemEgressSpec, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return "", runtime.SystemEgressSpec{}, fmt.Errorf("encode Docker system egress recovery contract: %w", err)
	}
	var clone runtime.SystemEgressSpec
	if err := json.Unmarshal(raw, &clone); err != nil {
		return "", runtime.SystemEgressSpec{}, fmt.Errorf("clone Docker system egress contract: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), clone, nil
}

func decodeDockerSystemEgress(encoded string) (runtime.SystemEgressSpec, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) == 0 {
		return runtime.SystemEgressSpec{}, fmt.Errorf("Docker system egress recovery contract is invalid")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var spec runtime.SystemEgressSpec
	if err := decoder.Decode(&spec); err != nil || decoder.Decode(&struct{}{}) != io.EOF || spec.Mode != runtime.SystemEgressCIDR {
		return runtime.SystemEgressSpec{}, fmt.Errorf("Docker system egress recovery contract is invalid")
	}
	canonical, _, err := encodeDockerSystemEgress(spec)
	if err != nil || canonical != encoded {
		return runtime.SystemEgressSpec{}, fmt.Errorf("Docker system egress recovery contract is not canonical")
	}
	return spec, nil
}

func dockerPreparationStateKey(preparationID string) string { return "preparation:" + preparationID }

func (r *Runtime) storeWorkspaceState(key string, state *dockerWorkspaceState) {
	r.stateMu.Lock()
	r.ensureWorkspaceStatesLocked()[key] = state
	r.stateMu.Unlock()
}

func (r *Runtime) moveWorkspaceState(from, to string, state *dockerWorkspaceState) {
	r.stateMu.Lock()
	delete(r.ensureWorkspaceStatesLocked(), from)
	r.ensureWorkspaceStatesLocked()[to] = state
	r.stateMu.Unlock()
}

func (r *Runtime) deleteWorkspaceState(key string, expected *dockerWorkspaceState) {
	r.stateMu.Lock()
	if r.ensureWorkspaceStatesLocked()[key] == expected {
		delete(r.ensureWorkspaceStatesLocked(), key)
	}
	r.stateMu.Unlock()
}

func (r *Runtime) rollbackPreparation(key string, state *dockerWorkspaceState, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
	defer cancel()
	if cleanupErr := r.cleanupPreparationResources(cleanupCtx, state); cleanupErr != nil {
		state.mu.Lock()
		state.poisoned = true
		state.mu.Unlock()
		return errors.Join(cause, runtime.ErrTerminationUnconfirmed, cleanupErr)
	}
	r.deleteWorkspaceState(key, state)
	return cause
}

func (r *Runtime) terminateAndRollbackPreparation(runtimeID string, state *dockerWorkspaceState, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
	defer cancel()
	removeErr := r.cli.ContainerRemove(cleanupCtx, runtimeID, container.RemoveOptions{Force: true})
	_, inspectErr := r.cli.ContainerInspect(cleanupCtx, runtimeID)
	if !dockerclient.IsErrNotFound(inspectErr) {
		state.mu.Lock()
		state.poisoned = true
		state.mu.Unlock()
		return errors.Join(cause, removeErr, inspectErr, runtime.ErrTerminationUnconfirmed)
	}
	if cleanupErr := r.cleanupPreparationResources(cleanupCtx, state); cleanupErr != nil {
		state.mu.Lock()
		state.poisoned = true
		state.mu.Unlock()
		return errors.Join(cause, cleanupErr, runtime.ErrTerminationUnconfirmed)
	}
	r.deleteWorkspaceState(runtimeID, state)
	return cause
}

func (r *Runtime) AuthorizeWorkspaceMount(ctx context.Context, ref runtime.RuntimeRef, auth runtime.WorkspaceMountAuthorization) error {
	state, err := r.exactWorkspaceState(ctx, ref)
	if err != nil || auth.RuntimeUID != ref.UID || auth.PoolKey == "" || auth.PoolKey != state.poolKey || auth.WorkspaceHash == "" || auth.Prefix == "" || auth.LeaseGeneration <= 0 || auth.MountAttempt != 1 {
		return fmt.Errorf("invalid workspace mount authorization")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.authorized || state.authAttempted || state.generation != 0 || state.poisoned {
		return fmt.Errorf("workspace mount authorization already consumed")
	}
	state.authAttempted = true
	request := fuseprotocol.AuthorizeRequest{Version: fuseprotocol.Version, RuntimeUID: auth.RuntimeUID, PoolKey: auth.PoolKey, WorkspaceHash: auth.WorkspaceHash, Prefix: auth.Prefix, LeaseGeneration: auth.LeaseGeneration, MountAttempt: auth.MountAttempt}
	raw, _ := marshalControl(request)
	response, err := r.execControl(ctx, ref.ID, []string{fuseprotocol.MounterBinary, "authorize"}, raw)
	if err != nil {
		state.poisoned = true
		return err
	}
	ack, err := decodeControlAck(response)
	if err != nil || !ack.Accepted || ack.RuntimeUID != ref.UID || ack.Generation != auth.LeaseGeneration {
		state.poisoned = true
		return fmt.Errorf("workspace authorization acknowledgement is invalid")
	}
	state.authorized, state.generation = true, auth.LeaseGeneration
	return nil
}

func (r *Runtime) WaitSandboxReady(ctx context.Context, ref runtime.RuntimeRef, expectedGeneration int64) (*runtime.SandboxInfo, error) {
	state, err := r.exactWorkspaceState(ctx, ref)
	if err != nil {
		return nil, err
	}
	state.mu.Lock()
	if !state.authorized || state.generation != expectedGeneration || state.poisoned {
		state.mu.Unlock()
		return nil, fmt.Errorf("workspace authorization is unavailable")
	}
	poolKey, sandboxID := state.poolKey, state.sandboxID
	state.mu.Unlock()
	status, err := r.readMounterStatus(ctx, ref, "ready")
	if err != nil || status.State != "ready" || status.RuntimeUID != ref.UID || status.PoolKey != poolKey || status.MountType != "fuse" || status.Generation != expectedGeneration || status.RestartDetected || status.CacheLimitBytes != state.cacheBytes || status.CacheBytes < 0 || status.CacheBytes >= status.CacheLimitBytes || status.CacheExceeded {
		return nil, fmt.Errorf("workspace ready status does not match authorization")
	}
	raw, err := r.execProbe(ctx, ref.ID, dockerProbeArgv("write-read-delete", ref.UID, expectedGeneration, false), nil)
	if err != nil {
		return nil, err
	}
	probe, err := decodeProbeStatus(raw)
	if err != nil || !probe.OK || probe.RuntimeUID != ref.UID || probe.Generation != expectedGeneration || probe.Token != "" {
		return nil, fmt.Errorf("sandbox workspace propagation probe failed")
	}
	return &runtime.SandboxInfo{ID: sandboxID, RuntimeID: ref.ID, RuntimeUID: ref.UID, State: "running", CreatedAt: time.Now()}, nil
}

func (r *Runtime) PreparedSandboxHealth(ctx context.Context, ref runtime.RuntimeRef, poolKey string) error {
	if poolKey == "" {
		return fmt.Errorf("prepared pool key is empty")
	}
	state, err := r.exactWorkspaceState(ctx, ref)
	if err != nil {
		return err
	}
	state.mu.Lock()
	if state.authorized || state.authAttempted || state.poisoned || state.generation != 0 || state.poolKey != poolKey {
		state.mu.Unlock()
		return fmt.Errorf("prepared workspace state is not pristine")
	}
	state.mu.Unlock()
	status, err := r.readMounterStatus(ctx, ref, "prepared")
	if err != nil || status.State != "prepared" || status.RuntimeUID != ref.UID || status.PoolKey != poolKey || status.MountType != "" || status.Generation != 0 || status.RestartDetected || status.CacheLimitBytes != state.cacheBytes || status.CacheBytes != 0 || status.CacheExceeded {
		return fmt.Errorf("prepared workspace status is not pristine")
	}
	return nil
}

func (r *Runtime) WorkspaceHealth(ctx context.Context, ref runtime.RuntimeRef) (*runtime.WorkspaceHealth, error) {
	state, err := r.exactWorkspaceState(ctx, ref)
	if err != nil {
		return nil, err
	}
	status, err := r.readMounterStatus(ctx, ref, "ready")
	if err != nil {
		return nil, err
	}
	state.mu.Lock()
	if !state.authorized && state.generation == 0 && status.State == "ready" && status.RuntimeUID == ref.UID && status.PoolKey == state.poolKey && status.Generation > 0 && status.MountType == "fuse" && !status.RestartDetected {
		// Restart adoption derives the generation only from the exact root
		// supervisor's persisted status; it never replays authorization.
		state.authorized, state.authAttempted, state.generation = true, true, status.Generation
	}
	ready := state.authorized && !state.poisoned && status.State == "ready" && status.RuntimeUID == ref.UID && status.PoolKey == state.poolKey && status.Generation == state.generation && status.MountType == "fuse" && !status.RestartDetected && status.CacheLimitBytes == state.cacheBytes && status.CacheBytes >= 0 && status.CacheBytes < status.CacheLimitBytes && !status.CacheExceeded
	state.mu.Unlock()
	return &runtime.WorkspaceHealth{
		Ready: ready, MountType: status.MountType, RuntimeUID: status.RuntimeUID, Generation: status.Generation,
		RestartDetected: status.RestartDetected, CacheBytes: status.CacheBytes, CacheLimitBytes: status.CacheLimitBytes,
		CacheExceeded: status.CacheExceeded, LastSuccessful: time.Now(),
	}, nil
}

func (r *Runtime) QuiesceWorkspace(ctx context.Context, ref runtime.RuntimeRef, expectedGeneration int64) (runtime.WorkspaceQuiesceToken, error) {
	state, err := r.exactWorkspaceState(ctx, ref)
	if err != nil {
		return runtime.WorkspaceQuiesceToken{}, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.authorized || state.generation != expectedGeneration || state.quiesceToken != nil || state.poisoned {
		return runtime.WorkspaceQuiesceToken{}, fmt.Errorf("workspace cannot be quiesced")
	}
	raw, err := r.execProbe(ctx, ref.ID, dockerProbeArgv("quiesce", ref.UID, expectedGeneration, false), nil)
	if err != nil {
		state.poisoned = true
		return runtime.WorkspaceQuiesceToken{}, err
	}
	probe, err := decodeProbeStatus(raw)
	if err != nil || !probe.OK || probe.RuntimeUID != ref.UID || probe.Generation != expectedGeneration || probe.Token == "" {
		state.poisoned = true
		return runtime.WorkspaceQuiesceToken{}, fmt.Errorf("workspace quiesce acknowledgement is invalid")
	}
	token := runtime.WorkspaceQuiesceToken{RuntimeUID: ref.UID, Generation: expectedGeneration, Opaque: probe.Token}
	state.quiesceToken = &token
	return token, nil
}

func (r *Runtime) ResumeWorkspace(ctx context.Context, ref runtime.RuntimeRef, token runtime.WorkspaceQuiesceToken) error {
	state, err := r.exactWorkspaceState(ctx, ref)
	if err != nil {
		return err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.quiesceToken == nil || *state.quiesceToken != token || token.RuntimeUID != ref.UID || token.Generation != state.generation || token.Opaque == "" || state.poisoned {
		return fmt.Errorf("workspace quiesce token is invalid")
	}
	state.quiesceToken = nil
	state.poisoned = true
	request := fuseprotocol.ProbeResumeRequest{Version: fuseprotocol.Version, RuntimeUID: ref.UID, Generation: token.Generation, Token: token.Opaque}
	raw, _ := marshalControl(request)
	response, err := r.execProbe(ctx, ref.ID, dockerProbeArgv("resume", ref.UID, token.Generation, true), raw)
	if err != nil {
		return err
	}
	probe, err := decodeProbeStatus(response)
	if err != nil || !probe.OK || probe.RuntimeUID != ref.UID || probe.Generation != token.Generation || probe.Token != "" {
		return fmt.Errorf("workspace resume acknowledgement is invalid")
	}
	state.poisoned = false
	return nil
}

func (r *Runtime) FlushWorkspace(ctx context.Context, ref runtime.RuntimeRef, expectedGeneration int64) error {
	state, err := r.exactWorkspaceState(ctx, ref)
	if err != nil {
		return err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.generation != expectedGeneration || state.quiesceToken == nil || state.poisoned {
		return fmt.Errorf("workspace generation is unavailable")
	}
	request := fuseprotocol.ControlRequest{Version: fuseprotocol.Version, RuntimeUID: ref.UID, Generation: expectedGeneration}
	raw, _ := marshalControl(request)
	response, err := r.execControl(ctx, ref.ID, []string{fuseprotocol.MounterBinary, "flush"}, raw)
	if err != nil {
		state.poisoned = true
		return err
	}
	ack, err := decodeControlAck(response)
	if err != nil || !ack.Accepted || ack.RuntimeUID != ref.UID || ack.Generation != expectedGeneration {
		state.poisoned = true
		return fmt.Errorf("workspace flush acknowledgement is invalid")
	}
	return nil
}

func (r *Runtime) readMounterStatus(ctx context.Context, ref runtime.RuntimeRef, state string) (fuseprotocol.MounterStatus, error) {
	raw, err := r.execControl(ctx, ref.ID, []string{fuseprotocol.MounterBinary, "health", state}, nil)
	if err != nil {
		return fuseprotocol.MounterStatus{}, err
	}
	return decodeMounterStatus(raw)
}

func (r *Runtime) exactWorkspaceState(ctx context.Context, ref runtime.RuntimeRef) (*dockerWorkspaceState, error) {
	if err := ref.Validate(); err != nil || ref.ID != ref.UID {
		return nil, runtime.ErrInvalidRuntimeRef
	}
	info, err := r.cli.ContainerInspect(ctx, ref.ID)
	if err != nil || info.ID != ref.UID || info.Config == nil || info.Config.Labels["sandbox.role"] != "fuse-runtime" || info.Config.Labels["sandbox.managed"] != "true" || info.State == nil || !info.State.Running {
		return nil, fmt.Errorf("exact Docker FUSE runtime is unavailable")
	}
	recovered, err := dockerWorkspaceStateFromLabels(ref.UID, info.Config.Labels)
	if err != nil {
		return nil, err
	}
	if err := r.requireCurrentWorkspaceSecretRoot(recovered); err != nil {
		return nil, err
	}
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if state := r.ensureWorkspaceStatesLocked()[ref.ID]; state != nil {
		if state.runtimeUID != recovered.runtimeUID || state.sandboxID != recovered.sandboxID || state.preparationID != recovered.preparationID || state.poolKey != recovered.poolKey || state.cacheVolume != recovered.cacheVolume || state.cacheBytes != recovered.cacheBytes || !systemEgressEqual(state.systemEgress, recovered.systemEgress) {
			return nil, runtime.ErrInvalidRuntimeRef
		}
		return state, nil
	}
	// Restart adoption trusts the immutable container ID and fixed labels. The
	// mounter's persisted bootstrap and health response are still revalidated by
	// the operation that follows; no authorization is replayed here.
	r.ensureWorkspaceStatesLocked()[ref.ID] = recovered
	return recovered, nil
}

func dockerWorkspaceStateFromLabels(runtimeUID string, labels map[string]string) (*dockerWorkspaceState, error) {
	preparationID := labels[dockerPreparationLabel]
	logicalID := labels[dockerLogicalIDLabel]
	poolKey := labels["sandbox.pool.key"]
	cacheBytes, err := strconv.ParseInt(labels[dockerCacheBytesLabel], 10, 64)
	secretRoot := labels[dockerSecretRootLabel]
	if runtimeUID == "" || !validDockerPreparationID(preparationID) || labels["sandbox.id"] != preparationID || logicalID == "" || poolKey == "" || cacheBytes <= 0 || err != nil || !validDockerWorkspaceSecretRoot(secretRoot) {
		return nil, fmt.Errorf("Docker workspace recovery identity is invalid")
	}
	systemEgress, err := decodeDockerSystemEgress(labels[dockerSystemEgressLabel])
	if err != nil {
		return nil, err
	}
	return &dockerWorkspaceState{
		runtimeUID: runtimeUID, sandboxID: logicalID, preparationID: preparationID, poolKey: poolKey,
		cacheVolume: fuseCacheVolumeName(preparationID), cacheBytes: cacheBytes, secretRoot: secretRoot, systemEgress: systemEgress,
	}, nil
}

func (r *Runtime) requireCurrentWorkspaceSecretRoot(state *dockerWorkspaceState) error {
	if state == nil || !validDockerWorkspaceSecretRoot(state.secretRoot) || state.secretRoot != r.effectiveSecretRoot() {
		return fmt.Errorf("Docker workspace secret root changed while managed FUSE resources still exist")
	}
	return nil
}

func systemEgressEqual(a, b runtime.SystemEgressSpec) bool {
	aRaw, _, aErr := encodeDockerSystemEgress(a)
	bRaw, _, bErr := encodeDockerSystemEgress(b)
	return aErr == nil && bErr == nil && aRaw == bRaw
}

func (r *Runtime) ensureWorkspaceStatesLocked() map[string]*dockerWorkspaceState {
	if r.workspaceStates == nil {
		r.workspaceStates = make(map[string]*dockerWorkspaceState)
	}
	return r.workspaceStates
}

func dockerSecretFilePath(name string) string {
	if name == "" {
		return ""
	}
	return filepath.Join(dockerMounterSecretPath, filepath.Base(name))
}

func ceilDockerSeconds(value time.Duration) int64 {
	if value <= 0 {
		return 0
	}
	return int64((value + time.Second - 1) / time.Second)
}

func (r *Runtime) StartSandbox(ctx context.Context, id string) error {
	return r.cli.ContainerStart(ctx, id, container.StartOptions{})
}

func (r *Runtime) StopSandbox(ctx context.Context, id string) error {
	timeout := 10
	return r.cli.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeout})
}

func (r *Runtime) RemoveSandbox(ctx context.Context, id string) error {
	// Inspect container to find sandbox ID for gateway/network cleanup
	var sandboxID string
	info, err := r.cli.ContainerInspect(ctx, id)
	if err == nil && info.Config != nil && info.Config.Labels != nil {
		sandboxID = info.Config.Labels["sandbox.id"]
	}

	if info.Config != nil && info.Config.Labels["sandbox.role"] == "fuse-runtime" && info.ID != "" {
		return r.RemovePreparedSandbox(ctx, info.ID, info.ID)
	}

	// Remove the legacy sandbox container
	removeErr := r.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true})
	if removeErr != nil && dockerclient.IsErrNotFound(removeErr) {
		removeErr = nil
	}

	// Always attempt gateway/network cleanup regardless of container removal result
	if sandboxID != "" {
		_ = removeSandboxPair(ctx, r.cli, sandboxID)
	}

	if removeErr != nil {
		return fmt.Errorf("remove container: %w", removeErr)
	}
	return nil
}

func (r *Runtime) RemovePreparedSandbox(_ context.Context, runtimeID, runtimeUID string) error {
	ref, err := runtime.NewRuntimeRef(runtimeID, runtimeUID)
	if err != nil || runtimeID != runtimeUID {
		return runtime.ErrInvalidRuntimeRef
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
	defer cancel()
	state := r.workspaceState(runtimeID)
	info, err := r.cli.ContainerInspect(cleanupCtx, runtimeID)
	if err != nil {
		if dockerclient.IsErrNotFound(err) {
			if state == nil || state.runtimeUID != runtimeUID {
				return runtime.ErrTerminationUnconfirmed
			}
			return r.cleanupConfirmedFUSEResources(runtimeID, state)
		}
		return runtime.ErrTerminationUnconfirmed
	}
	if info.ID != runtimeUID || info.Config == nil || info.Config.Labels["sandbox.role"] != "fuse-runtime" {
		return runtime.ErrInvalidRuntimeRef
	}
	recovered, err := dockerWorkspaceStateFromLabels(runtimeUID, info.Config.Labels)
	if err != nil {
		return runtime.ErrInvalidRuntimeRef
	}
	if err := r.requireCurrentWorkspaceSecretRoot(recovered); err != nil {
		return errors.Join(err, runtime.ErrTerminationUnconfirmed)
	}
	if state == nil {
		state = recovered
		r.storeWorkspaceState(runtimeID, state)
	} else if !sameDockerWorkspaceResourceIdentity(state, recovered) {
		return runtime.ErrInvalidRuntimeRef
	}
	generation := int64(0)
	if exact, stateErr := r.exactWorkspaceState(cleanupCtx, ref); stateErr == nil {
		exact.mu.Lock()
		generation = exact.generation
		exact.mu.Unlock()
	}
	request := fuseprotocol.ControlRequest{Version: fuseprotocol.Version, RuntimeUID: runtimeUID, Generation: generation}
	if raw, marshalErr := marshalControl(request); marshalErr == nil {
		// A locked/prepared supervisor rejects generation zero; shutdown is
		// best-effort here and exact daemon termination evidence remains mandatory.
		_, _ = r.execControl(cleanupCtx, runtimeID, []string{fuseprotocol.MounterBinary, "shutdown"}, raw)
	}
	removeErr := r.cli.ContainerRemove(cleanupCtx, runtimeID, container.RemoveOptions{Force: true})
	if _, inspectErr := r.cli.ContainerInspect(cleanupCtx, runtimeID); !dockerclient.IsErrNotFound(inspectErr) {
		if state != nil {
			state.mu.Lock()
			state.poisoned = true
			state.mu.Unlock()
		}
		return errors.Join(removeErr, inspectErr, runtime.ErrTerminationUnconfirmed)
	}
	if cleanupErr := r.cleanupPreparationResources(cleanupCtx, state); cleanupErr != nil {
		state.mu.Lock()
		state.poisoned = true
		state.mu.Unlock()
		return errors.Join(cleanupErr, runtime.ErrTerminationUnconfirmed)
	}
	r.deleteWorkspaceState(runtimeID, state)
	return nil
}

func sameDockerWorkspaceResourceIdentity(a, b *dockerWorkspaceState) bool {
	return a != nil && b != nil && a.runtimeUID == b.runtimeUID && a.sandboxID == b.sandboxID && a.preparationID == b.preparationID && a.poolKey == b.poolKey && a.cacheVolume == b.cacheVolume && a.cacheBytes == b.cacheBytes && a.secretRoot == b.secretRoot && systemEgressEqual(a.systemEgress, b.systemEgress)
}

func (r *Runtime) cleanupConfirmedFUSEResources(runtimeID string, state *dockerWorkspaceState) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
	defer cancel()
	if err := r.cleanupPreparationResources(cleanupCtx, state); err != nil {
		state.mu.Lock()
		state.poisoned = true
		state.mu.Unlock()
		return errors.Join(err, runtime.ErrTerminationUnconfirmed)
	}
	r.deleteWorkspaceState(runtimeID, state)
	return nil
}

func (r *Runtime) inspectExactPreparationVolume(ctx context.Context, state *dockerWorkspaceState) (volume.Volume, error) {
	item, err := r.cli.VolumeInspect(ctx, state.cacheVolume)
	if err != nil {
		return volume.Volume{}, err
	}
	if item.Name != state.cacheVolume || item.Labels[dockerPreparationLabel] != state.preparationID || item.Labels["sandbox.role"] != "fuse-cache" || item.Labels[dockerLogicalIDLabel] != state.sandboxID || item.Labels[dockerCacheBytesLabel] != fmt.Sprintf("%d", state.cacheBytes) || item.Labels[dockerSecretRootLabel] != state.secretRoot {
		return volume.Volume{}, fmt.Errorf("workspace cache volume identity is invalid")
	}
	return item, nil
}

func (r *Runtime) cleanupPreparationResources(ctx context.Context, state *dockerWorkspaceState) error {
	if state == nil || state.preparationID == "" || state.cacheVolume != fuseCacheVolumeName(state.preparationID) {
		return fmt.Errorf("workspace preparation identity is invalid")
	}
	if err := r.requireCurrentWorkspaceSecretRoot(state); err != nil {
		return err
	}
	var result error
	if _, err := r.inspectExactPreparationVolume(ctx, state); err == nil {
		removeErr := r.cli.VolumeRemove(ctx, state.cacheVolume, true)
		_, inspectErr := r.cli.VolumeInspect(ctx, state.cacheVolume)
		if !dockerclient.IsErrNotFound(inspectErr) {
			result = errors.Join(result, removeErr, inspectErr, fmt.Errorf("workspace cache volume termination is unconfirmed"))
		}
	} else if !dockerclient.IsErrNotFound(err) {
		result = errors.Join(result, err)
	}
	if err := removeSandboxPair(ctx, r.cli, state.preparationID); err != nil {
		result = errors.Join(result, err)
	}
	gateways, err := r.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(
		filters.Arg("label", "sandbox.managed=true"), filters.Arg("label", "sandbox.role=gateway"), filters.Arg("label", "sandbox.id="+state.preparationID),
	)})
	if err != nil || len(gateways) != 0 {
		result = errors.Join(result, err, fmt.Errorf("workspace gateway termination is unconfirmed"))
	}
	networks, err := r.cli.NetworkList(ctx, network.ListOptions{Filters: filters.NewArgs(filters.Arg("label", "sandbox.managed=true"), filters.Arg("label", "sandbox.id="+state.preparationID))})
	if err != nil || len(networks) != 0 {
		result = errors.Join(result, err, fmt.Errorf("workspace network termination is unconfirmed"))
	}
	if r.secretMaterializer == nil {
		result = errors.Join(result, fmt.Errorf("workspace secret materializer is unavailable during cleanup"))
	} else if err := r.secretMaterializer.RemoveSecret(ctx, filepath.Join(state.secretRoot, state.preparationID)); err != nil {
		result = errors.Join(result, err)
	}
	return result
}

func (r *Runtime) ConfirmTerminated(ctx context.Context, runtimeID, runtimeUID string) (runtime.TerminationEvidence, error) {
	if runtimeID == "" || runtimeID != runtimeUID {
		return runtime.TerminationEvidence{}, runtime.ErrInvalidRuntimeRef
	}
	_, err := r.cli.ContainerInspect(ctx, runtimeID)
	if err == nil || !dockerclient.IsErrNotFound(err) {
		return runtime.TerminationEvidence{}, runtime.ErrTerminationUnconfirmed
	}
	return runtime.TerminationEvidence{RuntimeUID: runtimeUID, ProcessExited: true}, nil
}

func (r *Runtime) ReconcileOrphanedResources(ctx context.Context, protectedRuntimeUIDs map[string]struct{}) error {
	containers, err := r.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", "sandbox.managed=true"))})
	if err != nil {
		return errors.Join(fmt.Errorf("list managed Docker resources: %w", err), runtime.ErrTerminationUnconfirmed)
	}
	var result error
	protectedPreparations := make(map[string]struct{})
	blockedPreparations := make(map[string]struct{})
	orphanPreparations := make(map[string]*dockerWorkspaceState)
	addOrphanPreparation := func(state *dockerWorkspaceState) {
		if err := r.requireCurrentWorkspaceSecretRoot(state); err != nil {
			result = errors.Join(result, err, runtime.ErrTerminationUnconfirmed)
			if state != nil && validDockerPreparationID(state.preparationID) {
				blockedPreparations[state.preparationID] = struct{}{}
			}
			return
		}
		if existing := orphanPreparations[state.preparationID]; existing != nil {
			if existing.secretRoot != state.secretRoot {
				result = errors.Join(result, fmt.Errorf("managed Docker resources disagree on workspace secret root"), runtime.ErrTerminationUnconfirmed)
				blockedPreparations[state.preparationID] = struct{}{}
				return
			}
			if existing.sandboxID != "" || state.sandboxID == "" {
				return
			}
		}
		orphanPreparations[state.preparationID] = state
	}
	for _, item := range containers {
		role := item.Labels["sandbox.role"]
		if role == "gateway" {
			if item.Labels["sandbox.gateway.contract"] != "fuse-v1" {
				// The legacy Docker lifecycle uses the same managed/gateway labels.
				// Its resources are outside this FUSE-only reconciler's authority.
				continue
			}
			state, stateErr := cleanupOnlyPreparationStateFromLabels(item.Labels)
			if stateErr != nil {
				result = errors.Join(result, fmt.Errorf("managed Docker gateway preparation identity is invalid"), runtime.ErrTerminationUnconfirmed)
			} else {
				addOrphanPreparation(state)
			}
			continue
		}
		if role != "fuse-runtime" {
			continue
		}
		state, stateErr := dockerWorkspaceStateFromLabels(item.ID, item.Labels)
		if stateErr != nil {
			result = errors.Join(result, stateErr, runtime.ErrTerminationUnconfirmed)
			if preparationID := item.Labels["sandbox.id"]; validDockerPreparationID(preparationID) {
				blockedPreparations[preparationID] = struct{}{}
			}
			continue
		}
		if rootErr := r.requireCurrentWorkspaceSecretRoot(state); rootErr != nil {
			result = errors.Join(result, rootErr, runtime.ErrTerminationUnconfirmed)
			blockedPreparations[state.preparationID] = struct{}{}
			continue
		}
		if _, protected := protectedRuntimeUIDs[item.ID]; protected {
			protectedPreparations[state.preparationID] = struct{}{}
			continue
		}
		r.storeWorkspaceState(item.ID, state)
		if err := r.RemovePreparedSandbox(ctx, item.ID, item.ID); err != nil {
			result = errors.Join(result, err)
			blockedPreparations[state.preparationID] = struct{}{}
		}
	}
	volumes, err := r.cli.VolumeList(ctx, volume.ListOptions{Filters: filters.NewArgs(filters.Arg("label", "sandbox.managed=true"), filters.Arg("label", "sandbox.role=fuse-cache"))})
	if err != nil {
		return errors.Join(result, err, runtime.ErrTerminationUnconfirmed)
	}
	for _, item := range volumes.Volumes {
		state, stateErr := cleanupPreparationStateFromVolume(item)
		if stateErr != nil {
			result = errors.Join(result, stateErr, runtime.ErrTerminationUnconfirmed)
			continue
		}
		addOrphanPreparation(state)
	}
	networks, err := r.cli.NetworkList(ctx, network.ListOptions{Filters: filters.NewArgs(filters.Arg("label", "sandbox.managed=true"), filters.Arg("label", "sandbox.role=fuse-pair"))})
	if err != nil {
		return errors.Join(result, err, runtime.ErrTerminationUnconfirmed)
	}
	for _, item := range networks {
		state, stateErr := cleanupOnlyPreparationStateFromLabels(item.Labels)
		if stateErr != nil {
			result = errors.Join(result, fmt.Errorf("managed Docker network preparation identity is invalid"), runtime.ErrTerminationUnconfirmed)
		} else {
			addOrphanPreparation(state)
		}
	}
	if r.secretMaterializer == nil {
		if len(orphanPreparations) != 0 {
			return errors.Join(result, fmt.Errorf("workspace secret materializer is unavailable during reconciliation"), runtime.ErrTerminationUnconfirmed)
		}
	} else {
		secretPreparations, err := r.secretMaterializer.ListSecretPreparations(ctx)
		if err != nil {
			return errors.Join(result, err, runtime.ErrTerminationUnconfirmed)
		}
		for _, preparationID := range secretPreparations {
			if !validDockerPreparationID(preparationID) {
				result = errors.Join(result, fmt.Errorf("managed Docker secret preparation identity is invalid"), runtime.ErrTerminationUnconfirmed)
				continue
			}
			if _, exists := orphanPreparations[preparationID]; !exists {
				addOrphanPreparation(cleanupOnlyPreparationState(preparationID, r.effectiveSecretRoot()))
			}
		}
	}
	for preparationID, state := range orphanPreparations {
		if _, protected := protectedPreparations[preparationID]; protected {
			continue
		}
		if _, blocked := blockedPreparations[preparationID]; blocked {
			continue
		}
		key := dockerPreparationStateKey(preparationID)
		r.storeWorkspaceState(key, state)
		cleanupCtx, cancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
		cleanupErr := r.cleanupPreparationResources(cleanupCtx, state)
		cancel()
		if cleanupErr != nil {
			state.mu.Lock()
			state.poisoned = true
			state.mu.Unlock()
			result = errors.Join(result, cleanupErr, runtime.ErrTerminationUnconfirmed)
			continue
		}
		r.deleteWorkspaceState(key, state)
	}
	return result
}

func cleanupPreparationStateFromVolume(item *volume.Volume) (*dockerWorkspaceState, error) {
	if item == nil || item.Labels["sandbox.role"] != "fuse-cache" {
		return nil, fmt.Errorf("managed Docker cache volume identity is invalid")
	}
	preparationID := item.Labels[dockerPreparationLabel]
	cacheBytes, err := strconv.ParseInt(item.Labels[dockerCacheBytesLabel], 10, 64)
	secretRoot := item.Labels[dockerSecretRootLabel]
	if !validDockerPreparationID(preparationID) || item.Labels["sandbox.id"] != preparationID || item.Name != fuseCacheVolumeName(preparationID) || item.Labels[dockerLogicalIDLabel] == "" || err != nil || cacheBytes <= 0 || !validDockerWorkspaceSecretRoot(secretRoot) {
		return nil, fmt.Errorf("managed Docker cache volume identity is invalid")
	}
	return &dockerWorkspaceState{sandboxID: item.Labels[dockerLogicalIDLabel], preparationID: preparationID, cacheVolume: item.Name, cacheBytes: cacheBytes, secretRoot: secretRoot}, nil
}

func cleanupOnlyPreparationStateFromLabels(labels map[string]string) (*dockerWorkspaceState, error) {
	preparationID := labels["sandbox.id"]
	secretRoot := labels[dockerSecretRootLabel]
	if !validDockerPreparationID(preparationID) || !validDockerWorkspaceSecretRoot(secretRoot) {
		return nil, fmt.Errorf("managed Docker preparation identity is invalid")
	}
	return cleanupOnlyPreparationState(preparationID, secretRoot), nil
}

func cleanupOnlyPreparationState(preparationID, secretRoot string) *dockerWorkspaceState {
	return &dockerWorkspaceState{preparationID: preparationID, cacheVolume: fuseCacheVolumeName(preparationID), secretRoot: secretRoot}
}

func validDockerPreparationID(value string) bool {
	if !strings.HasPrefix(value, "prep-") || len(value) != len("prep-")+32 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "prep-"))
	return err == nil
}

func (r *Runtime) workspaceState(runtimeID string) *dockerWorkspaceState {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	return r.ensureWorkspaceStatesLocked()[runtimeID]
}

func (r *Runtime) effectiveSecretRoot() string {
	if r.secretRoot == "" {
		return dockerWorkspaceSecretRoot
	}
	return r.secretRoot
}

func (r *Runtime) GetSandbox(ctx context.Context, id string) (*runtime.SandboxInfo, error) {
	info, err := r.cli.ContainerInspect(ctx, id)
	if err != nil {
		return nil, err
	}

	state := "unknown"
	if info.State.Running {
		state = "running"
	} else if info.State.Paused {
		state = "paused"
	} else {
		state = "stopped"
	}

	created, _ := time.Parse(time.RFC3339Nano, info.Created)

	return &runtime.SandboxInfo{
		ID:         id,
		RuntimeID:  info.ID,
		RuntimeUID: info.ID,
		State:      state,
		CreatedAt:  created,
	}, nil
}

func (r *Runtime) UpdateNetwork(ctx context.Context, containerID string, enabled bool, whitelist []string, blockPrivate bool) error {
	// Get sandbox ID from container — use the container name which equals spec.ID
	info, err := r.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return fmt.Errorf("inspect container: %w", err)
	}
	// Docker container names start with "/", strip it
	sandboxID := strings.TrimPrefix(info.Name, "/")

	hasGateway := hasExistingGateway(ctx, r.cli, sandboxID)

	if enabled && !hasGateway {
		// Enable network: create gateway pair and connect sandbox
		pairNetworkID, _, gatewayIP, err := createSandboxPair(
			ctx, r.cli, sandboxID, r.openNetworkID, r.gatewayImage, whitelist, blockPrivate,
		)
		if err != nil {
			return fmt.Errorf("create gateway pair: %w", err)
		}

		// Connect sandbox container to the pair network
		if err := r.cli.NetworkConnect(ctx, pairNetworkID, containerID, nil); err != nil {
			_ = removeSandboxPair(ctx, r.cli, sandboxID)
			return fmt.Errorf("connect sandbox to pair network: %w", err)
		}

		// Set default route through gateway
		if err := setupSandboxRoute(ctx, r.cli, containerID, gatewayIP); err != nil {
			_ = removeSandboxPair(ctx, r.cli, sandboxID)
			return fmt.Errorf("setup sandbox route: %w", err)
		}

		return nil
	}

	if enabled && hasGateway {
		// Update whitelist: re-run iptables rules in existing gateway
		resolved, err := resolveWhitelist(whitelist)
		if err != nil {
			return err
		}

		gatewayID, err := findGatewayID(ctx, r.cli, sandboxID)
		if err != nil {
			return err
		}

		// Flush chains, reset policies to ACCEPT, then rebuild rules
		flushCmd := "iptables -F FORWARD && iptables -P FORWARD ACCEPT && iptables -t nat -F POSTROUTING" +
			" && ip6tables -F FORWARD && ip6tables -P FORWARD ACCEPT"
		iptablesCmd := flushCmd + " && " + buildGatewayIptablesCmd(resolved, blockPrivate)

		execCfg := container.ExecOptions{
			Cmd:  []string{"sh", "-c", iptablesCmd},
			User: "root",
		}
		execResp, err := r.cli.ContainerExecCreate(ctx, gatewayID, execCfg)
		if err != nil {
			return fmt.Errorf("create iptables exec: %w", err)
		}
		if err := r.cli.ContainerExecStart(ctx, execResp.ID, container.ExecStartOptions{}); err != nil {
			return fmt.Errorf("start iptables exec: %w", err)
		}
		return waitExecDone(ctx, r.cli, execResp.ID)
	}

	if !enabled && hasGateway {
		// Disable network: disconnect sandbox from pair network and clean up
		pairNetName := pairNetworkPrefix + sandboxID
		_ = r.cli.NetworkDisconnect(ctx, pairNetName, containerID, true)
		return removeSandboxPair(ctx, r.cli, sandboxID)
	}

	// !enabled && !hasGateway — already disabled, nothing to do
	return nil
}

func (r *Runtime) UpdateFUSENetwork(ctx context.Context, ref runtime.RuntimeRef, enabled bool, whitelist []string, blockPrivate bool) error {
	state, err := r.exactWorkspaceState(ctx, ref)
	if err != nil {
		return err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	resolved, err := resolveFUSEWhitelist(whitelist)
	if err != nil {
		return err
	}
	for index, value := range resolved {
		if strings.Contains(value, "/") {
			continue
		}
		if strings.Contains(value, ":") {
			resolved[index] = value + "/128"
		} else {
			resolved[index] = value + "/32"
		}
	}
	command, err := buildFUSEGatewayUserIptablesCmd(enabled, resolved, blockPrivate)
	if err != nil {
		return err
	}
	gatewayID, err := findGatewayID(ctx, r.cli, state.preparationID)
	if err != nil {
		return runtime.ErrFUSENetworkStateUncertain
	}
	if err := runGatewayPolicy(ctx, r.cli, gatewayID, command); err != nil {
		return runtime.ErrFUSENetworkStateUncertain
	}
	return nil
}

func (r *Runtime) RenameSandbox(ctx context.Context, id string, newName string) error {
	return r.cli.ContainerRename(ctx, id, newName)
}

func (r *Runtime) UpdateLabels(_ context.Context, _ string, _ map[string]*string) error {
	// Docker container labels are immutable after creation.
	return nil
}

func (r *Runtime) ListSandboxes(ctx context.Context, labels map[string]string) ([]runtime.SandboxInfo, error) {
	args := filters.NewArgs()
	for k, v := range labels {
		args.Add("label", k+"="+v)
	}

	containers, err := r.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: args,
	})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	result := make([]runtime.SandboxInfo, 0, len(containers))
	for _, c := range containers {
		state := "unknown"
		switch c.State {
		case "running":
			state = "running"
		case "exited", "dead":
			state = c.State
		}
		result = append(result, runtime.SandboxInfo{
			ID:         c.Labels["sandbox.id"],
			RuntimeID:  c.ID,
			RuntimeUID: c.ID,
			State:      state,
			CreatedAt:  time.Unix(c.Created, 0),
		})
	}
	return result, nil
}

func (r *Runtime) IsStateful() bool {
	// Docker Engine owns container lifetime independently of sandbox-api.
	return true
}
