package kubernetes

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestAppArmorLoaderGateNormalizesOnlyKnownDaemonSetScheduling(t *testing.T) {
	for _, name := range []string{"controller mutations", "wrong node affinity", "extra toleration", "extra pod affinity", "template required affinity"} {
		t.Run(name, func(t *testing.T) {
			ds, pod, profile := loaderGateFixture()
			pod.Spec.NodeName = "node-a"
			pod.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{"node-a"}}}}}}}}
			pod.Spec.Tolerations = []corev1.Toleration{{Key: corev1.TaintNodeNotReady, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute}, {Key: corev1.TaintNodeUnreachable, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute}, {Key: corev1.TaintNodeDiskPressure, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}, {Key: corev1.TaintNodeMemoryPressure, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}, {Key: corev1.TaintNodePIDPressure, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}, {Key: corev1.TaintNodeUnschedulable, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}
			switch name {
			case "wrong node affinity":
				pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchFields[0].Values = []string{"node-b"}
			case "extra toleration":
				pod.Spec.Tolerations = append(pod.Spec.Tolerations, corev1.Toleration{Key: "tenant", Operator: corev1.TolerationOpExists})
			case "extra pod affinity":
				pod.Spec.Affinity.PodAffinity = &corev1.PodAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "tenant"}}}
			case "template required affinity":
				ds.Spec.Template.Spec.Affinity = pod.Spec.Affinity.DeepCopy()
			}
			ready, err := appArmorLoaderObservationReady(ds, []corev1.Pod{*pod}, profile, map[string]string{"kubernetes.io/os": "linux"})
			if name == "controller mutations" {
				require.NoError(t, err)
				require.True(t, ready)
			} else {
				require.True(t, err != nil || !ready)
			}
		})
	}
}
