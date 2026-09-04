package kubernetes

import (
	"context"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"path"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/goairix/sandbox/internal/runtime"
)

// createPod creates a sandbox pod from the given spec.
func createPod(ctx context.Context, client kubernetes.Interface, namespace string, spec runtime.SandboxSpec) (*corev1.Pod, error) {
	if spec.WorkspaceFUSE != nil {
		pod, err := buildPreparedFUSEPod(namespace, spec)
		if err != nil {
			return nil, err
		}
		created, err := client.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
		if err != nil {
			return nil, fmt.Errorf("create pod: %w", err)
		}
		return created, nil
	}

	labels := map[string]string{
		"app":             "sandbox",
		"sandbox.id":      spec.ID,
		"sandbox.managed": "true",
	}
	for k, v := range spec.Labels {
		labels[k] = v
	}

	resources := corev1.ResourceRequirements{}
	falseVal := false
	if spec.Memory != "" || spec.CPU != "" {
		resources.Limits = corev1.ResourceList{}
		resources.Requests = corev1.ResourceList{}
		if spec.Memory != "" {
			mem, err := resource.ParseQuantity(spec.Memory)
			if err != nil {
				return nil, fmt.Errorf("parse memory quantity %q: %w", spec.Memory, err)
			}
			resources.Limits[corev1.ResourceMemory] = mem
			req := mem
			if spec.MemoryRequest != "" {
				if req, err = resource.ParseQuantity(spec.MemoryRequest); err != nil {
					return nil, fmt.Errorf("parse memory request %q: %w", spec.MemoryRequest, err)
				}
			}
			resources.Requests[corev1.ResourceMemory] = req
		}
		if spec.CPU != "" {
			cpu, err := resource.ParseQuantity(spec.CPU)
			if err != nil {
				return nil, fmt.Errorf("parse cpu quantity %q: %w", spec.CPU, err)
			}
			resources.Limits[corev1.ResourceCPU] = cpu
			req := cpu
			if spec.CPURequest != "" {
				if req, err = resource.ParseQuantity(spec.CPURequest); err != nil {
					return nil, fmt.Errorf("parse cpu request %q: %w", spec.CPURequest, err)
				}
			}
			resources.Requests[corev1.ResourceCPU] = req
		}
	}

	securityContext := &corev1.SecurityContext{
		ReadOnlyRootFilesystem:   &spec.ReadOnlyRootFS,
		AllowPrivilegeEscalation: &falseVal,
	}
	if spec.RunAsUser > 0 {
		securityContext.RunAsUser = &spec.RunAsUser
	}

	// Determine workspace volume source: HostPath if a /workspace mount is
	// specified, otherwise EmptyDir with optional disk quota.
	workspaceEmptyDir := &corev1.EmptyDirVolumeSource{}
	if spec.Disk != "" {
		diskQty, err := resource.ParseQuantity(spec.Disk)
		if err != nil {
			return nil, fmt.Errorf("parse disk quantity %q: %w", spec.Disk, err)
		}
		workspaceEmptyDir.SizeLimit = &diskQty
	}
	workspaceVolume := corev1.VolumeSource{EmptyDir: workspaceEmptyDir}
	for _, m := range spec.Mounts {
		if m.ContainerPath == "/workspace" {
			hostPathType := corev1.HostPathDirectory
			workspaceVolume = corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: m.HostPath,
					Type: &hostPathType,
				},
			}
			break
		}
	}

	tmpDisk := spec.TmpDisk
	if tmpDisk == "" {
		tmpDisk = runtime.DefaultTmpDisk
	}
	tmpDiskQty, err := resource.ParseQuantity(tmpDisk)
	if err != nil {
		return nil, fmt.Errorf("parse tmp disk quantity %q: %w", tmpDisk, err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.ID,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			// Disable SA token mount and K8s service env injection to avoid
			// leaking cluster topology and credentials into the sandbox.
			AutomountServiceAccountToken: &falseVal,
			EnableServiceLinks:           &falseVal,
			// Pod-level seccomp: restrict syscalls to the runtime default allowlist.
			SecurityContext: &corev1.PodSecurityContext{
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			// Use public DNS instead of CoreDNS to prevent cluster service name
			// resolution and avoid leaking cluster topology via search domains.
			DNSPolicy: corev1.DNSNone,
			DNSConfig: &corev1.PodDNSConfig{
				Nameservers: []string{"8.8.8.8", "1.1.1.1"},
			},
			Containers: []corev1.Container{
				{
					Name:            "sandbox",
					Image:           spec.Image,
					Command:         []string{"sleep", "infinity"},
					WorkingDir:      "/workspace",
					Resources:       resources,
					SecurityContext: securityContext,
					// Override kubelet-injected KUBERNETES_* env vars to empty strings.
					// enableServiceLinks=false suppresses other service vars but not these.
					// The API server is unreachable anyway (no SA token + network policy),
					// but clearing them avoids information leakage in security audits.
					Env: []corev1.EnvVar{
						{Name: "KUBERNETES_SERVICE_HOST", Value: ""},
						{Name: "KUBERNETES_SERVICE_PORT", Value: ""},
						{Name: "KUBERNETES_SERVICE_PORT_HTTPS", Value: ""},
						{Name: "KUBERNETES_PORT", Value: ""},
						{Name: "KUBERNETES_PORT_443_TCP", Value: ""},
						{Name: "KUBERNETES_PORT_443_TCP_PROTO", Value: ""},
						{Name: "KUBERNETES_PORT_443_TCP_PORT", Value: ""},
						{Name: "KUBERNETES_PORT_443_TCP_ADDR", Value: ""},
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "workspace",
							MountPath: "/workspace",
						},
						{
							Name:      "tmp",
							MountPath: "/tmp",
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name:         "workspace",
					VolumeSource: workspaceVolume,
				},
				{
					Name: "tmp",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{
							SizeLimit: &tmpDiskQty,
						},
					},
				},
			},
		},
	}

	created, err := client.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create pod: %w", err)
	}
	return created, nil
}

const (
	workspaceMountPath      = "/workspace"
	mounterRunPath          = "/run/s3fs"
	mounterSecretPath       = "/run/secrets/workspace"
	mounterCachePath        = "/var/cache/s3fs"
	mounterBinary           = "/usr/local/bin/workspace-mounter"
	mounterRunVolumeSize    = "16Mi"
	preparedTerminationSecs = int64(90)
)

type preparedMounterBootstrap struct {
	Version               int    `json:"version"`
	Provider              string `json:"provider"`
	Bucket                string `json:"bucket"`
	Endpoint              string `json:"endpoint"`
	Region                string `json:"region,omitempty"`
	Profile               string `json:"profile"`
	AccessKeyFile         string `json:"access_key_file"`
	SecretKeyFile         string `json:"secret_key_file"`
	PasswdFile            string `json:"passwd_file"`
	CAFile                string `json:"ca_file,omitempty"`
	CacheDir              string `json:"cache_dir"`
	MountPath             string `json:"mount_path"`
	PoolKey               string `json:"pool_key"`
	MountTimeoutSeconds   int64  `json:"mount_timeout_seconds"`
	FlushTimeoutSeconds   int64  `json:"flush_timeout_seconds"`
	UnmountTimeoutSeconds int64  `json:"unmount_timeout_seconds"`
}

func buildPreparedFUSEPod(namespace string, spec runtime.SandboxSpec) (*corev1.Pod, error) {
	fuse := spec.WorkspaceFUSE
	if fuse.CacheMedium != "disk" {
		return nil, fmt.Errorf("workspace FUSE cache medium must be disk, got %q", fuse.CacheMedium)
	}
	cacheSize, err := parsePositiveQuantity("workspace FUSE cache size", fuse.CacheSize)
	if err != nil {
		return nil, err
	}
	mounterRunSize, err := resource.ParseQuantity(mounterRunVolumeSize)
	if err != nil {
		return nil, fmt.Errorf("parse mounter run volume size: %w", err)
	}
	tmpDisk := spec.TmpDisk
	if tmpDisk == "" {
		tmpDisk = runtime.DefaultTmpDisk
	}
	tmpDiskSize, err := resource.ParseQuantity(tmpDisk)
	if err != nil {
		return nil, fmt.Errorf("parse tmp disk quantity %q: %w", tmpDisk, err)
	}

	sandboxResources, err := preparedSandboxResources(spec)
	if err != nil {
		return nil, err
	}
	mounterResources, err := preparedMounterResources(fuse.MounterResources)
	if err != nil {
		return nil, err
	}
	nameservers, err := hostOnlyNameservers(fuse.SystemEgress.DNSCIDRs)
	if err != nil {
		return nil, err
	}
	poolKeyLabel, err := preparedPoolKeyLabel(fuse.PoolKey)
	if err != nil {
		return nil, err
	}
	terminationGraceSeconds, err := fuseTerminationGraceSeconds(fuse.FlushTimeout, fuse.UnmountTimeout)
	if err != nil {
		return nil, err
	}
	if fuse.MountTimeout <= 0 {
		return nil, fmt.Errorf("workspace FUSE mount timeout must be positive")
	}
	endpoint := fuse.Endpoint
	if fuse.Provider == "minio" && !strings.Contains(endpoint, "://") {
		scheme := "http://"
		if fuse.UseSSL {
			scheme = "https://"
		}
		endpoint = scheme + endpoint
	}
	bootstrap, err := json.Marshal(preparedMounterBootstrap{
		Version:               1,
		Provider:              fuse.Provider,
		Bucket:                fuse.Bucket,
		Endpoint:              endpoint,
		Region:                fuse.Region,
		Profile:               fuse.Profile,
		AccessKeyFile:         path.Join(mounterSecretPath, "accessKey"),
		SecretKeyFile:         path.Join(mounterSecretPath, "secretKey"),
		PasswdFile:            path.Join(mounterRunPath, "passwd-s3fs"),
		CAFile:                secretFilePath(fuse.CASecretKey),
		CacheDir:              mounterCachePath,
		MountPath:             workspaceMountPath,
		PoolKey:               fuse.PoolKey,
		MountTimeoutSeconds:   ceilDurationSeconds(fuse.MountTimeout),
		FlushTimeoutSeconds:   ceilDurationSeconds(fuse.FlushTimeout),
		UnmountTimeoutSeconds: ceilDurationSeconds(fuse.UnmountTimeout),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal workspace mounter bootstrap: %w", err)
	}

	falseVal := false
	trueVal := true
	rootUID := int64(0)
	sandboxUID := int64(1000)
	workspacePropagation := corev1.MountPropagationBidirectional
	sandboxPropagation := corev1.MountPropagationHostToContainer
	deviceType := corev1.HostPathCharDev
	secretMode := int32(0o400)
	restartAlways := corev1.ContainerRestartPolicyAlways

	mounterSecurity := &corev1.SecurityContext{
		Privileged:             &trueVal,
		RunAsUser:              &rootUID,
		ReadOnlyRootFilesystem: &trueVal,
	}
	if fuse.LSMProfile != "" {
		profile := fuse.LSMProfile
		mounterSecurity.AppArmorProfile = &corev1.AppArmorProfile{
			Type:             corev1.AppArmorProfileTypeLocalhost,
			LocalhostProfile: &profile,
		}
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.ID,
			Namespace: namespace,
			Labels: map[string]string{
				"app":                        "sandbox",
				"sandbox.id":                 spec.ID,
				"sandbox.managed":            "true",
				"sandbox.pool":               "true",
				"sandbox.pool.state":         "preparing",
				"sandbox.pool.key":           poolKeyLabel,
				"sandbox.pool.instance":      spec.ID,
				"sandbox.workspace.mode":     "fuse",
				"sandbox.workspace.provider": fuse.Provider,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			AutomountServiceAccountToken:  &falseVal,
			EnableServiceLinks:            &falseVal,
			ShareProcessNamespace:         &falseVal,
			DNSPolicy:                     corev1.DNSNone,
			DNSConfig:                     &corev1.PodDNSConfig{Nameservers: nameservers},
			TerminationGracePeriodSeconds: &terminationGraceSeconds,
			SecurityContext: &corev1.PodSecurityContext{
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			InitContainers: []corev1.Container{
				{
					Name:  "workspace-mounter",
					Image: fuse.MounterImage,
					// The trusted supervisor sets the emptyDir anchor to root:root
					// mode 0555 before its prepared health command can succeed.
					Command:         []string{mounterBinary, "supervise"},
					RestartPolicy:   &restartAlways,
					SecurityContext: mounterSecurity,
					Resources:       mounterResources,
					Env: []corev1.EnvVar{
						{
							Name: "SANDBOX_RUNTIME_UID",
							ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{
								APIVersion: "v1",
								FieldPath:  "metadata.uid",
							}},
						},
						{Name: "SANDBOX_MOUNTER_BOOTSTRAP", Value: string(bootstrap)},
					},
					Lifecycle: &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{
						Exec: &corev1.ExecAction{Command: []string{mounterBinary, "shutdown"}},
					}},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "workspace", MountPath: workspaceMountPath, MountPropagation: &workspacePropagation},
						{Name: "fuse-cache", MountPath: mounterCachePath},
						{Name: "dev-fuse", MountPath: "/dev/fuse"},
						{Name: "workspace-credentials", MountPath: mounterSecretPath, ReadOnly: true},
						{Name: "mounter-run", MountPath: mounterRunPath},
					},
					StartupProbe: &corev1.Probe{
						ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{mounterBinary, "health", "prepared"}}},
						PeriodSeconds:    2,
						FailureThreshold: 30,
					},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{mounterBinary, "health", "ready"}}},
						PeriodSeconds:    10,
						FailureThreshold: 3,
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:       "sandbox",
					Image:      spec.Image,
					Command:    []string{"sleep", "infinity"},
					WorkingDir: workspaceMountPath,
					Resources:  sandboxResources,
					SecurityContext: &corev1.SecurityContext{
						RunAsNonRoot:             &trueVal,
						RunAsUser:                &sandboxUID,
						RunAsGroup:               &sandboxUID,
						AllowPrivilegeEscalation: &falseVal,
						ReadOnlyRootFilesystem:   &trueVal,
						Capabilities: &corev1.Capabilities{
							Drop: []corev1.Capability{"ALL"},
						},
					},
					Env: sandboxKubernetesEnv(),
					VolumeMounts: []corev1.VolumeMount{
						{Name: "workspace", MountPath: workspaceMountPath, MountPropagation: &sandboxPropagation},
						{Name: "tmp", MountPath: "/tmp"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
				{Name: "fuse-cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &cacheSize}}},
				{Name: "dev-fuse", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev/fuse", Type: &deviceType}}},
				{Name: "workspace-credentials", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: fuse.SecretName, DefaultMode: &secretMode}}},
				{Name: "mounter-run", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: &mounterRunSize}}},
				{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &tmpDiskSize}}},
			},
		},
	}
	return pod, nil
}

func preparedSandboxResources(spec runtime.SandboxSpec) (corev1.ResourceRequirements, error) {
	resources := corev1.ResourceRequirements{}
	if spec.Memory != "" || spec.CPU != "" {
		resources.Requests = corev1.ResourceList{}
		resources.Limits = corev1.ResourceList{}
	}
	if spec.Memory != "" {
		limit, err := resource.ParseQuantity(spec.Memory)
		if err != nil {
			return resources, fmt.Errorf("parse memory quantity %q: %w", spec.Memory, err)
		}
		request := limit
		if spec.MemoryRequest != "" {
			request, err = resource.ParseQuantity(spec.MemoryRequest)
			if err != nil {
				return resources, fmt.Errorf("parse memory request %q: %w", spec.MemoryRequest, err)
			}
		}
		resources.Limits[corev1.ResourceMemory] = limit
		resources.Requests[corev1.ResourceMemory] = request
	}
	if spec.CPU != "" {
		limit, err := resource.ParseQuantity(spec.CPU)
		if err != nil {
			return resources, fmt.Errorf("parse cpu quantity %q: %w", spec.CPU, err)
		}
		request := limit
		if spec.CPURequest != "" {
			request, err = resource.ParseQuantity(spec.CPURequest)
			if err != nil {
				return resources, fmt.Errorf("parse cpu request %q: %w", spec.CPURequest, err)
			}
		}
		resources.Limits[corev1.ResourceCPU] = limit
		resources.Requests[corev1.ResourceCPU] = request
	}
	return resources, nil
}

func preparedMounterResources(spec runtime.WorkspaceFUSEResources) (corev1.ResourceRequirements, error) {
	fields := []struct {
		name     string
		value    string
		resource corev1.ResourceName
		list     string
	}{
		{"cpu request", spec.CPURequest, corev1.ResourceCPU, "request"},
		{"cpu limit", spec.CPULimit, corev1.ResourceCPU, "limit"},
		{"memory request", spec.MemoryRequest, corev1.ResourceMemory, "request"},
		{"memory limit", spec.MemoryLimit, corev1.ResourceMemory, "limit"},
		{"ephemeral storage request", spec.EphemeralStorageRequest, corev1.ResourceEphemeralStorage, "request"},
		{"ephemeral storage limit", spec.EphemeralStorageLimit, corev1.ResourceEphemeralStorage, "limit"},
	}
	result := corev1.ResourceRequirements{Requests: corev1.ResourceList{}, Limits: corev1.ResourceList{}}
	for _, field := range fields {
		quantity, err := parsePositiveQuantity("workspace mounter "+field.name, field.value)
		if err != nil {
			return corev1.ResourceRequirements{}, err
		}
		if field.list == "request" {
			result.Requests[field.resource] = quantity
		} else {
			result.Limits[field.resource] = quantity
		}
	}
	return result, nil
}

func parsePositiveQuantity(name, value string) (resource.Quantity, error) {
	if value == "" {
		return resource.Quantity{}, fmt.Errorf("%s must not be empty", name)
	}
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("parse %s %q: %w", name, value, err)
	}
	if quantity.Sign() <= 0 {
		return resource.Quantity{}, fmt.Errorf("%s must be positive", name)
	}
	return quantity, nil
}

func hostOnlyNameservers(cidrs []string) ([]string, error) {
	if len(cidrs) == 0 {
		return nil, fmt.Errorf("workspace FUSE DNS CIDRs must not be empty")
	}
	nameservers := make([]string, 0, len(cidrs))
	for _, cidr := range cidrs {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil || prefix.Bits() != prefix.Addr().BitLen() {
			return nil, fmt.Errorf("workspace FUSE DNS CIDR %q must be a host-only /32 or /128 CIDR", cidr)
		}
		nameservers = append(nameservers, prefix.Addr().Unmap().String())
	}
	return nameservers, nil
}

func secretFilePath(key string) string {
	if key == "" {
		return ""
	}
	return path.Join(mounterSecretPath, path.Base(key))
}

func preparedPoolKeyLabel(poolKey string) (string, error) {
	digest, err := hex.DecodeString(poolKey)
	if err != nil || len(digest) != 32 {
		return "", fmt.Errorf("workspace FUSE PoolKey must be a 64-character SHA-256 hex digest")
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest)), nil
}

func fuseTerminationGraceSeconds(flushTimeout, unmountTimeout time.Duration) (int64, error) {
	if flushTimeout <= 0 || unmountTimeout <= 0 {
		return 0, fmt.Errorf("workspace FUSE flush and unmount timeouts must be positive")
	}
	if flushTimeout > time.Duration(1<<63-1)-unmountTimeout {
		return 0, fmt.Errorf("workspace FUSE flush and unmount timeouts overflow")
	}
	seconds := ceilDurationSeconds(flushTimeout+unmountTimeout) + 15
	if seconds < preparedTerminationSecs {
		seconds = preparedTerminationSecs
	}
	return seconds, nil
}

func ceilDurationSeconds(value time.Duration) int64 {
	seconds := int64(value / time.Second)
	if value%time.Second != 0 {
		seconds++
	}
	return seconds
}

func sandboxKubernetesEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "KUBERNETES_SERVICE_HOST", Value: ""},
		{Name: "KUBERNETES_SERVICE_PORT", Value: ""},
		{Name: "KUBERNETES_SERVICE_PORT_HTTPS", Value: ""},
		{Name: "KUBERNETES_PORT", Value: ""},
		{Name: "KUBERNETES_PORT_443_TCP", Value: ""},
		{Name: "KUBERNETES_PORT_443_TCP_PROTO", Value: ""},
		{Name: "KUBERNETES_PORT_443_TCP_PORT", Value: ""},
		{Name: "KUBERNETES_PORT_443_TCP_ADDR", Value: ""},
	}
}

// deletePod deletes a pod by name.
func deletePod(ctx context.Context, client kubernetes.Interface, namespace, name string) error {
	return client.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

// getPod retrieves a pod by name.
func getPod(ctx context.Context, client kubernetes.Interface, namespace, name string) (*corev1.Pod, error) {
	return client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
}

// waitForPodReady waits until the pod is in Running phase.
func waitForPodReady(ctx context.Context, client kubernetes.Interface, namespace, name string, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timeout waiting for pod %s to be ready", name)
		case <-ticker.C:
			pod, err := getPod(ctx, client, namespace, name)
			if err != nil {
				continue
			}
			if pod.Status.Phase == corev1.PodRunning {
				return nil
			}
			if pod.Status.Phase == corev1.PodFailed {
				return fmt.Errorf("pod %s failed", name)
			}
		}
	}
}

// podStateString converts pod phase to a state string.
func podStateString(phase corev1.PodPhase) string {
	switch phase {
	case corev1.PodRunning:
		return "running"
	case corev1.PodPending:
		return "creating"
	case corev1.PodSucceeded:
		return "stopped"
	case corev1.PodFailed:
		return "error"
	default:
		return "unknown"
	}
}
