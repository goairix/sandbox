package kubernetes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	v1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

const taskCleanupFinalizer = "goairix.github.io/sandbox-runtime-termination"
const taskCleanupDescriptorLimit = 16384

// PreparedTaskRuntimeCleanup is an opaque, process-local diagnostic handle.
// Its descriptor does not authorize native operations or prove physical death.
type PreparedTaskRuntimeCleanup struct {
	self       *PreparedTaskRuntimeCleanup
	client     *TaskRuntimeCleanupClient
	descriptor taskCleanupDescriptor
	wire       []byte
	mu         sync.Mutex
	busy       bool
	sent       [7]bool
	pinned     bool
}

type taskCleanupTarget struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	UID  string `json:"uid"`
	RV   string `json:"resource_version"`
}

type taskCleanupDescriptor struct {
	Version   uint32                    `json:"version"`
	Binding   TaskRuntimeCleanupBinding `json:"binding"`
	Namespace string                    `json:"namespace"`
	Cilium    bool                      `json:"cilium"`
	Node      string                    `json:"node"`
	Profile   string                    `json:"profile"`
	Targets   []taskCleanupTarget       `json:"targets"`
}

// NewTaskRuntimeCleanupClient constructs a static, bounded one-request client.
// Construction never contacts the API server.
func NewTaskRuntimeCleanupClient(options TaskRuntimeCleanupOptions) (*TaskRuntimeCleanupClient, error) {
	if options.MaxActive < 1 || options.MaxActive > 32 || options.RequestTimeout <= 0 || options.RequestTimeout > 5*time.Second || len(validation.IsDNS1123Label(options.Namespace)) != 0 {
		return nil, errTaskCleanupRefused
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &TaskRuntimeCleanupClient{ctx: ctx, cancel: cancel, limit: options.MaxActive, timeout: options.RequestTimeout, namespace: strings.Clone(options.Namespace), cilium: options.IncludeCilium}
	c.self = c
	if err := c.configure(options.Config); err != nil {
		cancel()
		return nil, err
	}
	return c, nil
}

// Close seals admission, cancels and joins actual API/body and dial operations.
func (c *TaskRuntimeCleanupClient) Close() error {
	if c == nil || c.self != c {
		return errTaskCleanupRefused
	}
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.cancel()
	c.transport.CloseIdleConnections()
	c.wg.Wait()
	return nil
}

func (c *TaskRuntimeCleanupClient) trackDial() (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errTaskCleanupRefused
	}
	c.wg.Add(1)
	return c.wg.Done, nil
}

func (c *TaskRuntimeCleanupClient) begin(ctx context.Context) (context.Context, func(), error) {
	if c == nil || c.self != c || ctx == nil || ctx.Err() != nil {
		return nil, nil, errTaskCleanupRefused
	}
	c.mu.Lock()
	if c.closed || c.active >= c.limit {
		c.mu.Unlock()
		return nil, nil, errTaskCleanupRefused
	}
	c.active++
	c.wg.Add(1)
	c.mu.Unlock()
	opCtx, cancel := context.WithTimeout(ctx, c.timeout)
	stop := context.AfterFunc(c.ctx, cancel)
	return opCtx, func() {
		stop()
		cancel()
		c.mu.Lock()
		c.active--
		c.mu.Unlock()
		c.wg.Done()
	}, nil
}

// Prepare rechecks a fixed Pod, Node and at most four exact policy objects.
// saved must be an authenticated descriptor from the caller's durable record.
func (c *TaskRuntimeCleanupClient) Prepare(ctx context.Context, binding TaskRuntimeCleanupBinding, saved []byte) (*PreparedTaskRuntimeCleanup, error) {
	if len(saved) > taskCleanupDescriptorLimit || !validTaskCleanupBinding(binding) {
		return nil, errTaskCleanupRefused
	}
	opCtx, done, err := c.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	d := taskCleanupDescriptor{Version: 1, Binding: binding, Namespace: c.namespace, Cilium: c.cilium}
	if len(saved) != 0 {
		decoder := json.NewDecoder(bytes.NewReader(saved))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&d); err != nil {
			return nil, errTaskCleanupRefused
		}
		canonical, err := json.Marshal(d)
		if err != nil || !bytes.Equal(canonical, saved) || d.Version != 1 || d.Binding != binding || d.Namespace != c.namespace || d.Cilium != c.cilium || len(validation.IsDNS1123Subdomain(d.Node)) != 0 || !taskCleanupHash(d.Profile) || !validTaskCleanupTargets(d) {
			return nil, errTaskCleanupRefused
		}
	}
	h := &PreparedTaskRuntimeCleanup{client: c, descriptor: d, pinned: len(saved) != 0}
	h.self = h
	pod, err := h.pod(opCtx)
	if err != nil || pod == nil && len(saved) == 0 {
		return nil, errorsOrRefused(err)
	}
	if pod != nil && len(saved) == 0 {
		h.descriptor.Node = pod.Spec.NodeName
		h.descriptor.Profile = taskCleanupPodHash(pod)
	}
	if err := h.node(opCtx); err != nil {
		return nil, err
	}
	if len(saved) == 0 {
		for _, target := range taskCleanupTargetNames(binding, c.cilium) {
			actual, err := h.policy(opCtx, target)
			if err != nil {
				return nil, err
			}
			h.descriptor.Targets = append(h.descriptor.Targets, actual)
		}
	} else {
		for _, target := range h.descriptor.Targets {
			if _, err := h.policy(opCtx, target); err != nil {
				return nil, err
			}
		}
	}
	h.wire, err = json.Marshal(h.descriptor)
	if err != nil || len(h.wire) > taskCleanupDescriptorLimit {
		return nil, errTaskCleanupRefused
	}
	h.pinned = true
	return h, opCtx.Err()
}

func errorsOrRefused(err error) error {
	if err != nil {
		return err
	}
	return errTaskCleanupRefused
}

// Descriptor returns an independent canonical copy, never a physical receipt.
func (h *PreparedTaskRuntimeCleanup) Descriptor() []byte {
	if h == nil || h.self != h {
		return nil
	}
	return bytes.Clone(h.wire)
}

// Observe reports copied API state; Terminal is artifact absence only.
func (h *PreparedTaskRuntimeCleanup) Observe(ctx context.Context) (TaskRuntimeCleanupObservation, error) {
	if h == nil || h.self != h {
		return TaskRuntimeCleanupObservation{}, errTaskCleanupRefused
	}
	opCtx, done, err := h.client.begin(ctx)
	if err != nil {
		return TaskRuntimeCleanupObservation{}, err
	}
	defer done()
	pod, err := h.pod(opCtx)
	if err != nil {
		return TaskRuntimeCleanupObservation{}, err
	}
	if err := h.node(opCtx); err != nil {
		return TaskRuntimeCleanupObservation{}, err
	}
	obs := h.podObservation(pod)
	obs.Terminal = pod == nil
	for _, target := range h.descriptor.Targets {
		current, err := h.policy(opCtx, target)
		if err != nil {
			return TaskRuntimeCleanupObservation{}, err
		}
		obs.Terminal = obs.Terminal && current.UID == ""
	}
	return obs, opCtx.Err()
}

func (h *PreparedTaskRuntimeCleanup) get(ctx context.Context, path string, output any) (bool, error) {
	wire, err := h.client.api.Get().AbsPath(path).MaxRetries(0).DoRaw(ctx)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if len(wire) > taskCleanupBodyLimit || json.Unmarshal(wire, output) != nil {
		return false, errTaskCleanupRefused
	}
	return true, nil
}

func (h *PreparedTaskRuntimeCleanup) pod(ctx context.Context) (*v1.Pod, error) {
	var pod v1.Pod
	found, err := h.get(ctx, "/api/v1/namespaces/"+h.client.namespace+"/pods/"+h.descriptor.Binding.Runtime.ID, &pod)
	if err != nil || !found {
		return nil, err
	}
	if err := h.validatePod(&pod); err != nil {
		return nil, err
	}
	return &pod, nil
}

func (h *PreparedTaskRuntimeCleanup) node(ctx context.Context) error {
	var node v1.Node
	found, err := h.get(ctx, "/api/v1/nodes/"+h.descriptor.Node, &node)
	if err != nil {
		return err
	}
	b := h.descriptor.Binding
	if !found || node.Name != h.descriptor.Node || string(node.UID) != b.NodeUID || node.Status.NodeInfo.BootID != b.NodeBootID || node.DeletionTimestamp != nil || node.Status.NodeInfo.OperatingSystem != "linux" || !slices.Contains([]string{"amd64", "arm64"}, node.Status.NodeInfo.Architecture) {
		return errTaskCleanupRefused
	}
	return nil
}

func (h *PreparedTaskRuntimeCleanup) policy(ctx context.Context, target taskCleanupTarget) (taskCleanupTarget, error) {
	path := h.policyPath(target)
	identity := ordinaryNetworkIdentity{runtimeID: h.descriptor.Binding.Runtime.ID, runtimeUID: types.UID(h.descriptor.Binding.Runtime.UID), logicalID: strings.TrimPrefix(target.Name, "sandbox-")}
	var actual taskCleanupTarget
	var found bool
	var err error
	if target.Kind == "NetworkPolicy" {
		var policy networkingv1.NetworkPolicy
		found, err = h.get(ctx, path, &policy)
		if err == nil && found {
			err = validateOrdinaryNetworkPolicy(&policy, identity, "")
			if policy.Namespace != h.client.namespace || len(policy.Finalizers) != 0 || len(policy.OwnerReferences) != 0 {
				err = errTaskCleanupRefused
			}
			actual = taskCleanupTarget{Kind: target.Kind, Name: target.Name, UID: string(policy.UID), RV: policy.ResourceVersion}
		}
	} else {
		identity.logicalID = strings.TrimPrefix(target.Name, "sandbox-private-deny-")
		var policy unstructured.Unstructured
		found, err = h.get(ctx, path, &policy)
		if err == nil && found {
			err = validateOrdinaryCiliumPolicy(&policy, identity, "")
			if policy.GetNamespace() != h.client.namespace || len(policy.GetFinalizers()) != 0 || len(policy.GetOwnerReferences()) != 0 {
				err = errTaskCleanupRefused
			}
			actual = taskCleanupTarget{Kind: target.Kind, Name: target.Name, UID: string(policy.GetUID()), RV: policy.GetResourceVersion()}
		}
	}
	if err != nil {
		return taskCleanupTarget{}, err
	}
	if !found {
		return taskCleanupTarget{Kind: target.Kind, Name: target.Name}, nil
	}
	if actual.UID == "" || len(actual.UID) > 128 || actual.RV == "" || len(actual.RV) > 128 || target.UID != "" && (target.UID != actual.UID || target.RV != actual.RV) || target.UID == "" && h.pinned {
		return taskCleanupTarget{}, errTaskCleanupRefused
	}
	return actual, nil
}

func (h *PreparedTaskRuntimeCleanup) policyPath(target taskCleanupTarget) string {
	prefix := "/apis/networking.k8s.io/v1/namespaces/" + h.client.namespace + "/networkpolicies/"
	if target.Kind == "CiliumNetworkPolicy" {
		prefix = "/apis/cilium.io/v2/namespaces/" + h.client.namespace + "/ciliumnetworkpolicies/"
	}
	return prefix + target.Name
}

func (h *PreparedTaskRuntimeCleanup) podObservation(pod *v1.Pod) TaskRuntimeCleanupObservation {
	obs := TaskRuntimeCleanupObservation{Kind: "Pod", Name: h.descriptor.Binding.Runtime.ID, Absent: pod == nil}
	if pod != nil {
		obs.UID, obs.ResourceVersion = string(pod.UID), pod.ResourceVersion
		obs.Deleting = pod.DeletionTimestamp != nil
		obs.Retained = slices.Contains(pod.Finalizers, taskCleanupFinalizer)
	}
	return obs
}

func validTaskCleanupBinding(b TaskRuntimeCleanupBinding) bool {
	for _, value := range []string{b.Authority.Namespace, b.Authority.AuthorityID, b.Authority.Target, b.TaskID} {
		if len(value) == 0 || len(value) > 128 || strings.ContainsAny(value, "/\\\x00\r\n") {
			return false
		}
	}
	for _, value := range []string{b.Authority.RestoreEpoch, b.Runtime.UID, b.Runtime.BootID, b.CreationIntentID, b.NodeUID, b.NodeBootID} {
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil || id.String() != value {
			return false
		}
	}
	return len(validation.IsDNS1123Subdomain(b.Runtime.ID)) == 0 && len(validation.IsDNS1123Subdomain(b.LogicalID)) == 0 && len(b.Runtime.ID) <= 128 && len(b.LogicalID) <= 128 && b.Generation > 0 && b.DataGateEpoch > 0 && taskCleanupHash(b.WorkspaceHash) && taskCleanupHash(b.EnrollmentDigest) && taskCleanupHash(b.ContractDigest)
}

func taskCleanupHash(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && strings.ToLower(value) == value
}

func taskCleanupTargetNames(b TaskRuntimeCleanupBinding, cilium bool) []taskCleanupTarget {
	targets := make([]taskCleanupTarget, 0, 4)
	for _, kind := range []string{"NetworkPolicy", "CiliumNetworkPolicy"} {
		if kind == "CiliumNetworkPolicy" && !cilium {
			continue
		}
		for i, name := range []string{b.LogicalID, b.Runtime.ID} {
			if i == 1 && name == b.LogicalID {
				continue
			}
			prefix := "sandbox-"
			if kind == "CiliumNetworkPolicy" {
				prefix = "sandbox-private-deny-"
			}
			targets = append(targets, taskCleanupTarget{Kind: kind, Name: prefix + name})
		}
	}
	return targets
}

func validTaskCleanupTargets(d taskCleanupDescriptor) bool {
	expected := taskCleanupTargetNames(d.Binding, d.Cilium)
	if len(expected) != len(d.Targets) {
		return false
	}
	for i, target := range d.Targets {
		if target.Kind != expected[i].Kind || target.Name != expected[i].Name || len(target.UID) > 128 || len(target.RV) > 128 || (target.UID == "") != (target.RV == "") {
			return false
		}
	}
	return true
}

func taskCleanupPodHash(pod *v1.Pod) string {
	wire, _ := json.Marshal(pod.Spec)
	digest := sha256.Sum256(wire)
	return hex.EncodeToString(digest[:])
}
