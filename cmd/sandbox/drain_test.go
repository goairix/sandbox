package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestDrainKubernetesDeploymentDisablesHPAAndWaitsForAPIPods(t *testing.T) {
	replicas := int32(2)
	client := kubefake.NewSimpleClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "sandbox-api", Namespace: "sandbox-fuse"},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "sandbox", "release": "sandbox"}},
			},
			Status: appsv1.DeploymentStatus{Replicas: 2},
		},
		&autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-api", Namespace: "sandbox-fuse"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-a", Namespace: "sandbox-fuse", Labels: map[string]string{"app": "sandbox", "release": "sandbox"}, UID: types.UID("api-a")}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "sandbox-fuse", Labels: map[string]string{"app": "other"}}},
	)
	client.PrependReactor("update", "deployments", func(action ktesting.Action) (bool, runtime.Object, error) {
		updated := action.(ktesting.UpdateAction).GetObject().(*appsv1.Deployment).DeepCopy()
		require.Equal(t, int32(0), *updated.Spec.Replicas)
		updated.Status.Replicas = 0
		require.NoError(t, client.Tracker().Update(appsv1.SchemeGroupVersion.WithResource("deployments"), updated, "sandbox-fuse"))
		require.NoError(t, client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), "sandbox-fuse", "api-a"))
		return true, updated, nil
	})

	require.NoError(t, drainKubernetesDeployment(context.Background(), client, "sandbox-fuse", "sandbox-api", "sandbox-api", time.Millisecond))
	_, err := client.AutoscalingV2().HorizontalPodAutoscalers("sandbox-fuse").Get(context.Background(), "sandbox-api", metav1.GetOptions{})
	require.Error(t, err)
	deployment, err := client.AppsV1().Deployments("sandbox-fuse").Get(context.Background(), "sandbox-api", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, int32(0), *deployment.Spec.Replicas)
	_, err = client.CoreV1().Pods("sandbox-fuse").Get(context.Background(), "unrelated", metav1.GetOptions{})
	require.NoError(t, err)
}

func TestDrainKubernetesDeploymentHonorsCancellationWhilePodRemains(t *testing.T) {
	replicas := int32(1)
	client := kubefake.NewSimpleClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "sandbox-api", Namespace: "sandbox-fuse"},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "sandbox"}},
			},
			Status: appsv1.DeploymentStatus{Replicas: 1},
		},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-a", Namespace: "sandbox-fuse", Labels: map[string]string{"app": "sandbox"}}},
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	err := drainKubernetesDeployment(ctx, client, "sandbox-fuse", "sandbox-api", "", time.Millisecond)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
