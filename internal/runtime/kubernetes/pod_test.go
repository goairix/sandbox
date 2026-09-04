package kubernetes

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/goairix/sandbox/internal/runtime"
)

func TestWorkspaceFUSEContractIsUnsupported(t *testing.T) {
	rt := &Runtime{}
	ctx := context.Background()

	_, err := rt.PrepareSandbox(ctx, runtime.SandboxSpec{})
	require.ErrorIs(t, err, runtime.ErrWorkspaceFUSEUnsupported)
	require.ErrorIs(t, rt.AuthorizeWorkspaceMount(ctx, "id", runtime.WorkspaceMountAuthorization{}), runtime.ErrWorkspaceFUSEUnsupported)
	_, err = rt.WaitSandboxReady(ctx, "id")
	require.ErrorIs(t, err, runtime.ErrWorkspaceFUSEUnsupported)
	require.ErrorIs(t, rt.PreparedSandboxHealth(ctx, "id", "pool"), runtime.ErrWorkspaceFUSEUnsupported)
	_, err = rt.WorkspaceHealth(ctx, "id")
	require.ErrorIs(t, err, runtime.ErrWorkspaceFUSEUnsupported)
	_, err = rt.QuiesceWorkspace(ctx, "id")
	require.ErrorIs(t, err, runtime.ErrWorkspaceFUSEUnsupported)
	require.ErrorIs(t, rt.ResumeWorkspace(ctx, "id", runtime.WorkspaceQuiesceToken{}), runtime.ErrWorkspaceFUSEUnsupported)
	require.ErrorIs(t, rt.FlushWorkspace(ctx, "id"), runtime.ErrWorkspaceFUSEUnsupported)
}

func TestCreatePodUsesConfiguredTmpDiskLimit(t *testing.T) {
	client := fake.NewSimpleClientset()

	pod, err := createPod(context.Background(), client, "default", runtime.SandboxSpec{
		ID:      "sandbox-test",
		Image:   "sandbox:latest",
		TmpDisk: "200Mi",
	})

	require.NoError(t, err)
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == "tmp" {
			require.NotNil(t, volume.EmptyDir)
			require.NotNil(t, volume.EmptyDir.SizeLimit)
			assert.Equal(t, int64(200*1024*1024), volume.EmptyDir.SizeLimit.Value())
			return
		}
	}
	t.Fatal("tmp volume not found")
}

func TestCreatePodDefaultsTmpDiskLimitTo50Mi(t *testing.T) {
	client := fake.NewSimpleClientset()

	pod, err := createPod(context.Background(), client, "default", runtime.SandboxSpec{
		ID:    "sandbox-test",
		Image: "sandbox:latest",
	})

	require.NoError(t, err)
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == "tmp" {
			require.NotNil(t, volume.EmptyDir)
			require.NotNil(t, volume.EmptyDir.SizeLimit)
			assert.Equal(t, int64(50*1024*1024), volume.EmptyDir.SizeLimit.Value())
			return
		}
	}
	t.Fatal("tmp volume not found")
}

func TestCreatePodRendersPreparedFUSESidecar(t *testing.T) {
	client := fake.NewSimpleClientset()
	spec := preparedFUSESpecForTest()

	pod, err := createPod(context.Background(), client, "sandbox-runtime", spec)
	require.NoError(t, err)

	require.Len(t, pod.Spec.InitContainers, 1)
	mounter := pod.Spec.InitContainers[0]
	assert.Equal(t, "workspace-mounter", mounter.Name)
	assert.Equal(t, spec.WorkspaceFUSE.MounterImage, mounter.Image)
	require.NotNil(t, mounter.RestartPolicy)
	assert.Equal(t, corev1.ContainerRestartPolicyAlways, *mounter.RestartPolicy)
	assert.Equal(t, []string{"/usr/local/bin/workspace-mounter", "supervise"}, mounter.Command)
	require.NotNil(t, mounter.SecurityContext)
	require.NotNil(t, mounter.SecurityContext.Privileged)
	assert.True(t, *mounter.SecurityContext.Privileged)
	require.NotNil(t, mounter.SecurityContext.RunAsUser)
	assert.Equal(t, int64(0), *mounter.SecurityContext.RunAsUser)
	require.NotNil(t, mounter.SecurityContext.ReadOnlyRootFilesystem)
	assert.True(t, *mounter.SecurityContext.ReadOnlyRootFilesystem)
	require.NotNil(t, mounter.SecurityContext.AppArmorProfile)
	assert.Equal(t, corev1.AppArmorProfileTypeLocalhost, mounter.SecurityContext.AppArmorProfile.Type)
	require.NotNil(t, mounter.SecurityContext.AppArmorProfile.LocalhostProfile)
	assert.Equal(t, spec.WorkspaceFUSE.LSMProfile, *mounter.SecurityContext.AppArmorProfile.LocalhostProfile)

	assert.Equal(t, corev1.MountPropagationBidirectional, *podVolumeMount(t, mounter.VolumeMounts, "workspace").MountPropagation)
	assert.Equal(t, corev1.MountPropagationHostToContainer, *podVolumeMount(t, pod.Spec.Containers[0].VolumeMounts, "workspace").MountPropagation)
	assert.False(t, hasPodVolumeMount(pod.Spec.Containers[0].VolumeMounts, "workspace-credentials"))
	assert.False(t, hasPodVolumeMount(pod.Spec.Containers[0].VolumeMounts, "dev-fuse"))
	assert.False(t, hasPodVolumeMount(pod.Spec.Containers[0].VolumeMounts, "mounter-run"))

	workspace := podVolume(t, pod.Spec.Volumes, "workspace")
	require.NotNil(t, workspace.EmptyDir)
	assert.Equal(t, corev1.StorageMediumMemory, workspace.EmptyDir.Medium)
	cache := podVolume(t, pod.Spec.Volumes, "fuse-cache")
	require.NotNil(t, cache.EmptyDir)
	require.NotNil(t, cache.EmptyDir.SizeLimit)
	assert.Equal(t, int64(2*1024*1024*1024), cache.EmptyDir.SizeLimit.Value())
	assert.Empty(t, cache.EmptyDir.Medium)
	run := podVolume(t, pod.Spec.Volumes, "mounter-run")
	require.NotNil(t, run.EmptyDir)
	assert.Equal(t, corev1.StorageMediumMemory, run.EmptyDir.Medium)
	require.NotNil(t, run.EmptyDir.SizeLimit)
	assert.Equal(t, int64(16*1024*1024), run.EmptyDir.SizeLimit.Value())
	fuseDevice := podVolume(t, pod.Spec.Volumes, "dev-fuse")
	require.NotNil(t, fuseDevice.HostPath)
	assert.Equal(t, "/dev/fuse", fuseDevice.HostPath.Path)
	require.NotNil(t, fuseDevice.HostPath.Type)
	assert.Equal(t, corev1.HostPathCharDev, *fuseDevice.HostPath.Type)
	credentials := podVolume(t, pod.Spec.Volumes, "workspace-credentials")
	require.NotNil(t, credentials.Secret)
	assert.Equal(t, spec.WorkspaceFUSE.SecretName, credentials.Secret.SecretName)
	require.NotNil(t, credentials.Secret.DefaultMode)
	assert.Equal(t, int32(0o400), *credentials.Secret.DefaultMode)
	assert.True(t, podVolumeMount(t, mounter.VolumeMounts, "workspace-credentials").ReadOnly)

	require.NotNil(t, mounter.StartupProbe)
	require.NotNil(t, mounter.StartupProbe.Exec)
	assert.Equal(t, []string{"/usr/local/bin/workspace-mounter", "health", "prepared"}, mounter.StartupProbe.Exec.Command)
	assert.Equal(t, int32(2), mounter.StartupProbe.PeriodSeconds)
	assert.Equal(t, int32(30), mounter.StartupProbe.FailureThreshold)
	require.NotNil(t, mounter.ReadinessProbe)
	require.NotNil(t, mounter.ReadinessProbe.Exec)
	assert.Equal(t, []string{"/usr/local/bin/workspace-mounter", "health", "ready"}, mounter.ReadinessProbe.Exec.Command)
	assert.Equal(t, int32(10), mounter.ReadinessProbe.PeriodSeconds)
	assert.Equal(t, int32(3), mounter.ReadinessProbe.FailureThreshold)
	assert.Nil(t, mounter.LivenessProbe)
	require.NotNil(t, mounter.Lifecycle)
	require.NotNil(t, mounter.Lifecycle.PreStop)
	require.NotNil(t, mounter.Lifecycle.PreStop.Exec)
	assert.Equal(t, []string{"/usr/local/bin/workspace-mounter", "shutdown"}, mounter.Lifecycle.PreStop.Exec.Command)

	require.NotNil(t, pod.Spec.AutomountServiceAccountToken)
	assert.False(t, *pod.Spec.AutomountServiceAccountToken)
	require.NotNil(t, pod.Spec.ShareProcessNamespace)
	assert.False(t, *pod.Spec.ShareProcessNamespace)
	require.NotNil(t, pod.Spec.EnableServiceLinks)
	assert.False(t, *pod.Spec.EnableServiceLinks)
	require.NotNil(t, pod.Spec.TerminationGracePeriodSeconds)
	assert.Equal(t, int64(90), *pod.Spec.TerminationGracePeriodSeconds)

	require.Len(t, pod.Spec.Containers, 1)
	sandbox := pod.Spec.Containers[0]
	require.NotNil(t, sandbox.SecurityContext)
	require.NotNil(t, sandbox.SecurityContext.RunAsNonRoot)
	assert.True(t, *sandbox.SecurityContext.RunAsNonRoot)
	require.NotNil(t, sandbox.SecurityContext.RunAsUser)
	assert.Equal(t, int64(1000), *sandbox.SecurityContext.RunAsUser)
	require.NotNil(t, sandbox.SecurityContext.RunAsGroup)
	assert.Equal(t, int64(1000), *sandbox.SecurityContext.RunAsGroup)
	require.NotNil(t, sandbox.SecurityContext.AllowPrivilegeEscalation)
	assert.False(t, *sandbox.SecurityContext.AllowPrivilegeEscalation)
	require.NotNil(t, sandbox.SecurityContext.Capabilities)
	assert.Equal(t, []corev1.Capability{"ALL"}, sandbox.SecurityContext.Capabilities.Drop)
	assert.Nil(t, sandbox.SecurityContext.Privileged)

	assert.Equal(t, "true", pod.Labels["sandbox.pool"])
	assert.Equal(t, "preparing", pod.Labels["sandbox.pool.state"])
	assert.Equal(t, "aerukz4jvpg66ajdivtytk6n54asgrlhrgv433ybencwpcnlzxxq", pod.Labels["sandbox.pool.key"])
	assert.Len(t, pod.Labels["sandbox.pool.key"], 52)
	assert.Equal(t, spec.ID, pod.Labels["sandbox.pool.instance"])
	assert.Equal(t, "fuse", pod.Labels["sandbox.workspace.mode"])
	assert.Equal(t, spec.WorkspaceFUSE.Provider, pod.Labels["sandbox.workspace.provider"])

	runtimeUID := podEnv(t, mounter.Env, "SANDBOX_RUNTIME_UID")
	require.NotNil(t, runtimeUID.ValueFrom)
	require.NotNil(t, runtimeUID.ValueFrom.FieldRef)
	assert.Equal(t, "metadata.uid", runtimeUID.ValueFrom.FieldRef.FieldPath)
	bootstrapEnv := podEnv(t, mounter.Env, "SANDBOX_MOUNTER_BOOTSTRAP")
	var bootstrap map[string]any
	require.NoError(t, json.Unmarshal([]byte(bootstrapEnv.Value), &bootstrap))
	assert.Equal(t, float64(1), bootstrap["version"])
	assert.Equal(t, spec.WorkspaceFUSE.Provider, bootstrap["provider"])
	assert.Equal(t, spec.WorkspaceFUSE.Bucket, bootstrap["bucket"])
	assert.Equal(t, "https://minio.example.com:9000", bootstrap["endpoint"])
	assert.Equal(t, spec.WorkspaceFUSE.Region, bootstrap["region"])
	assert.Equal(t, spec.WorkspaceFUSE.Profile, bootstrap["profile"])
	assert.Equal(t, "/run/secrets/workspace/accessKey", bootstrap["access_key_file"])
	assert.Equal(t, "/run/secrets/workspace/secretKey", bootstrap["secret_key_file"])
	assert.Equal(t, "/run/s3fs/passwd-s3fs", bootstrap["passwd_file"])
	assert.Equal(t, "/run/secrets/workspace/ca.crt", bootstrap["ca_file"])
	assert.Equal(t, "/var/cache/s3fs", bootstrap["cache_dir"])
	assert.Equal(t, "/workspace", bootstrap["mount_path"])
	assert.Equal(t, spec.WorkspaceFUSE.PoolKey, bootstrap["pool_key"])
	assert.Equal(t, float64(30), bootstrap["mount_timeout_seconds"])
	assert.Equal(t, float64(45), bootstrap["flush_timeout_seconds"])
	assert.Equal(t, float64(20), bootstrap["unmount_timeout_seconds"])
	assert.NotContains(t, bootstrap, "prefix")
	assert.NotContains(t, bootstrap, "workspace_identity")
	assert.NotContains(t, bootstrap, "lease_generation")
	assert.False(t, hasPodEnv(sandbox.Env, "SANDBOX_RUNTIME_UID"))
	assert.False(t, hasPodEnv(sandbox.Env, "SANDBOX_MOUNTER_BOOTSTRAP"))

	assert.Equal(t, []string{"8.8.8.8", "2001:4860:4860::8888"}, pod.Spec.DNSConfig.Nameservers)
	assert.Equal(t, "50m", mounter.Resources.Requests.Cpu().String())
	assert.Equal(t, "64Mi", mounter.Resources.Requests.Memory().String())
	assert.Equal(t, "512Mi", mounter.Resources.Requests.StorageEphemeral().String())
	assert.Equal(t, "1", mounter.Resources.Limits.Cpu().String())
	assert.Equal(t, "512Mi", mounter.Resources.Limits.Memory().String())
	assert.Equal(t, "3Gi", mounter.Resources.Limits.StorageEphemeral().String())
}

func TestCreatePodPreparedFUSERejectsNonHostDNSCIDR(t *testing.T) {
	client := fake.NewSimpleClientset()
	spec := preparedFUSESpecForTest()
	spec.WorkspaceFUSE.SystemEgress.DNSCIDRs = []string{"10.0.0.0/24"}

	_, err := createPod(context.Background(), client, "sandbox-runtime", spec)
	require.ErrorContains(t, err, "host-only")
	pods, listErr := client.CoreV1().Pods("sandbox-runtime").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, listErr)
	assert.Empty(t, pods.Items)
}

func TestCreatePodPreparedFUSERejectsNonDigestPoolKey(t *testing.T) {
	client := fake.NewSimpleClientset()
	spec := preparedFUSESpecForTest()
	spec.WorkspaceFUSE.PoolKey = "not-a-sha256-digest"

	_, err := createPod(context.Background(), client, "sandbox-runtime", spec)
	require.ErrorContains(t, err, "PoolKey")
	pods, listErr := client.CoreV1().Pods("sandbox-runtime").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, listErr)
	assert.Empty(t, pods.Items)
}

func TestCreatePodPreparedFUSETerminationGraceCoversFlushAndUnmount(t *testing.T) {
	client := fake.NewSimpleClientset()
	spec := preparedFUSESpecForTest()
	spec.WorkspaceFUSE.FlushTimeout = 80*time.Second + time.Nanosecond
	spec.WorkspaceFUSE.UnmountTimeout = 20 * time.Second

	pod, err := createPod(context.Background(), client, "sandbox-runtime", spec)
	require.NoError(t, err)
	require.NotNil(t, pod.Spec.TerminationGracePeriodSeconds)
	assert.Equal(t, int64(116), *pod.Spec.TerminationGracePeriodSeconds)
}

func TestCreatePodPreparedFUSEDoesNotLeakWorkspaceAuthorization(t *testing.T) {
	client := fake.NewSimpleClientset()
	spec := preparedFUSESpecForTest()
	spec.Labels = map[string]string{
		"sandbox.pool.state":         "consumed",
		"sandbox.pool.instance":      "attacker-instance",
		"workspace.prefix":           "do-not-leak-prefix",
		"workspace.identity":         "do-not-leak-identity",
		"workspace.lease-generation": "do-not-leak-generation",
	}
	spec.Mounts = []runtime.Mount{{HostPath: "/do-not-leak-prefix", ContainerPath: "/workspace"}}
	spec.NetworkWhitelist = []string{"do-not-leak-identity"}

	pod, err := createPod(context.Background(), client, "sandbox-runtime", spec)
	require.NoError(t, err)
	raw, err := json.Marshal(pod)
	require.NoError(t, err)
	serialized := string(raw)
	assert.NotContains(t, serialized, "do-not-leak-prefix")
	assert.NotContains(t, serialized, "do-not-leak-identity")
	assert.NotContains(t, serialized, "do-not-leak-generation")
	assert.Equal(t, "preparing", pod.Labels["sandbox.pool.state"])
	assert.Equal(t, spec.ID, pod.Labels["sandbox.pool.instance"])
}

func preparedFUSESpecForTest() runtime.SandboxSpec {
	return runtime.SandboxSpec{
		ID: "prepared-shell-a", Image: "sandbox@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Memory: "512Mi", MemoryRequest: "128Mi", CPU: "500m", CPURequest: "100m", TmpDisk: "64Mi",
		WorkspaceFUSE: &runtime.WorkspaceFUSESpec{
			RuntimeType: "kubernetes", Provider: "minio", Driver: "s3fs", Profile: "minio-sigv4-path-style-v1",
			StorageIdentity: "storage-a", CredentialGeneration: "credentials-v1",
			MounterImage: "mounter@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			SecretName:   "workspace-secret", CASecretKey: "ca.crt", Bucket: "sandbox",
			Endpoint: "minio.example.com:9000", Region: "us-east-1", UseSSL: true,
			CacheSize: "2Gi", CacheMedium: "disk", MountTimeout: 30 * time.Second, FlushTimeout: 45 * time.Second, UnmountTimeout: 20 * time.Second,
			LSMProfile: "sandbox-fuse", PoolKey: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			MounterResources: runtime.WorkspaceFUSEResources{
				CPURequest: "50m", CPULimit: "1", MemoryRequest: "64Mi", MemoryLimit: "512Mi",
				EphemeralStorageRequest: "512Mi", EphemeralStorageLimit: "3Gi",
			},
			SystemEgress: runtime.SystemEgressSpec{DNSCIDRs: []string{"8.8.8.8/32", "2001:4860:4860::8888/128"}},
		},
	}
}

func podVolumeMount(t *testing.T, mounts []corev1.VolumeMount, name string) corev1.VolumeMount {
	t.Helper()
	for _, mount := range mounts {
		if mount.Name == name {
			return mount
		}
	}
	t.Fatalf("volume mount %q not found", name)
	return corev1.VolumeMount{}
}

func hasPodVolumeMount(mounts []corev1.VolumeMount, name string) bool {
	for _, mount := range mounts {
		if mount.Name == name {
			return true
		}
	}
	return false
}

func podVolume(t *testing.T, volumes []corev1.Volume, name string) corev1.Volume {
	t.Helper()
	for _, volume := range volumes {
		if volume.Name == name {
			return volume
		}
	}
	t.Fatalf("volume %q not found", name)
	return corev1.Volume{}
}

func podEnv(t *testing.T, env []corev1.EnvVar, name string) corev1.EnvVar {
	t.Helper()
	for _, item := range env {
		if item.Name == name {
			return item
		}
	}
	t.Fatalf("environment variable %q not found", name)
	return corev1.EnvVar{}
}

func hasPodEnv(env []corev1.EnvVar, name string) bool {
	for _, item := range env {
		if item.Name == name {
			return true
		}
	}
	return false
}

func TestPreparedFUSESpecFixtureUsesNoAuthorizationFields(t *testing.T) {
	// Guard the leak test's sentinels against accidental overlap with fixed values.
	raw, err := json.Marshal(preparedFUSESpecForTest().WorkspaceFUSE)
	require.NoError(t, err)
	assert.False(t, strings.Contains(string(raw), "do-not-leak"))
}
