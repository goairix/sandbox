package kubernetes

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestAppArmorLoaderGateExplicitPriorityClass(t *testing.T) {
	for _, name := range []string{
		"priority and policy admission", "priority admission", "policy admission",
		"class drift", "class missing", "empty class global admission",
		"empty class zero defaults", "empty class nonzero priority", "empty class nondefault policy",
		"explicit matching priority and policy", "explicit priority with policy admission", "explicit policy with priority admission",
		"explicit priority drift", "explicit policy drift",
		"security drift", "volume drift", "probe drift", "token drift", "mount drift",
	} {
		t.Run(name, func(t *testing.T) {
			ds, pod, profile := loaderGateFixture()
			ds.Spec.Template.Spec.PriorityClassName = "loader-trusted"
			pod.Spec.PriorityClassName = "loader-trusted"
			priority, otherPriority := int32(1000), int32(2000)
			policy, otherPolicy := corev1.PreemptNever, corev1.PreemptLowerPriority
			pod.Spec.Priority = &priority
			pod.Spec.PreemptionPolicy = &policy
			wantReady := false
			switch name {
			case "priority and policy admission":
				wantReady = true
			case "priority admission":
				pod.Spec.PreemptionPolicy = nil
				wantReady = true
			case "policy admission":
				pod.Spec.Priority = nil
				wantReady = true
			case "class drift":
				pod.Spec.PriorityClassName = "another-class"
			case "class missing":
				pod.Spec.PriorityClassName = ""
			case "empty class global admission":
				ds.Spec.Template.Spec.PriorityClassName = ""
			case "empty class zero defaults":
				ds.Spec.Template.Spec.PriorityClassName = ""
				pod.Spec.PriorityClassName = ""
				priority = 0
				policy = corev1.PreemptLowerPriority
				wantReady = true
			case "empty class nonzero priority":
				ds.Spec.Template.Spec.PriorityClassName = ""
				pod.Spec.PriorityClassName = ""
				pod.Spec.PreemptionPolicy = nil
			case "empty class nondefault policy":
				ds.Spec.Template.Spec.PriorityClassName = ""
				pod.Spec.PriorityClassName = ""
				pod.Spec.Priority = nil
			case "explicit matching priority and policy":
				ds.Spec.Template.Spec.Priority = &priority
				ds.Spec.Template.Spec.PreemptionPolicy = &policy
				wantReady = true
			case "explicit priority with policy admission":
				ds.Spec.Template.Spec.Priority = &priority
				wantReady = true
			case "explicit policy with priority admission":
				ds.Spec.Template.Spec.PreemptionPolicy = &policy
				wantReady = true
			case "explicit priority drift":
				ds.Spec.Template.Spec.Priority = &otherPriority
			case "explicit policy drift":
				ds.Spec.Template.Spec.PreemptionPolicy = &otherPolicy
			case "security drift":
				yes := true
				pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: &yes}
			case "volume drift":
				pod.Spec.Volumes = []corev1.Volume{{Name: "extra-host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}}}
			case "probe drift":
				ds.Spec.Template.Spec.Containers[0].ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"apparmor-loader", "readiness"}}}}
			case "token drift":
				no, yes := false, true
				ds.Spec.Template.Spec.AutomountServiceAccountToken = &no
				pod.Spec.AutomountServiceAccountToken = &yes
			case "mount drift":
				pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "policy", MountPath: "/policy"}}
			}
			ready, err := appArmorLoaderObservationReady(ds, []corev1.Pod{*pod}, profile, map[string]string{"kubernetes.io/os": "linux"})
			if wantReady {
				require.NoError(t, err)
				require.True(t, ready)
			} else {
				require.True(t, err != nil || !ready, "priority normalization must retain exact template safety")
			}
		})
	}
}
