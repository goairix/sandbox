package kubecontract_test

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"

	"github.com/goairix/sandbox/internal/fuseprotocol"
	"github.com/goairix/sandbox/internal/kubecontract"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// The fixture supplies original deployment intent. Observed Pod mutations never
// become expected input; otherwise matching a self-reported shape proves nothing.
func fuseProfileInput(t *testing.T) kubecontract.EnrolledFUSEPodSpecInput {
	t.Helper()
	input := kubecontract.EnrolledFUSEPodSpecInput{
		NodeName:               "node-a",
		LauncherImage:          "example.test/sandbox@sha256:" + strings.Repeat("b", 64),
		MounterImage:           "example.test/mounter@sha256:" + strings.Repeat("c", 64),
		MounterAppArmorProfile: "sandbox-fuse-" + strings.Repeat("e", 64),
		LauncherResources: v1.ResourceRequirements{
			Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("500m"), v1.ResourceMemory: resource.MustParse("512Mi")},
			Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("250m"), v1.ResourceMemory: resource.MustParse("256Mi")},
		},
		MounterResources: v1.ResourceRequirements{
			Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("250m"), v1.ResourceMemory: resource.MustParse("256Mi")},
			Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("100m"), v1.ResourceMemory: resource.MustParse("128Mi")},
		},
		WorkspaceBytes: 64 << 20,
		TmpBytes:       32 << 20,
		Bootstrap: fuseprotocol.BootstrapConfig{
			Version: 1, Provider: "minio", Bucket: "workspaces", Endpoint: "https://storage.example.test", Region: "us-east-1",
			Profile: "minio-sigv4-path-style-v1", PasswdFile: "/run/s3fs/passwd-s3fs", CacheDir: "/var/cache/s3fs", MountPath: "/workspace",
			CacheLimitBytes: 128 << 20, PoolKey: strings.Repeat("d", 64), MountTimeoutSeconds: 30, FlushTimeoutSeconds: 30, UnmountTimeoutSeconds: 15,
		},
	}
	contract, err := kubecontract.EnrolledFUSEContractDigest(input.LauncherImage, input.MounterImage, input.MounterAppArmorProfile)
	require.NoError(t, err)
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	config := launcherconfig.Config{
		Version: 2, Binding: launcherconfig.Binding{Namespace: "/sandbox/control/authority/", AuthorityID: "authority", Target: "target", RestoreEpoch: "restore"},
		Roots: []ed25519.PublicKey{key}, UID: 1000, GID: 1000, ContractDigest: contract,
		ClockSocket: "/run/sandbox-clock/clock.sock", ClockPublicKey: key, ClockAudience: "launcher", MaxActive: 4,
		Enrollment: launcherconfig.Enrollment{NodeUID: "22222222-2222-4222-8222-222222222222", NodeBootID: "33333333-3333-4333-8333-333333333333"},
	}
	input.LauncherConfig, err = json.Marshal(config)
	require.NoError(t, err)
	return input
}

func TestEnrolledFUSEStaticProtectedShape(t *testing.T) {
	input := fuseProfileInput(t)
	spec, err := kubecontract.EnrolledFUSEPodSpec(input)
	require.NoError(t, err)
	require.NoError(t, kubecontract.ValidateEnrolledFUSEPodSpec(spec, input))
	require.Equal(t, "node-a", spec.NodeName)
	require.Equal(t, v1.RestartPolicyNever, spec.RestartPolicy)
	require.False(t, spec.HostPID)
	require.False(t, spec.HostIPC)
	require.False(t, spec.HostNetwork)
	require.NotNil(t, spec.ShareProcessNamespace)
	require.False(t, *spec.ShareProcessNamespace)
	require.False(t, *spec.AutomountServiceAccountToken)
	require.False(t, *spec.EnableServiceLinks)
	require.Equal(t, int64(90), *spec.TerminationGracePeriodSeconds)
	require.Len(t, spec.Containers, 1)
	require.Len(t, spec.InitContainers, 1)
	require.Empty(t, spec.EphemeralContainers)

	launcher := spec.Containers[0]
	require.Equal(t, []string{"/usr/local/bin/sandbox-launcher"}, launcher.Command)
	require.Equal(t, []string{"serve-provisioned"}, launcher.Args)
	require.Equal(t, int64(0), *launcher.SecurityContext.RunAsUser)
	require.False(t, *launcher.SecurityContext.Privileged)
	require.False(t, *launcher.SecurityContext.AllowPrivilegeEscalation)
	require.True(t, *launcher.SecurityContext.ReadOnlyRootFilesystem)
	require.Equal(t, []v1.Capability{"ALL"}, launcher.SecurityContext.Capabilities.Drop)
	require.Equal(t, []v1.Capability{"KILL", "SETUID", "SETGID", "SETPCAP"}, launcher.SecurityContext.Capabilities.Add)
	require.Equal(t, v1.SeccompProfileTypeRuntimeDefault, spec.SecurityContext.SeccompProfile.Type)
	toUser, bidirectional := v1.MountPropagationHostToContainer, v1.MountPropagationBidirectional
	require.Equal(t, []v1.VolumeMount{
		{Name: "workspace", MountPath: "/workspace", MountPropagation: &toUser},
		{Name: "tmp", MountPath: "/tmp"}, {Name: "control", MountPath: "/run/sandbox-control"},
		{Name: "clock", MountPath: "/run/sandbox-clock", ReadOnly: true},
		{Name: "enrollment", MountPath: "/run/sandbox-control/enrollment.sock", ReadOnly: true},
	}, launcher.VolumeMounts)

	helper := spec.InitContainers[0]
	require.Equal(t, "workspace-mounter", helper.Name)
	require.Equal(t, []string{"/usr/local/bin/workspace-mounter", "supervise"}, helper.Command)
	require.Empty(t, helper.Args)
	require.Equal(t, v1.ContainerRestartPolicyAlways, *helper.RestartPolicy)
	require.True(t, *helper.SecurityContext.Privileged)
	require.Equal(t, int64(0), *helper.SecurityContext.RunAsUser)
	require.True(t, *helper.SecurityContext.ReadOnlyRootFilesystem)
	require.Equal(t, v1.AppArmorProfileTypeLocalhost, helper.SecurityContext.AppArmorProfile.Type)
	require.Equal(t, "sandbox-fuse-"+strings.Repeat("e", 64), *helper.SecurityContext.AppArmorProfile.LocalhostProfile)
	require.Equal(t, []v1.VolumeMount{
		{Name: "workspace", MountPath: "/workspace", MountPropagation: &bidirectional},
		{Name: "fuse-cache", MountPath: "/var/cache/s3fs"}, {Name: "dev-fuse", MountPath: "/dev/fuse"},
		{Name: "mounter-run", MountPath: "/run/s3fs"},
	}, helper.VolumeMounts)
	require.Len(t, helper.Env, 2)
	require.Equal(t, v1.EnvVar{Name: "SANDBOX_RUNTIME_UID", ValueFrom: &v1.EnvVarSource{FieldRef: &v1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.uid"}}}, helper.Env[0])
	bootstrap, err := json.Marshal(input.Bootstrap)
	require.NoError(t, err)
	require.Equal(t, v1.EnvVar{Name: "SANDBOX_MOUNTER_BOOTSTRAP", Value: string(bootstrap)}, helper.Env[1])
	require.Equal(t, &v1.Probe{ProbeHandler: v1.ProbeHandler{Exec: &v1.ExecAction{Command: []string{"/usr/local/bin/workspace-mounter", "health", "prepared"}}}, PeriodSeconds: 2, TimeoutSeconds: 1, SuccessThreshold: 1, FailureThreshold: 30}, helper.StartupProbe)
	require.Equal(t, &v1.Probe{ProbeHandler: v1.ProbeHandler{HTTPGet: &v1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt(9808), Scheme: v1.URISchemeHTTP}}, PeriodSeconds: 10, TimeoutSeconds: 1, SuccessThreshold: 1, FailureThreshold: 3}, helper.ReadinessProbe)
	require.Equal(t, &v1.Lifecycle{PreStop: &v1.LifecycleHandler{Exec: &v1.ExecAction{Command: []string{"/usr/local/bin/workspace-mounter", "shutdown"}}}}, helper.Lifecycle)

	require.Len(t, spec.Volumes, 8)
	require.Equal(t, v1.StorageMediumMemory, spec.Volumes[0].EmptyDir.Medium)
	require.Equal(t, int64(64<<20), spec.Volumes[0].EmptyDir.SizeLimit.Value())
	require.Equal(t, int64(32<<20), spec.Volumes[1].EmptyDir.SizeLimit.Value())
	require.Equal(t, v1.StorageMediumMemory, spec.Volumes[2].EmptyDir.Medium)
	require.Equal(t, int64(1<<20), spec.Volumes[2].EmptyDir.SizeLimit.Value())
	require.Equal(t, "/run/sandbox-clock", spec.Volumes[3].HostPath.Path)
	require.Equal(t, v1.HostPathDirectory, *spec.Volumes[3].HostPath.Type)
	require.Equal(t, "/run/sandbox-node-observer/enrollment.sock", spec.Volumes[4].HostPath.Path)
	require.Equal(t, v1.HostPathSocket, *spec.Volumes[4].HostPath.Type)
	require.Equal(t, "fuse-cache", spec.Volumes[5].Name)
	require.Equal(t, int64(128<<20), spec.Volumes[5].EmptyDir.SizeLimit.Value())
	require.Equal(t, v1.StorageMediumDefault, spec.Volumes[5].EmptyDir.Medium)
	require.Equal(t, "mounter-run", spec.Volumes[6].Name)
	require.Equal(t, v1.StorageMediumMemory, spec.Volumes[6].EmptyDir.Medium)
	require.Equal(t, int64(16<<20), spec.Volumes[6].EmptyDir.SizeLimit.Value())
	require.Equal(t, "dev-fuse", spec.Volumes[7].Name)
	require.Equal(t, "/dev/fuse", spec.Volumes[7].HostPath.Path)
	require.Equal(t, v1.HostPathCharDev, *spec.Volumes[7].HostPath.Type)
	wire, err := json.Marshal(spec)
	require.NoError(t, err)
	require.LessOrEqual(t, len(wire), 65536)
}

// Every mutation represents an added privilege, foreign executable/device,
// altered storage identity, or changed retained intent that must fail closed.
func TestEnrolledFUSEStaticRejectsObservedMutation(t *testing.T) {
	input := fuseProfileInput(t)
	spec, err := kubecontract.EnrolledFUSEPodSpec(input)
	require.NoError(t, err)
	for name, mutate := range map[string]func(*v1.PodSpec){
		"node":         func(s *v1.PodSpec) { s.NodeName = "other-node" },
		"host pid":     func(s *v1.PodSpec) { s.HostPID = true },
		"host ipc":     func(s *v1.PodSpec) { s.HostIPC = true },
		"host network": func(s *v1.PodSpec) { s.HostNetwork = true },
		"shared pid":   func(s *v1.PodSpec) { *s.ShareProcessNamespace = true },
		"user namespace": func(s *v1.PodSpec) {
			no := false
			s.HostUsers = &no
		},
		"token":       func(s *v1.PodSpec) { *s.AutomountServiceAccountToken = true },
		"service env": func(s *v1.PodSpec) { *s.EnableServiceLinks = true },
		"dns":         func(s *v1.PodSpec) { s.DNSPolicy = v1.DNSClusterFirst },
		"grace":       func(s *v1.PodSpec) { *s.TerminationGracePeriodSeconds = 1 },
		"restart":     func(s *v1.PodSpec) { s.RestartPolicy = v1.RestartPolicyAlways },
		"host alias": func(s *v1.PodSpec) {
			s.HostAliases = []v1.HostAlias{{IP: "127.0.0.1", Hostnames: []string{"storage.example.test"}}}
		},
		"pod claim":       func(s *v1.PodSpec) { s.ResourceClaims = []v1.PodResourceClaim{{Name: "other"}} },
		"pod security":    func(s *v1.PodSpec) { s.SecurityContext.SupplementalGroups = []int64{1} },
		"seccomp":         func(s *v1.PodSpec) { s.SecurityContext.SeccompProfile.Type = v1.SeccompProfileTypeUnconfined },
		"foreign sidecar": func(s *v1.PodSpec) { s.Containers = append(s.Containers, v1.Container{Name: "other"}) },
		"foreign init":    func(s *v1.PodSpec) { s.InitContainers = append(s.InitContainers, v1.Container{Name: "other"}) },
		"missing helper":  func(s *v1.PodSpec) { s.InitContainers = nil },
		"ephemeral": func(s *v1.PodSpec) {
			s.EphemeralContainers = []v1.EphemeralContainer{{EphemeralContainerCommon: v1.EphemeralContainerCommon{Name: "debug"}}}
		},
		"launcher image":     func(s *v1.PodSpec) { s.Containers[0].Image = "example.test/other@sha256:" + strings.Repeat("f", 64) },
		"launcher uid":       func(s *v1.PodSpec) { *s.Containers[0].SecurityContext.RunAsUser = 1000 },
		"launcher privilege": func(s *v1.PodSpec) { *s.Containers[0].SecurityContext.Privileged = true },
		"launcher cap": func(s *v1.PodSpec) {
			s.Containers[0].SecurityContext.Capabilities.Add = append(s.Containers[0].SecurityContext.Capabilities.Add, "SYS_ADMIN")
		},
		"launcher rootfs": func(s *v1.PodSpec) { *s.Containers[0].SecurityContext.ReadOnlyRootFilesystem = false },
		"launcher argv":   func(s *v1.PodSpec) { s.Containers[0].Command = []string{"/bin/sh"} },
		"launcher config": func(s *v1.PodSpec) { s.Containers[0].Env[0].Value += " " },
		"launcher env": func(s *v1.PodSpec) {
			s.Containers[0].Env = append(s.Containers[0].Env, v1.EnvVar{Name: "LD_PRELOAD", Value: "/workspace/attack.so"})
		},
		"launcher propagation": func(s *v1.PodSpec) {
			*s.Containers[0].VolumeMounts[0].MountPropagation = v1.MountPropagationBidirectional
		},
		"launcher helper socket": func(s *v1.PodSpec) {
			s.Containers[0].VolumeMounts = append(s.Containers[0].VolumeMounts, v1.VolumeMount{Name: "mounter-run", MountPath: "/run/s3fs"})
		},
		"launcher dev fuse": func(s *v1.PodSpec) {
			s.Containers[0].VolumeMounts = append(s.Containers[0].VolumeMounts, v1.VolumeMount{Name: "dev-fuse", MountPath: "/dev/fuse"})
		},
		"writable clock": func(s *v1.PodSpec) { s.Containers[0].VolumeMounts[3].ReadOnly = false },
		"helper image": func(s *v1.PodSpec) {
			s.InitContainers[0].Image = "example.test/other@sha256:" + strings.Repeat("f", 64)
		},
		"helper lsm": func(s *v1.PodSpec) {
			s.InitContainers[0].SecurityContext.AppArmorProfile.Type = v1.AppArmorProfileTypeUnconfined
		},
		"helper rootfs":  func(s *v1.PodSpec) { *s.InitContainers[0].SecurityContext.ReadOnlyRootFilesystem = false },
		"helper restart": func(s *v1.PodSpec) { s.InitContainers[0].RestartPolicy = nil },
		"helper argv":    func(s *v1.PodSpec) { s.InitContainers[0].Args = []string{"--foreign"} },
		"helper uid env": func(s *v1.PodSpec) {
			s.InitContainers[0].Env[0] = v1.EnvVar{Name: "SANDBOX_RUNTIME_UID", Value: "claimed"}
		},
		"helper bootstrap": func(s *v1.PodSpec) { s.InitContainers[0].Env[1].Value = "{}" },
		"helper credential env": func(s *v1.PodSpec) {
			s.InitContainers[0].Env = append(s.InitContainers[0].Env, v1.EnvVar{Name: "AWS_SECRET_ACCESS_KEY", Value: "secret"})
		},
		"helper host port": func(s *v1.PodSpec) {
			s.InitContainers[0].Ports = []v1.ContainerPort{{ContainerPort: 9808, HostPort: 9808}}
		},
		"helper host mount": func(s *v1.PodSpec) {
			s.InitContainers[0].VolumeMounts = append(s.InitContainers[0].VolumeMounts, v1.VolumeMount{Name: "host", MountPath: "/host"})
		},
		"helper subpath": func(s *v1.PodSpec) { s.InitContainers[0].VolumeMounts[0].SubPath = "other" },
		"helper propagation": func(s *v1.PodSpec) {
			*s.InitContainers[0].VolumeMounts[0].MountPropagation = v1.MountPropagationHostToContainer
		},
		"ready exec": func(s *v1.PodSpec) {
			s.InitContainers[0].ReadinessProbe.HTTPGet = nil
			s.InitContainers[0].ReadinessProbe.Exec = &v1.ExecAction{Command: []string{"/bin/sh"}}
		},
		"ready frequency":     func(s *v1.PodSpec) { s.InitContainers[0].ReadinessProbe.PeriodSeconds = 60 },
		"resource claim":      func(s *v1.PodSpec) { s.InitContainers[0].Resources.Claims = []v1.ResourceClaim{{Name: "other"}} },
		"resource limit":      func(s *v1.PodSpec) { delete(s.InitContainers[0].Resources.Limits, v1.ResourceMemory) },
		"workspace unbounded": func(s *v1.PodSpec) { s.Volumes[0].EmptyDir.SizeLimit = nil },
		"workspace pvc": func(s *v1.PodSpec) {
			s.Volumes[0].EmptyDir = nil
			s.Volumes[0].PersistentVolumeClaim = &v1.PersistentVolumeClaimVolumeSource{ClaimName: "other"}
		},
		"control disk":      func(s *v1.PodSpec) { s.Volumes[2].EmptyDir.Medium = v1.StorageMediumDefault },
		"clock host path":   func(s *v1.PodSpec) { s.Volumes[3].HostPath.Path = "/" },
		"enrollment source": func(s *v1.PodSpec) { s.Volumes[4].HostPath.Path = "/run/other.sock" },
		"cache memory":      func(s *v1.PodSpec) { s.Volumes[5].EmptyDir.Medium = v1.StorageMediumMemory },
		"cache size": func(s *v1.PodSpec) {
			q := resource.MustParse("129Mi")
			s.Volumes[5].EmptyDir.SizeLimit = &q
		},
		"mounter run size": func(s *v1.PodSpec) {
			q := resource.MustParse("32Mi")
			s.Volumes[6].EmptyDir.SizeLimit = &q
		},
		"fuse path":        func(s *v1.PodSpec) { s.Volumes[7].HostPath.Path = "/dev/mem" },
		"fuse device type": func(s *v1.PodSpec) { *s.Volumes[7].HostPath.Type = v1.HostPathFile },
		"secret volume": func(s *v1.PodSpec) {
			s.Volumes = append(s.Volumes, v1.Volume{Name: "secret", VolumeSource: v1.VolumeSource{Secret: &v1.SecretVolumeSource{SecretName: "other"}}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			mutated := spec.DeepCopy()
			mutate(mutated)
			require.Error(t, kubecontract.ValidateEnrolledFUSEPodSpec(*mutated, input))
		})
	}
}

func TestEnrolledFUSEStaticRejectsInvalidOriginal(t *testing.T) {
	for name, mutate := range map[string]func(*kubecontract.EnrolledFUSEPodSpecInput){
		"empty node":        func(i *kubecontract.EnrolledFUSEPodSpecInput) { i.NodeName = "" },
		"oversized node":    func(i *kubecontract.EnrolledFUSEPodSpecInput) { i.NodeName = strings.Repeat("n", 129) },
		"unpinned launcher": func(i *kubecontract.EnrolledFUSEPodSpecInput) { i.LauncherImage = "sandbox:v1.0.0" },
		"unpinned mounter":  func(i *kubecontract.EnrolledFUSEPodSpecInput) { i.MounterImage = "mounter:latest" },
		"changed mounter": func(i *kubecontract.EnrolledFUSEPodSpecInput) {
			i.MounterImage = "example.test/mounter@sha256:" + strings.Repeat("f", 64)
		},
		"lsm missing":         func(i *kubecontract.EnrolledFUSEPodSpecInput) { i.MounterAppArmorProfile = "" },
		"lsm mutable":         func(i *kubecontract.EnrolledFUSEPodSpecInput) { i.MounterAppArmorProfile = "sandbox-fuse-latest" },
		"config oversized":    func(i *kubecontract.EnrolledFUSEPodSpecInput) { i.LauncherConfig = make([]byte, 16385) },
		"config noncanonical": func(i *kubecontract.EnrolledFUSEPodSpecInput) { i.LauncherConfig = append(i.LauncherConfig, '\n') },
		"fixed mount uid": func(i *kubecontract.EnrolledFUSEPodSpecInput) {
			mutateFUSEConfig(t, i, func(c *launcherconfig.Config) { c.UID = 2000 })
		},
		"fixed mount gid": func(i *kubecontract.EnrolledFUSEPodSpecInput) {
			mutateFUSEConfig(t, i, func(c *launcherconfig.Config) { c.GID = 2000 })
		},
		"plain digest": func(i *kubecontract.EnrolledFUSEPodSpecInput) {
			mutateFUSEConfig(t, i, func(c *launcherconfig.Config) { c.ContractDigest = strings.Repeat("a", 64) })
		},
		"workspace zero":     func(i *kubecontract.EnrolledFUSEPodSpecInput) { i.WorkspaceBytes = 0 },
		"tmp negative":       func(i *kubecontract.EnrolledFUSEPodSpecInput) { i.TmpBytes = -1 },
		"cache zero":         func(i *kubecontract.EnrolledFUSEPodSpecInput) { i.Bootstrap.CacheLimitBytes = 0 },
		"bootstrap version":  func(i *kubecontract.EnrolledFUSEPodSpecInput) { i.Bootstrap.Version = 2 },
		"preclaimed uid":     func(i *kubecontract.EnrolledFUSEPodSpecInput) { i.Bootstrap.RuntimeUID = "claimed" },
		"foreign provider":   func(i *kubecontract.EnrolledFUSEPodSpecInput) { i.Bootstrap.Provider = "obs" },
		"unverified profile": func(i *kubecontract.EnrolledFUSEPodSpecInput) { i.Bootstrap.Profile = "custom" },
		"bad bucket":         func(i *kubecontract.EnrolledFUSEPodSpecInput) { i.Bootstrap.Bucket = "../escape" },
		"bucket oversized":   func(i *kubecontract.EnrolledFUSEPodSpecInput) { i.Bootstrap.Bucket = strings.Repeat("b", 64) },
		"endpoint http":      func(i *kubecontract.EnrolledFUSEPodSpecInput) { i.Bootstrap.Endpoint = "http://storage.example.test" },
		"endpoint user": func(i *kubecontract.EnrolledFUSEPodSpecInput) {
			i.Bootstrap.Endpoint = "https://secret@storage.example.test"
		},
		"endpoint query": func(i *kubecontract.EnrolledFUSEPodSpecInput) {
			i.Bootstrap.Endpoint = "https://storage.example.test?secret=x"
		},
		"endpoint path": func(i *kubecontract.EnrolledFUSEPodSpecInput) {
			i.Bootstrap.Endpoint = "https://storage.example.test/other"
		},
		"endpoint oversized": func(i *kubecontract.EnrolledFUSEPodSpecInput) {
			i.Bootstrap.Endpoint = "https://" + strings.Repeat("x", 1025)
		},
		"region flag":        func(i *kubecontract.EnrolledFUSEPodSpecInput) { i.Bootstrap.Region = "--other" },
		"pool identity":      func(i *kubecontract.EnrolledFUSEPodSpecInput) { i.Bootstrap.PoolKey = strings.Repeat("D", 64) },
		"passwd path":        func(i *kubecontract.EnrolledFUSEPodSpecInput) { i.Bootstrap.PasswdFile = "/workspace/passwd" },
		"cache path":         func(i *kubecontract.EnrolledFUSEPodSpecInput) { i.Bootstrap.CacheDir = "/workspace/cache" },
		"mount path":         func(i *kubecontract.EnrolledFUSEPodSpecInput) { i.Bootstrap.MountPath = "/other" },
		"ca path":            func(i *kubecontract.EnrolledFUSEPodSpecInput) { i.Bootstrap.CAFile = "/run/secrets/workspace/ca" },
		"mount timeout":      func(i *kubecontract.EnrolledFUSEPodSpecInput) { i.Bootstrap.MountTimeoutSeconds = 0 },
		"flush timeout":      func(i *kubecontract.EnrolledFUSEPodSpecInput) { i.Bootstrap.FlushTimeoutSeconds = 3601 },
		"unmount timeout":    func(i *kubecontract.EnrolledFUSEPodSpecInput) { i.Bootstrap.UnmountTimeoutSeconds = -1 },
		"launcher no memory": func(i *kubecontract.EnrolledFUSEPodSpecInput) { delete(i.LauncherResources.Limits, v1.ResourceMemory) },
		"helper request excess": func(i *kubecontract.EnrolledFUSEPodSpecInput) {
			i.MounterResources.Requests[v1.ResourceCPU] = resource.MustParse("1")
		},
		"helper memory zero": func(i *kubecontract.EnrolledFUSEPodSpecInput) {
			i.MounterResources.Limits[v1.ResourceMemory] = resource.MustParse("0")
		},
		"helper extra resource": func(i *kubecontract.EnrolledFUSEPodSpecInput) {
			i.MounterResources.Limits[v1.ResourceEphemeralStorage] = resource.MustParse("1Gi")
		},
		"helper claims": func(i *kubecontract.EnrolledFUSEPodSpecInput) {
			i.MounterResources.Claims = []v1.ResourceClaim{{Name: "other"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := fuseProfileInput(t)
			mutate(&input)
			_, err := kubecontract.EnrolledFUSEPodSpec(input)
			require.Error(t, err)
		})
	}
}

func mutateFUSEConfig(t *testing.T, input *kubecontract.EnrolledFUSEPodSpecInput, mutate func(*launcherconfig.Config)) {
	t.Helper()
	var config launcherconfig.Config
	require.NoError(t, json.Unmarshal(input.LauncherConfig, &config))
	mutate(&config)
	wire, err := json.Marshal(config)
	require.NoError(t, err)
	input.LauncherConfig = wire
}

func TestEnrolledFUSEStaticOriginalAndOwnership(t *testing.T) {
	input := fuseProfileInput(t)
	before, err := json.Marshal(input)
	require.NoError(t, err)
	spec, err := kubecontract.EnrolledFUSEPodSpec(input)
	require.NoError(t, err)
	observed, err := json.Marshal(spec)
	require.NoError(t, err)
	require.NoError(t, kubecontract.ValidateEnrolledFUSEPodSpec(spec, input))
	after, err := json.Marshal(input)
	require.NoError(t, err)
	require.Equal(t, before, after, "renderer and validator must not rewrite retained original intent")
	after, err = json.Marshal(spec)
	require.NoError(t, err)
	require.Equal(t, observed, after, "validation must not rewrite observed Pod fields")

	foreign := fuseProfileInput(t)
	foreign.Bootstrap.Bucket = "other-workspace"
	require.Error(t, kubecontract.ValidateEnrolledFUSEPodSpec(spec, foreign))
	foreign = fuseProfileInput(t)
	foreign.MounterResources.Limits[v1.ResourceMemory] = resource.MustParse("512Mi")
	require.Error(t, kubecontract.ValidateEnrolledFUSEPodSpec(spec, foreign))

	spec.Containers[0].Resources.Limits[v1.ResourceMemory] = resource.MustParse("1Gi")
	spec.InitContainers[0].Resources.Requests[v1.ResourceCPU] = resource.MustParse("200m")
	*spec.InitContainers[0].SecurityContext.AppArmorProfile.LocalhostProfile = "foreign"
	input.LauncherConfig[0] = 'x'
	input.MounterResources.Limits[v1.ResourceMemory] = resource.MustParse("2Gi")
	require.Equal(t, "512Mi", input.LauncherResources.Limits.Memory().String())
	require.Equal(t, "100m", input.MounterResources.Requests.Cpu().String())
	require.NotEqual(t, "x", spec.Containers[0].Env[0].Value[:1])
	require.Equal(t, "256Mi", spec.InitContainers[0].Resources.Limits.Memory().String())
}

func TestEnrolledFUSEStaticCannotEnablePlainProfile(t *testing.T) {
	input := fuseProfileInput(t)
	spec, err := kubecontract.EnrolledFUSEPodSpec(input)
	require.NoError(t, err)
	pod := profilePod(t)
	pod.Spec = spec
	require.Error(t, kubecontract.ValidateEnrolledPlainPod(pod))
	pod.Annotations[kubecontract.EnrolledProfileAnnotation] = "sandbox-enrolled-fuse/v1"
	_, err = kubecontract.SelectEnrolledPlainPodProfile(pod)
	require.Error(t, err, "static renderer must not widen existing profile selection")
	delete(pod.Annotations, kubecontract.EnrolledProfileAnnotation)
	_, err = kubecontract.SelectEnrolledPlainPodProfile(pod)
	require.Error(t, err, "protected launcher shape cannot downgrade to legacy")

	plain := profilePod(t)
	require.NoError(t, kubecontract.ValidateEnrolledPlainPod(plain))
	selected, err := kubecontract.SelectEnrolledPlainPodProfile(plain)
	require.NoError(t, err)
	require.True(t, selected)
}

func TestEnrolledFUSEStaticDigestPinsBothImagesAndLSM(t *testing.T) {
	input := fuseProfileInput(t)
	digest, err := kubecontract.EnrolledFUSEContractDigest(input.LauncherImage, input.MounterImage, input.MounterAppArmorProfile)
	require.NoError(t, err)
	for _, values := range [][3]string{
		{"example.test/sandbox@sha256:" + strings.Repeat("f", 64), input.MounterImage, input.MounterAppArmorProfile},
		{input.LauncherImage, "example.test/mounter@sha256:" + strings.Repeat("f", 64), input.MounterAppArmorProfile},
		{input.LauncherImage, input.MounterImage, "sandbox-fuse-" + strings.Repeat("f", 64)},
	} {
		changed, err := kubecontract.EnrolledFUSEContractDigest(values[0], values[1], values[2])
		require.NoError(t, err)
		require.NotEqual(t, digest, changed)
	}
	for _, values := range [][3]string{
		{"sandbox:latest", input.MounterImage, input.MounterAppArmorProfile},
		{input.LauncherImage, "mounter:v1.0.0", input.MounterAppArmorProfile},
		{input.LauncherImage, input.MounterImage, "sandbox-fuse-latest"},
	} {
		_, err := kubecontract.EnrolledFUSEContractDigest(values[0], values[1], values[2])
		require.Error(t, err)
	}
}
