package kubecontract_test

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
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
		{Name: "mounter-health", MountPath: "/run/sandbox-node-observer/mounter-health", ReadOnly: true},
	}, helper.VolumeMounts)
	require.Len(t, helper.Env, 7)
	require.Equal(t, v1.EnvVar{Name: "SANDBOX_RUNTIME_UID", ValueFrom: &v1.EnvVarSource{FieldRef: &v1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.uid"}}}, helper.Env[0])
	bootstrap, err := json.Marshal(input.Bootstrap)
	require.NoError(t, err)
	require.Equal(t, v1.EnvVar{Name: "SANDBOX_MOUNTER_BOOTSTRAP", Value: string(bootstrap)}, helper.Env[1])
	var launcherConfig launcherconfig.Config
	require.NoError(t, json.Unmarshal(input.LauncherConfig, &launcherConfig))
	require.Equal(t, []v1.EnvVar{
		{Name: "SANDBOX_MOUNTER_OBSERVER_SOCKET", Value: "/run/sandbox-node-observer/mounter-health/roothealth.sock"},
		{Name: "SANDBOX_MOUNTER_POD_NAME", ValueFrom: &v1.EnvVarSource{FieldRef: &v1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"}}},
		{Name: "SANDBOX_MOUNTER_NODE_UID", Value: launcherConfig.Enrollment.NodeUID},
		{Name: "SANDBOX_MOUNTER_NODE_BOOT_ID", Value: launcherConfig.Enrollment.NodeBootID},
		{Name: "SANDBOX_MOUNTER_CONTRACT_DIGEST", Value: launcherConfig.ContractDigest},
	}, helper.Env[2:])
	require.Equal(t, &v1.Probe{ProbeHandler: v1.ProbeHandler{Exec: &v1.ExecAction{Command: []string{"/usr/local/bin/workspace-mounter", "health", "prepared"}}}, PeriodSeconds: 2, TimeoutSeconds: 1, SuccessThreshold: 1, FailureThreshold: 30}, helper.StartupProbe)
	require.Equal(t, &v1.Probe{ProbeHandler: v1.ProbeHandler{HTTPGet: &v1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt(9808), Scheme: v1.URISchemeHTTP}}, PeriodSeconds: 10, TimeoutSeconds: 1, SuccessThreshold: 1, FailureThreshold: 3}, helper.ReadinessProbe)
	require.Equal(t, &v1.Lifecycle{PreStop: &v1.LifecycleHandler{Exec: &v1.ExecAction{Command: []string{"/usr/local/bin/workspace-mounter", "shutdown"}}}}, helper.Lifecycle)

	require.Len(t, spec.Volumes, 9)
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
	directory := v1.HostPathDirectory
	require.Equal(t, v1.Volume{Name: "mounter-health", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/run/sandbox-node-observer/mounter-health", Type: &directory}}}, spec.Volumes[8])
	wire, err := json.Marshal(spec)
	require.NoError(t, err)
	require.LessOrEqual(t, len(wire), 65536)
}

// A Kubernetes response necessarily parses Quantity's JSON representation.
// Numeric equality must survive that transport without treating omitted/empty
// fields, additional capabilities or different byte limits as equivalent.
func TestEnrolledFUSEJSONRoundTripKeepsStrictStructure(t *testing.T) {
	input := fuseProfileInput(t)
	original := fuseV2ProfilePod(t, input)
	wire, err := json.Marshal(original)
	require.NoError(t, err)
	var actual v1.Pod
	require.NoError(t, json.Unmarshal(wire, &actual))
	for _, index := range []int{0, 1, 2, 5, 6} {
		before := original.Spec.Volumes[index].EmptyDir.SizeLimit
		after := actual.Spec.Volumes[index].EmptyDir.SizeLimit
		require.Zero(t, before.Cmp(*after), "the real JSON roundtrip preserved each byte limit")
		t.Logf("volume %s source=%#v parsed=%#v", actual.Spec.Volumes[index].Name, *before, *after)
	}
	_, err = kubecontract.InspectEnrolledFUSEPod(&actual)
	require.NoError(t, err, "the complete Inspector must accept a legal API JSON response")
	require.NoError(t, kubecontract.ValidateEnrolledFUSEPodSpec(actual.Spec, input))

	// Compare each changed quantity against retained original intent. In
	// particular, Value() rounding must not erase a fractional-byte change.
	for _, index := range []int{0, 1, 2, 5, 6} {
		for _, delta := range []string{"1", "-1", "1m"} {
			t.Run(actual.Spec.Volumes[index].Name+"/"+delta, func(t *testing.T) {
				changed := actual.DeepCopy()
				quantity := changed.Spec.Volumes[index].EmptyDir.SizeLimit
				quantity.Add(resource.MustParse(delta))
				require.NotZero(t, quantity.Cmp(*actual.Spec.Volumes[index].EmptyDir.SizeLimit))
				require.Error(t, kubecontract.ValidateEnrolledFUSEPodSpec(changed.Spec, input))
				if delta == "1m" || index != 0 && index != 1 {
					// Workspace/tmp integer sizes are diagnostic inputs to the
					// Inspector, so only original-intent validation binds them.
					// A fractional value is never an exact template byte count.
					_, err := kubecontract.InspectEnrolledFUSEPod(changed)
					require.Error(t, err)
				}
			})
		}
	}
	for name, mutate := range map[string]func(*v1.PodSpec){
		"missing-limit": func(s *v1.PodSpec) { s.Volumes[0].EmptyDir.SizeLimit = nil },
		"different-memory": func(s *v1.PodSpec) {
			quantity := s.Containers[0].Resources.Limits[v1.ResourceMemory]
			quantity.Add(resource.MustParse("1"))
			s.Containers[0].Resources.Limits[v1.ResourceMemory] = quantity
		},
		"empty-ports":    func(s *v1.PodSpec) { s.Containers[0].Ports = []v1.ContainerPort{} },
		"empty-overhead": func(s *v1.PodSpec) { s.Overhead = v1.ResourceList{} },
		"launcher-escape": func(s *v1.PodSpec) {
			s.Containers[0].SecurityContext.Capabilities.Add = append(s.Containers[0].SecurityContext.Capabilities.Add, "SYS_ADMIN")
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := actual.Spec.DeepCopy()
			mutate(changed)
			require.Error(t, kubecontract.ValidateEnrolledFUSEPodSpec(*changed, input))
		})
	}
}

func TestEnrolledFUSEV2HealthChannelPinsTemplateVersion(t *testing.T) {
	input := fuseProfileInput(t)
	require.Equal(t, "sandbox-enrolled-fuse/v2", kubecontract.EnrolledFUSEProfile)
	actual, err := kubecontract.EnrolledFUSEContractDigest(input.LauncherImage, input.MounterImage, input.MounterAppArmorProfile)
	require.NoError(t, err)
	legacy := sha256.Sum256([]byte("goairix/sandbox/enrolled-fuse-contract/v1\x00sandbox-enrolled-fuse/v1\x00sha256:" + strings.Repeat("b", 64) + "\x00sha256:" + strings.Repeat("c", 64) + "\x00" + input.MounterAppArmorProfile))
	expected := sha256.Sum256([]byte("goairix/sandbox/enrolled-fuse-contract/v2\x00sandbox-enrolled-fuse/v2\x00sha256:" + strings.Repeat("b", 64) + "\x00sha256:" + strings.Repeat("c", 64) + "\x00" + input.MounterAppArmorProfile))
	require.NotEqual(t, hex.EncodeToString(legacy[:]), actual, "the new host directory and helper environment must not reuse v1 signed contracts")
	require.Equal(t, hex.EncodeToString(expected[:]), actual)
}

func TestEnrolledFUSEV2HealthChannelRejectsMutation(t *testing.T) {
	input := fuseProfileInput(t)
	spec, err := kubecontract.EnrolledFUSEPodSpec(input)
	require.NoError(t, err)
	require.Len(t, spec.Volumes, 9)
	require.Len(t, spec.InitContainers[0].VolumeMounts, 5)
	require.Len(t, spec.InitContainers[0].Env, 7)
	for name, mutate := range map[string]func(*v1.PodSpec){
		"writable-health": func(s *v1.PodSpec) { s.InitContainers[0].VolumeMounts[4].ReadOnly = false },
		"health-subpath":  func(s *v1.PodSpec) { s.InitContainers[0].VolumeMounts[4].SubPath = "roothealth.sock" },
		"health-propagation": func(s *v1.PodSpec) {
			mode := v1.MountPropagationHostToContainer
			s.InitContainers[0].VolumeMounts[4].MountPropagation = &mode
		},
		"foreign-health-directory": func(s *v1.PodSpec) { s.Volumes[8].HostPath.Path = "/run/foreign" },
		"inode-pinned-socket":      func(s *v1.PodSpec) { *s.Volumes[8].HostPath.Type = v1.HostPathSocket },
		"directory-create":         func(s *v1.PodSpec) { *s.Volumes[8].HostPath.Type = v1.HostPathDirectoryOrCreate },
		"launcher-health-access": func(s *v1.PodSpec) {
			s.Containers[0].VolumeMounts = append(s.Containers[0].VolumeMounts, s.InitContainers[0].VolumeMounts[4])
		},
		"foreign-observer-socket": func(s *v1.PodSpec) { s.InitContainers[0].Env[2].Value = "/run/other.sock" },
		"self-reported-pod-name": func(s *v1.PodSpec) {
			s.InitContainers[0].Env[3] = v1.EnvVar{Name: "SANDBOX_MOUNTER_POD_NAME", Value: "claimed"}
		},
		"foreign-node-uid":  func(s *v1.PodSpec) { s.InitContainers[0].Env[4].Value = "55555555-5555-4555-8555-555555555555" },
		"foreign-node-boot": func(s *v1.PodSpec) { s.InitContainers[0].Env[5].Value = "55555555-5555-4555-8555-555555555555" },
		"foreign-contract":  func(s *v1.PodSpec) { s.InitContainers[0].Env[6].Value = strings.Repeat("f", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := spec.DeepCopy()
			mutate(changed)
			require.Error(t, kubecontract.ValidateEnrolledFUSEPodSpec(*changed, input))
		})
	}
}

// These API objects are structural models, never kernel peer or enrollment
// proof. The inspector must return diagnostics for callers to compare against
// an independently verified registration; it cannot activate a plain consumer.
func TestEnrolledFUSEV2InspectorRemainsDiagnosticAndStrict(t *testing.T) {
	input := fuseProfileInput(t)
	pod := fuseV2ProfilePod(t, input)
	before, err := json.Marshal(pod)
	require.NoError(t, err)
	identity, err := kubecontract.InspectEnrolledFUSEPod(pod)
	require.NoError(t, err)
	config, err := launcherconfig.Decode(input.LauncherConfig)
	require.NoError(t, err)
	require.Equal(t, config, identity.Config)
	require.Equal(t, input.Bootstrap, identity.Bootstrap)
	require.Equal(t, input.MounterImage, identity.MounterImage)
	require.Equal(t, input.MounterAppArmorProfile, identity.MounterAppArmorProfile)
	require.Equal(t, pod.Labels["sandbox.id"], identity.LogicalID)
	require.Equal(t, pod.Annotations[kubecontract.EnrolledCreationIntentAnnotation], identity.CreationIntentID)
	require.Equal(t, config.Enrollment.NodeUID, identity.NodeUID)
	require.Equal(t, config.Enrollment.NodeBootID, identity.NodeBootID)
	require.EqualValues(t, 1, identity.Generation)
	after, err := json.Marshal(pod)
	require.NoError(t, err)
	require.Equal(t, before, after)
	_, err = kubecontract.SelectEnrolledPlainPodProfile(pod)
	require.Error(t, err, "FUSE recognition must not weaken the plain selector")
	for name, mutate := range map[string]func(*v1.Pod){
		"old-version":    func(p *v1.Pod) { p.Annotations[kubecontract.EnrolledProfileAnnotation] = "sandbox-enrolled-fuse/v1" },
		"missing-marker": func(p *v1.Pod) { delete(p.Annotations, kubecontract.EnrolledProfileAnnotation) },
		"wrong-mode":     func(p *v1.Pod) { p.Labels["sandbox.workspace.mode"] = "plain" },
		"foreign-node": func(p *v1.Pod) {
			p.Annotations[kubecontract.EnrolledNodeUIDAnnotation] = "55555555-5555-4555-8555-555555555555"
		},
		"foreign-boot": func(p *v1.Pod) {
			p.Annotations[kubecontract.EnrolledNodeBootIDAnnotation] = "55555555-5555-4555-8555-555555555555"
		},
		"foreign-helper-image": func(p *v1.Pod) {
			p.Spec.InitContainers[0].Image = "example.test/mounter@sha256:" + strings.Repeat("f", 64)
		},
		"unconfined-helper": func(p *v1.Pod) {
			p.Spec.InitContainers[0].SecurityContext.AppArmorProfile.Type = v1.AppArmorProfileTypeUnconfined
		},
		"launcher-health-access": func(p *v1.Pod) {
			p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, p.Spec.InitContainers[0].VolumeMounts[4])
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := pod.DeepCopy()
			mutate(changed)
			_, err := kubecontract.InspectEnrolledFUSEPod(changed)
			require.Error(t, err)
		})
	}
}

func fuseV2ProfilePod(t *testing.T, input kubecontract.EnrolledFUSEPodSpecInput) *v1.Pod {
	t.Helper()
	pod := profilePod(t)
	var err error
	pod.Spec, err = kubecontract.EnrolledFUSEPodSpec(input)
	require.NoError(t, err)
	pod.Labels["sandbox.workspace.mode"] = "fuse"
	pod.Annotations[kubecontract.EnrolledProfileAnnotation] = "sandbox-enrolled-fuse/v2"
	digest := sha256.Sum256(input.LauncherConfig)
	pod.Annotations[kubecontract.EnrolledConfigDigestAnnotation] = hex.EncodeToString(digest[:])
	return pod
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
		"endpoint invalid utf8": func(i *kubecontract.EnrolledFUSEPodSpecInput) {
			i.Bootstrap.Endpoint = "https://storage\xff.example.test"
		},
		"endpoint unicode control": func(i *kubecontract.EnrolledFUSEPodSpecInput) {
			i.Bootstrap.Endpoint = "https://storage\u0085.example.test"
		},
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
