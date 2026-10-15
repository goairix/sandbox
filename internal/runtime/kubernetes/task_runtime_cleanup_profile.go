package kubernetes

import (
	"reflect"
	"slices"

	"github.com/goairix/sandbox/internal/kubecontract"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// The enrolled plain profile is intentionally narrower than the general Pod
// builder. No external workspace source or admission-added container is safe
// to infer from a signed runtime identity alone.
func (h *PreparedTaskRuntimeCleanup) validatePod(pod *v1.Pod) error {
	enrolled, profileErr := kubecontract.SelectEnrolledPlainPodProfile(pod)
	if profileErr != nil {
		return errTaskCleanupRefused
	}
	b := h.descriptor.Binding
	if enrolled {
		profile, err := kubecontract.InspectEnrolledPlainPod(pod)
		if err != nil || profile.Config.TrustBinding() != b.Authority || profile.LogicalID != b.LogicalID || profile.Generation != b.Generation || profile.CreationIntentID != b.CreationIntentID || profile.NodeUID != b.NodeUID || profile.NodeBootID != b.NodeBootID || profile.Config.ContractDigest != b.ContractDigest {
			return errTaskCleanupRefused
		}
	}
	if pod.Name != b.Runtime.ID || pod.Namespace != h.client.namespace || string(pod.UID) != b.Runtime.UID || pod.ResourceVersion == "" || len(pod.ResourceVersion) > 128 || len(validation.IsDNS1123Subdomain(pod.Spec.NodeName)) != 0 || h.descriptor.Node != "" && pod.Spec.NodeName != h.descriptor.Node || pod.Annotations["sandbox.creation.intent"] != b.CreationIntentID || pod.Labels["sandbox.managed"] != "true" || len(pod.OwnerReferences) != 0 || pod.Labels["sandbox.workspace.mode"] != "" && pod.Labels["sandbox.workspace.mode"] != "plain" {
		return errTaskCleanupRefused
	}
	labels, err := ordinaryPodLabels(pod)
	if err != nil || labels["sandbox.id"] != b.LogicalID {
		return errTaskCleanupRefused
	}
	if len(pod.Finalizers) > 1 || len(pod.Finalizers) == 1 && pod.Finalizers[0] != taskCleanupFinalizer || pod.Spec.RestartPolicy != v1.RestartPolicyNever || pod.Spec.HostPID || pod.Spec.HostIPC || pod.Spec.HostNetwork || pod.Spec.ShareProcessNamespace != nil && *pod.Spec.ShareProcessNamespace || pod.Spec.HostUsers != nil && !*pod.Spec.HostUsers || len(pod.Spec.Containers) != 1 || len(pod.Spec.InitContainers) != 0 || len(pod.Spec.EphemeralContainers) != 0 || len(pod.Spec.ResourceClaims) != 0 {
		return errTaskCleanupRefused
	}
	container := &pod.Spec.Containers[0]
	security := container.SecurityContext
	if container.Name != "sandbox" || container.RestartPolicy != nil || len(container.VolumeDevices) != 0 || security == nil || security.Privileged != nil && *security.Privileged || security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation || security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem || security.RunAsUser == nil || *security.RunAsUser != 0 || security.Capabilities == nil || !slices.Equal(security.Capabilities.Drop, []v1.Capability{"ALL"}) || len(container.Resources.Claims) != 0 || security.ProcMount != nil && *security.ProcMount != v1.DefaultProcMount {
		return errTaskCleanupRefused
	}
	seen := map[v1.Capability]bool{}
	for _, capability := range security.Capabilities.Add {
		if seen[capability] || !slices.Contains([]v1.Capability{"KILL", "SETUID", "SETGID", "SETPCAP"}, capability) {
			return errTaskCleanupRefused
		}
		seen[capability] = true
	}
	profile := security.SeccompProfile
	if profile == nil && pod.Spec.SecurityContext != nil {
		profile = pod.Spec.SecurityContext.SeccompProfile
	}
	if profile == nil || profile.Type != v1.SeccompProfileTypeRuntimeDefault || profile.LocalhostProfile != nil {
		return errTaskCleanupRefused
	}
	if pod.Spec.SecurityContext != nil && (len(pod.Spec.SecurityContext.Sysctls) != 0 || pod.Spec.SecurityContext.WindowsOptions != nil) || security.WindowsOptions != nil {
		return errTaskCleanupRefused
	}
	if !taskCleanupResources(container.Resources) || pod.Spec.Resources != nil && !taskCleanupResources(*pod.Spec.Resources) {
		return errTaskCleanupRefused
	}
	if !enrolled && (len(pod.Spec.Volumes) != 0 || len(container.VolumeMounts) != 0) {
		if len(pod.Spec.Volumes) != 2 || len(container.VolumeMounts) != 2 {
			return errTaskCleanupRefused
		}
		volumes := map[string]bool{}
		for _, volume := range pod.Spec.Volumes {
			if volumes[volume.Name] || volume.Name != "workspace" && volume.Name != "tmp" || volume.EmptyDir == nil || volume.EmptyDir.Medium != "" || volume.EmptyDir.SizeLimit != nil && volume.EmptyDir.SizeLimit.Sign() <= 0 || !reflect.DeepEqual(volume.VolumeSource, v1.VolumeSource{EmptyDir: volume.EmptyDir}) {
				return errTaskCleanupRefused
			}
			volumes[volume.Name] = true
		}
		mounts := map[string]bool{}
		for _, mount := range container.VolumeMounts {
			path := "/workspace"
			if mount.Name == "tmp" {
				path = "/tmp"
			}
			if mounts[mount.Name] || !volumes[mount.Name] || !reflect.DeepEqual(mount, v1.VolumeMount{Name: mount.Name, MountPath: path}) {
				return errTaskCleanupRefused
			}
			mounts[mount.Name] = true
		}
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != "sandbox" || status.RestartCount != 0 {
			return errTaskCleanupRefused
		}
	}
	if h.descriptor.Profile != "" && taskCleanupPodHash(pod) != h.descriptor.Profile {
		return errTaskCleanupRefused
	}
	return nil
}

func taskCleanupResources(resources v1.ResourceRequirements) bool {
	if len(resources.Claims) != 0 {
		return false
	}
	for _, values := range []v1.ResourceList{resources.Requests, resources.Limits} {
		for name, value := range values {
			if name != v1.ResourceCPU && name != v1.ResourceMemory && name != v1.ResourceEphemeralStorage || value.Sign() < 0 {
				return false
			}
		}
	}
	for name, request := range resources.Requests {
		if limit, ok := resources.Limits[name]; ok && request.Cmp(limit) > 0 {
			return false
		}
	}
	return true
}
