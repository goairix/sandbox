package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

var drainCiliumNetworkPolicyGVR = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies"}

const (
	backendFingerprintAnnotation = "sandbox.huaxisy.com/backend-fingerprint"
	drainProtocolAnnotation      = "sandbox.huaxisy.com/drain-protocol"
	cleanupProtocolAnnotation    = "sandbox.huaxisy.com/cleanup-protocol"
)

func drainKubernetesDeployment(ctx context.Context, client kubernetes.Interface, namespace, deploymentName, hpaName string, pollInterval time.Duration) error {
	if client == nil || namespace == "" || deploymentName == "" || pollInterval <= 0 {
		return fmt.Errorf("invalid Kubernetes deployment drain configuration")
	}
	if hpaName != "" {
		hpa, err := client.AutoscalingV2().HorizontalPodAutoscalers(namespace).Get(ctx, hpaName, metav1.GetOptions{})
		if err == nil {
			uid := hpa.UID
			if err := client.AutoscalingV2().HorizontalPodAutoscalers(namespace).Delete(ctx, hpaName, metav1.DeleteOptions{
				Preconditions: &metav1.Preconditions{UID: &uid},
			}); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("disable Kubernetes API autoscaling before drain: %w", err)
			}
		} else if !apierrors.IsNotFound(err) {
			return fmt.Errorf("read Kubernetes API autoscaler before drain: %w", err)
		}
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deployment, err := client.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		zero := int32(0)
		deployment.Spec.Replicas = &zero
		_, err = client.AppsV1().Deployments(namespace).Update(ctx, deployment, metav1.UpdateOptions{})
		return err
	}); err != nil {
		return fmt.Errorf("scale Kubernetes API deployment to zero: %w", err)
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		deployment, err := client.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read Kubernetes API deployment during drain: %w", err)
		}
		selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
		if err != nil {
			return fmt.Errorf("resolve Kubernetes API Pod selector during drain: %w", err)
		}
		pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
		if err != nil {
			return fmt.Errorf("list Kubernetes API Pods during drain: %w", err)
		}
		if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == 0 && deployment.Status.Replicas == 0 && len(pods.Items) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func resumeKubernetesDeployment(ctx context.Context, client kubernetes.Interface, namespace, deploymentName string, replicas int32, pollInterval time.Duration) (bool, error) {
	if client == nil || namespace == "" || deploymentName == "" || replicas < 0 || pollInterval <= 0 {
		return false, fmt.Errorf("invalid Kubernetes deployment resume configuration")
	}
	if replicas == 0 {
		return false, nil
	}
	resumed := false
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deployment, err := client.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas > 0 {
			return nil
		}
		deployment.Spec.Replicas = &replicas
		_, err = client.AppsV1().Deployments(namespace).Update(ctx, deployment, metav1.UpdateOptions{})
		if err == nil {
			resumed = true
		}
		return err
	}); err != nil {
		return false, fmt.Errorf("scale Kubernetes API deployment after drain: %w", err)
	}
	if !resumed {
		return false, nil
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		deployment, err := client.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
		if err != nil {
			return true, fmt.Errorf("read Kubernetes API deployment during resume: %w", err)
		}
		if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas >= replicas &&
			deployment.Status.ObservedGeneration >= deployment.Generation &&
			deployment.Status.ReadyReplicas >= replicas && deployment.Status.AvailableReplicas >= replicas {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-ticker.C:
		}
	}
}

func kubernetesBackendFingerprintMatches(ctx context.Context, client kubernetes.Interface, namespace, deploymentName, desired string) (bool, error) {
	return kubernetesDeploymentTemplateAnnotationMatches(ctx, client, namespace, deploymentName, backendFingerprintAnnotation, desired)
}

func kubernetesDrainProtocolMatches(ctx context.Context, client kubernetes.Interface, namespace, deploymentName, desired string) (bool, error) {
	return kubernetesDeploymentTemplateAnnotationMatches(ctx, client, namespace, deploymentName, drainProtocolAnnotation, desired)
}

func kubernetesCleanupProtocolMatches(ctx context.Context, client kubernetes.Interface, namespace, deploymentName, desired string) (bool, error) {
	return kubernetesDeploymentTemplateAnnotationMatches(ctx, client, namespace, deploymentName, cleanupProtocolAnnotation, desired)
}

func shouldDrainKubernetesRelease(backendMatches, cleanupProtocolMatches bool) bool {
	return !backendMatches || !cleanupProtocolMatches
}

func kubernetesDeploymentTemplateAnnotationMatches(ctx context.Context, client kubernetes.Interface, namespace, deploymentName, annotation, desired string) (bool, error) {
	if client == nil || namespace == "" || deploymentName == "" || annotation == "" || desired == "" {
		return false, fmt.Errorf("invalid Kubernetes Deployment annotation comparison")
	}
	deployment, err := client.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read installed Kubernetes Deployment annotation: %w", err)
	}
	return deployment.Spec.Template.Annotations[annotation] == desired, nil
}

func auditKubernetesDrainedResources(ctx context.Context, client kubernetes.Interface, dynamicClient dynamic.Interface, namespace string) error {
	if client == nil || namespace == "" {
		return fmt.Errorf("invalid Kubernetes drain audit configuration")
	}
	const selector = "sandbox.managed=true"
	var auditErr error
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		auditErr = errors.Join(auditErr, fmt.Errorf("list managed sandbox Pods: %w", err))
	} else if len(pods.Items) != 0 {
		auditErr = errors.Join(auditErr, fmt.Errorf("drain blocked by %d managed sandbox Pods", len(pods.Items)))
	}
	policies, err := client.NetworkingV1().NetworkPolicies(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		auditErr = errors.Join(auditErr, fmt.Errorf("list managed sandbox NetworkPolicies: %w", err))
	} else if len(policies.Items) != 0 {
		auditErr = errors.Join(auditErr, fmt.Errorf("drain blocked by %d managed sandbox NetworkPolicies", len(policies.Items)))
	}
	if dynamicClient != nil {
		cilium, err := dynamicClient.Resource(drainCiliumNetworkPolicyGVR).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil && !apierrors.IsNotFound(err) {
			auditErr = errors.Join(auditErr, fmt.Errorf("list managed CiliumNetworkPolicies: %w", err))
		} else if err == nil && len(cilium.Items) != 0 {
			auditErr = errors.Join(auditErr, fmt.Errorf("drain blocked by %d managed CiliumNetworkPolicies", len(cilium.Items)))
		}
	}
	return auditErr
}
