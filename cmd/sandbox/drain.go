package main

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
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
		if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == 0 && deployment.Status.Replicas == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
