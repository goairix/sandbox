package kubernetes

import (
	"context"

	"github.com/goairix/sandbox/internal/runtime"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

// preparedPodCanRequestNormalDeletion is NOT termination evidence. All declared
// containers must be accounted for; any live/unknown/history-bearing status
// keeps the existing shutdown/fencer path. The common running path allocates 0.
func preparedPodCanRequestNormalDeletion(pod *corev1.Pod) bool {
	if pod == nil || pod.Spec.NodeName == "" {
		return false
	}
	for _, statuses := range [][]corev1.ContainerStatus{
		pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses, pod.Status.EphemeralContainerStatuses,
	} {
		for _, status := range statuses {
			if !containerStateTerminated(status) && !containerProvablyNeverStarted(status) {
				return false
			}
		}
	}
	return declaredContainersTerminated(pod.Spec.InitContainers, pod.Status.InitContainerStatuses, true) &&
		declaredContainersTerminated(pod.Spec.Containers, pod.Status.ContainerStatuses, true) &&
		declaredEphemeralContainersTerminated(pod.Spec.EphemeralContainers, pod.Status.EphemeralContainerStatuses, true)
}

func containerStateTerminated(status corev1.ContainerStatus) bool {
	return status.State.Terminated != nil && status.State.Running == nil && status.State.Waiting == nil
}

// confirmUnstartedPodTermination retains the finalizer while kubelet confirms
// the exact Pod's exit. A lost DELETE response is resolved only with reads; a
// missing/replaced Pod is never sufficient proof without the shutdown handshake.
func (r *Runtime) confirmUnstartedPodTermination(ctx context.Context, ref runtime.RuntimeRef, pod *corev1.Pod) *runtime.TerminationEvidence {
	if ctx.Err() != nil {
		return nil
	}
	bootstrap, err := exactFUSEPodBootstrap(pod, ref)
	if err != nil || pod.Namespace != r.namespace || !containsString(pod.Finalizers, fuseRuntimeCleanupFinalizer) {
		return nil
	}
	timeout := r.terminationTimeout
	if timeout <= 0 {
		timeout = defaultKubernetesControlTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var deleteErr error
	if pod.DeletionTimestamp == nil {
		// Even an uncertain result cannot authorize a second DELETE. The retained
		// object's deletion state and kubelet statuses must resolve the outcome.
		deleteErr = r.deleteUnstartedPod(waitCtx, pod)
	}
	for {
		if waitCtx.Err() != nil {
			return nil
		}
		current, err := r.getExactPod(waitCtx, ref)
		if waitCtx.Err() != nil || err != nil {
			return nil
		}
		currentBootstrap, identityErr := exactFUSEPodBootstrap(current, ref)
		if identityErr != nil || currentBootstrap != bootstrap ||
			current.Namespace != r.namespace ||
			current.Annotations[fusePrepareAttemptAnnotation] != pod.Annotations[fusePrepareAttemptAnnotation] ||
			current.Spec.NodeName != pod.Spec.NodeName ||
			!containsString(current.Finalizers, fuseRuntimeCleanupFinalizer) {
			return nil
		}
		if deleteErr != nil && current.DeletionTimestamp == nil {
			return nil
		}
		if current.DeletionTimestamp != nil && preparedPodContainersTerminated(current) {
			return &runtime.TerminationEvidence{RuntimeUID: ref.UID, NodeName: pod.Spec.NodeName, ProcessExited: true}
		}
		if err := waitPoll(waitCtx, r.pollInterval); err != nil {
			return nil
		}
	}
}

func (r *Runtime) deleteUnstartedPod(ctx context.Context, pod *corev1.Pod) error {
	client := r.client.CoreV1().RESTClient()
	if concrete, ok := client.(*rest.RESTClient); ok && concrete == nil {
		client = nil
	}
	if client == nil {
		return r.deleteExactPod(ctx, pod)
	}
	uid := pod.UID
	options := metav1.DeleteOptions{
		GracePeriodSeconds: pod.Spec.TerminationGracePeriodSeconds,
		Preconditions:      &metav1.Preconditions{UID: &uid},
	}
	// client-go's default Retry-After handling must not replay an uncertain
	// DELETE. Resolve its result with the retained exact object's status only.
	return client.Delete().Namespace(r.namespace).Resource("pods").Name(pod.Name).Body(&options).
		WarningHandlerWithContext(rest.NoWarnings{}).MaxRetries(0).Do(ctx).Error()
}
