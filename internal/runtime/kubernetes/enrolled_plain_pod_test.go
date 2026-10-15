package kubernetes

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"

	"github.com/goairix/sandbox/internal/kubecontract"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	"github.com/stretchr/testify/require"
)

func enrolledPodRequest(t *testing.T) EnrolledPlainPodRequest {
	t.Helper()
	key := ed25519.NewKeyFromSeed(make([]byte, 32)).Public().(ed25519.PublicKey)
	c := launcherconfig.Config{Version: 2, Binding: launcherconfig.Binding{Namespace: "/sandbox/control/authority/", AuthorityID: "authority", Target: "target", RestoreEpoch: "restore"}, Roots: []ed25519.PublicKey{key}, UID: 1000, GID: 1000, ContractDigest: strings.Repeat("a", 64), ClockSocket: "/run/sandbox-clock/clock.sock", ClockPublicKey: key, ClockAudience: "launcher", MaxActive: 4, Enrollment: launcherconfig.Enrollment{NodeUID: "22222222-2222-4222-8222-222222222222", NodeBootID: "33333333-3333-4333-8333-333333333333"}}
	c.ContractDigest, _ = kubecontract.EnrolledPlainContractDigest("example.test/sandbox@sha256:" + strings.Repeat("b", 64))
	wire, err := json.Marshal(c)
	require.NoError(t, err)
	return EnrolledPlainPodRequest{Spec: runtime.SandboxSpec{ID: "runtime-a", Image: "example.test/sandbox@sha256:" + strings.Repeat("b", 64), CPU: "500m", Memory: "512Mi", Disk: "100Mi", TmpDisk: "50Mi", RunAsUser: 1000, ReadOnlyRootFS: true, Labels: map[string]string{"sandbox.id": "logical-a", "team": "test"}}, NodeName: "node-a", NodeUID: c.Enrollment.NodeUID, NodeBootID: c.Enrollment.NodeBootID, CreationIntentID: "44444444-4444-4444-8444-444444444444", Generation: 1, Config: wire}
}

func TestEnrolledBuilderFixedLaunchAndFiveVolumes(t *testing.T) {
	r := enrolledPodRequest(t)
	pod, err := BuildEnrolledPlainPod("runtimes", r)
	require.NoError(t, err)
	require.Equal(t, "node-a", pod.Spec.NodeName)
	require.Equal(t, []string{"/usr/local/bin/sandbox-launcher"}, pod.Spec.Containers[0].Command)
	require.Equal(t, []string{"serve-provisioned"}, pod.Spec.Containers[0].Args)
	require.Len(t, pod.Spec.Volumes, 5)
	require.Len(t, pod.Spec.Containers[0].VolumeMounts, 5)
	require.Equal(t, "logical-a", pod.Labels["sandbox.id"])
	require.Equal(t, "true", pod.Labels["sandbox.managed"])
	require.Equal(t, "plain", pod.Labels["sandbox.workspace.mode"])
	require.Equal(t, "test", pod.Labels["team"])
	r.Config[0] = '!'
	r.Spec.Labels["team"] = "changed"
	require.Equal(t, "test", pod.Labels["team"], "builder must not alias caller metadata")
	require.Equal(t, byte('{'), pod.Spec.Containers[0].Env[0].Value[0])
}

func TestEnrolledBuilderRejectsUnsupportedAndUnboundInputs(t *testing.T) {
	for name, mutate := range map[string]func(*EnrolledPlainPodRequest){
		"node config mismatch": func(r *EnrolledPlainPodRequest) { r.NodeUID = "55555555-5555-4555-8555-555555555555" },
		"boot config mismatch": func(r *EnrolledPlainPodRequest) { r.NodeBootID = "55555555-5555-4555-8555-555555555555" },
		"bad intent":           func(r *EnrolledPlainPodRequest) { r.CreationIntentID = "intent" },
		"bad generation":       func(r *EnrolledPlainPodRequest) { r.Generation = 0 },
		"mutable image":        func(r *EnrolledPlainPodRequest) { r.Spec.Image = "example.test/sandbox:latest" },
		"external mount": func(r *EnrolledPlainPodRequest) {
			r.Spec.Mounts = []runtime.Mount{{HostPath: "/data", ContainerPath: "/workspace"}}
		},
		"unsupported pids":      func(r *EnrolledPlainPodRequest) { r.Spec.PidLimit = 256 },
		"pool identity":         func(r *EnrolledPlainPodRequest) { r.Spec.Labels["sandbox.pool"] = "true" },
		"no bounded workspace":  func(r *EnrolledPlainPodRequest) { r.Spec.Disk = "" },
		"negative resources":    func(r *EnrolledPlainPodRequest) { r.Spec.CPU = "-1" },
		"request exceeds limit": func(r *EnrolledPlainPodRequest) { r.Spec.MemoryRequest = "1Gi" },
		"network mismatch":      func(r *EnrolledPlainPodRequest) { r.Spec.NetworkEnabled = true },
		"user mismatch":         func(r *EnrolledPlainPodRequest) { r.Spec.RunAsUser = 1001 },
		"writable root":         func(r *EnrolledPlainPodRequest) { r.Spec.ReadOnlyRootFS = false },
		"config secret": func(r *EnrolledPlainPodRequest) {
			r.Config = []byte(strings.Replace(string(r.Config), `"version":2`, `"version":2,"private_key":"secret"`, 1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := enrolledPodRequest(t)
			mutate(&r)
			_, err := BuildEnrolledPlainPod("runtimes", r)
			require.Error(t, err)
		})
	}
}
