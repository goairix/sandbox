package kubernetes

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/goairix/sandbox/internal/runtime"
)

// createPod creates a sandbox pod from the given spec.
func createPod(ctx context.Context, client kubernetes.Interface, namespace string, spec runtime.SandboxSpec) (*corev1.Pod, error) {
	labels := map[string]string{
		"app":             "sandbox",
		"sandbox.id":      spec.ID,
		"sandbox.managed": "true",
	}
	for k, v := range spec.Labels {
		labels[k] = v
	}

	resources := corev1.ResourceRequirements{}
	falseVal := false
	if spec.Memory != "" || spec.CPU != "" {
		resources.Limits = corev1.ResourceList{}
		resources.Requests = corev1.ResourceList{}
		if spec.Memory != "" {
			mem, err := resource.ParseQuantity(spec.Memory)
			if err != nil {
				return nil, fmt.Errorf("parse memory quantity %q: %w", spec.Memory, err)
			}
			resources.Limits[corev1.ResourceMemory] = mem
			req := mem
			if spec.MemoryRequest != "" {
				if req, err = resource.ParseQuantity(spec.MemoryRequest); err != nil {
					return nil, fmt.Errorf("parse memory request %q: %w", spec.MemoryRequest, err)
				}
			}
			resources.Requests[corev1.ResourceMemory] = req
		}
		if spec.CPU != "" {
			cpu, err := resource.ParseQuantity(spec.CPU)
			if err != nil {
				return nil, fmt.Errorf("parse cpu quantity %q: %w", spec.CPU, err)
			}
			resources.Limits[corev1.ResourceCPU] = cpu
			req := cpu
			if spec.CPURequest != "" {
				if req, err = resource.ParseQuantity(spec.CPURequest); err != nil {
					return nil, fmt.Errorf("parse cpu request %q: %w", spec.CPURequest, err)
				}
			}
			resources.Requests[corev1.ResourceCPU] = req
		}
	}

	if spec.PidLimit > 0 {
		if resources.Limits == nil {
			resources.Limits = corev1.ResourceList{}
		}
		resources.Limits[corev1.ResourceName("pids")] = *resource.NewQuantity(int64(spec.PidLimit), resource.DecimalSI)
	}

	securityContext := &corev1.SecurityContext{
		ReadOnlyRootFilesystem:   &spec.ReadOnlyRootFS,
		AllowPrivilegeEscalation: &falseVal,
	}
	if spec.RunAsUser > 0 {
		securityContext.RunAsUser = &spec.RunAsUser
	}

	// Determine workspace volume source: HostPath if a /workspace mount is
	// specified, otherwise EmptyDir with optional disk quota.
	workspaceEmptyDir := &corev1.EmptyDirVolumeSource{}
	if spec.Disk != "" {
		diskQty, err := resource.ParseQuantity(spec.Disk)
		if err != nil {
			return nil, fmt.Errorf("parse disk quantity %q: %w", spec.Disk, err)
		}
		workspaceEmptyDir.SizeLimit = &diskQty
	}
	workspaceVolume := corev1.VolumeSource{EmptyDir: workspaceEmptyDir}
	for _, m := range spec.Mounts {
		if m.ContainerPath == "/workspace" {
			hostPathType := corev1.HostPathDirectory
			workspaceVolume = corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: m.HostPath,
					Type: &hostPathType,
				},
			}
			break
		}
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.ID,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			// Disable SA token mount and K8s service env injection to avoid
			// leaking cluster topology and credentials into the sandbox.
			AutomountServiceAccountToken: &falseVal,
			EnableServiceLinks:           &falseVal,
			// Pod-level seccomp: restrict syscalls to the runtime default allowlist.
			SecurityContext: &corev1.PodSecurityContext{
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			// Use public DNS instead of CoreDNS to prevent cluster service name
			// resolution and avoid leaking cluster topology via search domains.
			DNSPolicy: corev1.DNSNone,
			DNSConfig: &corev1.PodDNSConfig{
				Nameservers: []string{"8.8.8.8", "1.1.1.1"},
			},
			Containers: []corev1.Container{
				{
					Name:            "sandbox",
					Image:           spec.Image,
					Command:         []string{"sleep", "infinity"},
					WorkingDir:      "/workspace",
					Resources:       resources,
					SecurityContext: securityContext,
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "workspace",
							MountPath: "/workspace",
						},
						{
							Name:      "tmp",
							MountPath: "/tmp",
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name:         "workspace",
					VolumeSource: workspaceVolume,
				},
				{
					Name: "tmp",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{
							SizeLimit: resource.NewQuantity(50*1024*1024, resource.BinarySI),
						},
					},
				},
			},
		},
	}

	created, err := client.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create pod: %w", err)
	}
	return created, nil
}

// deletePod deletes a pod by name.
func deletePod(ctx context.Context, client kubernetes.Interface, namespace, name string) error {
	return client.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

// getPod retrieves a pod by name.
func getPod(ctx context.Context, client kubernetes.Interface, namespace, name string) (*corev1.Pod, error) {
	return client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
}

// waitForPodReady waits until the pod is in Running phase.
func waitForPodReady(ctx context.Context, client kubernetes.Interface, namespace, name string, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timeout waiting for pod %s to be ready", name)
		case <-ticker.C:
			pod, err := getPod(ctx, client, namespace, name)
			if err != nil {
				continue
			}
			if pod.Status.Phase == corev1.PodRunning {
				return nil
			}
			if pod.Status.Phase == corev1.PodFailed {
				return fmt.Errorf("pod %s failed", name)
			}
		}
	}
}

// podStateString converts pod phase to a state string.
func podStateString(phase corev1.PodPhase) string {
	switch phase {
	case corev1.PodRunning:
		return "running"
	case corev1.PodPending:
		return "creating"
	case corev1.PodSucceeded:
		return "stopped"
	case corev1.PodFailed:
		return "error"
	default:
		return "unknown"
	}
}
