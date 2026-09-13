package kubernetes

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func loaderGateFixture() (*appsv1.DaemonSet, *corev1.Pod, string) {
	digest := strings.Repeat("a", 64)
	profile := "sandbox-fuse-" + digest
	yes := true
	template := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "loader", appArmorDigestKey: digest[:63]}, Annotations: map[string]string{appArmorDigestKey: digest}}, Spec: corev1.PodSpec{NodeSelector: map[string]string{"kubernetes.io/os": "linux"}, Containers: []corev1.Container{{Name: "apparmor-loader", Image: "loader:v1.0.0", Args: []string{"--profile-name=" + profile, "--profile-digest=" + digest}}}}}
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "release-loader", Namespace: "release-ns", UID: types.UID("ds-current"), Generation: 2, Annotations: map[string]string{appArmorDigestKey: digest}}, Spec: appsv1.DaemonSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "loader"}}, Template: template}, Status: appsv1.DaemonSetStatus{ObservedGeneration: 2, DesiredNumberScheduled: 3, NumberReady: 1}}
	pod := &corev1.Pod{ObjectMeta: *template.ObjectMeta.DeepCopy(), Spec: *template.Spec.DeepCopy(), Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	pod.Name = "loader-current"
	pod.Namespace = ds.Namespace
	pod.UID = "pod-current"
	pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: ds.Name, UID: ds.UID, Controller: &yes}}
	return ds, pod, profile
}

func TestAppArmorLoaderGateCurrentTemplate(t *testing.T) {
	for _, name := range []string{"one of three ready", "empty uid", "stale generation", "zero desired", "metadata digest", "template digest", "pod digest", "owner uid", "noncontroller owner", "old image", "old args", "old labels", "deleting", "not ready", "terminal", "wrong selector", "duplicate profile argument"} {
		t.Run(name, func(t *testing.T) {
			ds, pod, profile := loaderGateFixture()
			switch name {
			case "empty uid":
				ds.UID = ""
			case "stale generation":
				ds.Status.ObservedGeneration = 1
			case "zero desired":
				ds.Status.DesiredNumberScheduled = 0
			case "metadata digest":
				ds.Annotations[appArmorDigestKey] = "old"
			case "template digest":
				ds.Spec.Template.Annotations[appArmorDigestKey] = "old"
			case "pod digest":
				pod.Annotations[appArmorDigestKey] = "old"
			case "owner uid":
				pod.OwnerReferences[0].UID = "old"
			case "noncontroller owner":
				no := false
				pod.OwnerReferences[0].Controller = &no
			case "old image":
				pod.Spec.Containers[0].Image = "loader:v0.9.0"
			case "old args":
				pod.Spec.Containers[0].Args = append(pod.Spec.Containers[0].Args, "--old")
			case "old labels":
				pod.Labels["app"] = "old"
			case "deleting":
				now := metav1.Now()
				pod.DeletionTimestamp = &now
			case "not ready":
				pod.Status.Conditions[0].Status = corev1.ConditionFalse
			case "terminal":
				pod.Status.Phase = corev1.PodSucceeded
			case "wrong selector":
				pod.Spec.NodeSelector["zone"] = "old"
			case "duplicate profile argument":
				ds.Spec.Template.Spec.Containers[0].Args = append(ds.Spec.Template.Spec.Containers[0].Args, "--profile-name="+profile)
				pod.Spec = *ds.Spec.Template.Spec.DeepCopy()
			}
			ready, err := appArmorLoaderObservationReady(ds, []corev1.Pod{*pod}, profile, map[string]string{"kubernetes.io/os": "linux"})
			if name == "one of three ready" {
				require.NoError(t, err)
				require.True(t, ready)
			} else {
				require.True(t, err != nil || !ready, "accepted incompatible loader")
			}
		})
	}
}

func TestAppArmorLoaderGateTimeoutIdentityAndSelector(t *testing.T) {
	ds, pod, profile := loaderGateFixture()
	client := kubefake.NewSimpleClientset(ds, pod)
	r := &Runtime{client: client, pollInterval: time.Millisecond}
	WithAppArmorLoader(ds.Name, ds.Namespace, profile, time.Second)(r)
	require.NoError(t, r.validateAppArmorLoaderOptions())
	require.NoError(t, r.waitForAppArmorLoader(context.Background()))
	for _, action := range client.Actions() {
		if action.GetVerb() == "list" && action.GetResource().Resource == "pods" {
			require.Equal(t, ds.Namespace, action.GetNamespace())
			require.Equal(t, "app=loader", action.(ktesting.ListAction).GetListRestrictions().Labels.String())
		}
	}
	require.Equal(t, profile, r.appArmorProfile)
	ds.Status.ObservedGeneration = 1
	client = kubefake.NewSimpleClientset(ds, pod)
	gets := 0
	client.PrependReactor("get", "daemonsets", func(ktesting.Action) (bool, k8sruntime.Object, error) {
		gets++
		copy := ds.DeepCopy()
		if gets >= 2 {
			copy.UID = "replacement"
		}
		return true, copy, nil
	})
	r.client = client
	r.appArmorLoaderTimeout = 100 * time.Millisecond
	require.ErrorContains(t, r.waitForAppArmorLoader(context.Background()), "identity changed")
	client = kubefake.NewSimpleClientset(ds)
	r.client = client
	r.appArmorLoaderTimeout = 5 * time.Millisecond
	require.ErrorIs(t, r.waitForAppArmorLoader(context.Background()), context.DeadlineExceeded)
	disabled := &Runtime{client: kubefake.NewSimpleClientset()}
	require.NoError(t, disabled.waitForAppArmorLoader(context.Background()))
	require.Empty(t, disabled.client.(*kubefake.Clientset).Actions())
}

func TestAppArmorLoaderOptionsRejectInvalidAndMergeLinux(t *testing.T) {
	_, _, profile := loaderGateFixture()
	for _, name := range []string{"valid", "profile", "namespace", "timeout", "selector conflict"} {
		t.Run(name, func(t *testing.T) {
			r := &Runtime{}
			p := profile
			ns := "release-ns"
			timeout := time.Second
			switch name {
			case "profile":
				p = "manual"
			case "namespace":
				ns = ""
			case "timeout":
				timeout = 0
			case "selector conflict":
				r.nodeSelector = map[string]string{"kubernetes.io/os": "windows"}
			}
			WithAppArmorLoader("release-loader", ns, p, timeout)(r)
			err := r.validateAppArmorLoaderOptions()
			if name != "valid" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "linux", r.nodeSelector["kubernetes.io/os"])
		})
	}
}
