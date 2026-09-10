package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	sandboxruntime "github.com/goairix/sandbox/internal/runtime"
)

type podExecutorFunc func(context.Context, string, string, []string, []byte) ([]byte, error)

func TestValidatePreparedContainerStateReportsSafeExitEvidence(t *testing.T) {
	started := false
	pod := &corev1.Pod{Status: corev1.PodStatus{
		Phase: corev1.PodFailed,
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name: workspaceMounterContainer, RestartCount: 1, Started: &started,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 23, Signal: 0}},
		}},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  sandboxContainer,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}},
		}},
	}}

	err := validatePreparedContainerState(pod)
	require.Error(t, err)
	assert.ErrorContains(t, err, "phase=Failed")
	assert.ErrorContains(t, err, "workspace-mounter=terminated(reason=Error exit=23 signal=0 restart=1 started=false)")
	assert.ErrorContains(t, err, "sandbox=waiting(reason=PodInitializing restart=0 started=unknown)")
}

func TestRemoveSandboxMapsMissingPodToRuntimeNotFound(t *testing.T) {
	r := &Runtime{client: kubefake.NewSimpleClientset(), namespace: "sandbox-runtime"}

	err := r.RemoveSandbox(context.Background(), "never-created")

	require.ErrorIs(t, err, sandboxruntime.ErrNotFound)
}

func TestPreparedPodIntentTreatsEmptyAdmissionMetadataAsEquivalent(t *testing.T) {
	desired, err := buildPreparedFUSEPod("sandbox-runtime", func() sandboxruntime.SandboxSpec {
		spec := preparedFUSESpecForTest()
		spec.WorkspaceFUSE.LSMProfile = ""
		spec.WorkspaceFUSE.AllowMissingLSMForKind = true
		return spec
	}())
	require.NoError(t, err)
	current := desired.DeepCopy()
	current.UID = "pod-uid"
	current.Annotations = nil

	assert.True(t, preparedPodIntentMatches(current, desired, false))
}

func (f podExecutorFunc) Exec(ctx context.Context, pod, container string, argv []string, stdin []byte) ([]byte, error) {
	return f(ctx, pod, container, argv, stdin)
}

type commandScript struct {
	mu       sync.Mutex
	commands []recordedPodCommand
	handler  func(recordedPodCommand) ([]byte, error)
}

type fakeInfrastructureFencer struct {
	mu       sync.Mutex
	refs     []sandboxruntime.RuntimeRef
	nodes    []string
	evidence sandboxruntime.TerminationEvidence
	err      error
}

type sharedFirstNetworkPolicyStore struct {
	mu          sync.Mutex
	name        string
	initialGets int
	release     chan struct{}
	policy      *networkingv1.NetworkPolicy
	deletes     int
}

func newSharedFirstNetworkPolicyStore(name string) *sharedFirstNetworkPolicyStore {
	return &sharedFirstNetworkPolicyStore{name: name, release: make(chan struct{})}
}

func (s *sharedFirstNetworkPolicyStore) react(action ktesting.Action) (bool, k8sruntime.Object, error) {
	if action.GetResource().Resource != "networkpolicies" {
		return false, nil, nil
	}
	switch typed := action.(type) {
	case ktesting.GetAction:
		if typed.GetName() != s.name {
			return false, nil, nil
		}
		s.mu.Lock()
		if s.policy != nil {
			result := s.policy.DeepCopy()
			s.mu.Unlock()
			return true, result, nil
		}
		s.initialGets++
		if s.initialGets == 2 {
			close(s.release)
		}
		release := s.release
		s.mu.Unlock()
		<-release
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}, s.name)
	case ktesting.CreateAction:
		candidate := typed.GetObject().(*networkingv1.NetworkPolicy).DeepCopy()
		if candidate.Name != s.name {
			return false, nil, nil
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.policy != nil {
			return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}, s.name)
		}
		candidate.UID = types.UID("shared-user-policy")
		candidate.ResourceVersion = "1"
		s.policy = candidate
		return true, candidate.DeepCopy(), nil
	case ktesting.DeleteAction:
		if typed.GetName() != s.name {
			return false, nil, nil
		}
		s.mu.Lock()
		s.deletes++
		s.policy = nil
		s.mu.Unlock()
		return true, nil, nil
	}
	return false, nil, nil
}

func (s *sharedFirstNetworkPolicyStore) snapshot() (*networkingv1.NetworkPolicy, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.policy.DeepCopy(), s.deletes
}

type sharedFirstCiliumPolicyStore struct {
	mu          sync.Mutex
	name        string
	initialGets int
	release     chan struct{}
	policy      *unstructured.Unstructured
	deletes     int
}

func newSharedFirstCiliumPolicyStore(name string) *sharedFirstCiliumPolicyStore {
	return &sharedFirstCiliumPolicyStore{name: name, release: make(chan struct{})}
}

func (s *sharedFirstCiliumPolicyStore) react(action ktesting.Action) (bool, k8sruntime.Object, error) {
	switch typed := action.(type) {
	case ktesting.GetAction:
		if typed.GetName() != s.name {
			return false, nil, nil
		}
		s.mu.Lock()
		if s.policy != nil {
			result := s.policy.DeepCopy()
			s.mu.Unlock()
			return true, result, nil
		}
		s.initialGets++
		if s.initialGets == 2 {
			close(s.release)
		}
		release := s.release
		s.mu.Unlock()
		<-release
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "cilium.io", Resource: "ciliumnetworkpolicies"}, s.name)
	case ktesting.CreateAction:
		candidate := typed.GetObject().(*unstructured.Unstructured).DeepCopy()
		if candidate.GetName() != s.name {
			return false, nil, nil
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.policy != nil {
			return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Group: "cilium.io", Resource: "ciliumnetworkpolicies"}, s.name)
		}
		candidate.SetUID(types.UID("shared-user-deny-policy"))
		candidate.SetResourceVersion("1")
		s.policy = candidate
		return true, candidate.DeepCopy(), nil
	case ktesting.DeleteAction:
		if typed.GetName() != s.name {
			return false, nil, nil
		}
		s.mu.Lock()
		s.deletes++
		s.policy = nil
		s.mu.Unlock()
		return true, nil, nil
	}
	return false, nil, nil
}

func (s *sharedFirstCiliumPolicyStore) snapshot() (*unstructured.Unstructured, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.policy.DeepCopy(), s.deletes
}

func (f *fakeInfrastructureFencer) FenceRuntime(_ context.Context, ref sandboxruntime.RuntimeRef, node string) (sandboxruntime.TerminationEvidence, error) {
	f.mu.Lock()
	f.refs = append(f.refs, ref)
	f.nodes = append(f.nodes, node)
	f.mu.Unlock()
	return f.evidence, f.err
}

func (s *commandScript) Exec(_ context.Context, pod, container string, argv []string, stdin []byte) ([]byte, error) {
	command := recordedPodCommand{pod: pod, container: container, argv: append([]string(nil), argv...), stdin: append([]byte(nil), stdin...)}
	s.mu.Lock()
	s.commands = append(s.commands, command)
	handler := s.handler
	s.mu.Unlock()
	return handler(command)
}

func newFakeKubernetesRuntime(t *testing.T, script *commandScript) (*Runtime, *kubefake.Clientset) {
	t.Helper()
	client := kubefake.NewSimpleClientset()
	client.PrependReactor("create", "networkpolicies", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
		policy := action.(ktesting.CreateAction).GetObject().(*networkingv1.NetworkPolicy)
		if policy.UID == "" {
			policy.UID = types.UID("network-policy-uid-a")
		}
		if policy.ResourceVersion == "" {
			policy.ResourceVersion = "1"
		}
		return false, nil, nil
	})
	client.PrependReactor("create", "pods", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
		create := action.(ktesting.CreateAction)
		pod := create.GetObject().(*corev1.Pod)
		pod.UID = types.UID("pod-uid-a")
		pod.ResourceVersion = "7"
		started := true
		pod.Status.Phase = corev1.PodRunning
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
			Name: workspaceMounterContainer, RestartCount: 0, Started: &started,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: sandboxContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}
		return false, nil, nil
	})
	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(), map[schema.GroupVersionResource]string{
		ciliumNetworkPolicyGVR: "CiliumNetworkPolicyList",
	})
	dynamicClient.PrependReactor("create", "ciliumnetworkpolicies", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
		policy := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured)
		if policy.GetUID() == "" {
			policy.SetUID(types.UID("cilium-policy-uid-a"))
		}
		if policy.GetResourceVersion() == "" {
			policy.SetResourceVersion("1")
		}
		return false, nil, nil
	})
	rt := &Runtime{
		client: client, dynClient: dynamicClient, namespace: "runtime", controlExecutor: script,
		pollInterval: time.Millisecond, prepareTimeout: 100 * time.Millisecond, readyTimeout: 100 * time.Millisecond,
		terminationTimeout: 100 * time.Millisecond,
		fuseCredentials:    sandboxruntime.FUSECredentials{AccessKey: []byte("do-not-persist-access"), SecretKey: []byte("do-not-persist-secret")},
	}
	return rt, client
}

func TestListSandboxesReturnsRuntimeLabels(t *testing.T) {
	rt, client := newFakeKubernetesRuntime(t, preparedScript())
	labels := map[string]string{
		"sandbox.id":             "sandbox-pool-fuse",
		"sandbox.pool":           "true",
		"sandbox.workspace.mode": "fuse",
	}
	_, err := client.CoreV1().Pods("runtime").Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-pool-fuse", Labels: labels},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	infos, err := rt.ListSandboxes(context.Background(), map[string]string{"sandbox.pool": "true"})
	require.NoError(t, err)
	require.Len(t, infos, 1)
	assert.Equal(t, labels, infos[0].Labels)
}

func setWorkspaceNodeForTest(t *testing.T, rt *Runtime, client *kubefake.Clientset, runtimeID, nodeName string) {
	t.Helper()
	pod, err := client.CoreV1().Pods("runtime").Get(context.Background(), runtimeID, metav1.GetOptions{})
	require.NoError(t, err)
	pod.Spec.NodeName = nodeName
	_, err = client.CoreV1().Pods("runtime").Update(context.Background(), pod, metav1.UpdateOptions{})
	require.NoError(t, err)
	rt.stateMu.Lock()
	state := rt.workspaceStates[runtimeID]
	require.NotNil(t, state)
	state.nodeName = nodeName
	rt.stateMu.Unlock()
}

func preparedScript() *commandScript {
	return &commandScript{handler: func(command recordedPodCommand) ([]byte, error) {
		switch fmt.Sprint(command.argv) {
		case fmt.Sprint([]string{mounterBinary, "health", "prepared"}):
			return []byte(`{"version":1,"state":"prepared","runtime_uid":"pod-uid-a","pool_key":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","mount_type":"","generation":0,"restart_detected":false,"cache_bytes":0,"cache_limit_bytes":2147483648,"cache_exceeded":false}`), nil
		case fmt.Sprint([]string{mounterBinary, "authorize"}):
			return []byte(`{"version":1,"accepted":true,"runtime_uid":"pod-uid-a","generation":7}`), nil
		case fmt.Sprint([]string{mounterBinary, "health", "ready"}):
			return []byte(`{"version":1,"state":"ready","runtime_uid":"pod-uid-a","pool_key":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","mount_type":"fuse","generation":7,"restart_detected":false,"cache_bytes":0,"cache_limit_bytes":2147483648,"cache_exceeded":false}`), nil
		case fmt.Sprint([]string{workspaceProbeBinary, "write-read-delete", "--runtime-uid", "pod-uid-a", "--generation", "7"}):
			return []byte(`{"version":1,"runtime_uid":"pod-uid-a","generation":7,"ok":true,"token":""}`), nil
		case fmt.Sprint([]string{workspaceProbeBinary, "quiesce", "--runtime-uid", "pod-uid-a", "--generation", "7"}):
			return []byte(`{"version":1,"runtime_uid":"pod-uid-a","generation":7,"ok":true,"token":"token-a"}`), nil
		case fmt.Sprint([]string{workspaceProbeBinary, "resume", "--runtime-uid", "pod-uid-a", "--generation", "7", "--token-stdin"}):
			return []byte(`{"version":1,"runtime_uid":"pod-uid-a","generation":7,"ok":true,"token":""}`), nil
		case fmt.Sprint([]string{mounterBinary, "shutdown"}):
			return []byte(`{"version":1,"runtime_uid":"pod-uid-a","generation":7,"graceful_unmount":true}`), nil
		case fmt.Sprint([]string{mounterBinary, "flush"}):
			return []byte(`{"version":1,"accepted":true,"runtime_uid":"pod-uid-a","generation":7}`), nil
		default:
			return nil, fmt.Errorf("unexpected command")
		}
	}}
}

func TestPrepareSandboxRetriesCiliumPolicyBindingConflict(t *testing.T) {
	rt, _ := newFakeKubernetesRuntime(t, preparedScript())
	rt.hasCilium = true
	dynamicClient := rt.dynClient.(*fake.FakeDynamicClient)
	var updates atomic.Int32
	dynamicClient.PrependReactor("update", "ciliumnetworkpolicies", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
		if updates.Add(1) == 1 {
			name := action.(ktesting.UpdateAction).GetObject().(*unstructured.Unstructured).GetName()
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "cilium.io", Resource: "ciliumnetworkpolicies"}, name, errors.New("controller updated resourceVersion"))
		}
		return false, nil, nil
	})

	spec := preparedFUSESpecForTest()
	spec.WorkspaceFUSE.EndpointHostIPs = nil
	spec.WorkspaceFUSE.SystemEgress = sandboxruntime.SystemEgressSpec{
		Mode: sandboxruntime.SystemEgressCiliumFQDN, DNSCIDRs: []string{"8.8.8.8/32"}, DNSPorts: []int32{53},
		EndpointFQDNs: []string{"minio.example.com"}, EndpointPorts: []int32{9000},
	}
	info, err := rt.PrepareSandbox(context.Background(), spec)
	require.NoError(t, err)
	require.NotNil(t, info)
	assert.Equal(t, int32(2), updates.Load())
}

func preparedOrphanCleanupScript() *commandScript {
	return &commandScript{handler: func(command recordedPodCommand) ([]byte, error) {
		switch fmt.Sprint(command.argv) {
		case fmt.Sprint([]string{mounterBinary, "health", "prepared"}):
			return []byte(`{"version":1,"state":"prepared","runtime_uid":"pod-uid-a","pool_key":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","mount_type":"","generation":0,"restart_detected":false,"cache_bytes":0,"cache_limit_bytes":2147483648,"cache_exceeded":false}`), nil
		case fmt.Sprint([]string{mounterBinary, "shutdown"}):
			return []byte(`{"version":1,"runtime_uid":"pod-uid-a","generation":0,"graceful_unmount":true}`), nil
		default:
			return nil, fmt.Errorf("unexpected command")
		}
	}}
}

func TestReconcileOrphanedResourcesRemovesOnlyUnprotectedManagedFUSEPods(t *testing.T) {
	rt, client := newFakeKubernetesRuntime(t, preparedOrphanCleanupScript())
	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)

	rt.stateMu.Lock()
	rt.workspaceStates = nil // simulate a new sandbox-api process
	rt.stateMu.Unlock()

	require.NoError(t, rt.ReconcileOrphanedResources(context.Background(), nil))
	_, podErr := client.CoreV1().Pods("runtime").Get(context.Background(), info.RuntimeID, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(podErr))
	_, policyErr := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseSystemPolicyPrefix+info.RuntimeID, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(policyErr))
}

func TestReconcileOrphanedResourcesRemovesCiliumPolicyAfterInterruptedPodCleanup(t *testing.T) {
	rt, client := newFakeKubernetesRuntime(t, preparedOrphanCleanupScript())
	rt.hasCilium = true
	spec := preparedFUSESpecForTest()
	spec.WorkspaceFUSE.Endpoint = "objects.example.com:443"
	spec.WorkspaceFUSE.EndpointHostIPs = nil
	spec.WorkspaceFUSE.SystemEgress.Mode = sandboxruntime.SystemEgressCiliumFQDN
	spec.WorkspaceFUSE.SystemEgress.EndpointCIDRs = nil
	spec.WorkspaceFUSE.SystemEgress.EndpointFQDNs = []string{"objects.example.com"}
	spec.WorkspaceFUSE.SystemEgress.EndpointPorts = []int32{443}
	info, err := rt.PrepareSandbox(context.Background(), spec)
	require.NoError(t, err)
	require.NoError(t, client.CoreV1().Pods("runtime").Delete(context.Background(), info.RuntimeID, metav1.DeleteOptions{}))
	rt.stateMu.Lock()
	rt.workspaceStates = nil
	rt.stateMu.Unlock()

	require.NoError(t, rt.ReconcileOrphanedResources(context.Background(), nil))
	_, err = rt.dynClient.Resource(ciliumNetworkPolicyGVR).Namespace("runtime").Get(context.Background(), fuseSystemPolicyPrefix+info.RuntimeID, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err))
}

func TestReconcileOrphanedResourcesProtectsRuntimeUIDAndFailsClosedOnInvalidIdentity(t *testing.T) {
	t.Run("protected", func(t *testing.T) {
		rt, client := newFakeKubernetesRuntime(t, preparedOrphanCleanupScript())
		info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
		require.NoError(t, err)

		require.NoError(t, rt.ReconcileOrphanedResources(context.Background(), map[string]struct{}{info.RuntimeUID: {}}))
		_, err = client.CoreV1().Pods("runtime").Get(context.Background(), info.RuntimeID, metav1.GetOptions{})
		require.NoError(t, err)
	})

	t.Run("invalid identity", func(t *testing.T) {
		rt, client := newFakeKubernetesRuntime(t, preparedOrphanCleanupScript())
		info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
		require.NoError(t, err)
		pod, err := client.CoreV1().Pods("runtime").Get(context.Background(), info.RuntimeID, metav1.GetOptions{})
		require.NoError(t, err)
		pod.Labels["sandbox.pool.instance"] = "replacement"
		_, err = client.CoreV1().Pods("runtime").Update(context.Background(), pod, metav1.UpdateOptions{})
		require.NoError(t, err)

		err = rt.ReconcileOrphanedResources(context.Background(), nil)
		require.ErrorIs(t, err, sandboxruntime.ErrTerminationUnconfirmed)
		_, getErr := client.CoreV1().Pods("runtime").Get(context.Background(), info.RuntimeID, metav1.GetOptions{})
		require.NoError(t, getErr)
	})
}

func TestPrepareSandboxCreatesSystemPolicyBeforePodAndPatchesPreparedWithResourceVersion(t *testing.T) {
	rt, client := newFakeKubernetesRuntime(t, preparedScript())
	spec := preparedFUSESpecForTest()

	info, err := rt.PrepareSandbox(context.Background(), spec)
	require.NoError(t, err)
	assert.Equal(t, "pod-uid-a", info.RuntimeUID)

	var first []string
	var patch []byte
	for _, action := range client.Actions() {
		if action.GetVerb() == "create" && (action.GetResource().Resource == "networkpolicies" || action.GetResource().Resource == "pods") {
			first = append(first, action.GetVerb()+"-"+action.GetResource().Resource)
		}
		if action.GetVerb() == "patch" && action.GetResource().Resource == "pods" {
			patch = action.(ktesting.PatchAction).GetPatch()
		}
	}
	assert.Equal(t, []string{"create-networkpolicies", "create-pods"}, first)
	assert.Contains(t, string(patch), `"resourceVersion":"7"`)
	assert.Contains(t, string(patch), `"sandbox.pool.state":"prepared"`)
}

func TestKubernetesPrepareDerivesEndpointPolicyAndHostAliases(t *testing.T) {
	rt, client := newFakeKubernetesRuntime(t, preparedScript())
	rt.clusterDNSLookup = func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("10.96.0.10")}, nil }
	rt.endpointLookup = func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("36.170.50.43")}, nil
	}

	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	pod, err := client.CoreV1().Pods("runtime").Get(context.Background(), info.RuntimeID, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, []corev1.HostAlias{{IP: "36.170.50.43", Hostnames: []string{"minio.example.com"}}}, pod.Spec.HostAliases)
	policy, err := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseSystemPolicyPrefix+info.RuntimeID, metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, policy.Spec.Egress, 2)
	assert.Equal(t, "10.96.0.10/32", policy.Spec.Egress[0].To[0].IPBlock.CIDR)
	assert.Equal(t, "36.170.50.43/32", policy.Spec.Egress[1].To[0].IPBlock.CIDR)
}

func TestCreateSandboxRejectsFUSEBeforeAnyMutation(t *testing.T) {
	rt, client := newFakeKubernetesRuntime(t, preparedScript())
	_, err := rt.CreateSandbox(context.Background(), preparedFUSESpecForTest())
	require.Error(t, err)
	assert.Empty(t, client.Actions())
}

func TestPrepareSandboxAcceptsOnlyVerifiedWriteAfterErrorTransitions(t *testing.T) {
	t.Run("system policy create", func(t *testing.T) {
		rt, client := newFakeKubernetesRuntime(t, preparedScript())
		var once sync.Once
		client.PrependReactor("create", "networkpolicies", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
			var resultErr error
			once.Do(func() {
				create := action.(ktesting.CreateAction)
				policy := create.GetObject().(*networkingv1.NetworkPolicy).DeepCopy()
				policy.UID = types.UID("network-policy-uid-a")
				policy.ResourceVersion = "1"
				resultErr = client.Tracker().Create(networkingv1.SchemeGroupVersion.WithResource("networkpolicies"), policy, "runtime")
			})
			if resultErr != nil {
				return true, nil, resultErr
			}
			return true, nil, errors.New("write-after-error")
		})
		info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
		require.NoError(t, err)
		assert.Equal(t, "pod-uid-a", info.RuntimeUID)
	})

	t.Run("system policy create cannot adopt another attempt", func(t *testing.T) {
		rt, client := newFakeKubernetesRuntime(t, preparedScript())
		client.PrependReactor("create", "networkpolicies", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
			policy := action.(ktesting.CreateAction).GetObject().(*networkingv1.NetworkPolicy).DeepCopy()
			policy.UID = types.UID("foreign-policy-uid")
			policy.Annotations[fusePrepareAttemptAnnotation] = "foreign-attempt"
			require.NoError(t, client.Tracker().Create(networkingv1.SchemeGroupVersion.WithResource("networkpolicies"), policy, "runtime"))
			return true, nil, errors.New("transport failed before this write")
		})

		_, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
		require.Error(t, err)
		retained, getErr := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseSystemPolicyPrefix+preparedFUSESpecForTest().ID, metav1.GetOptions{})
		require.NoError(t, getErr)
		assert.Equal(t, "foreign-attempt", retained.Annotations[fusePrepareAttemptAnnotation])
	})

	t.Run("pod create", func(t *testing.T) {
		rt, client := newFakeKubernetesRuntime(t, preparedScript())
		client.PrependReactor("create", "pods", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
			create := action.(ktesting.CreateAction)
			pod := create.GetObject().(*corev1.Pod).DeepCopy()
			pod.UID = types.UID("pod-uid-a")
			pod.ResourceVersion = "7"
			started := true
			pod.Status.Phase = corev1.PodRunning
			pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: workspaceMounterContainer, Started: &started, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: sandboxContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
			require.NoError(t, client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), pod, "runtime"))
			return true, nil, errors.New("write-after-error")
		})
		info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
		require.NoError(t, err)
		assert.Equal(t, "pod-uid-a", info.RuntimeUID)
	})

	t.Run("pod create incompatible readback", func(t *testing.T) {
		rt, client := newFakeKubernetesRuntime(t, preparedScript())
		client.PrependReactor("create", "pods", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
			create := action.(ktesting.CreateAction)
			pod := create.GetObject().(*corev1.Pod).DeepCopy()
			pod.UID = types.UID("pod-uid-a")
			pod.ResourceVersion = "7"
			pod.Spec.Containers[0].Image = "untrusted/replacement:latest"
			pod.Annotations[fusePrepareAttemptAnnotation] = "foreign-attempt"
			require.NoError(t, client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), pod, "runtime"))
			return true, nil, errors.New("write-after-error")
		})

		_, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
		require.Error(t, err)
		_, policyErr := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseSystemPolicyPrefix+preparedFUSESpecForTest().ID, metav1.GetOptions{})
		require.True(t, apierrors.IsNotFound(policyErr), "incompatible Pod must not inherit this operation's system egress")
		foreign, getErr := client.CoreV1().Pods("runtime").Get(context.Background(), preparedFUSESpecForTest().ID, metav1.GetOptions{})
		require.NoError(t, getErr)
		assert.Equal(t, "untrusted/replacement:latest", foreign.Spec.Containers[0].Image)
	})

	t.Run("pod create own mutated readback", func(t *testing.T) {
		rt, client := newFakeKubernetesRuntime(t, preparedScript())
		client.PrependReactor("create", "pods", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
			pod := action.(ktesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
			pod.UID = types.UID("pod-uid-a")
			pod.ResourceVersion = "7"
			pod.Spec.HostNetwork = true
			require.NoError(t, client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), pod, "runtime"))
			return true, nil, errors.New("write-after-error")
		})

		_, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
		require.Error(t, err)
		_, podErr := client.CoreV1().Pods("runtime").Get(context.Background(), preparedFUSESpecForTest().ID, metav1.GetOptions{})
		require.True(t, apierrors.IsNotFound(podErr))
		_, policyErr := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseSystemPolicyPrefix+preparedFUSESpecForTest().ID, metav1.GetOptions{})
		require.True(t, apierrors.IsNotFound(policyErr))
	})

	t.Run("system policy UID bind", func(t *testing.T) {
		rt, client := newFakeKubernetesRuntime(t, preparedScript())
		client.PrependReactor("update", "networkpolicies", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
			update := action.(ktesting.UpdateAction)
			require.NoError(t, client.Tracker().Update(networkingv1.SchemeGroupVersion.WithResource("networkpolicies"), update.GetObject(), "runtime"))
			return true, nil, errors.New("write-after-error")
		})
		info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
		require.NoError(t, err)
		policy, err := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseSystemPolicyPrefix+info.RuntimeID, metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, info.RuntimeUID, policy.Annotations[fuseRuntimeUIDAnnotation])
	})

	t.Run("prepared patch", func(t *testing.T) {
		rt, client := newFakeKubernetesRuntime(t, preparedScript())
		client.PrependReactor("patch", "pods", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
			pod, getErr := client.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), "runtime", action.(ktesting.PatchAction).GetName())
			require.NoError(t, getErr)
			updated := pod.(*corev1.Pod).DeepCopy()
			updated.Labels["sandbox.pool.state"] = "prepared"
			require.NoError(t, client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), updated, "runtime"))
			return true, nil, errors.New("write-after-error")
		})
		info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
		require.NoError(t, err)
		assert.Equal(t, "pod-uid-a", info.RuntimeUID)
	})
}

func TestPrepareSandboxRejectsSuccessfulMutatedResources(t *testing.T) {
	t.Run("system policy create", func(t *testing.T) {
		rt, client := newFakeKubernetesRuntime(t, preparedScript())
		client.PrependReactor("create", "networkpolicies", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
			created := action.(ktesting.CreateAction).GetObject().(*networkingv1.NetworkPolicy).DeepCopy()
			created.UID = types.UID("policy-uid-a")
			created.Spec.PodSelector.MatchLabels["sandbox.pool.instance"] = "mutated-instance"
			require.NoError(t, client.Tracker().Create(networkingv1.SchemeGroupVersion.WithResource("networkpolicies"), created, "runtime"))
			return true, created, nil
		})

		_, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
		require.Error(t, err)
		for _, action := range client.Actions() {
			assert.False(t, action.GetVerb() == "create" && action.GetResource().Resource == "pods")
		}
		_, policyErr := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseSystemPolicyPrefix+preparedFUSESpecForTest().ID, metav1.GetOptions{})
		require.True(t, apierrors.IsNotFound(policyErr))
	})

	t.Run("pod create", func(t *testing.T) {
		rt, client := newFakeKubernetesRuntime(t, preparedScript())
		client.PrependReactor("create", "pods", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
			created := action.(ktesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
			created.UID = types.UID("pod-uid-a")
			created.ResourceVersion = "7"
			created.Spec.HostNetwork = true
			created.Annotations[fusePrepareAttemptAnnotation] = "admission-mutated-attempt"
			require.NoError(t, client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), created, "runtime"))
			return true, created, nil
		})

		_, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
		require.Error(t, err)
		_, podErr := client.CoreV1().Pods("runtime").Get(context.Background(), preparedFUSESpecForTest().ID, metav1.GetOptions{})
		require.True(t, apierrors.IsNotFound(podErr))
		_, policyErr := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseSystemPolicyPrefix+preparedFUSESpecForTest().ID, metav1.GetOptions{})
		require.True(t, apierrors.IsNotFound(policyErr))
	})

	t.Run("system policy UID bind", func(t *testing.T) {
		rt, client := newFakeKubernetesRuntime(t, preparedScript())
		client.PrependReactor("update", "networkpolicies", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
			updated := action.(ktesting.UpdateAction).GetObject().(*networkingv1.NetworkPolicy).DeepCopy()
			updated.Annotations[fuseRuntimeUIDAnnotation] = "mutated-uid"
			require.NoError(t, client.Tracker().Update(networkingv1.SchemeGroupVersion.WithResource("networkpolicies"), updated, "runtime"))
			return true, updated, nil
		})

		_, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
		require.Error(t, err)
		_, podErr := client.CoreV1().Pods("runtime").Get(context.Background(), preparedFUSESpecForTest().ID, metav1.GetOptions{})
		require.True(t, apierrors.IsNotFound(podErr))
		_, policyErr := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseSystemPolicyPrefix+preparedFUSESpecForTest().ID, metav1.GetOptions{})
		require.True(t, apierrors.IsNotFound(policyErr))
	})

	t.Run("system policy intent drift before UID bind", func(t *testing.T) {
		rt, client := newFakeKubernetesRuntime(t, preparedScript())
		client.PrependReactor("create", "pods", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
			policy, err := client.Tracker().Get(networkingv1.SchemeGroupVersion.WithResource("networkpolicies"), "runtime", fuseSystemPolicyPrefix+preparedFUSESpecForTest().ID)
			require.NoError(t, err)
			mutated := policy.(*networkingv1.NetworkPolicy).DeepCopy()
			mutated.Spec.Egress = append(mutated.Spec.Egress, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}}}})
			require.NoError(t, client.Tracker().Update(networkingv1.SchemeGroupVersion.WithResource("networkpolicies"), mutated, "runtime"))
			return false, nil, nil
		})

		_, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
		require.Error(t, err)
		_, podErr := client.CoreV1().Pods("runtime").Get(context.Background(), preparedFUSESpecForTest().ID, metav1.GetOptions{})
		require.True(t, apierrors.IsNotFound(podErr))
		_, policyErr := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseSystemPolicyPrefix+preparedFUSESpecForTest().ID, metav1.GetOptions{})
		require.True(t, apierrors.IsNotFound(policyErr))
	})
}

func TestConcurrentFirstStandardFUSENetworkUpdatesNeverDeleteWinner(t *testing.T) {
	rtA, client := newFakeKubernetesRuntime(t, preparedScript())
	info, err := rtA.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	pod, err := client.CoreV1().Pods("runtime").Get(context.Background(), ref.ID, metav1.GetOptions{})
	require.NoError(t, err)
	clientB := kubefake.NewSimpleClientset(pod.DeepCopy())
	rtB := &Runtime{client: clientB, namespace: "runtime", controlExecutor: preparedScript()}
	shared := newSharedFirstNetworkPolicyStore(fuseUserPolicyPrefix + ref.ID)
	client.PrependReactor("*", "networkpolicies", shared.react)
	clientB.PrependReactor("*", "networkpolicies", shared.react)

	errs := make(chan error, 2)
	go func() { errs <- rtA.UpdateFUSENetwork(context.Background(), ref, false, nil, false) }()
	go func() { errs <- rtB.UpdateFUSENetwork(context.Background(), ref, true, []string{"8.8.4.4/32"}, false) }()
	first, second := <-errs, <-errs
	assert.Equal(t, 1, boolCount(first == nil, second == nil))
	if first != nil {
		assert.ErrorIs(t, first, sandboxruntime.ErrFUSENetworkStateUncertain)
	}
	if second != nil {
		assert.ErrorIs(t, second, sandboxruntime.ErrFUSENetworkStateUncertain)
	}
	winner, deletes := shared.snapshot()
	require.NotNil(t, winner)
	assert.Zero(t, deletes, "an AlreadyExists loser must not delete the winner")
	assert.NotEmpty(t, winner.Annotations[fuseNetworkAttemptAnnotation])
}

func TestConcurrentFirstCiliumFUSENetworkUpdatesNeverDeleteWinner(t *testing.T) {
	rtA, client := newFakeKubernetesRuntime(t, preparedScript())
	rtA.hasCilium = true
	info, err := rtA.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	pod, err := client.CoreV1().Pods("runtime").Get(context.Background(), ref.ID, metav1.GetOptions{})
	require.NoError(t, err)
	system, err := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseSystemPolicyPrefix+ref.ID, metav1.GetOptions{})
	require.NoError(t, err)
	clientB := kubefake.NewSimpleClientset(pod.DeepCopy(), system.DeepCopy())
	dynamicClientA := rtA.dynClient.(*fake.FakeDynamicClient)
	dynamicClientB := fake.NewSimpleDynamicClient(k8sruntime.NewScheme())
	rtB := &Runtime{client: clientB, dynClient: dynamicClientB, namespace: "runtime", hasCilium: true, controlExecutor: preparedScript()}
	sharedDeny := newSharedFirstCiliumPolicyStore(fuseUserDenyPolicyPrefix + ref.ID)
	dynamicClientA.PrependReactor("*", "ciliumnetworkpolicies", sharedDeny.react)
	dynamicClientB.PrependReactor("*", "ciliumnetworkpolicies", sharedDeny.react)
	sharedAllow := newSharedFirstNetworkPolicyStore(fuseUserPolicyPrefix + ref.ID)
	// Only the winning deny-policy creator advances to the allow policy, so its
	// first read must not wait for a second participant.
	close(sharedAllow.release)
	sharedAllow.initialGets = 2
	client.PrependReactor("*", "networkpolicies", sharedAllow.react)
	clientB.PrependReactor("*", "networkpolicies", sharedAllow.react)

	errs := make(chan error, 2)
	go func() { errs <- rtA.UpdateFUSENetwork(context.Background(), ref, true, nil, false) }()
	go func() {
		errs <- rtB.UpdateFUSENetwork(context.Background(), ref, true, []string{"10.20.0.10/32"}, true)
	}()
	first, second := <-errs, <-errs
	assert.Equal(t, 1, boolCount(first == nil, second == nil))
	if first != nil {
		assert.ErrorIs(t, first, sandboxruntime.ErrFUSENetworkStateUncertain)
	}
	if second != nil {
		assert.ErrorIs(t, second, sandboxruntime.ErrFUSENetworkStateUncertain)
	}
	deny, denyDeletes := sharedDeny.snapshot()
	require.NotNil(t, deny)
	assert.Zero(t, denyDeletes, "an AlreadyExists loser must not delete the winner deny policy")
	allow, allowDeletes := sharedAllow.snapshot()
	require.NotNil(t, allow)
	assert.Zero(t, allowDeletes, "the loser must not delete the winner allow policy")
	assert.NotEmpty(t, deny.GetAnnotations()[fuseNetworkAttemptAnnotation])
	assert.Equal(t, deny.GetAnnotations()[fuseNetworkAttemptAnnotation], allow.Annotations[fuseNetworkAttemptAnnotation])
}

func boolCount(values ...bool) int {
	count := 0
	for _, value := range values {
		if value {
			count++
		}
	}
	return count
}

func TestPrepareSandboxBindsCiliumSystemPolicyToExactPodUID(t *testing.T) {
	rt, _ := newFakeKubernetesRuntime(t, preparedScript())
	rt.hasCilium = true
	spec := preparedFUSESpecForTest()
	spec.WorkspaceFUSE.Endpoint = "objects.example.com:443"
	spec.WorkspaceFUSE.UseSSL = true
	spec.WorkspaceFUSE.EndpointHostIPs = nil
	spec.WorkspaceFUSE.SystemEgress.Mode = sandboxruntime.SystemEgressCiliumFQDN
	spec.WorkspaceFUSE.SystemEgress.EndpointCIDRs = nil
	spec.WorkspaceFUSE.SystemEgress.EndpointFQDNs = []string{"objects.example.com"}
	spec.WorkspaceFUSE.SystemEgress.EndpointPorts = []int32{443}

	info, err := rt.PrepareSandbox(context.Background(), spec)
	require.NoError(t, err)
	policy, err := rt.dynClient.Resource(ciliumNetworkPolicyGVR).Namespace("runtime").Get(context.Background(), fuseSystemPolicyPrefix+info.RuntimeID, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, info.RuntimeUID, policy.GetAnnotations()[fuseRuntimeUIDAnnotation])
}

func TestPrepareSandboxRejectsMutatedSuccessfulCiliumPolicyBind(t *testing.T) {
	rt, client := newFakeKubernetesRuntime(t, preparedScript())
	rt.hasCilium = true
	spec := preparedFUSESpecForTest()
	spec.WorkspaceFUSE.Endpoint = "objects.example.com:443"
	spec.WorkspaceFUSE.EndpointHostIPs = nil
	spec.WorkspaceFUSE.SystemEgress.Mode = sandboxruntime.SystemEgressCiliumFQDN
	spec.WorkspaceFUSE.SystemEgress.EndpointCIDRs = nil
	spec.WorkspaceFUSE.SystemEgress.EndpointFQDNs = []string{"objects.example.com"}
	spec.WorkspaceFUSE.SystemEgress.EndpointPorts = []int32{443}
	dynamicClient := rt.dynClient.(*fake.FakeDynamicClient)
	dynamicClient.PrependReactor("update", "ciliumnetworkpolicies", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
		updated := action.(ktesting.UpdateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		annotations := updated.GetAnnotations()
		annotations[fuseRuntimeUIDAnnotation] = "mutated-uid"
		updated.SetAnnotations(annotations)
		require.NoError(t, dynamicClient.Tracker().Update(ciliumNetworkPolicyGVR, updated, "runtime"))
		return true, updated, nil
	})

	_, err := rt.PrepareSandbox(context.Background(), spec)
	require.Error(t, err)
	_, podErr := client.CoreV1().Pods("runtime").Get(context.Background(), spec.ID, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(podErr))
	_, policyErr := dynamicClient.Resource(ciliumNetworkPolicyGVR).Namespace("runtime").Get(context.Background(), fuseSystemPolicyPrefix+spec.ID, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(policyErr))
}

func TestPrepareSandboxCompensatesOnlyAfterPodCreateIsDefinitelyAbsent(t *testing.T) {
	t.Run("definitely absent deletes policy", func(t *testing.T) {
		rt, client := newFakeKubernetesRuntime(t, preparedScript())
		client.PrependReactor("create", "pods", func(ktesting.Action) (bool, k8sruntime.Object, error) {
			return true, nil, errors.New("create rejected")
		})

		_, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
		require.Error(t, err)
		_, policyErr := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseSystemPolicyPrefix+preparedFUSESpecForTest().ID, metav1.GetOptions{})
		require.True(t, apierrors.IsNotFound(policyErr))
	})

	t.Run("unknown readback retains policy", func(t *testing.T) {
		rt, client := newFakeKubernetesRuntime(t, preparedScript())
		client.PrependReactor("create", "pods", func(ktesting.Action) (bool, k8sruntime.Object, error) {
			return true, nil, errors.New("create response lost")
		})
		client.PrependReactor("get", "pods", func(ktesting.Action) (bool, k8sruntime.Object, error) {
			return true, nil, errors.New("readback unavailable")
		})

		_, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
		require.Error(t, err)
		_, policyErr := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseSystemPolicyPrefix+preparedFUSESpecForTest().ID, metav1.GetOptions{})
		require.NoError(t, policyErr)
	})
}

func TestPrepareSandboxVerifiesWriteAfterErrorDuringCompensation(t *testing.T) {
	rt, client := newFakeKubernetesRuntime(t, preparedScript())
	client.PrependReactor("patch", "pods", func(ktesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, errors.New("patch rejected")
	})
	client.PrependReactor("delete", "pods", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
		err := client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), "runtime", action.(ktesting.DeleteAction).GetName())
		require.NoError(t, err)
		return true, nil, errors.New("delete response lost")
	})

	_, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.Error(t, err)
	_, policyErr := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseSystemPolicyPrefix+preparedFUSESpecForTest().ID, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(policyErr))
}

func TestAuthorizeWorkspaceMountRequiresExactUIDAndIsOneShot(t *testing.T) {
	script := preparedScript()
	rt, _ := newFakeKubernetesRuntime(t, script)
	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	auth := sandboxruntime.WorkspaceMountAuthorization{
		RuntimeUID: info.RuntimeUID, PoolKey: preparedFUSESpecForTest().WorkspaceFUSE.PoolKey,
		WorkspaceHash: "workspace-hash", Prefix: "workspaces/team/", LeaseGeneration: 7, MountAttempt: 1,
	}
	require.ErrorIs(t, rt.AuthorizeWorkspaceMount(context.Background(), sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: "wrong"}, auth), sandboxruntime.ErrInvalidRuntimeRef)
	require.NoError(t, rt.AuthorizeWorkspaceMount(context.Background(), sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}, auth))
	require.Error(t, rt.AuthorizeWorkspaceMount(context.Background(), sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}, auth))

	count := 0
	for _, command := range script.commands {
		if fmt.Sprint(command.argv) == fmt.Sprint([]string{mounterBinary, "authorize"}) {
			count++
			var request authorizeRequestWire
			require.NoError(t, json.Unmarshal(command.stdin, &request))
			assert.Equal(t, info.RuntimeUID, request.RuntimeUID)
			assert.Equal(t, []byte("do-not-persist-access"), request.Credentials.AccessKey)
			assert.Equal(t, []byte("do-not-persist-secret"), request.Credentials.SecretKey)
		}
	}
	assert.Equal(t, 1, count)
}

func TestAuthorizeResponseLossRetainsAttemptedGenerationForRemoval(t *testing.T) {
	base := preparedScript()
	var lost atomic.Bool
	script := &commandScript{handler: func(command recordedPodCommand) ([]byte, error) {
		if fmt.Sprint(command.argv) == fmt.Sprint([]string{mounterBinary, "authorize"}) && !lost.Swap(true) {
			return nil, errors.New("authorize response lost")
		}
		return base.handler(command)
	}}
	rt, _ := newFakeKubernetesRuntime(t, script)
	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	auth := sandboxruntime.WorkspaceMountAuthorization{RuntimeUID: ref.UID, PoolKey: preparedFUSESpecForTest().WorkspaceFUSE.PoolKey, WorkspaceHash: "hash", Prefix: "p/", LeaseGeneration: 7, MountAttempt: 1}
	require.Error(t, rt.AuthorizeWorkspaceMount(context.Background(), ref, auth))
	_, err = rt.WaitSandboxReady(context.Background(), ref, 7)
	require.Error(t, err, "an attempted generation must not be treated as authorized")
	require.NoError(t, rt.RemovePreparedSandbox(context.Background(), ref.ID, ref.UID))

	var shutdown *recordedPodCommand
	script.mu.Lock()
	for index := range script.commands {
		if fmt.Sprint(script.commands[index].argv) == fmt.Sprint([]string{mounterBinary, "shutdown"}) {
			command := script.commands[index]
			shutdown = &command
		}
	}
	script.mu.Unlock()
	require.NotNil(t, shutdown)
	var request controlRequestWire
	require.NoError(t, json.Unmarshal(shutdown.stdin, &request))
	assert.Equal(t, int64(7), request.Generation)
}

func TestWaitReadyChecksGenerationAndRunsFixedExactUIDProbe(t *testing.T) {
	script := preparedScript()
	rt, client := newFakeKubernetesRuntime(t, script)
	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	auth := sandboxruntime.WorkspaceMountAuthorization{RuntimeUID: info.RuntimeUID, PoolKey: preparedFUSESpecForTest().WorkspaceFUSE.PoolKey, WorkspaceHash: "hash", Prefix: "p/", LeaseGeneration: 7, MountAttempt: 1}
	require.NoError(t, rt.AuthorizeWorkspaceMount(context.Background(), ref, auth))
	pod, err := client.CoreV1().Pods("runtime").Get(context.Background(), ref.ID, metav1.GetOptions{})
	require.NoError(t, err)
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	_, err = client.CoreV1().Pods("runtime").UpdateStatus(context.Background(), pod, metav1.UpdateOptions{})
	require.NoError(t, err)

	ready, err := rt.WaitSandboxReady(context.Background(), ref, 7)
	require.NoError(t, err)
	assert.Equal(t, ref.UID, ready.RuntimeUID)
	last := script.commands[len(script.commands)-1]
	assert.Equal(t, sandboxContainer, last.container)
	assert.Equal(t, []string{workspaceProbeBinary, "write-read-delete", "--runtime-uid", ref.UID, "--generation", "7"}, last.argv)
}

func TestQuiesceResumeTokenIsExactAndSingleUse(t *testing.T) {
	script := preparedScript()
	rt, client := newFakeKubernetesRuntime(t, script)
	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	auth := sandboxruntime.WorkspaceMountAuthorization{RuntimeUID: ref.UID, PoolKey: preparedFUSESpecForTest().WorkspaceFUSE.PoolKey, WorkspaceHash: "hash", Prefix: "p/", LeaseGeneration: 7, MountAttempt: 1}
	require.NoError(t, rt.AuthorizeWorkspaceMount(context.Background(), ref, auth))
	pod, _ := client.CoreV1().Pods("runtime").Get(context.Background(), ref.ID, metav1.GetOptions{})
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	_, _ = client.CoreV1().Pods("runtime").UpdateStatus(context.Background(), pod, metav1.UpdateOptions{})

	token, err := rt.QuiesceWorkspace(context.Background(), ref, 7)
	require.NoError(t, err)
	wrong := token
	wrong.Generation++
	require.Error(t, rt.ResumeWorkspace(context.Background(), ref, wrong))
	require.NoError(t, rt.ResumeWorkspace(context.Background(), ref, token))
	require.Error(t, rt.ResumeWorkspace(context.Background(), ref, token))
}

func TestWorkspaceOperationsRequireExpectedGeneration(t *testing.T) {
	script := preparedScript()
	rt, client := newFakeKubernetesRuntime(t, script)
	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	auth := sandboxruntime.WorkspaceMountAuthorization{RuntimeUID: ref.UID, PoolKey: preparedFUSESpecForTest().WorkspaceFUSE.PoolKey, WorkspaceHash: "hash", Prefix: "p/", LeaseGeneration: 7, MountAttempt: 1}
	require.NoError(t, rt.AuthorizeWorkspaceMount(context.Background(), ref, auth))
	pod, err := client.CoreV1().Pods("runtime").Get(context.Background(), ref.ID, metav1.GetOptions{})
	require.NoError(t, err)
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	_, err = client.CoreV1().Pods("runtime").UpdateStatus(context.Background(), pod, metav1.UpdateOptions{})
	require.NoError(t, err)

	require.Error(t, func() error { _, err := rt.WaitSandboxReady(context.Background(), ref, 8); return err }())
	require.Error(t, func() error { _, err := rt.QuiesceWorkspace(context.Background(), ref, 0); return err }())
	require.Error(t, func() error { _, err := rt.QuiesceWorkspace(context.Background(), ref, 8); return err }())
	require.Error(t, rt.FlushWorkspace(context.Background(), ref, 7), "flush requires a live quiesce token")
	token, err := rt.QuiesceWorkspace(context.Background(), ref, 7)
	require.NoError(t, err)
	require.Error(t, rt.FlushWorkspace(context.Background(), ref, 8))
	require.NoError(t, rt.FlushWorkspace(context.Background(), ref, token.Generation))
}

func TestQuiesceConcurrentCallsDispatchOnlyOnce(t *testing.T) {
	base := preparedScript()
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	script := &commandScript{handler: func(command recordedPodCommand) ([]byte, error) {
		if fmt.Sprint(command.argv) == fmt.Sprint([]string{workspaceProbeBinary, "quiesce", "--runtime-uid", "pod-uid-a", "--generation", "7"}) {
			if calls.Add(1) == 1 {
				close(entered)
			}
			<-release
		}
		return base.handler(command)
	}}
	rt, _ := newFakeKubernetesRuntime(t, script)
	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	auth := sandboxruntime.WorkspaceMountAuthorization{RuntimeUID: ref.UID, PoolKey: preparedFUSESpecForTest().WorkspaceFUSE.PoolKey, WorkspaceHash: "hash", Prefix: "p/", LeaseGeneration: 7, MountAttempt: 1}
	require.NoError(t, rt.AuthorizeWorkspaceMount(context.Background(), ref, auth))

	first := make(chan error, 1)
	go func() { _, callErr := rt.QuiesceWorkspace(context.Background(), ref, 7); first <- callErr }()
	<-entered
	_, secondErr := rt.QuiesceWorkspace(context.Background(), ref, 7)
	require.Error(t, secondErr)
	close(release)
	require.NoError(t, <-first)
	assert.Equal(t, int32(1), calls.Load())
}

func TestAmbiguousQuiesceOrResumePoisonsCycle(t *testing.T) {
	for _, operation := range []string{"quiesce", "resume"} {
		t.Run(operation, func(t *testing.T) {
			base := preparedScript()
			var failed atomic.Bool
			script := &commandScript{handler: func(command recordedPodCommand) ([]byte, error) {
				if len(command.argv) > 1 && command.argv[1] == operation && !failed.Swap(true) {
					return nil, errors.New("response lost")
				}
				return base.handler(command)
			}}
			rt, _ := newFakeKubernetesRuntime(t, script)
			info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
			require.NoError(t, err)
			ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
			auth := sandboxruntime.WorkspaceMountAuthorization{RuntimeUID: ref.UID, PoolKey: preparedFUSESpecForTest().WorkspaceFUSE.PoolKey, WorkspaceHash: "hash", Prefix: "p/", LeaseGeneration: 7, MountAttempt: 1}
			require.NoError(t, rt.AuthorizeWorkspaceMount(context.Background(), ref, auth))
			if operation == "quiesce" {
				_, err = rt.QuiesceWorkspace(context.Background(), ref, 7)
			} else {
				token, tokenErr := rt.QuiesceWorkspace(context.Background(), ref, 7)
				require.NoError(t, tokenErr)
				err = rt.ResumeWorkspace(context.Background(), ref, token)
			}
			require.Error(t, err)
			_, err = rt.QuiesceWorkspace(context.Background(), ref, 7)
			require.Error(t, err)
		})
	}
}

func TestRemovePreparedSandboxRequiresProofBeforePolicyDeletion(t *testing.T) {
	script := preparedScript()
	rt, client := newFakeKubernetesRuntime(t, script)
	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	auth := sandboxruntime.WorkspaceMountAuthorization{RuntimeUID: ref.UID, PoolKey: preparedFUSESpecForTest().WorkspaceFUSE.PoolKey, WorkspaceHash: "hash", Prefix: "p/", LeaseGeneration: 7, MountAttempt: 1}
	require.NoError(t, rt.AuthorizeWorkspaceMount(context.Background(), ref, auth))

	var events []string
	client.PrependReactor("delete", "pods", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
		deleteAction := action.(ktesting.DeleteAction)
		require.NotNil(t, deleteAction.GetDeleteOptions().Preconditions)
		require.Equal(t, types.UID(ref.UID), *deleteAction.GetDeleteOptions().Preconditions.UID)
		events = append(events, "delete-pod")
		return false, nil, nil
	})
	client.PrependReactor("delete", "networkpolicies", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
		events = append(events, "delete-policy")
		return false, nil, nil
	})

	require.NoError(t, rt.RemovePreparedSandbox(context.Background(), ref.ID, ref.UID))
	evidence, err := rt.ConfirmTerminated(context.Background(), ref.ID, ref.UID)
	require.NoError(t, err)
	assert.Equal(t, ref.UID, evidence.RuntimeUID)
	assert.True(t, evidence.GracefulUnmount)
	assert.True(t, evidence.ProcessExited)
	require.NotEmpty(t, events)
	assert.Equal(t, "delete-pod", events[0])
	assert.Equal(t, "delete-policy", events[len(events)-1])
}

func TestRemovePreparedSandboxNotFoundWithoutPriorProofFailsClosed(t *testing.T) {
	rt, client := newFakeKubernetesRuntime(t, preparedScript())
	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	require.NoError(t, client.CoreV1().Pods("runtime").Delete(context.Background(), ref.ID, metav1.DeleteOptions{}))

	err = rt.RemovePreparedSandbox(context.Background(), ref.ID, ref.UID)
	require.ErrorIs(t, err, sandboxruntime.ErrTerminationUnconfirmed)
	_, policyErr := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseSystemPolicyPrefix+ref.ID, metav1.GetOptions{})
	require.NoError(t, policyErr)
	_, err = rt.ConfirmTerminated(context.Background(), ref.ID, ref.UID)
	require.ErrorIs(t, err, sandboxruntime.ErrTerminationUnconfirmed)
}

func TestRemovePreparedSandboxUsesOnlyMatchingInfrastructureFence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		evidence sandboxruntime.TerminationEvidence
		wantErr  bool
	}{
		{name: "matching", evidence: sandboxruntime.TerminationEvidence{RuntimeUID: "pod-uid-a", NodeName: "node-a", InfrastructureFenced: true}},
		{name: "wrong UID", evidence: sandboxruntime.TerminationEvidence{RuntimeUID: "other", NodeName: "node-a", InfrastructureFenced: true}, wantErr: true},
		{name: "wrong node", evidence: sandboxruntime.TerminationEvidence{RuntimeUID: "pod-uid-a", NodeName: "node-b", InfrastructureFenced: true}, wantErr: true},
		{name: "no proof bit", evidence: sandboxruntime.TerminationEvidence{RuntimeUID: "pod-uid-a"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, client := newFakeKubernetesRuntime(t, preparedScript())
			info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
			require.NoError(t, err)
			ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
			setWorkspaceNodeForTest(t, rt, client, ref.ID, "node-a")
			require.NoError(t, client.CoreV1().Pods("runtime").Delete(context.Background(), ref.ID, metav1.DeleteOptions{}))
			fencer := &fakeInfrastructureFencer{evidence: tc.evidence}
			rt.infraFencer = fencer

			err = rt.RemovePreparedSandbox(context.Background(), ref.ID, ref.UID)
			if tc.wantErr {
				require.ErrorIs(t, err, sandboxruntime.ErrTerminationUnconfirmed)
				_, policyErr := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseSystemPolicyPrefix+ref.ID, metav1.GetOptions{})
				require.NoError(t, policyErr)
				return
			}
			require.NoError(t, err)
			evidence, err := rt.ConfirmTerminated(context.Background(), ref.ID, ref.UID)
			require.NoError(t, err)
			assert.True(t, evidence.InfrastructureFenced)
		})
	}
}

func TestRemovePreparedSandboxDeleteWriteAfterErrorRequiresFencer(t *testing.T) {
	rt, client := newFakeKubernetesRuntime(t, preparedScript())
	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	auth := sandboxruntime.WorkspaceMountAuthorization{RuntimeUID: ref.UID, PoolKey: preparedFUSESpecForTest().WorkspaceFUSE.PoolKey, WorkspaceHash: "hash", Prefix: "p/", LeaseGeneration: 7, MountAttempt: 1}
	require.NoError(t, rt.AuthorizeWorkspaceMount(context.Background(), ref, auth))
	client.PrependReactor("delete", "pods", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
		err := client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), "runtime", action.(ktesting.DeleteAction).GetName())
		require.NoError(t, err)
		return true, nil, errors.New("delete response lost")
	})

	err = rt.RemovePreparedSandbox(context.Background(), ref.ID, ref.UID)
	require.ErrorIs(t, err, sandboxruntime.ErrTerminationUnconfirmed)
	_, policyErr := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseSystemPolicyPrefix+ref.ID, metav1.GetOptions{})
	require.NoError(t, policyErr)
	_, err = rt.ConfirmTerminated(context.Background(), ref.ID, ref.UID)
	require.ErrorIs(t, err, sandboxruntime.ErrTerminationUnconfirmed)
}

func TestRemovePreparedSandboxNameReplacementRequiresFencerAndPreservesNewPolicy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		withFencer bool
	}{
		{name: "without fencer"},
		{name: "with fencer", withFencer: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, client := newFakeKubernetesRuntime(t, preparedScript())
			info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
			require.NoError(t, err)
			ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
			setWorkspaceNodeForTest(t, rt, client, ref.ID, "node-a")
			auth := sandboxruntime.WorkspaceMountAuthorization{RuntimeUID: ref.UID, PoolKey: preparedFUSESpecForTest().WorkspaceFUSE.PoolKey, WorkspaceHash: "hash", Prefix: "p/", LeaseGeneration: 7, MountAttempt: 1}
			require.NoError(t, rt.AuthorizeWorkspaceMount(context.Background(), ref, auth))
			client.PrependReactor("delete", "pods", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
				name := action.(ktesting.DeleteAction).GetName()
				current, getErr := client.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), "runtime", name)
				require.NoError(t, getErr)
				replacement := current.(*corev1.Pod).DeepCopy()
				replacement.UID = types.UID("replacement-uid")
				require.NoError(t, client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), "runtime", name))
				require.NoError(t, client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), replacement, "runtime"))
				return true, nil, nil
			})
			if tc.withFencer {
				rt.infraFencer = &fakeInfrastructureFencer{evidence: sandboxruntime.TerminationEvidence{RuntimeUID: ref.UID, NodeName: "node-a", InfrastructureFenced: true}}
			}

			err = rt.RemovePreparedSandbox(context.Background(), ref.ID, ref.UID)
			if tc.withFencer {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, sandboxruntime.ErrTerminationUnconfirmed)
			}
			replacement, getErr := client.CoreV1().Pods("runtime").Get(context.Background(), ref.ID, metav1.GetOptions{})
			require.NoError(t, getErr)
			assert.Equal(t, types.UID("replacement-uid"), replacement.UID)
		})
	}
}

func TestRemovePreparedSandboxNeverDeletesPolicyReboundToReplacementUID(t *testing.T) {
	rt, client := newFakeKubernetesRuntime(t, preparedScript())
	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	setWorkspaceNodeForTest(t, rt, client, ref.ID, "node-a")
	system, err := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseSystemPolicyPrefix+ref.ID, metav1.GetOptions{})
	require.NoError(t, err)
	system.Annotations[fuseRuntimeUIDAnnotation] = "replacement-uid"
	_, err = client.NetworkingV1().NetworkPolicies("runtime").Update(context.Background(), system, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, client.CoreV1().Pods("runtime").Delete(context.Background(), ref.ID, metav1.DeleteOptions{}))
	rt.infraFencer = &fakeInfrastructureFencer{evidence: sandboxruntime.TerminationEvidence{RuntimeUID: ref.UID, NodeName: "node-a", InfrastructureFenced: true}}

	err = rt.RemovePreparedSandbox(context.Background(), ref.ID, ref.UID)
	require.ErrorIs(t, err, sandboxruntime.ErrInvalidRuntimeRef)
	retained, getErr := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseSystemPolicyPrefix+ref.ID, metav1.GetOptions{})
	require.NoError(t, getErr)
	assert.Equal(t, "replacement-uid", retained.Annotations[fuseRuntimeUIDAnnotation])
	_, err = rt.ConfirmTerminated(context.Background(), ref.ID, ref.UID)
	require.ErrorIs(t, err, sandboxruntime.ErrTerminationUnconfirmed)
}

func TestDeleteExactCiliumSystemPolicyConfirmsPolicyObjectUID(t *testing.T) {
	newPolicy := func(uid types.UID) *unstructured.Unstructured {
		policy, err := buildCiliumSystemEgressPolicy("runtime", "instance-a", sandboxruntime.SystemEgressSpec{
			Mode: sandboxruntime.SystemEgressCiliumFQDN, DNSCIDRs: []string{"8.8.8.8/32"}, DNSPorts: []int32{53},
			EndpointCIDRs: []string{"192.0.2.10/32"}, EndpointFQDNs: []string{"objects.example.com"}, EndpointPorts: []int32{443},
		}, nil)
		require.NoError(t, err)
		policy.SetUID(uid)
		policy.SetAnnotations(map[string]string{fuseRuntimeUIDAnnotation: "runtime-a"})
		return policy
	}
	t.Run("annotation drift on same object is not deletion", func(t *testing.T) {
		dynamicClient := fake.NewSimpleDynamicClient(k8sruntime.NewScheme(), newPolicy(types.UID("policy-old")))
		dynamicClient.PrependReactor("delete", "ciliumnetworkpolicies", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
			current, err := dynamicClient.Tracker().Get(ciliumNetworkPolicyGVR, "runtime", action.(ktesting.DeleteAction).GetName())
			require.NoError(t, err)
			updated := current.(*unstructured.Unstructured).DeepCopy()
			updated.SetAnnotations(map[string]string{fuseRuntimeUIDAnnotation: "runtime-rebound"})
			require.NoError(t, dynamicClient.Tracker().Update(ciliumNetworkPolicyGVR, updated, "runtime"))
			return true, nil, nil
		})
		rt := &Runtime{dynClient: dynamicClient, namespace: "runtime"}
		err := rt.deleteExactCiliumSystemPolicy(context.Background(), "instance-a", "runtime-a", "", false)
		require.Error(t, err)
	})

	t.Run("replacement object with same annotation proves old deletion", func(t *testing.T) {
		dynamicClient := fake.NewSimpleDynamicClient(k8sruntime.NewScheme(), newPolicy(types.UID("policy-old")))
		dynamicClient.PrependReactor("delete", "ciliumnetworkpolicies", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
			name := action.(ktesting.DeleteAction).GetName()
			require.NoError(t, dynamicClient.Tracker().Delete(ciliumNetworkPolicyGVR, "runtime", name))
			require.NoError(t, dynamicClient.Tracker().Create(ciliumNetworkPolicyGVR, newPolicy(types.UID("policy-new")), "runtime"))
			return true, nil, nil
		})
		rt := &Runtime{dynClient: dynamicClient, namespace: "runtime"}
		require.NoError(t, rt.deleteExactCiliumSystemPolicy(context.Background(), "instance-a", "runtime-a", "", false))
	})
}

func TestConfirmTerminatedWaitsForPolicyCleanupAndRetryConverges(t *testing.T) {
	rt, client := newFakeKubernetesRuntime(t, preparedScript())
	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	auth := sandboxruntime.WorkspaceMountAuthorization{RuntimeUID: ref.UID, PoolKey: preparedFUSESpecForTest().WorkspaceFUSE.PoolKey, WorkspaceHash: "hash", Prefix: "p/", LeaseGeneration: 7, MountAttempt: 1}
	require.NoError(t, rt.AuthorizeWorkspaceMount(context.Background(), ref, auth))

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	client.PrependReactor("delete", "networkpolicies", func(ktesting.Action) (bool, k8sruntime.Object, error) {
		once.Do(func() { close(entered); <-release })
		return false, nil, nil
	})
	removed := make(chan error, 1)
	go func() { removed <- rt.RemovePreparedSandbox(context.Background(), ref.ID, ref.UID) }()
	<-entered
	_, err = rt.ConfirmTerminated(context.Background(), ref.ID, ref.UID)
	require.ErrorIs(t, err, sandboxruntime.ErrTerminationUnconfirmed)
	close(release)
	require.NoError(t, <-removed)
	_, err = rt.ConfirmTerminated(context.Background(), ref.ID, ref.UID)
	require.NoError(t, err)
}

func TestRemovePreparedSandboxRetriesPolicyCleanupFromCachedProof(t *testing.T) {
	rt, client := newFakeKubernetesRuntime(t, preparedScript())
	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	auth := sandboxruntime.WorkspaceMountAuthorization{RuntimeUID: ref.UID, PoolKey: preparedFUSESpecForTest().WorkspaceFUSE.PoolKey, WorkspaceHash: "hash", Prefix: "p/", LeaseGeneration: 7, MountAttempt: 1}
	require.NoError(t, rt.AuthorizeWorkspaceMount(context.Background(), ref, auth))

	var failures atomic.Int32
	client.PrependReactor("delete", "networkpolicies", func(ktesting.Action) (bool, k8sruntime.Object, error) {
		if failures.Add(1) == 1 {
			return true, nil, errors.New("policy API unavailable")
		}
		return false, nil, nil
	})
	require.Error(t, rt.RemovePreparedSandbox(context.Background(), ref.ID, ref.UID))
	_, err = rt.ConfirmTerminated(context.Background(), ref.ID, ref.UID)
	require.ErrorIs(t, err, sandboxruntime.ErrTerminationUnconfirmed)

	require.NoError(t, rt.RemovePreparedSandbox(context.Background(), ref.ID, ref.UID))
	evidence, err := rt.ConfirmTerminated(context.Background(), ref.ID, ref.UID)
	require.NoError(t, err)
	assert.True(t, evidence.ProcessExited)
}

func TestExactFUSEMethodsRejectNameReuseWithoutMutation(t *testing.T) {
	script := preparedScript()
	rt, client := newFakeKubernetesRuntime(t, script)
	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	pod, err := client.CoreV1().Pods("runtime").Get(context.Background(), info.RuntimeID, metav1.GetOptions{})
	require.NoError(t, err)
	pod.UID = types.UID("replacement-uid")
	client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), "runtime", info.RuntimeID)
	require.NoError(t, client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), pod, "runtime"))
	beforeCommands := len(script.commands)
	beforeActions := len(client.Actions())
	ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}

	require.ErrorIs(t, rt.PreparedSandboxHealth(context.Background(), ref, preparedFUSESpecForTest().WorkspaceFUSE.PoolKey), sandboxruntime.ErrInvalidRuntimeRef)
	require.Error(t, rt.UpdateFUSENetwork(context.Background(), ref, false, nil, false))
	assert.Len(t, script.commands, beforeCommands)
	for _, action := range client.Actions()[beforeActions:] {
		assert.NotEqual(t, "create", action.GetVerb())
		assert.NotEqual(t, "update", action.GetVerb())
		assert.NotEqual(t, "patch", action.GetVerb())
		assert.NotEqual(t, "delete", action.GetVerb())
	}
}

func TestUpdateFUSENetworkCreatesOnlySeparateUserPolicy(t *testing.T) {
	rt, client := newFakeKubernetesRuntime(t, preparedScript())
	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	auth := sandboxruntime.WorkspaceMountAuthorization{RuntimeUID: ref.UID, PoolKey: preparedFUSESpecForTest().WorkspaceFUSE.PoolKey, WorkspaceHash: "hash", Prefix: "p/", LeaseGeneration: 7, MountAttempt: 1}
	require.NoError(t, rt.AuthorizeWorkspaceMount(context.Background(), ref, auth))
	systemBefore, err := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseSystemPolicyPrefix+ref.ID, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, ref.UID, systemBefore.Annotations[fuseRuntimeUIDAnnotation])

	require.NoError(t, rt.UpdateFUSENetwork(context.Background(), ref, false, nil, false))
	user, err := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseUserPolicyPrefix+ref.ID, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "user", user.Labels["sandbox.policy.role"])
	assert.Equal(t, ref.UID, user.Annotations[fuseRuntimeUIDAnnotation])
	assert.Equal(t, map[string]string{"sandbox.pool.instance": ref.ID}, user.Spec.PodSelector.MatchLabels)
	assert.Empty(t, user.Spec.Egress)
	systemAfter, err := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseSystemPolicyPrefix+ref.ID, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, systemBefore.Spec, systemAfter.Spec)
}

func TestUpdateFUSENetworkDoesNotOverwriteReplacementUIDPolicy(t *testing.T) {
	rt, client := newFakeKubernetesRuntime(t, preparedScript())
	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	auth := sandboxruntime.WorkspaceMountAuthorization{RuntimeUID: ref.UID, PoolKey: preparedFUSESpecForTest().WorkspaceFUSE.PoolKey, WorkspaceHash: "hash", Prefix: "p/", LeaseGeneration: 7, MountAttempt: 1}
	require.NoError(t, rt.AuthorizeWorkspaceMount(context.Background(), ref, auth))
	require.NoError(t, rt.UpdateFUSENetwork(context.Background(), ref, false, nil, false))

	policy, err := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseUserPolicyPrefix+ref.ID, metav1.GetOptions{})
	require.NoError(t, err)
	policy.Annotations[fuseRuntimeUIDAnnotation] = "replacement-uid"
	replacementSpec := policy.Spec.DeepCopy()
	replacementSpec.Egress = []networkingv1.NetworkPolicyEgressRule{{}}
	policy.Spec = *replacementSpec
	_, err = client.NetworkingV1().NetworkPolicies("runtime").Update(context.Background(), policy, metav1.UpdateOptions{})
	require.NoError(t, err)

	err = rt.UpdateFUSENetwork(context.Background(), ref, true, nil, false)
	require.ErrorIs(t, err, sandboxruntime.ErrInvalidRuntimeRef)
	current, getErr := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseUserPolicyPrefix+ref.ID, metav1.GetOptions{})
	require.NoError(t, getErr)
	assert.Equal(t, "replacement-uid", current.Annotations[fuseRuntimeUIDAnnotation])
	assert.Equal(t, *replacementSpec, current.Spec)
}

func TestRecoveredRuntimeCanUpdateExactFUSENetwork(t *testing.T) {
	rt, client := newFakeKubernetesRuntime(t, preparedScript())
	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	auth := sandboxruntime.WorkspaceMountAuthorization{RuntimeUID: ref.UID, PoolKey: preparedFUSESpecForTest().WorkspaceFUSE.PoolKey, WorkspaceHash: "hash", Prefix: "p/", LeaseGeneration: 7, MountAttempt: 1}
	require.NoError(t, rt.AuthorizeWorkspaceMount(context.Background(), ref, auth))

	recovered := &Runtime{client: client, dynClient: rt.dynClient, namespace: "runtime", controlExecutor: preparedScript()}
	require.NoError(t, recovered.UpdateFUSENetwork(context.Background(), ref, true, []string{"192.0.2.10/32"}, false))
	policy, err := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseUserPolicyPrefix+ref.ID, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, ref.UID, policy.Annotations[fuseRuntimeUIDAnnotation])
}

func TestUpdateFUSENetworkInstallsCiliumDenyBeforeAllowAndPreservesSystemPrivateEndpoint(t *testing.T) {
	rt, client := newFakeKubernetesRuntime(t, preparedScript())
	rt.hasCilium = true
	spec := preparedFUSESpecForTest()
	spec.WorkspaceFUSE.EndpointHostIPs = []string{"10.10.0.10"}
	spec.WorkspaceFUSE.SystemEgress.EndpointCIDRs = []string{"10.10.0.10/32"}
	info, err := rt.PrepareSandbox(context.Background(), spec)
	require.NoError(t, err)
	ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	auth := sandboxruntime.WorkspaceMountAuthorization{RuntimeUID: ref.UID, PoolKey: spec.WorkspaceFUSE.PoolKey, WorkspaceHash: "hash", Prefix: "p/", LeaseGeneration: 7, MountAttempt: 1}
	require.NoError(t, rt.AuthorizeWorkspaceMount(context.Background(), ref, auth))

	var mu sync.Mutex
	var order []string
	rt.dynClient.(*fake.FakeDynamicClient).PrependReactor("create", "ciliumnetworkpolicies", func(ktesting.Action) (bool, k8sruntime.Object, error) {
		mu.Lock()
		order = append(order, "deny")
		mu.Unlock()
		return false, nil, nil
	})
	client.PrependReactor("create", "networkpolicies", func(ktesting.Action) (bool, k8sruntime.Object, error) {
		mu.Lock()
		order = append(order, "allow")
		mu.Unlock()
		return false, nil, nil
	})
	require.NoError(t, rt.UpdateFUSENetwork(context.Background(), ref, true, []string{"10.20.0.10/32"}, true))
	assert.Equal(t, []string{"deny", "allow"}, order)

	deny, err := rt.dynClient.Resource(ciliumNetworkPolicyGVR).Namespace("runtime").Get(context.Background(), fuseUserDenyPolicyPrefix+ref.ID, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, ref.UID, deny.GetAnnotations()[fuseRuntimeUIDAnnotation])
	egressDeny, found, err := unstructured.NestedSlice(deny.Object, "spec", "egressDeny")
	require.NoError(t, err)
	require.True(t, found)
	cidrRules := egressDeny[0].(map[string]any)["toCIDRSet"].([]any)
	var tenRule map[string]any
	for _, raw := range cidrRules {
		rule := raw.(map[string]any)
		if rule["cidr"] == "10.0.0.0/8" {
			tenRule = rule
		}
	}
	require.NotNil(t, tenRule)
	assert.ElementsMatch(t, []any{"10.10.0.10/32", "10.20.0.10/32"}, tenRule["except"])
}

func TestUpdateFUSENetworkMarksPartialCiliumPairUncertainWhenAllowIsRejected(t *testing.T) {
	rt, client := newFakeKubernetesRuntime(t, preparedScript())
	rt.hasCilium = true
	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	client.PrependReactor("create", "networkpolicies", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
		name := action.(ktesting.CreateAction).GetObject().(*networkingv1.NetworkPolicy).Name
		if name != fuseUserPolicyPrefix+ref.ID {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}, name, errors.New("admission rejected user policy"))
	})

	err = rt.UpdateFUSENetwork(context.Background(), ref, true, nil, true)
	require.ErrorIs(t, err, sandboxruntime.ErrFUSENetworkStateUncertain)
	deny, getErr := rt.dynClient.Resource(ciliumNetworkPolicyGVR).Namespace("runtime").Get(context.Background(), fuseUserDenyPolicyPrefix+ref.ID, metav1.GetOptions{})
	require.NoError(t, getErr)
	assert.NotEmpty(t, deny.GetAnnotations()[fuseNetworkAttemptAnnotation])
}

func TestUpdateFUSENetworkNeverDeletesExistingCiliumDenyOnMutatedUpdate(t *testing.T) {
	rt, _ := newFakeKubernetesRuntime(t, preparedScript())
	rt.hasCilium = true
	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	auth := sandboxruntime.WorkspaceMountAuthorization{RuntimeUID: ref.UID, PoolKey: preparedFUSESpecForTest().WorkspaceFUSE.PoolKey, WorkspaceHash: "hash", Prefix: "p/", LeaseGeneration: 7, MountAttempt: 1}
	require.NoError(t, rt.AuthorizeWorkspaceMount(context.Background(), ref, auth))
	require.NoError(t, rt.UpdateFUSENetwork(context.Background(), ref, true, nil, true))
	dynamicClient := rt.dynClient.(*fake.FakeDynamicClient)
	before, err := dynamicClient.Resource(ciliumNetworkPolicyGVR).Namespace("runtime").Get(context.Background(), fuseUserDenyPolicyPrefix+ref.ID, metav1.GetOptions{})
	require.NoError(t, err)
	dynamicClient.PrependReactor("update", "ciliumnetworkpolicies", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
		updated := action.(ktesting.UpdateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		require.NoError(t, unstructured.SetNestedSlice(updated.Object, []any{}, "spec", "egressDeny"))
		require.NoError(t, dynamicClient.Tracker().Update(ciliumNetworkPolicyGVR, updated, "runtime"))
		return true, updated, nil
	})

	err = rt.UpdateFUSENetwork(context.Background(), ref, true, []string{"10.20.0.10/32"}, true)
	require.ErrorIs(t, err, sandboxruntime.ErrFUSENetworkStateUncertain)
	retained, getErr := dynamicClient.Resource(ciliumNetworkPolicyGVR).Namespace("runtime").Get(context.Background(), fuseUserDenyPolicyPrefix+ref.ID, metav1.GetOptions{})
	require.NoError(t, getErr)
	assert.Equal(t, before.GetUID(), retained.GetUID())
}

func TestUpdateFUSENetworkPreservesApprovedPrivateCiliumFQDNWithoutHostAlias(t *testing.T) {
	rt, _ := newFakeKubernetesRuntime(t, preparedScript())
	rt.hasCilium = true
	spec := preparedFUSESpecForTest()
	spec.WorkspaceFUSE.Endpoint = "objects.example.com:443"
	spec.WorkspaceFUSE.EndpointHostIPs = nil
	spec.WorkspaceFUSE.SystemEgress.Mode = sandboxruntime.SystemEgressCiliumFQDN
	spec.WorkspaceFUSE.SystemEgress.EndpointCIDRs = []string{"10.10.0.0/24"}
	spec.WorkspaceFUSE.SystemEgress.EndpointFQDNs = []string{"objects.example.com"}
	spec.WorkspaceFUSE.SystemEgress.EndpointPorts = []int32{443}
	info, err := rt.PrepareSandbox(context.Background(), spec)
	require.NoError(t, err)
	ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	auth := sandboxruntime.WorkspaceMountAuthorization{RuntimeUID: ref.UID, PoolKey: spec.WorkspaceFUSE.PoolKey, WorkspaceHash: "hash", Prefix: "p/", LeaseGeneration: 7, MountAttempt: 1}
	require.NoError(t, rt.AuthorizeWorkspaceMount(context.Background(), ref, auth))
	require.NoError(t, rt.UpdateFUSENetwork(context.Background(), ref, true, nil, true))

	deny, err := rt.dynClient.Resource(ciliumNetworkPolicyGVR).Namespace("runtime").Get(context.Background(), fuseUserDenyPolicyPrefix+ref.ID, metav1.GetOptions{})
	require.NoError(t, err)
	egressDeny, found, err := unstructured.NestedSlice(deny.Object, "spec", "egressDeny")
	require.NoError(t, err)
	require.True(t, found)
	cidrRules := egressDeny[0].(map[string]any)["toCIDRSet"].([]any)
	for _, raw := range cidrRules {
		rule := raw.(map[string]any)
		if rule["cidr"] == "10.0.0.0/8" {
			assert.Contains(t, rule["except"], "10.10.0.0/24")
			return
		}
	}
	t.Fatal("missing RFC1918 deny rule")
}

func TestPreparedPodIntentRejectsSecurityExpansions(t *testing.T) {
	desired, err := buildPreparedFUSEPod("runtime", preparedFUSESpecForTest())
	require.NoError(t, err)
	desired.Annotations[fusePrepareAttemptAnnotation] = "attempt-a"
	base := desired.DeepCopy()
	base.UID = types.UID("pod-uid-a")
	defaultPriority := int32(0)
	defaultPreemption := corev1.PreemptLowerPriority
	base.Spec.Priority = &defaultPriority
	base.Spec.PreemptionPolicy = &defaultPreemption
	require.True(t, preparedPodIntentMatches(base, desired, false))

	scheduled := base.DeepCopy()
	scheduled.Spec.NodeName = "node-a"
	assert.False(t, preparedPodIntentMatches(scheduled, desired, false), "a successful create response must not hide an admission-forced node")
	assert.True(t, preparedPodIntentMatches(scheduled, desired, true), "an ambiguous create readback may already be scheduler-bound")

	withServiceAccountPullSecrets := base.DeepCopy()
	withServiceAccountPullSecrets.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "registry-credentials"}}
	assert.True(t, preparedPodIntentMatches(withServiceAccountPullSecrets, desired, false), "ServiceAccount admission may copy imagePullSecrets")
	withServiceAccountPullSecrets.Spec.ImagePullSecrets = append(withServiceAccountPullSecrets.Spec.ImagePullSecrets, corev1.LocalObjectReference{Name: "INVALID_SECRET"})
	assert.False(t, preparedPodIntentMatches(withServiceAccountPullSecrets, desired, false), "only valid ServiceAccount imagePullSecret references are normalized")

	for _, tc := range []struct {
		name   string
		mutate func(*corev1.Pod)
	}{
		{name: "host network", mutate: func(p *corev1.Pod) { p.Spec.HostNetwork = true }},
		{name: "extra sidecar", mutate: func(p *corev1.Pod) {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "injected", Image: "evil"})
		}},
		{name: "extra volume", mutate: func(p *corev1.Pod) {
			p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}})
		}},
		{name: "sandbox sys admin", mutate: func(p *corev1.Pod) {
			p.Spec.Containers[0].SecurityContext.Capabilities.Add = []corev1.Capability{"SYS_ADMIN"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := base.DeepCopy()
			tc.mutate(current)
			assert.False(t, preparedPodIntentMatches(current, desired, false))
		})
	}
}

func TestPrepareSandboxRejectsNodeNameInjectedIntoSuccessfulCreateResponse(t *testing.T) {
	rt, client := newFakeKubernetesRuntime(t, preparedScript())
	client.PrependReactor("create", "pods", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
		pod := action.(ktesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Spec.NodeName = "admission-forced-node"
		return false, nil, nil
	})

	_, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.ErrorContains(t, err, "does not match the requested security contract")
	_, podErr := client.CoreV1().Pods("runtime").Get(context.Background(), preparedFUSESpecForTest().ID, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(podErr))
	_, policyErr := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseSystemPolicyPrefix+preparedFUSESpecForTest().ID, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(policyErr))
}

func TestPreparedHealthAndFUSENetworkRejectProtectedLabelDrift(t *testing.T) {
	rt, client := newFakeKubernetesRuntime(t, preparedScript())
	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	auth := sandboxruntime.WorkspaceMountAuthorization{RuntimeUID: ref.UID, PoolKey: preparedFUSESpecForTest().WorkspaceFUSE.PoolKey, WorkspaceHash: "hash", Prefix: "p/", LeaseGeneration: 7, MountAttempt: 1}
	require.NoError(t, rt.AuthorizeWorkspaceMount(context.Background(), ref, auth))
	pod, err := client.CoreV1().Pods("runtime").Get(context.Background(), ref.ID, metav1.GetOptions{})
	require.NoError(t, err)
	pod.Labels["sandbox.pool.instance"] = "other-instance"
	_, err = client.CoreV1().Pods("runtime").Update(context.Background(), pod, metav1.UpdateOptions{})
	require.NoError(t, err)

	require.Error(t, rt.PreparedSandboxHealth(context.Background(), ref, preparedFUSESpecForTest().WorkspaceFUSE.PoolKey))
	require.Error(t, rt.UpdateFUSENetwork(context.Background(), ref, true, nil, false))
	_, ownErr := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseUserPolicyPrefix+ref.ID, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(ownErr))
	_, otherErr := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), fuseUserPolicyPrefix+"other-instance", metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(otherErr))
}

func TestExactFUSEOperationsRejectEveryProtectedIdentityLabelDrift(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{key: "sandbox.id", value: "other-id"},
		{key: "sandbox.managed", value: "false"},
		{key: "sandbox.pool", value: "false"},
		{key: "sandbox.pool.key", value: "invalid"},
		{key: "sandbox.pool.instance", value: "other-instance"},
		{key: "sandbox.workspace.mode", value: "sync"},
		{key: "sandbox.workspace.provider", value: "obs"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			rt, client := newFakeKubernetesRuntime(t, preparedScript())
			info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
			require.NoError(t, err)
			ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
			pod, err := client.CoreV1().Pods("runtime").Get(context.Background(), ref.ID, metav1.GetOptions{})
			require.NoError(t, err)
			pod.Labels[tc.key] = tc.value
			_, err = client.CoreV1().Pods("runtime").Update(context.Background(), pod, metav1.UpdateOptions{})
			require.NoError(t, err)
			_, healthErr := rt.WorkspaceHealth(context.Background(), ref)
			require.Error(t, healthErr)
			updateErr := rt.UpdateFUSENetwork(context.Background(), ref, false, nil, false)
			require.Error(t, updateErr)
		})
	}
}

func TestWorkspaceHealthRejectsNonPreparedPoolState(t *testing.T) {
	rt, client := newFakeKubernetesRuntime(t, preparedScript())
	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}

	pod, err := client.CoreV1().Pods("runtime").Get(context.Background(), ref.ID, metav1.GetOptions{})
	require.NoError(t, err)
	pod.Labels["sandbox.pool.state"] = "consumed"
	_, err = client.CoreV1().Pods("runtime").Update(context.Background(), pod, metav1.UpdateOptions{})
	require.NoError(t, err)

	_, err = rt.WorkspaceHealth(context.Background(), ref)
	require.Error(t, err)
}

func TestRecoveredWorkspaceHealthRejectsMounterPoolKeyDrift(t *testing.T) {
	base := preparedScript()
	script := &commandScript{handler: func(command recordedPodCommand) ([]byte, error) {
		if fmt.Sprint(command.argv) == fmt.Sprint([]string{mounterBinary, "health", "ready"}) {
			return []byte(`{"version":1,"state":"ready","runtime_uid":"pod-uid-a","pool_key":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff","mount_type":"fuse","generation":7,"restart_detected":false,"cache_bytes":0,"cache_limit_bytes":2147483648,"cache_exceeded":false}`), nil
		}
		return base.handler(command)
	}}
	rt, _ := newFakeKubernetesRuntime(t, script)
	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	health, err := rt.WorkspaceHealth(context.Background(), ref)
	require.NoError(t, err)
	require.False(t, health.Ready)
}

func TestResumeWorkspaceConcurrentReplayHasOneWinner(t *testing.T) {
	script := preparedScript()
	rt, _ := newFakeKubernetesRuntime(t, script)
	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	auth := sandboxruntime.WorkspaceMountAuthorization{RuntimeUID: ref.UID, PoolKey: preparedFUSESpecForTest().WorkspaceFUSE.PoolKey, WorkspaceHash: "hash", Prefix: "p/", LeaseGeneration: 7, MountAttempt: 1}
	require.NoError(t, rt.AuthorizeWorkspaceMount(context.Background(), ref, auth))
	token, err := rt.QuiesceWorkspace(context.Background(), ref, 7)
	require.NoError(t, err)

	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			results <- rt.ResumeWorkspace(context.Background(), ref, token)
		}()
	}
	close(start)
	err1, err2 := <-results, <-results
	assert.Equal(t, 1, boolToInt(err1 == nil)+boolToInt(err2 == nil))
}

func TestUpdateLabelsRejectsProtectedFUSELabels(t *testing.T) {
	rt, _ := newFakeKubernetesRuntime(t, preparedScript())
	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	value := "changed"
	err = rt.UpdateLabels(context.Background(), info.RuntimeID, map[string]*string{"sandbox.pool.instance": &value})
	require.Error(t, err)
}

func TestRuntimeRefRejectsEmptyIdentity(t *testing.T) {
	_, err := sandboxruntime.NewRuntimeRef("pod", "")
	require.ErrorIs(t, err, sandboxruntime.ErrInvalidRuntimeRef)
	_, err = sandboxruntime.NewRuntimeRef("", "uid")
	require.ErrorIs(t, err, sandboxruntime.ErrInvalidRuntimeRef)
}

func TestWithInfrastructureFencerConfiguresExplicitFallback(t *testing.T) {
	fencer := &fakeInfrastructureFencer{}
	rt := &Runtime{}
	WithInfrastructureFencer(fencer)(rt)
	assert.Same(t, fencer, rt.infraFencer)
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
