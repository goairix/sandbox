package kubecontract_test

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"

	"github.com/goairix/sandbox/internal/kubecontract"
	"github.com/goairix/sandbox/internal/runtime"
	kube "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func profilePod(t *testing.T) *v1.Pod {
	t.Helper()
	key := ed25519.NewKeyFromSeed(make([]byte, 32)).Public().(ed25519.PublicKey)
	c := launcherconfig.Config{Version: 2, Binding: launcherconfig.Binding{Namespace: "/sandbox/control/authority/", AuthorityID: "authority", Target: "target", RestoreEpoch: "restore"}, Roots: []ed25519.PublicKey{key}, UID: 1000, GID: 1000, ContractDigest: strings.Repeat("a", 64), ClockSocket: "/run/sandbox-clock/clock.sock", ClockPublicKey: key, ClockAudience: "launcher", MaxActive: 4, Enrollment: launcherconfig.Enrollment{NodeUID: "22222222-2222-4222-8222-222222222222", NodeBootID: "33333333-3333-4333-8333-333333333333"}}
	c.ContractDigest, _ = kubecontract.EnrolledPlainContractDigest("example.test/sandbox@sha256:" + strings.Repeat("b", 64))
	wire, err := json.Marshal(c)
	require.NoError(t, err)
	pod, err := kube.BuildEnrolledPlainPod("runtimes", kube.EnrolledPlainPodRequest{Spec: runtime.SandboxSpec{ID: "pod-a", Image: "example.test/sandbox@sha256:" + strings.Repeat("b", 64), CPU: "500m", Memory: "512Mi", Disk: "100Mi", ReadOnlyRootFS: true, RunAsUser: 1000}, NodeName: "node-a", NodeUID: c.Enrollment.NodeUID, NodeBootID: c.Enrollment.NodeBootID, CreationIntentID: "44444444-4444-4444-8444-444444444444", Generation: 1, Config: wire})
	require.NoError(t, err)
	return pod
}

func TestEnrolledProfileRejectsSecurityAndIdentityMutation(t *testing.T) {
	pod := profilePod(t)
	require.NoError(t, kubecontract.ValidateEnrolledPlainPod(pod))
	for name, mutate := range map[string]func(*v1.Pod){
		"host source":     func(p *v1.Pod) { p.Spec.Volumes[3].HostPath.Path = "/run/other" },
		"host type":       func(p *v1.Pod) { kind := v1.HostPathDirectoryOrCreate; p.Spec.Volumes[3].HostPath.Type = &kind },
		"writable clock":  func(p *v1.Pod) { p.Spec.Containers[0].VolumeMounts[3].ReadOnly = false },
		"socket source":   func(p *v1.Pod) { p.Spec.Volumes[4].HostPath.Path = "/run/foreign.sock" },
		"writable socket": func(p *v1.Pod) { p.Spec.Containers[0].VolumeMounts[4].ReadOnly = false },
		"propagation": func(p *v1.Pod) {
			mode := v1.MountPropagationHostToContainer
			p.Spec.Containers[0].VolumeMounts[4].MountPropagation = &mode
		},
		"subpath":             func(p *v1.Pod) { p.Spec.Containers[0].VolumeMounts[4].SubPath = "socket" },
		"mount path":          func(p *v1.Pod) { p.Spec.Containers[0].VolumeMounts[2].MountPath = "/run" },
		"control memory":      func(p *v1.Pod) { p.Spec.Volumes[2].EmptyDir.Medium = "" },
		"control unbounded":   func(p *v1.Pod) { p.Spec.Volumes[2].EmptyDir.SizeLimit = nil },
		"control excess":      func(p *v1.Pod) { q := resource.MustParse("2Mi"); p.Spec.Volumes[2].EmptyDir.SizeLimit = &q },
		"workspace unbounded": func(p *v1.Pod) { p.Spec.Volumes[0].EmptyDir.SizeLimit = nil },
		"extra volume":        func(p *v1.Pod) { p.Spec.Volumes = append(p.Spec.Volumes, v1.Volume{Name: "other"}) },
		"command":             func(p *v1.Pod) { p.Spec.Containers[0].Command[0] = "/bin/sh" },
		"args":                func(p *v1.Pod) { p.Spec.Containers[0].Args = []string{"serve"} },
		"extra env": func(p *v1.Pod) {
			p.Spec.Containers[0].Env = append(p.Spec.Containers[0].Env, v1.EnvVar{Name: "TOKEN", Value: "secret"})
		},
		"env source": func(p *v1.Pod) { p.Spec.Containers[0].Env[0].ValueFrom = &v1.EnvVarSource{} },
		"node":       func(p *v1.Pod) { p.Spec.NodeName = "" },
		"node uid": func(p *v1.Pod) {
			p.Annotations[kubecontract.EnrolledNodeUIDAnnotation] = "55555555-5555-4555-8555-555555555555"
		},
		"node boot": func(p *v1.Pod) {
			p.Annotations[kubecontract.EnrolledNodeBootIDAnnotation] = "55555555-5555-4555-8555-555555555555"
		},
		"generation":         func(p *v1.Pod) { p.Annotations[kubecontract.EnrolledGenerationAnnotation] = "01" },
		"intent":             func(p *v1.Pod) { p.Annotations[kubecontract.EnrolledCreationIntentAnnotation] = "intent" },
		"config digest":      func(p *v1.Pod) { p.Annotations[kubecontract.EnrolledConfigDigestAnnotation] = strings.Repeat("0", 64) },
		"image":              func(p *v1.Pod) { p.Spec.Containers[0].Image = "sandbox:latest" },
		"unknown annotation": func(p *v1.Pod) { p.Annotations["foreign"] = "value" },
		"capability": func(p *v1.Pod) {
			p.Spec.Containers[0].SecurityContext.Capabilities.Add = append(p.Spec.Containers[0].SecurityContext.Capabilities.Add, "SYS_ADMIN")
		},
		"rootfs":         func(p *v1.Pod) { *p.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem = false },
		"privilege":      func(p *v1.Pod) { *p.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation = true },
		"seccomp":        func(p *v1.Pod) { p.Spec.SecurityContext.SeccompProfile.Type = v1.SeccompProfileTypeUnconfined },
		"security field": func(p *v1.Pod) { p.Spec.SecurityContext.SupplementalGroups = []int64{1} },
		"host namespace": func(p *v1.Pod) { p.Spec.HostPID = true },
		"sa token":       func(p *v1.Pod) { *p.Spec.AutomountServiceAccountToken = true },
		"service links":  func(p *v1.Pod) { *p.Spec.EnableServiceLinks = true },
		"sidecar":        func(p *v1.Pod) { p.Spec.InitContainers = []v1.Container{{Name: "other"}} },
		"probe":          func(p *v1.Pod) { p.Spec.Containers[0].ReadinessProbe = &v1.Probe{} },
		"host port":      func(p *v1.Pod) { p.Spec.Containers[0].Ports = []v1.ContainerPort{{ContainerPort: 80, HostPort: 80}} },
		"lifecycle":      func(p *v1.Pod) { p.Spec.Containers[0].Lifecycle = &v1.Lifecycle{} },
		"resource claim": func(p *v1.Pod) { p.Spec.ResourceClaims = []v1.PodResourceClaim{{Name: "other"}} },
	} {
		t.Run(name, func(t *testing.T) {
			copy := pod.DeepCopy()
			mutate(copy)
			require.Error(t, kubecontract.ValidateEnrolledPlainPod(copy))
		})
	}
}

func TestEnrolledProfileCannotDowngrade(t *testing.T) {
	pod := profilePod(t)
	selected, err := kubecontract.SelectEnrolledPlainPodProfile(pod)
	require.NoError(t, err)
	require.True(t, selected)
	pod.Annotations[kubecontract.EnrolledProfileAnnotation] = "unknown"
	_, err = kubecontract.SelectEnrolledPlainPodProfile(pod)
	require.Error(t, err)
	delete(pod.Annotations, kubecontract.EnrolledProfileAnnotation)
	_, err = kubecontract.SelectEnrolledPlainPodProfile(pod)
	require.Error(t, err)
	pod.Annotations = nil
	_, err = kubecontract.SelectEnrolledPlainPodProfile(pod)
	require.Error(t, err)
	pod.Spec.Containers[0].Command = nil
	pod.Spec.Containers[0].Args = nil
	pod.Spec.Containers[0].Env = nil
	_, err = kubecontract.SelectEnrolledPlainPodProfile(pod)
	require.Error(t, err)
	selected, err = kubecontract.SelectEnrolledPlainPodProfile(&v1.Pod{})
	require.NoError(t, err)
	require.False(t, selected)
}

func TestEnrolledProfileAdmissionDefaults(t *testing.T) {
	pod := profilePod(t)
	seconds := int64(300)
	require.Equal(t, []v1.Toleration{{Key: "node.kubernetes.io/not-ready", Operator: v1.TolerationOpExists, Effect: v1.TaintEffectNoExecute, TolerationSeconds: &seconds}, {Key: "node.kubernetes.io/unreachable", Operator: v1.TolerationOpExists, Effect: v1.TaintEffectNoExecute, TolerationSeconds: &seconds}}, pod.Spec.Tolerations)
	priority, preemption := int32(0), v1.PreemptLowerPriority
	pod.Spec.Priority, pod.Spec.PreemptionPolicy = &priority, &preemption
	require.NoError(t, kubecontract.ValidateEnrolledPlainPod(pod))
	require.NotNil(t, pod.Spec.Priority, "validation must not mutate the Pod hash input")
	for name, mutate := range map[string]func(*v1.Pod){
		"toleration duration": func(p *v1.Pod) { *p.Spec.Tolerations[0].TolerationSeconds = 301 },
		"priority":            func(p *v1.Pod) { *p.Spec.Priority = 1 },
		"preemption":          func(p *v1.Pod) { *p.Spec.PreemptionPolicy = v1.PreemptNever },
		"priority class":      func(p *v1.Pod) { p.Spec.PriorityClassName = "system-node-critical" },
	} {
		t.Run(name, func(t *testing.T) {
			copy := pod.DeepCopy()
			mutate(copy)
			require.Error(t, kubecontract.ValidateEnrolledPlainPod(copy))
		})
	}
}

func TestEnrolledProfileImageContract(t *testing.T) {
	contract, err := kubecontract.EnrolledPlainContractDigest("example.test/sandbox@sha256:" + strings.Repeat("b", 64))
	require.NoError(t, err)
	require.Equal(t, "160e7289cf141e9ff0b286aed0b2274b3a98698db50290587c2889318d82e33d", contract)
	pod := profilePod(t)
	// Both self-reported digests may change together; original signed contract
	// must still prevent a different executable image from joining that birth.
	pod.Spec.Containers[0].Image = "example.test/sandbox@sha256:" + strings.Repeat("c", 64)
	pod.Annotations[kubecontract.EnrolledImageDigestAnnotation] = strings.Repeat("c", 64)
	require.Error(t, kubecontract.ValidateEnrolledPlainPod(pod))
}
