package kubernetes

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/netip"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
	typednetworkingv1 "k8s.io/client-go/kubernetes/typed/networking/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/goairix/sandbox/internal/fuseprotocol"
	"github.com/goairix/sandbox/internal/logger"
	"github.com/goairix/sandbox/internal/runtime"
)

const defaultKubernetesControlTimeout = 60 * time.Second

// InfrastructureFencer is an optional out-of-band authority that can prove an
// exact Pod UID/node can no longer access its workspace.
type InfrastructureFencer interface {
	FenceRuntime(ctx context.Context, ref runtime.RuntimeRef, nodeName string) (runtime.TerminationEvidence, error)
}

// Option configures optional Kubernetes runtime integrations.
type Option func(*Runtime)

// WithInfrastructureFencer installs the only permitted fallback when the
// Kubernetes control path cannot prove an exact Pod process has exited.
func WithInfrastructureFencer(fencer InfrastructureFencer) Option {
	return func(r *Runtime) {
		r.infraFencer = fencer
	}
}

// WithFUSECredentials installs an owned, process-local credential copy used
// only for the private one-shot mounter authorization request.
func WithFUSECredentials(credentials runtime.FUSECredentials) Option {
	return func(r *Runtime) {
		r.fuseCredentials.Zero()
		r.fuseCredentials = credentials.Clone()
	}
}

// WithEndpointLookup overrides endpoint DNS resolution. Production uses the
// process resolver; this option exists for deterministic runtime tests.
func WithEndpointLookup(lookup runtime.LookupNetIPFunc) Option {
	return func(r *Runtime) { r.endpointLookup = lookup }
}

type workspaceRuntimeState struct {
	runtimeUID      string
	poolKey         string
	nodeName        string
	systemMode      runtime.SystemEgressMode
	authAttempted   bool
	authorized      bool
	generation      int64
	removing        bool
	active          int
	idle            chan struct{}
	quiesceInFlight bool
	quiescePoisoned bool
	flushInFlight   bool
	quiesceToken    *runtime.WorkspaceQuiesceToken
	proof           *runtime.TerminationEvidence
	policiesDeleted bool
	networkMu       sync.Mutex
}

// Runtime implements runtime.Runtime using Kubernetes.
type Runtime struct {
	client             kubernetes.Interface
	dynClient          dynamic.Interface
	restConfig         *rest.Config
	namespace          string
	hasCilium          bool // whether CiliumNetworkPolicy CRD is available on this cluster
	controlExecutor    podCommandExecutor
	infraFencer        InfrastructureFencer
	fuseCredentials    runtime.FUSECredentials
	endpointLookup     runtime.LookupNetIPFunc
	clusterDNSLookup   func() ([]netip.Addr, error)
	pollInterval       time.Duration
	prepareTimeout     time.Duration
	readyTimeout       time.Duration
	terminationTimeout time.Duration
	stateMu            sync.Mutex
	workspaceStates    map[string]*workspaceRuntimeState
}

// New creates a new Kubernetes runtime.
func New(kubeconfig string, namespace string, options ...Option) (*Runtime, error) {
	var restConfig *rest.Config
	var err error

	if kubeconfig != "" {
		restConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		restConfig, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("build k8s config: %w", err)
	}

	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create k8s client: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create dynamic k8s client: %w", err)
	}

	hasCilium := detectCilium(client)
	if hasCilium {
		logger.Info(context.Background(), "Cilium CNI detected: CiliumNetworkPolicy will be used for private range enforcement")
	} else {
		logger.Info(context.Background(), "Cilium CNI not detected: relying on standard NetworkPolicy only")
	}
	runtimeImpl := &Runtime{
		client:             client,
		dynClient:          dynClient,
		restConfig:         restConfig,
		namespace:          namespace,
		hasCilium:          hasCilium,
		pollInterval:       250 * time.Millisecond,
		prepareTimeout:     defaultKubernetesControlTimeout,
		readyTimeout:       defaultKubernetesControlTimeout,
		terminationTimeout: defaultKubernetesControlTimeout,
		workspaceStates:    make(map[string]*workspaceRuntimeState),
		endpointLookup:     net.DefaultResolver.LookupIP,
		clusterDNSLookup:   systemResolverAddresses,
	}
	for _, option := range options {
		if option != nil {
			option(runtimeImpl)
		}
	}
	runtimeImpl.controlExecutor = &spdyPodCommandExecutor{client: client, restConfig: restConfig, namespace: namespace}
	return runtimeImpl, nil
}

func (r *Runtime) CreateSandbox(ctx context.Context, spec runtime.SandboxSpec) (*runtime.SandboxInfo, error) {
	if spec.WorkspaceFUSE != nil {
		return nil, fmt.Errorf("FUSE sandboxes require the prepare/authorize/ready lifecycle")
	}
	pod, err := createPod(ctx, r.client, r.namespace, spec)
	if err != nil {
		return nil, err
	}

	// Wait for pod to be ready
	if err := waitForPodReady(ctx, r.client, r.namespace, pod.Name, 60*time.Second); err != nil {
		_ = deletePod(ctx, r.client, r.namespace, pod.Name)
		return nil, fmt.Errorf("wait for pod: %w", err)
	}

	// Always apply a NetworkPolicy. Without one, K8s allows all egress by default.
	// updateNetworkPolicy handles all modes: isolation, whitelist, block-private, open.
	if err := updateNetworkPolicy(ctx, r.client, r.namespace, spec.ID, spec.NetworkEnabled, spec.NetworkWhitelist, spec.NetworkBlockPrivate); err != nil {
		_ = deletePod(ctx, r.client, r.namespace, pod.Name)
		return nil, fmt.Errorf("apply network policy: %w", err)
	}

	// When network is enabled without an explicit whitelist, apply a CiliumNetworkPolicy
	// egressDeny to block private ranges. Standard K8s NetworkPolicy IPBlock/Except is
	// unreliable in Cilium (CIDR identity may not be assigned before the "world" catch-all
	// matches), so an eBPF-level deny is the only reliable fix.
	// Whitelist mode is excluded: the whitelist may intentionally allow private CIDRs.
	// On non-Cilium clusters this is skipped; the standard NetworkPolicy suffices.
	if r.hasCilium && spec.NetworkEnabled && len(spec.NetworkWhitelist) == 0 {
		if err := applyCiliumPrivateDeny(ctx, r.dynClient, r.namespace, spec.ID); err != nil {
			_ = deleteNetworkPolicy(ctx, r.client, r.namespace, spec.ID)
			_ = deletePod(ctx, r.client, r.namespace, pod.Name)
			return nil, fmt.Errorf("apply cilium private deny: %w", err)
		}
	}

	return &runtime.SandboxInfo{
		ID:         spec.ID,
		RuntimeID:  pod.Name,
		RuntimeUID: string(pod.UID),
		State:      "running",
		CreatedAt:  pod.CreationTimestamp.Time,
	}, nil
}

func (r *Runtime) PrepareSandbox(ctx context.Context, spec runtime.SandboxSpec) (*runtime.SandboxInfo, error) {
	if spec.WorkspaceFUSE == nil {
		return nil, runtime.ErrWorkspaceFUSEUnsupported
	}
	if r.endpointLookup != nil {
		resolved, err := runtime.ResolveFUSEEndpointPolicy(ctx, spec.WorkspaceFUSE, r.endpointLookup, runtime.EndpointIPv4AndIPv6)
		if err != nil {
			return nil, err
		}
		dnsCIDRs, err := r.clusterDNSCIDRs(ctx)
		if err != nil {
			return nil, err
		}
		resolved.SystemEgress.DNSCIDRs = dnsCIDRs
		resolved.SystemEgress.DNSPorts = []int32{53}
		spec.WorkspaceFUSE = resolved
	}
	// Pure construction validates both resources before the first API mutation.
	pod, err := buildPreparedFUSEPod(r.namespace, spec)
	if err != nil {
		return nil, err
	}
	policy, ciliumPolicy, err := r.buildPreparedSystemPolicy(spec)
	if err != nil {
		return nil, err
	}
	prepareAttempt, err := newPrepareAttemptToken()
	if err != nil {
		return nil, err
	}
	pod.Annotations[fusePrepareAttemptAnnotation] = prepareAttempt
	if policy != nil {
		if policy.Annotations == nil {
			policy.Annotations = make(map[string]string)
		}
		policy.Annotations[fusePrepareAttemptAnnotation] = prepareAttempt
	}
	if ciliumPolicy != nil {
		annotations := ciliumPolicy.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[fusePrepareAttemptAnnotation] = prepareAttempt
		ciliumPolicy.SetAnnotations(annotations)
	}
	if err := r.createPreparedSystemPolicy(ctx, policy, ciliumPolicy); err != nil {
		return nil, err
	}
	policyCreated := true
	var created *corev1.Pod
	created, err = r.client.CoreV1().Pods(r.namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		var safeToRemovePolicy bool
		created, safeToRemovePolicy, err = r.verifyAmbiguousPodCreate(ctx, pod, err)
		if err != nil && safeToRemovePolicy && policyCreated {
			cleanupCtx, cancel := r.newCleanupContext(ctx)
			cleanupErr := r.deletePreparedSystemPolicy(cleanupCtx, spec.ID, spec.WorkspaceFUSE.SystemEgress.Mode, "", prepareAttempt, true)
			cancel()
			if cleanupErr != nil {
				return nil, errors.Join(err, cleanupErr)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if created.UID == "" {
		return nil, r.compensatePreparedFailure(ctx, created, spec.WorkspaceFUSE.SystemEgress.Mode, prepareAttempt, fmt.Errorf("prepared Pod has no immutable UID"))
	}
	if !preparedPodIntentMatches(created, pod, false) {
		reason := preparedPodIntentMismatchReason(created, pod)
		return nil, r.compensatePreparedFailure(ctx, created, spec.WorkspaceFUSE.SystemEgress.Mode, prepareAttempt, fmt.Errorf("created prepared Pod does not match the requested security contract: %s", reason))
	}
	ref := runtime.RuntimeRef{ID: created.Name, UID: string(created.UID)}
	if err := r.bindPreparedSystemPolicy(ctx, ref, spec.WorkspaceFUSE.SystemEgress.Mode, policy, ciliumPolicy); err != nil {
		return nil, r.compensatePreparedFailure(ctx, created, spec.WorkspaceFUSE.SystemEgress.Mode, prepareAttempt, err)
	}
	r.stateMu.Lock()
	r.ensureWorkspaceStatesLocked()[ref.ID] = &workspaceRuntimeState{
		runtimeUID: ref.UID, poolKey: spec.WorkspaceFUSE.PoolKey, nodeName: created.Spec.NodeName,
		systemMode: spec.WorkspaceFUSE.SystemEgress.Mode,
	}
	r.stateMu.Unlock()
	if err := r.waitPreparedContainers(ctx, ref); err != nil {
		return nil, r.compensatePreparedFailure(ctx, created, spec.WorkspaceFUSE.SystemEgress.Mode, prepareAttempt, err)
	}
	if err := r.PreparedSandboxHealth(ctx, ref, spec.WorkspaceFUSE.PoolKey); err != nil {
		return nil, r.compensatePreparedFailure(ctx, created, spec.WorkspaceFUSE.SystemEgress.Mode, prepareAttempt, err)
	}
	preparedPod, err := r.patchPreparedState(ctx, ref)
	if err != nil {
		return nil, r.compensatePreparedFailure(ctx, created, spec.WorkspaceFUSE.SystemEgress.Mode, prepareAttempt, err)
	}
	return sandboxInfoForPod(spec.ID, preparedPod), nil
}

func (r *Runtime) clusterDNSCIDRs(ctx context.Context) ([]string, error) {
	_ = ctx
	if r.clusterDNSLookup == nil {
		return nil, fmt.Errorf("discover Kubernetes cluster DNS: resolver lookup is unavailable")
	}
	addresses, err := r.clusterDNSLookup()
	if err != nil {
		return nil, fmt.Errorf("discover Kubernetes cluster DNS: %w", err)
	}
	set := make(map[string]struct{})
	for _, address := range addresses {
		if !address.IsValid() || address.IsUnspecified() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsMulticast() {
			continue
		}
		set[netip.PrefixFrom(address, address.BitLen()).String()] = struct{}{}
	}
	if len(set) == 0 || len(set) > 3 {
		return nil, fmt.Errorf("discover Kubernetes cluster DNS: expected one to three kube-dns ClusterIPs")
	}
	result := make([]string, 0, len(set))
	for cidr := range set {
		result = append(result, cidr)
	}
	sort.Strings(result)
	return result, nil
}

func systemResolverAddresses() ([]netip.Addr, error) {
	file, err := os.Open("/etc/resolv.conf")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var result []netip.Addr
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || fields[0] != "nameserver" {
			continue
		}
		address, parseErr := netip.ParseAddr(fields[1])
		if parseErr == nil {
			result = append(result, address.Unmap())
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (r *Runtime) AuthorizeWorkspaceMount(ctx context.Context, ref runtime.RuntimeRef, auth runtime.WorkspaceMountAuthorization) error {
	if err := ref.Validate(); err != nil || auth.RuntimeUID != ref.UID {
		return runtime.ErrInvalidRuntimeRef
	}
	if auth.PoolKey == "" || auth.WorkspaceHash == "" || auth.Prefix == "" || auth.LeaseGeneration <= 0 || auth.MountAttempt != 1 {
		return fmt.Errorf("invalid workspace mount authorization")
	}
	state, done, err := r.beginAuthorization(ref, auth)
	if err != nil {
		return err
	}
	defer done()
	if _, err := r.getExactPod(ctx, ref); err != nil {
		return err
	}
	request := authorizeRequestWire{
		Version: controlWireVersion, RuntimeUID: auth.RuntimeUID, PoolKey: auth.PoolKey,
		WorkspaceHash: auth.WorkspaceHash, Prefix: auth.Prefix, LeaseGeneration: auth.LeaseGeneration, MountAttempt: auth.MountAttempt,
	}
	credentials := r.fuseCredentials.Clone()
	defer credentials.Zero()
	request.Credentials = fuseprotocol.MountCredentials{AccessKey: credentials.AccessKey, SecretKey: credentials.SecretKey}
	stdin, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("marshal workspace authorization")
	}
	defer clear(stdin)
	raw, err := r.execControl(ctx, ref.ID, workspaceMounterContainer, []string{mounterBinary, "authorize"}, stdin)
	if err != nil {
		return err
	}
	ack, err := decodeControlAck(raw)
	if err != nil || !ack.Accepted || ack.RuntimeUID != ref.UID || ack.Generation != auth.LeaseGeneration {
		return fmt.Errorf("workspace authorization acknowledgement is invalid")
	}
	r.stateMu.Lock()
	state.authorized = true
	state.generation = auth.LeaseGeneration
	r.stateMu.Unlock()
	return nil
}

func (r *Runtime) WaitSandboxReady(ctx context.Context, ref runtime.RuntimeRef, expectedGeneration int64) (*runtime.SandboxInfo, error) {
	if expectedGeneration <= 0 {
		return nil, fmt.Errorf("expected workspace generation must be positive")
	}
	state, done, err := r.beginWorkspaceOperation(ref, true)
	if err != nil {
		return nil, err
	}
	defer done()
	r.stateMu.Lock()
	generation, poolKey := state.generation, state.poolKey
	r.stateMu.Unlock()
	if generation != expectedGeneration {
		return nil, fmt.Errorf("workspace authorization is unavailable")
	}
	pod, err := r.waitReadyPod(ctx, ref)
	if err != nil {
		return nil, err
	}
	health, err := r.readMounterStatus(ctx, ref, "ready")
	if err != nil {
		return nil, err
	}
	bootstrap, err := exactFUSEPodBootstrap(pod, ref)
	if err != nil {
		return nil, err
	}
	if health.State != "ready" || health.RuntimeUID != ref.UID || health.PoolKey != poolKey || health.MountType != "fuse" || health.Generation != generation || health.RestartDetected || health.CacheExceeded || health.CacheBytes < 0 || health.CacheBytes >= health.CacheLimitBytes || health.CacheLimitBytes != bootstrap.CacheLimitBytes {
		return nil, fmt.Errorf("workspace ready status does not match authorization")
	}
	argv := probeArgv("write-read-delete", ref, generation, false)
	raw, err := r.execSandboxProbe(ctx, ref.ID, argv, nil)
	if err != nil {
		return nil, err
	}
	probe, err := decodeProbeStatus(raw)
	if err != nil || !probe.OK || probe.RuntimeUID != ref.UID || probe.Generation != generation || probe.Token != "" {
		return nil, fmt.Errorf("sandbox workspace propagation probe failed")
	}
	return sandboxInfoForPod(pod.Labels["sandbox.id"], pod), nil
}

func (r *Runtime) PreparedSandboxHealth(ctx context.Context, ref runtime.RuntimeRef, poolKey string) error {
	if poolKey == "" {
		return fmt.Errorf("prepared pool key is empty")
	}
	state, done, err := r.beginWorkspaceOperation(ref, false)
	if err != nil {
		return err
	}
	defer done()
	pod, err := r.getExactPod(ctx, ref)
	if err != nil {
		return err
	}
	poolKeyLabel, err := preparedPoolKeyLabel(poolKey)
	if err != nil || validateExactFUSEPodIdentity(pod, ref) != nil || pod.Labels["sandbox.pool.key"] != poolKeyLabel ||
		(pod.Labels["sandbox.pool.state"] != "preparing" && pod.Labels["sandbox.pool.state"] != "prepared") {
		return fmt.Errorf("prepared Pod identity labels do not match")
	}
	if err := validatePreparedContainerState(pod); err != nil {
		return err
	}
	bootstrap, err := exactFUSEPodBootstrap(pod, ref)
	if err != nil {
		return err
	}
	mappings := endpointMappingsFromHostAliases(pod.Spec.HostAliases)
	current, err := runtime.FUSEEndpointMappingsCurrent(ctx, mappings, r.endpointLookup, runtime.EndpointIPv4AndIPv6)
	if err != nil {
		return fmt.Errorf("revalidate prepared workspace endpoint: %w", err)
	}
	if !current {
		return fmt.Errorf("prepared workspace endpoint addresses changed")
	}
	status, err := r.readMounterStatus(ctx, ref, "prepared")
	if err != nil {
		return err
	}
	if status.State != "prepared" || status.RuntimeUID != ref.UID || status.PoolKey != poolKey || status.MountType != "" || status.Generation != 0 || status.RestartDetected || status.CacheExceeded || status.CacheBytes != 0 || status.CacheLimitBytes != bootstrap.CacheLimitBytes {
		return fmt.Errorf("prepared workspace status is not pristine")
	}
	r.stateMu.Lock()
	state.poolKey = poolKey
	state.nodeName = pod.Spec.NodeName
	r.stateMu.Unlock()
	return nil
}

func endpointMappingsFromHostAliases(aliases []corev1.HostAlias) []runtime.EndpointHostMapping {
	byHost := make(map[string][]string)
	for _, alias := range aliases {
		for _, host := range alias.Hostnames {
			byHost[host] = append(byHost[host], alias.IP)
		}
	}
	hosts := make([]string, 0, len(byHost))
	for host := range byHost {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	result := make([]runtime.EndpointHostMapping, 0, len(hosts))
	for _, host := range hosts {
		ips := byHost[host]
		sort.Strings(ips)
		result = append(result, runtime.EndpointHostMapping{Host: host, IPs: ips})
	}
	return result
}

func (r *Runtime) WorkspaceHealth(ctx context.Context, ref runtime.RuntimeRef) (*runtime.WorkspaceHealth, error) {
	_, done, err := r.beginWorkspaceOperation(ref, false)
	if err != nil {
		return nil, err
	}
	defer done()
	pod, err := r.getExactPod(ctx, ref)
	if err != nil {
		return nil, err
	}
	bootstrap, err := exactFUSEPodBootstrap(pod, ref)
	if err != nil || pod.Labels["sandbox.pool.state"] != "prepared" {
		return nil, fmt.Errorf("exact runtime is not an eligible FUSE pool Pod")
	}
	restartCount, err := mounterRestartCount(pod)
	if err != nil {
		return nil, err
	}
	status, err := r.readMounterStatus(ctx, ref, "ready")
	if err != nil {
		return nil, err
	}
	return &runtime.WorkspaceHealth{
		Ready:     status.State == "ready" && status.MountType == "fuse" && status.PoolKey == bootstrap.PoolKey && !status.RestartDetected && !status.CacheExceeded && status.CacheBytes >= 0 && status.CacheBytes < status.CacheLimitBytes && status.CacheLimitBytes == bootstrap.CacheLimitBytes && restartCount == 0,
		MountType: status.MountType, RuntimeUID: status.RuntimeUID, Generation: status.Generation,
		RestartCount: restartCount, RestartDetected: status.RestartDetected,
		CacheBytes: status.CacheBytes, CacheLimitBytes: status.CacheLimitBytes, CacheExceeded: status.CacheExceeded,
		LastSuccessful: time.Now(),
	}, nil
}

func (r *Runtime) QuiesceWorkspace(ctx context.Context, ref runtime.RuntimeRef, expectedGeneration int64) (runtime.WorkspaceQuiesceToken, error) {
	if expectedGeneration <= 0 {
		return runtime.WorkspaceQuiesceToken{}, fmt.Errorf("expected workspace generation must be positive")
	}
	state, done, err := r.beginWorkspaceOperation(ref, false)
	if err != nil {
		return runtime.WorkspaceQuiesceToken{}, err
	}
	defer done()
	r.stateMu.Lock()
	if (state.generation != 0 && state.generation != expectedGeneration) || state.quiesceToken != nil ||
		state.quiesceInFlight || state.quiescePoisoned || state.flushInFlight {
		r.stateMu.Unlock()
		return runtime.WorkspaceQuiesceToken{}, fmt.Errorf("workspace cannot be quiesced")
	}
	state.generation = expectedGeneration
	state.quiesceInFlight = true
	r.stateMu.Unlock()
	dispatched := false
	succeeded := false
	defer func() {
		r.stateMu.Lock()
		state.quiesceInFlight = false
		if dispatched && !succeeded {
			state.quiescePoisoned = true
		}
		r.stateMu.Unlock()
	}()
	if _, err := r.getExactPod(ctx, ref); err != nil {
		return runtime.WorkspaceQuiesceToken{}, err
	}
	dispatched = true
	raw, err := r.execSandboxProbe(ctx, ref.ID, probeArgv("quiesce", ref, expectedGeneration, false), nil)
	if err != nil {
		return runtime.WorkspaceQuiesceToken{}, err
	}
	probe, err := decodeProbeStatus(raw)
	if err != nil || !probe.OK || probe.RuntimeUID != ref.UID || probe.Generation != expectedGeneration || probe.Token == "" {
		return runtime.WorkspaceQuiesceToken{}, fmt.Errorf("workspace quiesce acknowledgement is invalid")
	}
	token := runtime.WorkspaceQuiesceToken{RuntimeUID: ref.UID, Generation: expectedGeneration, Opaque: probe.Token}
	r.stateMu.Lock()
	state.quiesceToken = &token
	state.quiescePoisoned = false
	r.stateMu.Unlock()
	succeeded = true
	return token, nil
}

func (r *Runtime) ResumeWorkspace(ctx context.Context, ref runtime.RuntimeRef, token runtime.WorkspaceQuiesceToken) error {
	state, done, err := r.beginWorkspaceOperation(ref, false)
	if err != nil {
		return err
	}
	defer done()
	r.stateMu.Lock()
	if state.quiesceToken == nil || *state.quiesceToken != token || token.RuntimeUID != ref.UID || token.Generation != state.generation ||
		token.Opaque == "" || state.quiesceInFlight || state.flushInFlight || state.quiescePoisoned {
		r.stateMu.Unlock()
		return fmt.Errorf("workspace quiesce token is invalid")
	}
	// Consume before the side effect. An ambiguous exec result is never replayed.
	state.quiesceToken = nil
	state.quiescePoisoned = true
	generation := state.generation
	r.stateMu.Unlock()
	request, _ := json.Marshal(probeResumeRequestWire{Version: controlWireVersion, RuntimeUID: ref.UID, Generation: generation, Token: token.Opaque})
	raw, err := r.execSandboxProbe(ctx, ref.ID, probeArgv("resume", ref, generation, true), request)
	if err != nil {
		return err
	}
	probe, err := decodeProbeStatus(raw)
	if err != nil || !probe.OK || probe.RuntimeUID != ref.UID || probe.Generation != generation || probe.Token != "" {
		return fmt.Errorf("workspace resume acknowledgement is invalid")
	}
	r.stateMu.Lock()
	state.quiescePoisoned = false
	r.stateMu.Unlock()
	return nil
}

func (r *Runtime) FlushWorkspace(ctx context.Context, ref runtime.RuntimeRef, expectedGeneration int64) error {
	if expectedGeneration <= 0 {
		return fmt.Errorf("expected workspace generation must be positive")
	}
	state, done, err := r.beginWorkspaceOperation(ref, false)
	if err != nil {
		return err
	}
	defer done()
	r.stateMu.Lock()
	if state.generation != expectedGeneration || state.quiesceToken == nil || state.quiesceToken.Generation != expectedGeneration ||
		state.quiesceInFlight || state.flushInFlight || state.quiescePoisoned {
		r.stateMu.Unlock()
		return fmt.Errorf("workspace generation is unavailable")
	}
	state.flushInFlight = true
	r.stateMu.Unlock()
	succeeded := false
	defer func() {
		r.stateMu.Lock()
		state.flushInFlight = false
		if !succeeded {
			state.quiescePoisoned = true
		}
		r.stateMu.Unlock()
	}()
	request, _ := json.Marshal(controlRequestWire{Version: controlWireVersion, RuntimeUID: ref.UID, Generation: expectedGeneration})
	raw, err := r.execControl(ctx, ref.ID, workspaceMounterContainer, []string{mounterBinary, "flush"}, request)
	if err != nil {
		return err
	}
	ack, err := decodeControlAck(raw)
	if err != nil || !ack.Accepted || ack.RuntimeUID != ref.UID || ack.Generation != expectedGeneration {
		return fmt.Errorf("workspace flush acknowledgement is invalid")
	}
	succeeded = true
	return nil
}

func (r *Runtime) UpdateFUSENetwork(ctx context.Context, ref runtime.RuntimeRef, enabled bool, whitelist []string, blockPrivate bool) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	state, done, err := r.beginWorkspaceOperation(ref, false)
	if err != nil {
		return err
	}
	defer done()
	state.networkMu.Lock()
	defer state.networkMu.Unlock()
	pod, err := r.getExactPod(ctx, ref)
	if err != nil {
		return err
	}
	if err := validateExactFUSEPodIdentity(pod, ref); err != nil || pod.Labels["sandbox.pool.state"] != "prepared" {
		return fmt.Errorf("exact runtime is not an eligible FUSE pool Pod")
	}
	if pod.Spec.DNSConfig == nil {
		return fmt.Errorf("exact FUSE Pod DNS configuration is missing")
	}
	dnsCIDRs, err := canonicalNameserverHostCIDRs(pod.Spec.DNSConfig.Nameservers)
	if err != nil {
		return err
	}
	resolvedCIDRs, err := resolveToCIDRs(whitelist)
	if err != nil {
		return err
	}
	resolvedCIDRs, err = canonicalFUSEUserCIDRs(resolvedCIDRs)
	if err != nil {
		return err
	}
	policy, err := buildFUSEUserNetworkPolicy(r.namespace, ref.ID, ref.UID, enabled, resolvedCIDRs, blockPrivate, dnsCIDRs)
	if err != nil {
		return err
	}
	networkAttempt, err := newNetworkAttemptToken()
	if err != nil {
		return err
	}
	policy.Annotations[fuseNetworkAttemptAnnotation] = networkAttempt
	var denyPolicy *unstructured.Unstructured
	var denyToDelete *unstructured.Unstructured
	if r.hasCilium && enabled {
		if r.dynClient == nil {
			return fmt.Errorf("Cilium user egress enforcement is unavailable")
		}
		exceptions := append([]string(nil), resolvedCIDRs...)
		if blockPrivate {
			systemCIDRs, systemErr := r.approvedSystemPrivateCIDRs(ctx, ref)
			if systemErr != nil {
				return systemErr
			}
			exceptions = append(exceptions, systemCIDRs...)
		}
		denyPolicy, err = buildFUSECiliumUserDenyPolicy(r.namespace, ref.ID, ref.UID, blockPrivate, exceptions)
		if err != nil {
			return err
		}
		annotations := denyPolicy.GetAnnotations()
		annotations[fuseNetworkAttemptAnnotation] = networkAttempt
		denyPolicy.SetAnnotations(annotations)
		if err := upsertExactCiliumUserPolicy(ctx, r.dynClient.Resource(ciliumNetworkPolicyGVR).Namespace(r.namespace), denyPolicy); err != nil {
			return err
		}
	} else if r.hasCilium {
		if r.dynClient == nil {
			return fmt.Errorf("Cilium user egress enforcement is unavailable")
		}
		denyToDelete, err = r.readExactCiliumUserPolicyForNetworkUpdate(ctx, ref)
		if err != nil {
			return err
		}
	}
	if err := upsertExactNetworkPolicy(ctx, r.client, policy); err != nil {
		if denyPolicy != nil {
			return errors.Join(runtime.ErrFUSENetworkStateUncertain, fmt.Errorf("Cilium deny policy changed before user allow policy failed: %w", err))
		}
		return err
	}
	if denyPolicy != nil {
		return r.verifyFUSEUserNetworkIntent(ctx, policy, denyPolicy)
	}
	if r.hasCilium {
		if err := r.deleteCiliumUserPolicyRevision(ctx, ref, denyToDelete); err != nil {
			return err
		}
		return r.verifyFUSEUserNetworkIntent(ctx, policy, nil)
	}
	return nil
}

func (r *Runtime) readExactCiliumUserPolicyForNetworkUpdate(ctx context.Context, ref runtime.RuntimeRef) (*unstructured.Unstructured, error) {
	current, err := r.dynClient.Resource(ciliumNetworkPolicyGVR).Namespace(r.namespace).Get(ctx, fuseUserDenyPolicyPrefix+ref.ID, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Cilium user deny policy before network update: %w", err)
	}
	if !ciliumUserPolicyOwnershipMatches(current, ref.ID) || current.GetAnnotations()[fuseRuntimeUIDAnnotation] != ref.UID ||
		current.GetUID() == "" || current.GetResourceVersion() == "" {
		return nil, runtime.ErrInvalidRuntimeRef
	}
	return current, nil
}

func (r *Runtime) deleteCiliumUserPolicyRevision(ctx context.Context, ref runtime.RuntimeRef, expected *unstructured.Unstructured) error {
	policies := r.dynClient.Resource(ciliumNetworkPolicyGVR).Namespace(r.namespace)
	if expected == nil {
		_, err := policies.Get(ctx, fuseUserDenyPolicyPrefix+ref.ID, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		return errors.Join(runtime.ErrFUSENetworkStateUncertain, fmt.Errorf("concurrent Cilium user deny policy appeared during network update: %w", err))
	}
	uid, resourceVersion := expected.GetUID(), expected.GetResourceVersion()
	deleteErr := policies.Delete(ctx, expected.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &resourceVersion}})
	verifyCtx, cancel := networkCleanupContext(ctx)
	defer cancel()
	_, verifyErr := policies.Get(verifyCtx, expected.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(verifyErr) {
		return nil
	}
	return errors.Join(runtime.ErrFUSENetworkStateUncertain, fmt.Errorf("delete prior Cilium user deny policy: %w", deleteErr), verifyErr)
}

func (r *Runtime) verifyFUSEUserNetworkIntent(ctx context.Context, desiredAllow *networkingv1.NetworkPolicy, desiredDeny *unstructured.Unstructured) error {
	verifyCtx, cancel := networkCleanupContext(ctx)
	defer cancel()
	allow, allowErr := r.client.NetworkingV1().NetworkPolicies(r.namespace).Get(verifyCtx, desiredAllow.Name, metav1.GetOptions{})
	if allowErr != nil || !networkPolicyIntentMatches(allow, desiredAllow) {
		return errors.Join(runtime.ErrFUSENetworkStateUncertain, fmt.Errorf("verify FUSE user allow policy after network update: %w", allowErr))
	}
	if desiredDeny == nil {
		_, denyErr := r.dynClient.Resource(ciliumNetworkPolicyGVR).Namespace(r.namespace).Get(verifyCtx, fuseUserDenyPolicyPrefix+desiredAllow.Labels["sandbox.pool.instance"], metav1.GetOptions{})
		if apierrors.IsNotFound(denyErr) {
			return nil
		}
		return errors.Join(runtime.ErrFUSENetworkStateUncertain, fmt.Errorf("verify absence of Cilium user deny policy: %w", denyErr))
	}
	deny, denyErr := r.dynClient.Resource(ciliumNetworkPolicyGVR).Namespace(r.namespace).Get(verifyCtx, desiredDeny.GetName(), metav1.GetOptions{})
	if denyErr != nil || !ciliumUserPolicyIntentMatches(deny, desiredDeny) {
		return errors.Join(runtime.ErrFUSENetworkStateUncertain, fmt.Errorf("verify Cilium user deny policy after network update: %w", denyErr))
	}
	return nil
}

func newNetworkAttemptToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate FUSE network update attempt identity: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func (r *Runtime) approvedSystemPrivateCIDRs(ctx context.Context, ref runtime.RuntimeRef) ([]string, error) {
	name := fuseSystemPolicyPrefix + ref.ID
	standard, standardErr := r.client.NetworkingV1().NetworkPolicies(r.namespace).Get(ctx, name, metav1.GetOptions{})
	if standardErr == nil {
		if !networkPolicyOwnershipMatches(standard, ref.ID, "system") || standard.Annotations[fuseRuntimeUIDAnnotation] != ref.UID {
			return nil, runtime.ErrInvalidRuntimeRef
		}
		var values []string
		for _, rule := range standard.Spec.Egress {
			for _, peer := range rule.To {
				if peer.IPBlock != nil {
					values = append(values, peer.IPBlock.CIDR)
				}
			}
		}
		return privateCIDRExceptions(values)
	}
	if !apierrors.IsNotFound(standardErr) {
		return nil, fmt.Errorf("read exact system policy for private-range exceptions: %w", standardErr)
	}
	if r.dynClient == nil {
		return nil, fmt.Errorf("exact system policy is unavailable")
	}
	cilium, ciliumErr := r.dynClient.Resource(ciliumNetworkPolicyGVR).Namespace(r.namespace).Get(ctx, name, metav1.GetOptions{})
	if ciliumErr != nil {
		return nil, fmt.Errorf("read exact Cilium system policy for private-range exceptions: %w", ciliumErr)
	}
	if !ciliumSystemPolicyOwnershipMatches(cilium, ref.ID) || cilium.GetAnnotations()[fuseRuntimeUIDAnnotation] != ref.UID {
		return nil, runtime.ErrInvalidRuntimeRef
	}
	var values []string
	var approvedEndpointCIDRs []string
	if err := json.Unmarshal([]byte(cilium.GetAnnotations()[fuseEndpointCIDRsAnnotation]), &approvedEndpointCIDRs); err != nil {
		return nil, fmt.Errorf("Cilium system policy approved endpoint CIDRs are invalid")
	}
	canonicalApproved, err := canonicalEndpointCIDRs(approvedEndpointCIDRs)
	if err != nil || !reflect.DeepEqual(canonicalApproved, approvedEndpointCIDRs) {
		return nil, fmt.Errorf("Cilium system policy approved endpoint CIDRs are not canonical")
	}
	values = append(values, approvedEndpointCIDRs...)
	egress, found, nestedErr := unstructured.NestedSlice(cilium.Object, "spec", "egress")
	if nestedErr != nil || !found {
		return nil, fmt.Errorf("Cilium system policy egress is invalid")
	}
	for _, rawRule := range egress {
		rule, ok := rawRule.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("Cilium system policy egress rule is invalid")
		}
		cidrSet, _, err := unstructured.NestedSlice(rule, "toCIDRSet")
		if err != nil {
			return nil, fmt.Errorf("Cilium system policy CIDR set is invalid")
		}
		for _, rawCIDR := range cidrSet {
			cidrRule, ok := rawCIDR.(map[string]any)
			cidr, _ := cidrRule["cidr"].(string)
			if !ok || cidr == "" {
				return nil, fmt.Errorf("Cilium system policy CIDR rule is invalid")
			}
			values = append(values, cidr)
		}
	}
	return privateCIDRExceptions(values)
}

func privateCIDRExceptions(values []string) ([]string, error) {
	var result []string
	for _, raw := range canonicalStringSet(values) {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Masked().String() != raw || prefix.Addr().Is4In6() {
			return nil, fmt.Errorf("system private-range exception is not a canonical CIDR")
		}
		for _, privateRaw := range privateCIDRs {
			private, _ := netip.ParsePrefix(privateRaw)
			if prefix.Bits() >= private.Bits() && private.Contains(prefix.Addr()) {
				result = append(result, raw)
			}
		}
	}
	return canonicalStringSet(result), nil
}

func (r *Runtime) RemovePreparedSandbox(ctx context.Context, runtimeID, runtimeUID string) error {
	ref, err := runtime.NewRuntimeRef(runtimeID, runtimeUID)
	if err != nil {
		return err
	}
	state, err := r.beginWorkspaceRemoval(ctx, ref)
	if err != nil {
		return err
	}
	defer r.finishWorkspaceRemoval(state)

	r.stateMu.Lock()
	proof := cloneTerminationEvidence(state.proof)
	policiesDeleted := state.policiesDeleted
	generation := state.generation
	nodeName := state.nodeName
	r.stateMu.Unlock()
	if proof != nil && policiesDeleted {
		return nil
	}
	if proof == nil {
		pod, getErr := r.getExactPod(ctx, ref)
		if getErr == nil {
			nodeName = pod.Spec.NodeName
			request, _ := json.Marshal(controlRequestWire{Version: controlWireVersion, RuntimeUID: ref.UID, Generation: generation})
			raw, shutdownErr := r.execControl(ctx, ref.ID, workspaceMounterContainer, []string{mounterBinary, "shutdown"}, request)
			if shutdownErr == nil {
				ack, decodeErr := decodeShutdownAck(raw)
				if decodeErr != nil || ack.RuntimeUID != ref.UID || ack.Generation != generation || !ack.GracefulUnmount {
					shutdownErr = fmt.Errorf("workspace shutdown acknowledgement is invalid")
				}
			}
			if shutdownErr == nil {
				grace := pod.Spec.TerminationGracePeriodSeconds
				deleteErr := r.client.CoreV1().Pods(r.namespace).Delete(ctx, ref.ID, metav1.DeleteOptions{
					GracePeriodSeconds: grace,
					Preconditions:      &metav1.Preconditions{UID: &pod.UID},
				})
				if deleteErr == nil {
					if waitErr := r.waitExactPodGone(ctx, ref); waitErr == nil {
						proof = &runtime.TerminationEvidence{RuntimeUID: ref.UID, GracefulUnmount: true, ProcessExited: true}
					}
				}
			}
		} else if !errors.Is(getErr, runtime.ErrNotFound) {
			// The infrastructure fencer below is the only safe fallback.
		}
	}
	if proof == nil {
		if r.infraFencer == nil {
			return runtime.ErrTerminationUnconfirmed
		}
		evidence, fenceErr := r.infraFencer.FenceRuntime(ctx, ref, nodeName)
		if fenceErr != nil || evidence.RuntimeUID != ref.UID ||
			(evidence.InfrastructureFenced && (nodeName == "" || evidence.NodeName != nodeName)) ||
			(!evidence.InfrastructureFenced && !evidence.ProcessExited) {
			return runtime.ErrTerminationUnconfirmed
		}
		proof = &evidence
	}
	r.stateMu.Lock()
	state.nodeName = nodeName
	state.proof = cloneTerminationEvidence(proof)
	state.policiesDeleted = false
	r.stateMu.Unlock()
	if err := r.deleteFUSEPolicies(ctx, ref, state.systemMode); err != nil {
		return err
	}
	r.stateMu.Lock()
	state.policiesDeleted = true
	r.stateMu.Unlock()
	return nil
}

// ReconcileOrphanedResources removes managed FUSE Pods and policies that are
// not referenced by any restored session, workspace owner, or pool record.
// Identity validation is deliberately performed before deletion so malformed
// or replaced resources are retained fail-closed for operator inspection.
func (r *Runtime) ReconcileOrphanedResources(ctx context.Context, protectedRuntimeUIDs map[string]struct{}) error {
	pods, err := r.client.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "sandbox.managed=true,sandbox.pool=true,sandbox.workspace.mode=fuse",
	})
	if err != nil {
		return errors.Join(fmt.Errorf("list managed Kubernetes FUSE Pods: %w", err), runtime.ErrTerminationUnconfirmed)
	}
	var result error
	for i := range pods.Items {
		pod := &pods.Items[i]
		ref := runtime.RuntimeRef{ID: pod.Name, UID: string(pod.UID)}
		if validateExactFUSEPodIdentity(pod, ref) != nil {
			result = errors.Join(result, fmt.Errorf("managed Kubernetes FUSE Pod identity is invalid: %s", pod.Name), runtime.ErrTerminationUnconfirmed)
			continue
		}
		if _, protected := protectedRuntimeUIDs[ref.UID]; protected {
			continue
		}
		if err := r.RemovePreparedSandbox(ctx, ref.ID, ref.UID); err != nil {
			result = errors.Join(result, fmt.Errorf("remove orphaned Kubernetes FUSE Pod %s: %w", ref.ID, err))
		}
	}
	return errors.Join(result, r.reconcileOrphanedFUSEPolicies(ctx, protectedRuntimeUIDs))
}

func (r *Runtime) reconcileOrphanedFUSEPolicies(ctx context.Context, protectedRuntimeUIDs map[string]struct{}) error {
	var result error
	policies, err := r.client.NetworkingV1().NetworkPolicies(r.namespace).List(ctx, metav1.ListOptions{LabelSelector: "sandbox.managed=true"})
	if err != nil {
		result = errors.Join(result, fmt.Errorf("list managed Kubernetes FUSE NetworkPolicies: %w", err), runtime.ErrTerminationUnconfirmed)
	} else {
		for i := range policies.Items {
			policy := &policies.Items[i]
			instance, role := policy.Labels["sandbox.pool.instance"], policy.Labels["sandbox.policy.role"]
			if role != "system" && role != "user" {
				continue
			}
			expectedName := fuseSystemPolicyPrefix + instance
			if role == "user" {
				expectedName = fuseUserPolicyPrefix + instance
			}
			if policy.Name != expectedName || !networkPolicyOwnershipMatches(policy, instance, role) ||
				(role == "system" && policy.Annotations[fusePrepareAttemptAnnotation] == "") {
				result = errors.Join(result, fmt.Errorf("managed Kubernetes FUSE NetworkPolicy identity is invalid: %s", policy.Name), runtime.ErrTerminationUnconfirmed)
				continue
			}
			runtimeUID := policy.Annotations[fuseRuntimeUIDAnnotation]
			if role == "user" && runtimeUID == "" {
				result = errors.Join(result, fmt.Errorf("managed Kubernetes FUSE user NetworkPolicy has no runtime UID: %s", policy.Name), runtime.ErrTerminationUnconfirmed)
				continue
			}
			if runtimeUID != "" {
				if _, err := runtime.NewRuntimeRef(instance, runtimeUID); err != nil {
					result = errors.Join(result, fmt.Errorf("managed Kubernetes FUSE NetworkPolicy runtime identity is invalid: %s", policy.Name), runtime.ErrTerminationUnconfirmed)
					continue
				}
			}
			if _, protected := protectedRuntimeUIDs[runtimeUID]; runtimeUID != "" && protected {
				continue
			}
			absent, podErr := r.orphanPolicyPodAbsent(ctx, instance)
			if podErr != nil {
				result = errors.Join(result, podErr)
				continue
			}
			if !absent {
				result = errors.Join(result, fmt.Errorf("managed Kubernetes FUSE NetworkPolicy still selects an unprotected Pod: %s", policy.Name), runtime.ErrTerminationUnconfirmed)
				continue
			}
			if err := deleteCreatedNetworkPolicy(ctx, r.client.NetworkingV1().NetworkPolicies(r.namespace), policy); err != nil {
				result = errors.Join(result, fmt.Errorf("remove orphaned Kubernetes FUSE NetworkPolicy %s: %w", policy.Name, err))
			}
		}
	}
	if !r.hasCilium || r.dynClient == nil {
		return result
	}
	cilium, err := r.dynClient.Resource(ciliumNetworkPolicyGVR).Namespace(r.namespace).List(ctx, metav1.ListOptions{LabelSelector: "sandbox.managed=true"})
	if err != nil && !apierrors.IsNotFound(err) {
		return errors.Join(result, fmt.Errorf("list managed Kubernetes FUSE CiliumNetworkPolicies: %w", err), runtime.ErrTerminationUnconfirmed)
	}
	if err != nil {
		return result
	}
	for i := range cilium.Items {
		policy := &cilium.Items[i]
		instance, role := policy.GetLabels()["sandbox.pool.instance"], policy.GetLabels()["sandbox.policy.role"]
		valid := role == "system" && ciliumSystemPolicyOwnershipMatches(policy, instance)
		valid = valid || role == "user-deny" && ciliumUserPolicyOwnershipMatches(policy, instance)
		if role != "system" && role != "user-deny" {
			continue
		}
		expectedName := fuseSystemPolicyPrefix + instance
		if role == "user-deny" {
			expectedName = fuseUserDenyPolicyPrefix + instance
		}
		if policy.GetName() != expectedName || !valid ||
			(role == "system" && policy.GetAnnotations()[fusePrepareAttemptAnnotation] == "") {
			result = errors.Join(result, fmt.Errorf("managed Kubernetes FUSE CiliumNetworkPolicy identity is invalid: %s", policy.GetName()), runtime.ErrTerminationUnconfirmed)
			continue
		}
		runtimeUID := policy.GetAnnotations()[fuseRuntimeUIDAnnotation]
		if role == "user-deny" && runtimeUID == "" {
			result = errors.Join(result, fmt.Errorf("managed Kubernetes FUSE user CiliumNetworkPolicy has no runtime UID: %s", policy.GetName()), runtime.ErrTerminationUnconfirmed)
			continue
		}
		if runtimeUID != "" {
			if _, err := runtime.NewRuntimeRef(instance, runtimeUID); err != nil {
				result = errors.Join(result, fmt.Errorf("managed Kubernetes FUSE CiliumNetworkPolicy runtime identity is invalid: %s", policy.GetName()), runtime.ErrTerminationUnconfirmed)
				continue
			}
		}
		if _, protected := protectedRuntimeUIDs[runtimeUID]; runtimeUID != "" && protected {
			continue
		}
		absent, podErr := r.orphanPolicyPodAbsent(ctx, instance)
		if podErr != nil {
			result = errors.Join(result, podErr)
			continue
		}
		if !absent {
			result = errors.Join(result, fmt.Errorf("managed Kubernetes FUSE CiliumNetworkPolicy still selects an unprotected Pod: %s", policy.GetName()), runtime.ErrTerminationUnconfirmed)
			continue
		}
		if err := deleteCreatedCiliumPolicy(ctx, r.dynClient.Resource(ciliumNetworkPolicyGVR).Namespace(r.namespace), policy); err != nil {
			result = errors.Join(result, fmt.Errorf("remove orphaned Kubernetes FUSE CiliumNetworkPolicy %s: %w", policy.GetName(), err))
		}
	}
	return result
}

func (r *Runtime) orphanPolicyPodAbsent(ctx context.Context, instance string) (bool, error) {
	if instance == "" || len(validation.IsDNS1123Subdomain(instance)) != 0 {
		return false, runtime.ErrTerminationUnconfirmed
	}
	_, err := r.client.CoreV1().Pods(r.namespace).Get(ctx, instance, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, errors.Join(fmt.Errorf("verify orphaned Kubernetes FUSE policy Pod: %w", err), runtime.ErrTerminationUnconfirmed)
	}
	return false, nil
}

func (r *Runtime) ConfirmTerminated(_ context.Context, runtimeID, runtimeUID string) (runtime.TerminationEvidence, error) {
	ref, err := runtime.NewRuntimeRef(runtimeID, runtimeUID)
	if err != nil {
		return runtime.TerminationEvidence{}, err
	}
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	state := r.ensureWorkspaceStatesLocked()[ref.ID]
	if state == nil || state.runtimeUID != ref.UID || state.proof == nil || !state.policiesDeleted || state.proof.RuntimeUID != ref.UID ||
		(!state.proof.ProcessExited && !state.proof.InfrastructureFenced) {
		return runtime.TerminationEvidence{}, runtime.ErrTerminationUnconfirmed
	}
	return *state.proof, nil
}

func (r *Runtime) buildPreparedSystemPolicy(spec runtime.SandboxSpec) (*networkingv1.NetworkPolicy, *unstructured.Unstructured, error) {
	if spec.WorkspaceFUSE.SystemEgress.Mode == runtime.SystemEgressCIDR {
		policy, err := buildSystemEgressPolicy(r.namespace, spec.ID, spec.WorkspaceFUSE.SystemEgress)
		return policy, nil, err
	}
	if !r.hasCilium || r.dynClient == nil {
		return nil, nil, fmt.Errorf("Cilium FQDN system egress is unavailable")
	}
	policy, err := buildCiliumSystemEgressPolicy(r.namespace, spec.ID, spec.WorkspaceFUSE.SystemEgress, spec.WorkspaceFUSE.EndpointHostIPs)
	return nil, policy, err
}

func newPrepareAttemptToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate prepared sandbox attempt identity: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func (r *Runtime) createPreparedSystemPolicy(ctx context.Context, policy *networkingv1.NetworkPolicy, ciliumPolicy *unstructured.Unstructured) error {
	if policy != nil {
		policies := r.client.NetworkingV1().NetworkPolicies(r.namespace)
		created, err := policies.Create(ctx, policy, metav1.CreateOptions{})
		if err == nil {
			if networkPolicyIntentMatches(created, policy) {
				return nil
			}
			cleanupCtx, cancel := r.newCleanupContext(ctx)
			defer cancel()
			cleanupErr := deleteCreatedNetworkPolicy(cleanupCtx, policies, created)
			return errors.Join(fmt.Errorf("created prepared system policy does not match requested intent"), cleanupErr)
		}
		if !mayVerifyAmbiguousCreate(err) {
			return fmt.Errorf("create prepared system network policy: %w", err)
		}
		verifyCtx, cancel := r.newCleanupContext(ctx)
		defer cancel()
		current, verifyErr := policies.Get(verifyCtx, policy.Name, metav1.GetOptions{})
		if verifyErr == nil && networkPolicyIntentMatches(current, policy) {
			return nil
		}
		if verifyErr == nil && current.Annotations[fusePrepareAttemptAnnotation] == policy.Annotations[fusePrepareAttemptAnnotation] {
			cleanupErr := deleteCreatedNetworkPolicy(verifyCtx, policies, current)
			return errors.Join(fmt.Errorf("create prepared system network policy: %w", err), fmt.Errorf("created policy intent was mutated"), cleanupErr)
		}
		return fmt.Errorf("create prepared system network policy: %w", err)
	}
	if ciliumPolicy == nil {
		return fmt.Errorf("prepared system egress policy is empty")
	}
	policies := r.dynClient.Resource(ciliumNetworkPolicyGVR).Namespace(r.namespace)
	created, err := policies.Create(ctx, ciliumPolicy, metav1.CreateOptions{})
	if err == nil {
		if ciliumSystemPolicyIntentMatches(created, ciliumPolicy) {
			return nil
		}
		cleanupCtx, cancel := r.newCleanupContext(ctx)
		defer cancel()
		cleanupErr := deleteCreatedCiliumPolicy(cleanupCtx, policies, created)
		return errors.Join(fmt.Errorf("created prepared Cilium system policy does not match requested intent"), cleanupErr)
	}
	if !mayVerifyAmbiguousCreate(err) {
		return fmt.Errorf("create prepared Cilium system policy: %w", err)
	}
	verifyCtx, cancel := r.newCleanupContext(ctx)
	defer cancel()
	current, verifyErr := policies.Get(verifyCtx, ciliumPolicy.GetName(), metav1.GetOptions{})
	if verifyErr == nil && ciliumSystemPolicyIntentMatches(current, ciliumPolicy) {
		return nil
	}
	if verifyErr == nil && current.GetAnnotations()[fusePrepareAttemptAnnotation] == ciliumPolicy.GetAnnotations()[fusePrepareAttemptAnnotation] {
		cleanupErr := deleteCreatedCiliumPolicy(verifyCtx, policies, current)
		return errors.Join(fmt.Errorf("create prepared Cilium system policy: %w", err), fmt.Errorf("created Cilium policy intent was mutated"), cleanupErr)
	}
	return fmt.Errorf("create prepared Cilium system policy: %w", err)
}

func (r *Runtime) bindPreparedSystemPolicy(ctx context.Context, ref runtime.RuntimeRef, mode runtime.SystemEgressMode, desiredPolicy *networkingv1.NetworkPolicy, desiredCilium *unstructured.Unstructured) error {
	switch mode {
	case runtime.SystemEgressCIDR:
		if desiredPolicy == nil {
			return fmt.Errorf("prepared system policy intent is missing")
		}
		policies := r.client.NetworkingV1().NetworkPolicies(r.namespace)
		name := fuseSystemPolicyPrefix + ref.ID
		current, err := policies.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get prepared system policy for runtime binding: %w", err)
		}
		if !networkPolicyIntentMatches(current, desiredPolicy) {
			return fmt.Errorf("prepared system policy intent changed before runtime binding")
		}
		updated := desiredPolicy.DeepCopy()
		updated.ResourceVersion = current.ResourceVersion
		updated.UID = current.UID
		if updated.Annotations == nil {
			updated.Annotations = make(map[string]string)
		}
		updated.Annotations[fuseRuntimeUIDAnnotation] = ref.UID
		result, updateErr := policies.Update(ctx, updated, metav1.UpdateOptions{})
		if updateErr == nil {
			if result.UID == current.UID && networkPolicyIntentMatches(result, updated) {
				return nil
			}
			cleanupCtx, cancel := r.newCleanupContext(ctx)
			defer cancel()
			cleanupErr := deleteCreatedNetworkPolicy(cleanupCtx, policies, result)
			return errors.Join(fmt.Errorf("bound prepared system policy does not match requested intent"), cleanupErr)
		}
		verified, verifyErr := policies.Get(ctx, name, metav1.GetOptions{})
		if verifyErr == nil && verified.UID == current.UID && networkPolicyIntentMatches(verified, updated) {
			return nil
		}
		return fmt.Errorf("bind prepared system policy to runtime UID: %w", updateErr)
	case runtime.SystemEgressCiliumFQDN:
		return r.bindPreparedCiliumSystemPolicy(ctx, ref, desiredCilium)
	default:
		return fmt.Errorf("prepared system egress mode is invalid")
	}
}

func (r *Runtime) bindPreparedCiliumSystemPolicy(ctx context.Context, ref runtime.RuntimeRef, desired *unstructured.Unstructured) error {
	if r.dynClient == nil {
		return fmt.Errorf("Cilium system policy client is unavailable")
	}
	if desired == nil {
		return fmt.Errorf("prepared Cilium system policy intent is missing")
	}
	policies := r.dynClient.Resource(ciliumNetworkPolicyGVR).Namespace(r.namespace)
	name := fuseSystemPolicyPrefix + ref.ID
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, getErr := policies.Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("get prepared Cilium system policy for runtime binding: %w", getErr)
		}
		updated := desired.DeepCopy()
		updated.SetResourceVersion(current.GetResourceVersion())
		updated.SetUID(current.GetUID())
		annotations := updated.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[fuseRuntimeUIDAnnotation] = ref.UID
		updated.SetAnnotations(annotations)
		if current.GetUID() == updated.GetUID() && ciliumSystemPolicyIntentMatches(current, updated) {
			return nil
		}
		if !ciliumSystemPolicyIntentMatches(current, desired) {
			return fmt.Errorf("prepared Cilium system policy intent changed before runtime binding")
		}
		result, updateErr := policies.Update(ctx, updated, metav1.UpdateOptions{})
		if updateErr == nil {
			if result.GetUID() == current.GetUID() && ciliumSystemPolicyIntentMatches(result, updated) {
				return nil
			}
			cleanupCtx, cancel := r.newCleanupContext(ctx)
			defer cancel()
			cleanupErr := deleteCreatedCiliumPolicy(cleanupCtx, policies, result)
			return errors.Join(fmt.Errorf("bound prepared Cilium system policy does not match requested intent"), cleanupErr)
		}
		verified, verifyErr := policies.Get(ctx, name, metav1.GetOptions{})
		if verifyErr == nil && verified.GetUID() == current.GetUID() && ciliumSystemPolicyIntentMatches(verified, updated) {
			return nil
		}
		return updateErr
	})
	if err != nil {
		return fmt.Errorf("bind prepared Cilium system policy to runtime UID: %w", err)
	}
	return nil
}

func ciliumSystemPolicyIntentMatches(current, desired *unstructured.Unstructured) bool {
	if current == nil || desired == nil || !ciliumSystemPolicyOwnershipMatches(current, desired.GetLabels()["sandbox.pool.instance"]) ||
		!reflect.DeepEqual(current.GetAnnotations(), desired.GetAnnotations()) {
		return false
	}
	return reflect.DeepEqual(current.Object["spec"], desired.Object["spec"])
}

func ciliumSystemPolicyOwnershipMatches(current *unstructured.Unstructured, instance string) bool {
	if current == nil || current.GetLabels()["sandbox.managed"] != "true" ||
		current.GetLabels()["sandbox.pool.instance"] != instance || current.GetLabels()["sandbox.policy.role"] != "system" {
		return false
	}
	selector, found, err := unstructured.NestedStringMap(current.Object, "spec", "endpointSelector", "matchLabels")
	return err == nil && found && reflect.DeepEqual(selector, map[string]string{"sandbox.pool.instance": instance})
}

func deleteCreatedNetworkPolicy(ctx context.Context, policies typednetworkingv1.NetworkPolicyInterface, created *networkingv1.NetworkPolicy) error {
	if created == nil || created.UID == "" {
		return fmt.Errorf("created network policy has no immutable UID")
	}
	uid := created.UID
	deleteErr := policies.Delete(ctx, created.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	verified, verifyErr := policies.Get(ctx, created.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(verifyErr) || (verifyErr == nil && verified.UID != uid) {
		return nil
	}
	if deleteErr != nil {
		return fmt.Errorf("delete rejected network policy: %w", deleteErr)
	}
	if verifyErr != nil {
		return fmt.Errorf("verify rejected network policy deletion: %w", verifyErr)
	}
	return fmt.Errorf("rejected network policy deletion is unconfirmed")
}

func deleteCreatedCiliumPolicy(ctx context.Context, policies dynamic.ResourceInterface, created *unstructured.Unstructured) error {
	if created == nil || created.GetUID() == "" {
		return fmt.Errorf("created Cilium policy has no immutable UID")
	}
	uid := created.GetUID()
	deleteErr := policies.Delete(ctx, created.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	verified, verifyErr := policies.Get(ctx, created.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(verifyErr) || (verifyErr == nil && verified.GetUID() != uid) {
		return nil
	}
	if deleteErr != nil {
		return fmt.Errorf("delete rejected Cilium policy: %w", deleteErr)
	}
	if verifyErr != nil {
		return fmt.Errorf("verify rejected Cilium policy deletion: %w", verifyErr)
	}
	return fmt.Errorf("rejected Cilium policy deletion is unconfirmed")
}

func (r *Runtime) verifyAmbiguousPodCreate(ctx context.Context, desired *corev1.Pod, createErr error) (*corev1.Pod, bool, error) {
	if !mayVerifyAmbiguousCreate(createErr) {
		return nil, true, fmt.Errorf("create prepared Pod: %w", createErr)
	}
	cleanupCtx, cancel := r.newCleanupContext(ctx)
	defer cancel()
	current, err := r.client.CoreV1().Pods(r.namespace).Get(cleanupCtx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, true, fmt.Errorf("create prepared Pod: %w", createErr)
	}
	if err != nil {
		return nil, false, errors.Join(fmt.Errorf("create prepared Pod: %w", createErr), fmt.Errorf("verify prepared Pod creation: %w", err))
	}
	if !preparedPodIntentMatches(current, desired, true) {
		// This invocation created (or exactly adopted) only the unbound system
		// policy. An incompatible Pod must be left untouched, while removing that
		// unbound policy prevents it from inheriting this operation's egress.
		if current.UID != "" && current.Annotations[fusePrepareAttemptAnnotation] == desired.Annotations[fusePrepareAttemptAnnotation] {
			uid := current.UID
			deleteErr := r.client.CoreV1().Pods(r.namespace).Delete(cleanupCtx, current.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
			ref := runtime.RuntimeRef{ID: current.Name, UID: string(current.UID)}
			confirmErr := r.waitPreparedPodGoneOrReplaced(cleanupCtx, ref)
			return nil, confirmErr == nil, errors.Join(fmt.Errorf("create prepared Pod result was mutated"), deleteErr, confirmErr)
		}
		return nil, true, fmt.Errorf("create prepared Pod result is incompatible")
	}
	return current, false, nil
}

func preparedPodIntentMatches(current, desired *corev1.Pod, allowScheduledNodeName bool) bool {
	if current == nil || desired == nil || current.UID == "" || current.Name != desired.Name || current.Namespace != desired.Namespace {
		return false
	}
	for key, value := range desired.Labels {
		if current.Labels[key] != value {
			return false
		}
	}
	for key, value := range desired.Annotations {
		if current.Annotations[key] != value {
			return false
		}
	}
	currentCopy := current.DeepCopy()
	desiredCopy := desired.DeepCopy()
	// Apply the same registered Kubernetes defaults to both sides. A scheduler
	// may bind a Pod while an ambiguous create response is being verified, but a
	// successful Create response has not passed through the scheduler and must
	// retain the exact requested NodeName.
	kubescheme.Scheme.Default(currentCopy)
	kubescheme.Scheme.Default(desiredCopy)
	if len(currentCopy.Annotations) == 0 && len(desiredCopy.Annotations) == 0 {
		currentCopy.Annotations = nil
		desiredCopy.Annotations = nil
	}
	if allowScheduledNodeName {
		currentCopy.Spec.NodeName = ""
		desiredCopy.Spec.NodeName = ""
	}
	// The built-in ServiceAccount admission plugin copies the selected service
	// account's imagePullSecrets when the request leaves this list empty. These
	// references are consumed only by the kubelet image pull path and are not
	// mounted into either container, so normalize this one bounded, name-only
	// admission mutation while keeping all container/volume fields exact.
	if len(desiredCopy.Spec.ImagePullSecrets) == 0 && validServiceAccountImagePullSecrets(currentCopy.Spec.ImagePullSecrets) {
		currentCopy.Spec.ImagePullSecrets = nil
	}
	if desiredCopy.Spec.PriorityClassName == "" && desiredCopy.Spec.Priority == nil && currentCopy.Spec.Priority != nil && *currentCopy.Spec.Priority == 0 {
		currentCopy.Spec.Priority = nil
	}
	if desiredCopy.Spec.PriorityClassName == "" && desiredCopy.Spec.PreemptionPolicy == nil && currentCopy.Spec.PreemptionPolicy != nil && *currentCopy.Spec.PreemptionPolicy == corev1.PreemptLowerPriority {
		currentCopy.Spec.PreemptionPolicy = nil
	}
	return reflect.DeepEqual(currentCopy.Labels, desiredCopy.Labels) &&
		reflect.DeepEqual(currentCopy.Annotations, desiredCopy.Annotations) &&
		apiequality.Semantic.DeepEqual(currentCopy.Spec, desiredCopy.Spec)
}

func preparedPodIntentMismatchReason(current, desired *corev1.Pod) string {
	if current == nil || desired == nil {
		return "identity"
	}
	currentCopy, desiredCopy := current.DeepCopy(), desired.DeepCopy()
	kubescheme.Scheme.Default(currentCopy)
	kubescheme.Scheme.Default(desiredCopy)
	if len(currentCopy.Annotations) == 0 && len(desiredCopy.Annotations) == 0 {
		currentCopy.Annotations, desiredCopy.Annotations = nil, nil
	}
	if !reflect.DeepEqual(currentCopy.Labels, desiredCopy.Labels) {
		return "labels"
	}
	if !reflect.DeepEqual(currentCopy.Annotations, desiredCopy.Annotations) {
		return "annotations"
	}
	currentSpec, desiredSpec := reflect.ValueOf(currentCopy.Spec), reflect.ValueOf(desiredCopy.Spec)
	typ := currentSpec.Type()
	for index := 0; index < currentSpec.NumField(); index++ {
		if !apiequality.Semantic.DeepEqual(currentSpec.Field(index).Interface(), desiredSpec.Field(index).Interface()) {
			if typ.Field(index).Name == "InitContainers" && currentSpec.Field(index).Len() == desiredSpec.Field(index).Len() && currentSpec.Field(index).Len() > 0 {
				currentContainer := currentSpec.Field(index).Index(0)
				desiredContainer := desiredSpec.Field(index).Index(0)
				containerType := currentContainer.Type()
				for field := 0; field < currentContainer.NumField(); field++ {
					if !apiequality.Semantic.DeepEqual(currentContainer.Field(field).Interface(), desiredContainer.Field(field).Interface()) {
						return "spec.InitContainers[0]." + containerType.Field(field).Name
					}
				}
			}
			return "spec." + typ.Field(index).Name
		}
	}
	return "identity"
}

func validServiceAccountImagePullSecrets(refs []corev1.LocalObjectReference) bool {
	const maxServiceAccountImagePullSecrets = 64
	if len(refs) == 0 || len(refs) > maxServiceAccountImagePullSecrets {
		return false
	}
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if ref.Name == "" || len(validation.IsDNS1123Subdomain(ref.Name)) != 0 {
			return false
		}
		if _, duplicate := seen[ref.Name]; duplicate {
			return false
		}
		seen[ref.Name] = struct{}{}
	}
	return true
}

func mayVerifyAmbiguousCreate(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var status interface{ Status() metav1.Status }
	return !errors.As(err, &status)
}

func (r *Runtime) compensatePreparedFailure(ctx context.Context, pod *corev1.Pod, mode runtime.SystemEgressMode, prepareAttempt string, cause error) error {
	cleanupCtx, cancel := r.newCleanupContext(ctx)
	defer cancel()
	ref := runtime.RuntimeRef{ID: pod.Name, UID: string(pod.UID)}
	deleteErr := r.client.CoreV1().Pods(r.namespace).Delete(cleanupCtx, ref.ID, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &pod.UID}})
	// A delete response can be lost after the API accepted it. Only an exact
	// readback that proves this immutable UID is gone permits policy cleanup.
	deleteErr = r.waitPreparedPodGoneOrReplaced(cleanupCtx, ref)
	if deleteErr == nil {
		deleteErr = r.deletePreparedSystemPolicy(cleanupCtx, ref.ID, mode, ref.UID, prepareAttempt, true)
	}
	if deleteErr != nil {
		return errors.Join(cause, fmt.Errorf("compensate prepared Pod: %w", deleteErr))
	}
	return cause
}

func (r *Runtime) newCleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := r.terminationTimeout
	if timeout <= 0 {
		timeout = defaultKubernetesControlTimeout
	}
	return context.WithTimeout(context.WithoutCancel(parent), timeout)
}

func (r *Runtime) waitPreparedContainers(ctx context.Context, ref runtime.RuntimeRef) error {
	deadline := r.prepareTimeout
	if deadline <= 0 {
		deadline = defaultKubernetesControlTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	for {
		pod, err := r.getExactPod(waitCtx, ref)
		if err != nil {
			return err
		}
		if err := validatePreparedContainerState(pod); err == nil {
			return nil
		} else if hasFatalPreparedState(pod) {
			return err
		}
		if err := waitPoll(waitCtx, r.pollInterval); err != nil {
			return fmt.Errorf("wait for prepared containers: %w", err)
		}
	}
}

func validatePreparedContainerState(pod *corev1.Pod) error {
	mounterOK := false
	for _, status := range pod.Status.InitContainerStatuses {
		if status.Name == workspaceMounterContainer {
			mounterOK = status.RestartCount == 0 && status.Started != nil && *status.Started && status.State.Running != nil
		}
	}
	sandboxOK := false
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == sandboxContainer {
			sandboxOK = status.State.Running != nil
		}
	}
	if pod.DeletionTimestamp != nil || !mounterOK || !sandboxOK {
		return fmt.Errorf("prepared Pod containers are not healthy: phase=%s workspace-mounter=%s sandbox=%s",
			pod.Status.Phase,
			preparedContainerStatusSummary(pod.Status.InitContainerStatuses, workspaceMounterContainer),
			preparedContainerStatusSummary(pod.Status.ContainerStatuses, sandboxContainer))
	}
	return nil
}

func preparedContainerStatusSummary(statuses []corev1.ContainerStatus, name string) string {
	for _, status := range statuses {
		if status.Name != name {
			continue
		}
		started := "unknown"
		if status.Started != nil {
			started = strconv.FormatBool(*status.Started)
		}
		switch {
		case status.State.Terminated != nil:
			state := status.State.Terminated
			return fmt.Sprintf("terminated(reason=%s exit=%d signal=%d restart=%d started=%s)", state.Reason, state.ExitCode, state.Signal, status.RestartCount, started)
		case status.State.Waiting != nil:
			return fmt.Sprintf("waiting(reason=%s restart=%d started=%s)", status.State.Waiting.Reason, status.RestartCount, started)
		case status.State.Running != nil:
			return fmt.Sprintf("running(restart=%d started=%s)", status.RestartCount, started)
		default:
			return fmt.Sprintf("unknown(restart=%d started=%s)", status.RestartCount, started)
		}
	}
	return "missing"
}

func hasFatalPreparedState(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
		return true
	}
	for _, status := range pod.Status.InitContainerStatuses {
		if status.Name == workspaceMounterContainer && (status.RestartCount != 0 || status.State.Terminated != nil) {
			return true
		}
	}
	return false
}

func (r *Runtime) waitReadyPod(ctx context.Context, ref runtime.RuntimeRef) (*corev1.Pod, error) {
	deadline := r.readyTimeout
	if deadline <= 0 {
		deadline = defaultKubernetesControlTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	for {
		pod, err := r.getExactPod(waitCtx, ref)
		if err != nil {
			return nil, err
		}
		if err := validateExactFUSEPodIdentity(pod, ref); err != nil || pod.Labels["sandbox.pool.state"] != "prepared" {
			return nil, fmt.Errorf("FUSE Pod identity labels changed")
		}
		restartCount, restartErr := mounterRestartCount(pod)
		if restartErr != nil || restartCount != 0 || pod.DeletionTimestamp != nil {
			return nil, fmt.Errorf("workspace mounter restarted or disappeared")
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				return pod, nil
			}
		}
		if err := waitPoll(waitCtx, r.pollInterval); err != nil {
			return nil, fmt.Errorf("wait for FUSE Pod Ready: %w", err)
		}
	}
}

func (r *Runtime) patchPreparedState(ctx context.Context, ref runtime.RuntimeRef) (*corev1.Pod, error) {
	pod, err := r.getExactPod(ctx, ref)
	if err != nil {
		return nil, err
	}
	if validateExactFUSEPodIdentity(pod, ref) != nil || pod.Labels["sandbox.pool.state"] != "preparing" || pod.ResourceVersion == "" {
		return nil, fmt.Errorf("prepared state transition precondition failed")
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{
		"uid": string(pod.UID), "resourceVersion": pod.ResourceVersion,
		"labels": map[string]any{"sandbox.pool.state": "prepared"},
	}})
	updated, patchErr := r.client.CoreV1().Pods(r.namespace).Patch(ctx, ref.ID, types.MergePatchType, patch, metav1.PatchOptions{})
	if patchErr == nil {
		if validateExactFUSEPodIdentity(updated, ref) != nil || updated.Labels["sandbox.pool.state"] != "prepared" {
			return nil, runtime.ErrInvalidRuntimeRef
		}
		return updated, nil
	}
	current, getErr := r.getExactPod(ctx, ref)
	if getErr == nil && validateExactFUSEPodIdentity(current, ref) == nil && current.Labels["sandbox.pool.state"] == "prepared" {
		return current, nil
	}
	return nil, fmt.Errorf("patch prepared state: %w", patchErr)
}

func validateExactFUSEPodIdentity(pod *corev1.Pod, ref runtime.RuntimeRef) error {
	_, err := exactFUSEPodBootstrap(pod, ref)
	return err
}

func exactFUSEPodBootstrap(pod *corev1.Pod, ref runtime.RuntimeRef) (preparedMounterBootstrap, error) {
	var bootstrap preparedMounterBootstrap
	prepareAttempt := ""
	if pod != nil {
		prepareAttempt = pod.Annotations[fusePrepareAttemptAnnotation]
	}
	decodedAttempt, decodeAttemptErr := hex.DecodeString(prepareAttempt)
	if pod == nil || string(pod.UID) != ref.UID || pod.Name != ref.ID || pod.Labels["sandbox.managed"] != "true" ||
		pod.Labels["sandbox.pool"] != "true" || pod.Labels["sandbox.workspace.mode"] != "fuse" ||
		pod.Labels["sandbox.pool.instance"] != ref.ID || pod.Labels["sandbox.id"] != ref.ID ||
		decodeAttemptErr != nil || len(decodedAttempt) != 32 {
		return bootstrap, runtime.ErrInvalidRuntimeRef
	}
	var rawBootstrap string
	mounterCount := 0
	for _, container := range pod.Spec.InitContainers {
		if container.Name != workspaceMounterContainer {
			continue
		}
		mounterCount++
		for _, env := range container.Env {
			if env.Name == "SANDBOX_MOUNTER_BOOTSTRAP" {
				if rawBootstrap != "" || env.ValueFrom != nil {
					return bootstrap, runtime.ErrInvalidRuntimeRef
				}
				rawBootstrap = env.Value
			}
		}
	}
	if mounterCount != 1 || rawBootstrap == "" || len(rawBootstrap) > maxControlJSONBytes || json.Unmarshal([]byte(rawBootstrap), &bootstrap) != nil ||
		bootstrap.Version != controlWireVersion || bootstrap.Provider == "" || bootstrap.PoolKey == "" ||
		pod.Labels["sandbox.workspace.provider"] != bootstrap.Provider {
		return preparedMounterBootstrap{}, runtime.ErrInvalidRuntimeRef
	}
	poolKeyLabel, err := preparedPoolKeyLabel(bootstrap.PoolKey)
	if err != nil || pod.Labels["sandbox.pool.key"] != poolKeyLabel {
		return preparedMounterBootstrap{}, runtime.ErrInvalidRuntimeRef
	}
	return bootstrap, nil
}

func (r *Runtime) getExactPod(ctx context.Context, ref runtime.RuntimeRef) (*corev1.Pod, error) {
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	pod, err := r.client.CoreV1().Pods(r.namespace).Get(ctx, ref.ID, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, runtime.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get exact Pod: %w", err)
	}
	if string(pod.UID) != ref.UID {
		return nil, runtime.ErrInvalidRuntimeRef
	}
	return pod, nil
}

func (r *Runtime) readMounterStatus(ctx context.Context, ref runtime.RuntimeRef, kind string) (mounterStatusWire, error) {
	raw, err := r.execControl(ctx, ref.ID, workspaceMounterContainer, []string{mounterBinary, "health", kind}, nil)
	if err != nil {
		return mounterStatusWire{}, err
	}
	status, err := decodeMounterStatus(raw)
	if err != nil {
		return mounterStatusWire{}, err
	}
	if status.RuntimeUID != ref.UID {
		return mounterStatusWire{}, runtime.ErrInvalidRuntimeRef
	}
	return status, nil
}

func mounterRestartCount(pod *corev1.Pod) (int32, error) {
	for _, status := range pod.Status.InitContainerStatuses {
		if status.Name == workspaceMounterContainer {
			return status.RestartCount, nil
		}
	}
	return 0, fmt.Errorf("workspace mounter status is missing")
}

func probeArgv(operation string, ref runtime.RuntimeRef, generation int64, tokenStdin bool) []string {
	argv := []string{workspaceProbeBinary, operation, "--runtime-uid", ref.UID, "--generation", strconv.FormatInt(generation, 10)}
	if tokenStdin {
		argv = append(argv, "--token-stdin")
	}
	return argv
}

func sandboxInfoForPod(id string, pod *corev1.Pod) *runtime.SandboxInfo {
	return &runtime.SandboxInfo{ID: id, RuntimeID: pod.Name, RuntimeUID: string(pod.UID), State: "running", CreatedAt: pod.CreationTimestamp.Time}
}

func (r *Runtime) ensureWorkspaceStatesLocked() map[string]*workspaceRuntimeState {
	if r.workspaceStates == nil {
		r.workspaceStates = make(map[string]*workspaceRuntimeState)
	}
	return r.workspaceStates
}

func (r *Runtime) beginWorkspaceOperation(ref runtime.RuntimeRef, requireAuthorized bool) (*workspaceRuntimeState, func(), error) {
	if err := ref.Validate(); err != nil {
		return nil, nil, err
	}
	r.stateMu.Lock()
	states := r.ensureWorkspaceStatesLocked()
	state := states[ref.ID]
	if state == nil {
		state = &workspaceRuntimeState{runtimeUID: ref.UID}
		states[ref.ID] = state
	}
	if state.runtimeUID != ref.UID {
		r.stateMu.Unlock()
		return nil, nil, runtime.ErrInvalidRuntimeRef
	}
	if state.removing || (requireAuthorized && !state.authorized) {
		r.stateMu.Unlock()
		return nil, nil, fmt.Errorf("workspace runtime is unavailable")
	}
	state.active++
	r.stateMu.Unlock()
	return state, func() { r.endWorkspaceOperation(state) }, nil
}

func (r *Runtime) beginAuthorization(ref runtime.RuntimeRef, auth runtime.WorkspaceMountAuthorization) (*workspaceRuntimeState, func(), error) {
	if err := ref.Validate(); err != nil {
		return nil, nil, err
	}
	r.stateMu.Lock()
	states := r.ensureWorkspaceStatesLocked()
	state := states[ref.ID]
	if state == nil || state.runtimeUID != ref.UID {
		r.stateMu.Unlock()
		return nil, nil, runtime.ErrInvalidRuntimeRef
	}
	if state.removing || state.authAttempted || state.poolKey != auth.PoolKey {
		r.stateMu.Unlock()
		return nil, nil, fmt.Errorf("workspace authorization cannot be replayed")
	}
	state.authAttempted = true
	// Persist the attempted generation before any control I/O. A lost authorize
	// response must still allow shutdown to address the generation the sidecar
	// may already have accepted, without making WaitReady consider it authorized.
	state.generation = auth.LeaseGeneration
	state.active++
	r.stateMu.Unlock()
	return state, func() { r.endWorkspaceOperation(state) }, nil
}

func (r *Runtime) endWorkspaceOperation(state *workspaceRuntimeState) {
	r.stateMu.Lock()
	state.active--
	if state.active == 0 && state.idle != nil {
		close(state.idle)
		state.idle = nil
	}
	r.stateMu.Unlock()
}

func (r *Runtime) beginWorkspaceRemoval(ctx context.Context, ref runtime.RuntimeRef) (*workspaceRuntimeState, error) {
	for {
		r.stateMu.Lock()
		states := r.ensureWorkspaceStatesLocked()
		state := states[ref.ID]
		if state == nil {
			state = &workspaceRuntimeState{runtimeUID: ref.UID}
			states[ref.ID] = state
		}
		if state.runtimeUID != ref.UID {
			r.stateMu.Unlock()
			return nil, runtime.ErrInvalidRuntimeRef
		}
		state.removing = true
		if state.active == 0 {
			state.active = -1
			state.idle = make(chan struct{})
			r.stateMu.Unlock()
			return state, nil
		}
		if state.idle == nil {
			state.idle = make(chan struct{})
		}
		wait := state.idle
		r.stateMu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (r *Runtime) finishWorkspaceRemoval(state *workspaceRuntimeState) {
	r.stateMu.Lock()
	state.active = 0
	if state.idle != nil {
		close(state.idle)
		state.idle = nil
	}
	r.stateMu.Unlock()
}

func cloneTerminationEvidence(value *runtime.TerminationEvidence) *runtime.TerminationEvidence {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func waitPoll(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Millisecond
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runtime) waitExactPodGone(ctx context.Context, ref runtime.RuntimeRef) error {
	timeout := r.terminationTimeout
	if timeout <= 0 {
		timeout = defaultKubernetesControlTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		pod, err := r.client.CoreV1().Pods(r.namespace).Get(waitCtx, ref.ID, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("observe exact Pod termination: %w", err)
		}
		if string(pod.UID) != ref.UID {
			return runtime.ErrInvalidRuntimeRef
		}
		if err := waitPoll(waitCtx, r.pollInterval); err != nil {
			return fmt.Errorf("observe exact Pod termination: %w", err)
		}
	}
}

func (r *Runtime) waitPreparedPodGoneOrReplaced(ctx context.Context, ref runtime.RuntimeRef) error {
	timeout := r.terminationTimeout
	if timeout <= 0 {
		timeout = defaultKubernetesControlTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		pod, err := r.client.CoreV1().Pods(r.namespace).Get(waitCtx, ref.ID, metav1.GetOptions{})
		if apierrors.IsNotFound(err) || (err == nil && string(pod.UID) != ref.UID) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("observe prepared Pod cleanup: %w", err)
		}
		if err := waitPoll(waitCtx, r.pollInterval); err != nil {
			return fmt.Errorf("observe prepared Pod cleanup: %w", err)
		}
	}
}

func (r *Runtime) deleteFUSEPolicies(ctx context.Context, ref runtime.RuntimeRef, mode runtime.SystemEgressMode) error {
	if err := deleteExactNetworkPolicy(ctx, r.client, r.namespace, fuseUserPolicyPrefix+ref.ID, ref.ID, "user", ref.UID, "", false); err != nil {
		return err
	}
	if err := r.deleteExactCiliumUserPolicy(ctx, ref); err != nil {
		return err
	}
	return r.deletePreparedSystemPolicy(ctx, ref.ID, mode, ref.UID, "", false)
}

func (r *Runtime) deleteExactCiliumUserPolicy(ctx context.Context, ref runtime.RuntimeRef) error {
	if r.dynClient == nil {
		return nil
	}
	policies := r.dynClient.Resource(ciliumNetworkPolicyGVR).Namespace(r.namespace)
	name := fuseUserDenyPolicyPrefix + ref.ID
	current, err := policies.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get exact Cilium user deny policy for deletion: %w", err)
	}
	if !ciliumUserPolicyOwnershipMatches(current, ref.ID) || current.GetAnnotations()[fuseRuntimeUIDAnnotation] != ref.UID {
		return runtime.ErrInvalidRuntimeRef
	}
	if current.GetUID() == "" {
		return fmt.Errorf("Cilium user deny policy has no immutable UID")
	}
	originalUID := current.GetUID()
	deleteErr := policies.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &originalUID}})
	verified, verifyErr := policies.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(verifyErr) || (verifyErr == nil && verified.GetUID() != originalUID) {
		return nil
	}
	if deleteErr != nil {
		return fmt.Errorf("delete exact Cilium user deny policy: %w", deleteErr)
	}
	if verifyErr != nil {
		return fmt.Errorf("verify exact Cilium user deny policy deletion: %w", verifyErr)
	}
	return fmt.Errorf("exact Cilium user deny policy deletion is unconfirmed")
}

func (r *Runtime) deletePreparedSystemPolicy(ctx context.Context, instance string, mode runtime.SystemEgressMode, runtimeUID, prepareAttempt string, allowUnbound bool) error {
	switch mode {
	case runtime.SystemEgressCIDR:
		return deleteExactNetworkPolicy(ctx, r.client, r.namespace, fuseSystemPolicyPrefix+instance, instance, "system", runtimeUID, prepareAttempt, allowUnbound)
	case runtime.SystemEgressCiliumFQDN:
		return r.deleteExactCiliumSystemPolicy(ctx, instance, runtimeUID, prepareAttempt, allowUnbound)
	default:
		standardErr := deleteExactNetworkPolicy(ctx, r.client, r.namespace, fuseSystemPolicyPrefix+instance, instance, "system", runtimeUID, prepareAttempt, allowUnbound)
		ciliumErr := r.deleteExactCiliumSystemPolicy(ctx, instance, runtimeUID, prepareAttempt, allowUnbound)
		return errors.Join(standardErr, ciliumErr)
	}
}

func (r *Runtime) deleteExactCiliumSystemPolicy(ctx context.Context, instance, runtimeUID, prepareAttempt string, allowUnbound bool) error {
	if r.dynClient == nil {
		return nil
	}
	policies := r.dynClient.Resource(ciliumNetworkPolicyGVR).Namespace(r.namespace)
	name := fuseSystemPolicyPrefix + instance
	current, err := policies.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get exact Cilium system policy for deletion: %w", err)
	}
	if !ciliumSystemPolicyOwnershipMatches(current, instance) {
		return fmt.Errorf("Cilium system policy ownership does not match")
	}
	boundUID := current.GetAnnotations()[fuseRuntimeUIDAnnotation]
	if boundUID != runtimeUID && !(allowUnbound && boundUID == "") {
		return runtime.ErrInvalidRuntimeRef
	}
	if prepareAttempt != "" && current.GetAnnotations()[fusePrepareAttemptAnnotation] != prepareAttempt {
		return runtime.ErrInvalidRuntimeRef
	}
	if current.GetUID() == "" {
		return fmt.Errorf("Cilium system policy has no immutable UID")
	}
	originalUID := current.GetUID()
	options := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &originalUID}}
	deleteErr := policies.Delete(ctx, name, options)
	verified, verifyErr := policies.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(verifyErr) || (verifyErr == nil && verified.GetUID() != originalUID) {
		return nil
	}
	if deleteErr != nil {
		return fmt.Errorf("delete exact Cilium system policy: %w", deleteErr)
	}
	if verifyErr != nil {
		return fmt.Errorf("verify exact Cilium system policy deletion: %w", verifyErr)
	}
	return fmt.Errorf("exact Cilium system policy deletion is unconfirmed")
}

func (r *Runtime) StartSandbox(_ context.Context, _ string) error {
	// Kubernetes pods do not support pause/resume semantics like Docker containers.
	// Once a pod is created, it runs until deleted. StartSandbox is a no-op for K8s.
	// Callers should use CreateSandbox to launch a new pod instead.
	return nil
}

func (r *Runtime) StopSandbox(ctx context.Context, id string) error {
	// Kubernetes pods cannot be stopped and restarted. The only way to "stop" a pod
	// is to delete it. Note this is a destructive operation: the pod and its ephemeral
	// storage are permanently removed. Callers should be aware that StopSandbox on K8s
	// is equivalent to RemoveSandbox without network policy cleanup.
	return deletePod(ctx, r.client, r.namespace, id)
}

func (r *Runtime) RemoveSandbox(ctx context.Context, id string) error {
	// Clean up network policy
	if r.hasCilium {
		_ = deleteCiliumPrivateDeny(ctx, r.dynClient, r.namespace, id)
	}
	_ = deleteNetworkPolicy(ctx, r.client, r.namespace, id)
	return deletePod(ctx, r.client, r.namespace, id)
}

func (r *Runtime) GetSandbox(ctx context.Context, id string) (*runtime.SandboxInfo, error) {
	pod, err := getPod(ctx, r.client, r.namespace, id)
	if err != nil {
		return nil, err
	}

	return &runtime.SandboxInfo{
		ID:         id,
		RuntimeID:  pod.Name,
		RuntimeUID: string(pod.UID),
		State:      podStateString(pod.Status.Phase),
		CreatedAt:  pod.CreationTimestamp.Time,
	}, nil
}

func (r *Runtime) Exec(ctx context.Context, id string, req runtime.ExecRequest) (*runtime.ExecResult, error) {
	return execInPod(ctx, r.client, r.restConfig, r.namespace, id, req)
}

func (r *Runtime) ExecStream(ctx context.Context, id string, req runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
	return execStreamInPod(ctx, r.client, r.restConfig, r.namespace, id, req)
}

func (r *Runtime) ExecPipe(ctx context.Context, id string, cmd []string, stdin io.Reader) error {
	return execPipeInPod(ctx, r.client, r.restConfig, r.namespace, id, cmd, stdin)
}

func (r *Runtime) UploadFile(ctx context.Context, id, destPath string, size int64, reader io.Reader) error {
	return uploadFileToPod(ctx, r.client, r.restConfig, r.namespace, id, destPath, size, reader)
}

func (r *Runtime) DownloadFile(ctx context.Context, id string, srcPath string) (io.ReadCloser, error) {
	return downloadFileFromPod(ctx, r.client, r.restConfig, r.namespace, id, srcPath)
}

func (r *Runtime) UploadArchive(ctx context.Context, id string, destDir string, archive io.Reader) error {
	return uploadArchiveToPod(ctx, r.client, r.restConfig, r.namespace, id, destDir, archive)
}

func (r *Runtime) DownloadDir(ctx context.Context, id string, dirPath string) (io.ReadCloser, error) {
	return downloadDirFromPod(ctx, r.client, r.restConfig, r.namespace, id, dirPath)
}

func (r *Runtime) ListFiles(ctx context.Context, id string, dirPath string) ([]runtime.FileInfo, error) {
	return listFilesInPod(ctx, r.client, r.restConfig, r.namespace, id, dirPath)
}

func (r *Runtime) ListFilesRecursive(ctx context.Context, id string, dirPath string, maxDepth int, page int, pageSize int) (*runtime.FileListResult, error) {
	return listFilesRecursiveInPod(ctx, r.client, r.restConfig, r.namespace, id, dirPath, maxDepth, page, pageSize)
}

func (r *Runtime) GlobFiles(ctx context.Context, id string, baseDir string, pattern string, page int, pageSize int) (*runtime.FileListResult, error) {
	return globFilesInPod(ctx, r.client, r.restConfig, r.namespace, id, baseDir, pattern, page, pageSize)
}

func (r *Runtime) CountReservedFiles(ctx context.Context, id, baseDir string, maxDepth int, globPattern string) (int, error) {
	return countReservedFilesInPod(ctx, r.client, r.restConfig, r.namespace, id, baseDir, maxDepth, globPattern)
}

func (r *Runtime) ReadFileLines(ctx context.Context, id string, filePath string, startLine int, endLine int) (*runtime.FileLineResult, error) {
	return readFileLinesInPod(ctx, r.client, r.restConfig, r.namespace, id, filePath, startLine, endLine)
}

func (r *Runtime) EditFile(ctx context.Context, id string, filePath string, oldStr string, newStr string, replaceAll bool) error {
	return editFileInPod(ctx, r.client, r.restConfig, r.namespace, id, filePath, oldStr, newStr, replaceAll)
}

func (r *Runtime) EditFileLines(ctx context.Context, id string, filePath string, startLine int, endLine int, newContent string) error {
	return editFileLinesInPod(ctx, r.client, r.restConfig, r.namespace, id, filePath, startLine, endLine, newContent)
}

func (r *Runtime) UpdateNetwork(ctx context.Context, id string, enabled bool, whitelist []string, blockPrivate bool) error {
	if err := updateNetworkPolicy(ctx, r.client, r.namespace, id, enabled, whitelist, blockPrivate); err != nil {
		return err
	}
	if !r.hasCilium {
		return nil
	}
	if enabled && len(whitelist) == 0 {
		return applyCiliumPrivateDeny(ctx, r.dynClient, r.namespace, id)
	}
	return deleteCiliumPrivateDeny(ctx, r.dynClient, r.namespace, id)
}

func (r *Runtime) RenameSandbox(_ context.Context, _ string, _ string) error {
	// Kubernetes pods cannot be renamed; this is a no-op.
	return nil
}

func (r *Runtime) UpdateLabels(ctx context.Context, id string, labels map[string]*string) error {
	protected := false
	for key := range labels {
		switch key {
		case "sandbox.id", "sandbox.managed", "sandbox.pool", "sandbox.pool.state", "sandbox.pool.key", "sandbox.pool.instance", "sandbox.workspace.mode", "sandbox.workspace.provider":
			protected = true
		}
	}
	var exactMetadata map[string]interface{}
	if protected {
		pod, err := r.client.CoreV1().Pods(r.namespace).Get(ctx, id, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get pod before protected label patch: %w", err)
		}
		if pod.Labels["sandbox.workspace.mode"] == "fuse" {
			return fmt.Errorf("protected FUSE Pod labels cannot be changed through generic UpdateLabels")
		}
		exactMetadata = map[string]interface{}{"uid": string(pod.UID), "resourceVersion": pod.ResourceVersion}
	}
	// Build a merge-patch that only touches the labels we care about.
	// Using Patch avoids the GET+PUT race (409 Conflict on resourceVersion mismatch)
	// and sidesteps admission webhooks that reject full pod Updates.
	labelMap := make(map[string]interface{}, len(labels))
	for k, v := range labels {
		if v == nil {
			labelMap[k] = nil // JSON merge-patch: null removes the key
		} else {
			labelMap[k] = *v
		}
	}
	metadata := exactMetadata
	if metadata == nil {
		metadata = make(map[string]interface{})
	}
	metadata["labels"] = labelMap
	patch := map[string]interface{}{"metadata": metadata}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal label patch: %w", err)
	}
	_, err = r.client.CoreV1().Pods(r.namespace).Patch(ctx, id, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patch pod labels: %w", err)
	}
	return nil
}

func (r *Runtime) ListSandboxes(ctx context.Context, labels map[string]string) ([]runtime.SandboxInfo, error) {
	var parts []string
	for k, v := range labels {
		parts = append(parts, k+"="+v)
	}
	selector := ""
	if len(parts) > 0 {
		selector = joinStrings(parts, ",")
	}

	pods, err := r.client.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}

	result := make([]runtime.SandboxInfo, 0, len(pods.Items))
	for _, pod := range pods.Items {
		result = append(result, runtime.SandboxInfo{
			ID:         pod.Labels["sandbox.id"],
			RuntimeID:  pod.Name,
			RuntimeUID: string(pod.UID),
			State:      podStateString(pod.Status.Phase),
			CreatedAt:  pod.CreationTimestamp.Time,
			Labels:     maps.Clone(pod.Labels),
		})
	}
	return result, nil
}

func (r *Runtime) IsStateful() bool {
	// Kubernetes pods survive a process restart independently; they must be
	// restored (not recreated) on startup.
	return true
}

func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	result := parts[0]
	for _, p := range parts[1:] {
		result += sep + p
	}
	return result
}
