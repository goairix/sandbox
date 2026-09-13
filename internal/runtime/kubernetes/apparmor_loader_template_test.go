package kubernetes

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestAppArmorLoaderGateRejectsOlderTemplateOnlyReadyPod(t *testing.T) {
	for _, name := range []string{"readiness probe", "container security", "volumes", "mounts", "pod security", "service account", "automount", "template annotation", "container env"} {
		t.Run(name, func(t *testing.T) {
			ds, pod, profile := loaderGateFixture()
			yes := true
			switch name {
			case "readiness probe":
				ds.Spec.Template.Spec.Containers[0].ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"apparmor-loader", "readiness"}}}}
			case "container security":
				ds.Spec.Template.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: &yes}
			case "volumes":
				ds.Spec.Template.Spec.Volumes = []corev1.Volume{{Name: "policy", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "new-policy"}}}}}
			case "mounts":
				ds.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "policy", MountPath: "/policy"}}
			case "pod security":
				uid := int64(0)
				ds.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsUser: &uid}
			case "service account":
				ds.Spec.Template.Spec.ServiceAccountName = "new-account"
			case "automount":
				ds.Spec.Template.Spec.AutomountServiceAccountToken = &yes
			case "template annotation":
				ds.Spec.Template.Annotations["template-version"] = "new"
			case "container env":
				ds.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "NEW_POLICY", Value: "new"}}
			}
			ready, err := appArmorLoaderObservationReady(ds, []corev1.Pod{*pod}, profile, map[string]string{"kubernetes.io/os": "linux"})
			require.True(t, err != nil || !ready, "old template Pod accepted")
		})
	}
}
