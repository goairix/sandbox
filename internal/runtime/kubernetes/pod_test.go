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
	"k8s.io/apimachinery/pkg/api/resource"
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

func TestCreatePodLegacyRenderingRemainsDeepEqual(t *testing.T) {
	client := fake.NewSimpleClientset()
	spec := runtime.SandboxSpec{ID: "legacy-a", Image: "sandbox:legacy", Labels: map[string]string{"custom": "value"}}

	pod, err := createPod(context.Background(), client, "sandbox-runtime", spec)
	require.NoError(t, err)
	falseVal := false
	tmpSize := resource.MustParse(runtime.DefaultTmpDisk)
	expected := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy-a", Namespace: "sandbox-runtime", Labels: map[string]string{
			"app": "sandbox", "sandbox.id": "legacy-a", "sandbox.managed": "true", "custom": "value",
		}},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever, AutomountServiceAccountToken: &falseVal, EnableServiceLinks: &falseVal,
			SecurityContext: &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			DNSPolicy:       corev1.DNSNone, DNSConfig: &corev1.PodDNSConfig{Nameservers: []string{"8.8.8.8", "1.1.1.1"}},
			Containers: []corev1.Container{{
				Name: "sandbox", Image: "sandbox:legacy", Command: []string{"sleep", "infinity"}, WorkingDir: "/workspace",
				Resources:       corev1.ResourceRequirements{},
				SecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: &falseVal, AllowPrivilegeEscalation: &falseVal},
				Env: []corev1.EnvVar{
					{Name: "KUBERNETES_SERVICE_HOST", Value: ""}, {Name: "KUBERNETES_SERVICE_PORT", Value: ""},
					{Name: "KUBERNETES_SERVICE_PORT_HTTPS", Value: ""}, {Name: "KUBERNETES_PORT", Value: ""},
					{Name: "KUBERNETES_PORT_443_TCP", Value: ""}, {Name: "KUBERNETES_PORT_443_TCP_PROTO", Value: ""},
					{Name: "KUBERNETES_PORT_443_TCP_PORT", Value: ""}, {Name: "KUBERNETES_PORT_443_TCP_ADDR", Value: ""},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "workspace", MountPath: "/workspace"}, {Name: "tmp", MountPath: "/tmp"}},
			}},
			Volumes: []corev1.Volume{
				{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &tmpSize}}},
			},
		},
	}
	assert.Equal(t, expected, pod)
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
	assert.Nil(t, mounter.SecurityContext.AppArmorProfile)
	assert.Equal(t, "localhost/sandbox-fuse", pod.Annotations["container.apparmor.security.beta.kubernetes.io/workspace-mounter"])

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
	assert.Equal(t, []corev1.KeyToPath{
		{Key: "accessKey", Path: "accessKey"},
		{Key: "secretKey", Path: "secretKey"},
		{Key: "ca.crt", Path: "ca.crt"},
	}, credentials.Secret.Items)
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

	assert.Equal(t, []string{"2001:4860:4860::8888", "8.8.8.8"}, pod.Spec.DNSConfig.Nameservers)
	assert.Equal(t, "50m", mounter.Resources.Requests.Cpu().String())
	assert.Equal(t, "64Mi", mounter.Resources.Requests.Memory().String())
	assert.Equal(t, "512Mi", mounter.Resources.Requests.StorageEphemeral().String())
	assert.Equal(t, "1", mounter.Resources.Limits.Cpu().String())
	assert.Equal(t, "512Mi", mounter.Resources.Limits.Memory().String())
	assert.Equal(t, "3Gi", mounter.Resources.Limits.StorageEphemeral().String())
	assert.Equal(t, []corev1.HostAlias{
		{IP: "192.0.2.10", Hostnames: []string{"minio.example.com"}},
		{IP: "192.0.2.11", Hostnames: []string{"minio.example.com"}},
	}, pod.Spec.HostAliases)
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

func TestCreatePodPreparedFUSEValidatesBeforeAPICreate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*runtime.SandboxSpec)
	}{
		{"runtime type", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.RuntimeType = "docker" }},
		{"provider", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.Provider = "s3" }},
		{"driver", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.Driver = "goofys" }},
		{"profile", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.Profile = "" }},
		{"bucket", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.Bucket = "" }},
		{"storage identity", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.StorageIdentity = "" }},
		{"credential generation", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.CredentialGeneration = "" }},
		{"unpinned mounter image", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.MounterImage = "mounter:latest" }},
		{"secret name", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.SecretName = "INVALID_SECRET" }},
		{"ca secret key", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.CASecretKey = "invalid/key" }},
		{"ca collides with access key", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.CASecretKey = "accessKey" }},
		{"empty lsm", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.LSMProfile = "" }},
		{"unconfined lsm", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.LSMProfile = "unconfined" }},
		{"uppercase pool key", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.PoolKey = strings.ToUpper(s.WorkspaceFUSE.PoolKey) }},
		{"sandbox id", func(s *runtime.SandboxSpec) { s.ID = "invalid/id" }},
		{"system egress mode", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.SystemEgress.Mode = "unknown" }},
		{"cidr mode without endpoint cidr", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.SystemEgress.EndpointCIDRs = nil }},
		{"dns ports", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.SystemEgress.DNSPorts = nil }},
		{"dns port 53 not approved", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.SystemEgress.DNSPorts = []int32{54} }},
		{"endpoint ports", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.SystemEgress.EndpointPorts = nil }},
		{"proxy", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.SystemEgress.ProxyURL = "http://proxy.invalid" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			spec := preparedFUSESpecForTest()
			tt.mutate(&spec)
			_, err := createPod(context.Background(), client, "sandbox-runtime", spec)
			require.Error(t, err)
			assertNoPods(t, client, "sandbox-runtime")
		})
	}
}

func TestCreatePodPreparedFUSEValidatesEndpointWithoutEchoingIt(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		endpoint string
		aliases  []string
	}{
		{"minio scheme", "minio", "https://secret.invalid:9000", nil},
		{"minio path", "minio", "secret.invalid:9000/bucket", nil},
		{"obs missing scheme", "obs", "secret.invalid", nil},
		{"obs userinfo", "obs", "https://user:pass@secret.invalid", nil},
		{"obs query", "obs", "https://secret.invalid?token=secret", nil},
		{"obs fragment", "obs", "https://secret.invalid/#secret", nil},
		{"obs business path", "obs", "https://secret.invalid/bucket", nil},
		{"ip endpoint with aliases", "minio", "192.0.2.44:9000", []string{"192.0.2.10"}},
		{"ip endpoint outside approved cidr", "minio", "198.51.100.44:9000", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			spec := preparedFUSESpecForTest()
			spec.WorkspaceFUSE.Provider = tt.provider
			spec.WorkspaceFUSE.Endpoint = tt.endpoint
			spec.WorkspaceFUSE.EndpointHostIPs = tt.aliases
			_, err := createPod(context.Background(), client, "sandbox-runtime", spec)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), tt.endpoint)
			assertNoPods(t, client, "sandbox-runtime")
		})
	}
}

func TestCreatePodPreparedFUSEOBSRetainsTLSEndpointHostname(t *testing.T) {
	client := fake.NewSimpleClientset()
	spec := preparedFUSESpecForTest()
	spec.WorkspaceFUSE.Provider = "obs"
	spec.WorkspaceFUSE.Endpoint = "https://obs.private.example.com:443"
	spec.WorkspaceFUSE.EndpointHostIPs = []string{"192.0.2.20"}
	spec.WorkspaceFUSE.SystemEgress.Mode = runtime.SystemEgressCiliumFQDN
	spec.WorkspaceFUSE.SystemEgress.EndpointFQDNs = []string{"obs.private.example.com"}
	spec.WorkspaceFUSE.SystemEgress.EndpointPorts = []int32{443}

	pod, err := createPod(context.Background(), client, "sandbox-runtime", spec)
	require.NoError(t, err)
	mounter := pod.Spec.InitContainers[0]
	var bootstrap map[string]any
	require.NoError(t, json.Unmarshal([]byte(podEnv(t, mounter.Env, "SANDBOX_MOUNTER_BOOTSTRAP").Value), &bootstrap))
	assert.Equal(t, spec.WorkspaceFUSE.Endpoint, bootstrap["endpoint"])
	assert.Equal(t, []corev1.HostAlias{{IP: "192.0.2.20", Hostnames: []string{"obs.private.example.com"}}}, pod.Spec.HostAliases)
}

func TestCreatePodPreparedFUSEValidatesAndCanonicalizesEndpointHostIPs(t *testing.T) {
	for _, hostIP := range []string{"not-an-ip", "192.0.2.010", "2001:0db8::1", "::ffff:192.0.2.10"} {
		t.Run(hostIP, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			spec := preparedFUSESpecForTest()
			spec.WorkspaceFUSE.EndpointHostIPs = []string{hostIP}
			_, err := createPod(context.Background(), client, "sandbox-runtime", spec)
			require.Error(t, err)
			assertNoPods(t, client, "sandbox-runtime")
		})
	}
}

func TestCreatePodPreparedFUSEValidatesDNSResolvers(t *testing.T) {
	tests := [][]string{
		{"1.1.1.1/32", "8.8.8.8/32", "9.9.9.9/32", "2001:4860:4860::8888/128"},
		{"::ffff:192.0.2.10/128"},
	}
	for _, dnsCIDRs := range tests {
		client := fake.NewSimpleClientset()
		spec := preparedFUSESpecForTest()
		spec.WorkspaceFUSE.SystemEgress.DNSCIDRs = dnsCIDRs
		_, err := createPod(context.Background(), client, "sandbox-runtime", spec)
		require.Error(t, err)
		assertNoPods(t, client, "sandbox-runtime")
	}
}

func TestCreatePodPreparedFUSEValidatesResourceRelationships(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*runtime.SandboxSpec)
	}{
		{"sandbox cpu required", func(s *runtime.SandboxSpec) { s.CPU = "" }},
		{"sandbox cpu request", func(s *runtime.SandboxSpec) { s.CPURequest = "2" }},
		{"sandbox memory required", func(s *runtime.SandboxSpec) { s.Memory = "" }},
		{"sandbox memory request", func(s *runtime.SandboxSpec) { s.MemoryRequest = "1Gi" }},
		{"tmp positive", func(s *runtime.SandboxSpec) { s.TmpDisk = "0" }},
		{"mounter cpu request", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.MounterResources.CPURequest = "2" }},
		{"mounter memory request", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.MounterResources.MemoryRequest = "1Gi" }},
		{"mounter ephemeral request", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.MounterResources.EphemeralStorageRequest = "4Gi" }},
		{"cache exceeds ephemeral limit", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.CacheSize = "4Gi" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			spec := preparedFUSESpecForTest()
			tt.mutate(&spec)
			_, err := createPod(context.Background(), client, "sandbox-runtime", spec)
			require.Error(t, err)
			assertNoPods(t, client, "sandbox-runtime")
		})
	}
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
			SecretName:   "workspace-secret", CASecretKey: "ca.crt", EndpointHostIPs: []string{"192.0.2.11", "192.0.2.10", "192.0.2.10"}, Bucket: "sandbox",
			Endpoint: "minio.example.com:9000", Region: "us-east-1", UseSSL: true,
			CacheSize: "2Gi", CacheMedium: "disk", MountTimeout: 30 * time.Second, FlushTimeout: 45 * time.Second, UnmountTimeout: 20 * time.Second,
			LSMProfile: "sandbox-fuse", PoolKey: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			MounterResources: runtime.WorkspaceFUSEResources{
				CPURequest: "50m", CPULimit: "1", MemoryRequest: "64Mi", MemoryLimit: "512Mi",
				EphemeralStorageRequest: "512Mi", EphemeralStorageLimit: "3Gi",
			},
			SystemEgress: runtime.SystemEgressSpec{
				Mode: runtime.SystemEgressCIDR, DNSCIDRs: []string{"2001:4860:4860::8888/128", "8.8.8.8/32", "8.8.8.8/32"}, DNSPorts: []int32{53},
				EndpointCIDRs: []string{"192.0.2.0/24"}, EndpointPorts: []int32{9000},
			},
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

func assertNoPods(t *testing.T, client *fake.Clientset, namespace string) {
	t.Helper()
	pods, err := client.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, pods.Items)
}

func TestPreparedFUSESpecFixtureUsesNoAuthorizationFields(t *testing.T) {
	// Guard the leak test's sentinels against accidental overlap with fixed values.
	raw, err := json.Marshal(preparedFUSESpecForTest().WorkspaceFUSE)
	require.NoError(t, err)
	assert.False(t, strings.Contains(string(raw), "do-not-leak"))
}
