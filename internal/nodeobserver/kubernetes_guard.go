package nodeobserver

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/goairix/sandbox/internal/kubecontract"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
)

const runtimeTerminationFinalizer = "goairix.github.io/sandbox-runtime-termination"
const creationIntentAnnotation = "sandbox.creation.intent"

type providerRegistration struct {
	Binding          p.TrustBinding
	Identity         p.RuntimeIdentityContext
	Birth            p.BirthContext
	CreationIntentID string
}

type providerObservation struct {
	PodUID           string    `json:"pod_uid"`
	ResourceVersion  string    `json:"resource_version"`
	NodeUID          string    `json:"node_uid"`
	NodeBootID       string    `json:"node_boot_id"`
	CreationIntentID string    `json:"creation_intent_id"`
	DeletingAt       time.Time `json:"deleting_at"`
}

type providerGuard struct {
	client                                   *kubernetes.Clientset
	namespace, nodeName, nodeUID, nodeBootID string
}

func (g *providerGuard) observe(ctx context.Context, registration providerRegistration, requireDeleting bool) (providerObservation, error) {
	fail := func(reason string) (providerObservation, error) {
		return providerObservation{}, fmt.Errorf("observer provider refused: %s", reason)
	}
	if ctx == nil || g.client == nil || len(validation.IsDNS1123Subdomain(registration.Identity.Runtime.ID)) != 0 || registration.Birth.UID == 0 || registration.Birth.GID == 0 {
		return fail("missing fixed provider identity")
	}
	node, err := g.client.CoreV1().Nodes().Get(ctx, g.nodeName, metav1.GetOptions{})
	if err != nil {
		return providerObservation{}, err
	}
	if node == nil || node.Name != g.nodeName || string(node.UID) != g.nodeUID || node.DeletionTimestamp != nil || node.Status.NodeInfo.BootID != g.nodeBootID || node.Status.NodeInfo.OperatingSystem != "linux" || (node.Status.NodeInfo.Architecture != "amd64" && node.Status.NodeInfo.Architecture != "arm64") {
		return fail("node UID/kernel boot/profile changed")
	}
	pod, err := g.client.CoreV1().Pods(g.namespace).Get(ctx, registration.Identity.Runtime.ID, metav1.GetOptions{})
	if err != nil {
		return providerObservation{}, err
	}
	if pod == nil || pod.Name != registration.Identity.Runtime.ID || pod.Namespace != g.namespace || string(pod.UID) != registration.Identity.Runtime.UID || pod.Spec.NodeName != g.nodeName || len(pod.ResourceVersion) == 0 || len(pod.ResourceVersion) > 128 {
		return fail("exact retained Pod UID/node unavailable")
	}
	enrolled, err := kubecontract.SelectEnrolledPlainPodProfile(pod)
	if err != nil {
		return fail("unknown or downgraded launcher profile")
	}
	if enrolled {
		profile, err := kubecontract.InspectEnrolledPlainPod(pod)
		if err != nil {
			return fail("invalid enrolled launcher profile")
		}
		c := profile.Config
		if c.TrustBinding() != registration.Binding || profile.LogicalID != registration.Identity.SandboxID || profile.Generation != registration.Identity.Generation || profile.NodeUID != g.nodeUID || profile.NodeBootID != g.nodeBootID || c.UID != registration.Birth.UID || c.GID != registration.Birth.GID || c.NetworkAllowed != registration.Birth.NetworkAllowed || c.ContractDigest != registration.Birth.ContractDigest {
			return fail("enrolled profile differs from verified activation")
		}
	}
	creation := pod.Annotations[creationIntentAnnotation]
	id, err := uuid.Parse(creation)
	if err != nil || id == uuid.Nil || id.String() != creation || (registration.CreationIntentID != "" && registration.CreationIntentID != creation) {
		return fail("observed creation identity missing or changed")
	}
	if pod.Spec.RestartPolicy != v1.RestartPolicyNever || pod.Spec.HostPID || pod.Spec.HostIPC || pod.Spec.HostNetwork || pod.Spec.ShareProcessNamespace != nil && *pod.Spec.ShareProcessNamespace || pod.Spec.HostUsers != nil && !*pod.Spec.HostUsers || len(pod.Spec.Containers) != 1 || len(pod.Spec.InitContainers) != 0 || len(pod.Spec.EphemeralContainers) != 0 {
		return fail("Pod is not a private single-use Never runtime")
	}
	container := pod.Spec.Containers[0]
	security := container.SecurityContext
	if container.Name != "sandbox" || container.RestartPolicy != nil || len(container.VolumeDevices) != 0 || security == nil || security.Privileged != nil && *security.Privileged || security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation || security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem || security.RunAsUser == nil || *security.RunAsUser != 0 || security.Capabilities == nil || len(security.Capabilities.Drop) != 1 || security.Capabilities.Drop[0] != "ALL" {
		return fail("supervisor root/user filesystem boundary is not explicit")
	}
	seen := make(map[v1.Capability]bool, 4)
	for _, cap := range security.Capabilities.Add {
		if seen[cap] || (cap != "KILL" && cap != "SETUID" && cap != "SETGID" && cap != "SETPCAP") {
			return fail("supervisor has cgroup/namespace escape capability")
		}
		seen[cap] = true
	}
	profile := security.SeccompProfile
	if profile == nil && pod.Spec.SecurityContext != nil {
		profile = pod.Spec.SecurityContext.SeccompProfile
	}
	if profile == nil || profile.Type != v1.SeccompProfileTypeRuntimeDefault {
		return fail("supervisor seccomp contract unavailable")
	}
	for _, volume := range pod.Spec.Volumes {
		if volume.HostPath == nil {
			continue
		}
		for _, mount := range container.VolumeMounts {
			if mount.Name == volume.Name && (!mount.ReadOnly || mount.MountPropagation != nil && *mount.MountPropagation != v1.MountPropagationNone) {
				return fail("writable/propagating host mount permits runtime escape")
			}
		}
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != "sandbox" || status.RestartCount != 0 {
			return fail("runtime container restarted or has foreign status")
		}
	}
	if requireDeleting && (pod.DeletionTimestamp == nil || !slices.Contains(pod.Finalizers, runtimeTerminationFinalizer)) {
		return fail("retained exact Pod DELETE/finalizer is unresolved")
	}
	result := providerObservation{PodUID: string(pod.UID), ResourceVersion: pod.ResourceVersion, NodeUID: string(node.UID), NodeBootID: node.Status.NodeInfo.BootID, CreationIntentID: creation}
	if pod.DeletionTimestamp != nil {
		result.DeletingAt = pod.DeletionTimestamp.Time.UTC()
	}
	return result, ctx.Err()
}
