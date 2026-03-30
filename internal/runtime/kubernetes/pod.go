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
	if spec.Memory != "" || spec.CPU != "" {
		resources.Limits = corev1.ResourceList{}
		resources.Requests = corev1.ResourceList{}
		if spec.Memory != "" {
			mem := resource.MustParse(spec.Memory)
			resources.Limits[corev1.ResourceMemory] = mem
			resources.Requests[corev1.ResourceMemory] = mem
		}
		if spec.CPU != "" {
			cpu := resource.MustParse(spec.CPU)
			resources.Limits[corev1.ResourceCPU] = cpu
			resources.Requests[corev1.ResourceCPU] = cpu
		}
	}

	securityContext := &corev1.SecurityContext{
		ReadOnlyRootFilesystem: &spec.ReadOnlyRootFS,
	}
	if spec.RunAsUser > 0 {
		securityContext.RunAsUser = &spec.RunAsUser
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.ID,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
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
					Name: "workspace",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
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
