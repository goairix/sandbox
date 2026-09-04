package kubernetes

import (
	"context"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/distribution/reference"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kvalidation "k8s.io/apimachinery/pkg/util/validation"
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

type validatedPreparedFUSEPod struct {
	cacheSize               resource.Quantity
	tmpDiskSize             resource.Quantity
	mounterRunSize          resource.Quantity
	sandboxResources        corev1.ResourceRequirements
	mounterResources        corev1.ResourceRequirements
	nameservers             []string
	poolKeyLabel            string
	endpoint                string
	hostAliases             []corev1.HostAlias
	secretItems             []corev1.KeyToPath
	terminationGraceSeconds int64
}

func buildPreparedFUSEPod(namespace string, spec runtime.SandboxSpec) (*corev1.Pod, error) {
	fuse := spec.WorkspaceFUSE
	validated, err := validatePreparedFUSEPod(spec)
	if err != nil {
		return nil, err
	}
	bootstrap, err := json.Marshal(preparedMounterBootstrap{
		Version:               1,
		Provider:              fuse.Provider,
		Bucket:                fuse.Bucket,
		Endpoint:              validated.endpoint,
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

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.ID,
			Namespace: namespace,
			Annotations: map[string]string{
				"container.apparmor.security.beta.kubernetes.io/workspace-mounter": "localhost/" + fuse.LSMProfile,
			},
			Labels: map[string]string{
				"app":                        "sandbox",
				"sandbox.id":                 spec.ID,
				"sandbox.managed":            "true",
				"sandbox.pool":               "true",
				"sandbox.pool.state":         "preparing",
				"sandbox.pool.key":           validated.poolKeyLabel,
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
			DNSConfig:                     &corev1.PodDNSConfig{Nameservers: validated.nameservers},
			HostAliases:                   validated.hostAliases,
			TerminationGracePeriodSeconds: &validated.terminationGraceSeconds,
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
					Resources:       validated.mounterResources,
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
					Resources:  validated.sandboxResources,
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
				{Name: "fuse-cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &validated.cacheSize}}},
				{Name: "dev-fuse", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev/fuse", Type: &deviceType}}},
				{Name: "workspace-credentials", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: fuse.SecretName, DefaultMode: &secretMode, Items: validated.secretItems}}},
				{Name: "mounter-run", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: &validated.mounterRunSize}}},
				{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &validated.tmpDiskSize}}},
			},
		},
	}
	return pod, nil
}

func validatePreparedFUSEPod(spec runtime.SandboxSpec) (validatedPreparedFUSEPod, error) {
	var validated validatedPreparedFUSEPod
	fuse := spec.WorkspaceFUSE
	if fuse == nil {
		return validated, fmt.Errorf("workspace FUSE spec is required")
	}
	if errs := kvalidation.IsDNS1123Subdomain(spec.ID); len(errs) != 0 {
		return validated, fmt.Errorf("workspace FUSE sandbox ID is invalid")
	}
	if errs := kvalidation.IsValidLabelValue(spec.ID); len(errs) != 0 {
		return validated, fmt.Errorf("workspace FUSE sandbox ID is not label-safe")
	}
	if fuse.RuntimeType != "kubernetes" {
		return validated, fmt.Errorf("workspace FUSE runtime type must be kubernetes")
	}
	if fuse.Provider != "minio" && fuse.Provider != "obs" {
		return validated, fmt.Errorf("workspace FUSE provider must be minio or obs")
	}
	if fuse.Driver != "s3fs" {
		return validated, fmt.Errorf("workspace FUSE driver must be s3fs")
	}
	if !nonEmptyCanonicalValue(fuse.Profile) || !nonEmptyCanonicalValue(fuse.Bucket) ||
		!nonEmptyCanonicalValue(fuse.StorageIdentity) || !nonEmptyCanonicalValue(fuse.CredentialGeneration) {
		return validated, fmt.Errorf("workspace FUSE fixed identity fields must not be empty")
	}
	if !isDigestPinnedImage(spec.Image) {
		return validated, fmt.Errorf("workspace FUSE sandbox image must be pinned by sha256 digest")
	}
	if !isDigestPinnedImage(fuse.MounterImage) {
		return validated, fmt.Errorf("workspace FUSE mounter image must be pinned by sha256 digest")
	}
	if errs := kvalidation.IsDNS1123Subdomain(fuse.SecretName); len(errs) != 0 {
		return validated, fmt.Errorf("workspace FUSE Secret name is invalid")
	}
	if err := validateLSMProfile(fuse.LSMProfile); err != nil {
		return validated, err
	}
	poolKeyLabel, err := preparedPoolKeyLabel(fuse.PoolKey)
	if err != nil {
		return validated, err
	}
	validated.poolKeyLabel = poolKeyLabel

	if fuse.CASecretKey != "" {
		if errs := kvalidation.IsConfigMapKey(fuse.CASecretKey); len(errs) != 0 || fuse.CASecretKey == "accessKey" || fuse.CASecretKey == "secretKey" {
			return validated, fmt.Errorf("workspace FUSE CA Secret key is invalid or conflicts with credential keys")
		}
	}
	validated.secretItems = []corev1.KeyToPath{{Key: "accessKey", Path: "accessKey"}, {Key: "secretKey", Path: "secretKey"}}
	if fuse.CASecretKey != "" {
		validated.secretItems = append(validated.secretItems, corev1.KeyToPath{Key: fuse.CASecretKey, Path: fuse.CASecretKey})
	}

	endpoint, endpointHostname, endpointIP, endpointPort, err := validateFUSEEndpoint(fuse)
	if err != nil {
		return validated, err
	}
	validated.endpoint = endpoint

	approvedNetworks, err := validateSystemEgress(fuse.SystemEgress, endpointHostname, endpointIP, endpointPort)
	if err != nil {
		return validated, err
	}
	if endpointIP {
		addr, _ := netip.ParseAddr(endpointHostname)
		approved := false
		for _, network := range approvedNetworks {
			approved = approved || network.Contains(addr)
		}
		if !approved {
			return validated, fmt.Errorf("workspace FUSE endpoint is outside approved system egress CIDRs")
		}
	}
	validated.nameservers, err = hostOnlyNameservers(fuse.SystemEgress.DNSCIDRs)
	if err != nil {
		return validated, err
	}
	validated.hostAliases, err = endpointHostAliases(endpointHostname, endpointIP, fuse.EndpointHostIPs, approvedNetworks)
	if err != nil {
		return validated, err
	}

	if fuse.CacheMedium != "disk" {
		return validated, fmt.Errorf("workspace FUSE cache medium must be disk")
	}
	validated.cacheSize, err = parsePositiveQuantity("workspace FUSE cache size", fuse.CacheSize)
	if err != nil {
		return validated, err
	}
	validated.mounterRunSize, err = resource.ParseQuantity(mounterRunVolumeSize)
	if err != nil {
		return validated, fmt.Errorf("parse mounter run volume size: %w", err)
	}
	tmpDisk := spec.TmpDisk
	if tmpDisk == "" {
		tmpDisk = runtime.DefaultTmpDisk
	}
	validated.tmpDiskSize, err = parsePositiveQuantity("workspace FUSE tmp disk", tmpDisk)
	if err != nil {
		return validated, err
	}
	if spec.Disk != "" {
		if _, err := parsePositiveQuantity("workspace FUSE sandbox disk", spec.Disk); err != nil {
			return validated, err
		}
	}
	validated.sandboxResources, err = preparedSandboxResources(spec)
	if err != nil {
		return validated, err
	}
	validated.mounterResources, err = preparedMounterResources(fuse.MounterResources)
	if err != nil {
		return validated, err
	}
	if validated.mounterResources.Requests.Cpu().Cmp(*validated.mounterResources.Limits.Cpu()) > 0 ||
		validated.mounterResources.Requests.Memory().Cmp(*validated.mounterResources.Limits.Memory()) > 0 ||
		validated.mounterResources.Requests.StorageEphemeral().Cmp(*validated.mounterResources.Limits.StorageEphemeral()) > 0 {
		return validated, fmt.Errorf("workspace FUSE mounter resource request must not exceed its limit")
	}
	if validated.cacheSize.Cmp(*validated.mounterResources.Limits.StorageEphemeral()) > 0 {
		return validated, fmt.Errorf("workspace FUSE cache size must not exceed mounter ephemeral storage limit")
	}
	validated.terminationGraceSeconds, err = fuseTerminationGraceSeconds(fuse.FlushTimeout, fuse.UnmountTimeout)
	if err != nil {
		return validated, err
	}
	if fuse.MountTimeout <= 0 {
		return validated, fmt.Errorf("workspace FUSE mount timeout must be positive")
	}
	return validated, nil
}

func nonEmptyCanonicalValue(value string) bool {
	return value != "" && strings.TrimSpace(value) == value
}

func isDigestPinnedImage(image string) bool {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return false
	}
	digested, ok := named.(reference.Digested)
	if !ok || digested.Digest().Algorithm() != "sha256" {
		return false
	}
	encoded := digested.Digest().Encoded()
	if len(encoded) != 64 || encoded != strings.ToLower(encoded) {
		return false
	}
	_, err = hex.DecodeString(encoded)
	return err == nil
}

func validateLSMProfile(profile string) error {
	lower := strings.ToLower(profile)
	if !nonEmptyCanonicalValue(profile) || lower == "unconfined" || lower == "label=disable" || strings.HasPrefix(profile, "/") || strings.Contains(profile, "..") || len(profile) > 128 {
		return fmt.Errorf("workspace FUSE LSM profile must be a confined profile name")
	}
	for _, char := range profile {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && !strings.ContainsRune("._/-", char) {
			return fmt.Errorf("workspace FUSE LSM profile is invalid")
		}
	}
	return nil
}

func validateFUSEEndpoint(fuse *runtime.WorkspaceFUSESpec) (string, string, bool, int32, error) {
	const invalidEndpoint = "invalid workspace FUSE endpoint"
	raw := fuse.Endpoint
	var parsed *url.URL
	var err error
	if fuse.Provider == "minio" {
		if strings.Contains(raw, "://") {
			return "", "", false, 0, fmt.Errorf("%s", invalidEndpoint)
		}
		scheme := "http"
		if fuse.UseSSL {
			scheme = "https"
		}
		parsed, err = url.Parse(scheme + "://" + raw)
	} else {
		parsed, err = url.Parse(raw)
		if err == nil && parsed.Scheme != "http" && parsed.Scheme != "https" {
			err = fmt.Errorf("unsupported scheme")
		}
		if err == nil && fuse.UseSSL != (parsed.Scheme == "https") {
			err = fmt.Errorf("scheme mismatch")
		}
	}
	if err != nil || parsed == nil || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" || parsed.Path != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", "", false, 0, fmt.Errorf("%s", invalidEndpoint)
	}
	hostname := parsed.Hostname()
	if hostname == "" {
		return "", "", false, 0, fmt.Errorf("%s", invalidEndpoint)
	}
	endpointIP := false
	if addr, parseErr := netip.ParseAddr(hostname); parseErr == nil {
		if addr.Is4In6() || addr.Zone() != "" || addr.String() != hostname {
			return "", "", false, 0, fmt.Errorf("%s", invalidEndpoint)
		}
		endpointIP = true
	} else if hostname != strings.ToLower(hostname) || len(kvalidation.IsDNS1123Subdomain(hostname)) != 0 {
		return "", "", false, 0, fmt.Errorf("%s", invalidEndpoint)
	}
	port := int32(80)
	if parsed.Scheme == "https" {
		port = 443
	}
	if rawPort := parsed.Port(); rawPort != "" {
		parsedPort, parseErr := strconv.Atoi(rawPort)
		if parseErr != nil || parsedPort < 1 || parsedPort > 65535 {
			return "", "", false, 0, fmt.Errorf("%s", invalidEndpoint)
		}
		port = int32(parsedPort)
	}
	return parsed.String(), hostname, endpointIP, port, nil
}

func validateSystemEgress(spec runtime.SystemEgressSpec, endpointHostname string, endpointIP bool, endpointPort int32) ([]netip.Prefix, error) {
	if spec.ProxyURL != "" {
		return nil, fmt.Errorf("workspace FUSE system egress proxy must be empty")
	}
	dnsPorts, err := canonicalPorts("DNS", spec.DNSPorts)
	if err != nil {
		return nil, err
	}
	if len(dnsPorts) != 1 || dnsPorts[0] != 53 {
		return nil, fmt.Errorf("workspace FUSE system egress DNS port set must be exactly 53")
	}
	endpointPorts, err := canonicalPorts("endpoint", spec.EndpointPorts)
	if err != nil {
		return nil, err
	}
	if !containsCanonicalPort(endpointPorts, endpointPort) {
		return nil, fmt.Errorf("workspace FUSE system egress endpoint port set must include the endpoint")
	}
	endpointCIDRs := canonicalStringSet(spec.EndpointCIDRs)
	networks := make([]netip.Prefix, 0, len(endpointCIDRs))
	for _, raw := range endpointCIDRs {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Addr().Is4In6() || prefix.Addr().Zone() != "" || prefix.String() != raw || prefix != prefix.Masked() {
			return nil, fmt.Errorf("workspace FUSE system egress contains an invalid endpoint CIDR")
		}
		networks = append(networks, prefix)
	}
	switch spec.Mode {
	case runtime.SystemEgressCIDR:
		if len(networks) == 0 {
			return nil, fmt.Errorf("workspace FUSE CIDR system egress requires endpoint CIDRs")
		}
	case runtime.SystemEgressCiliumFQDN:
		if endpointIP {
			return nil, fmt.Errorf("workspace FUSE Cilium system egress requires an FQDN endpoint")
		}
		endpointFQDNs := canonicalStringSet(spec.EndpointFQDNs)
		if len(endpointFQDNs) == 0 {
			return nil, fmt.Errorf("workspace FUSE Cilium system egress requires endpoint FQDNs")
		}
		found := false
		for _, fqdn := range endpointFQDNs {
			if _, parseErr := netip.ParseAddr(fqdn); parseErr == nil || fqdn != strings.ToLower(fqdn) || len(kvalidation.IsDNS1123Subdomain(fqdn)) != 0 {
				return nil, fmt.Errorf("workspace FUSE system egress contains an invalid endpoint FQDN")
			}
			found = found || fqdn == endpointHostname
		}
		if !found {
			return nil, fmt.Errorf("workspace FUSE endpoint FQDN is not approved by system egress")
		}
	default:
		return nil, fmt.Errorf("workspace FUSE system egress mode is invalid")
	}
	return networks, nil
}

func canonicalPorts(kind string, ports []int32) ([]int32, error) {
	if len(ports) == 0 {
		return nil, fmt.Errorf("workspace FUSE system egress %s ports must not be empty", kind)
	}
	canonical := append([]int32(nil), ports...)
	sort.Slice(canonical, func(i, j int) bool { return canonical[i] < canonical[j] })
	result := canonical[:0]
	for _, port := range canonical {
		if port < 1 || port > 65535 {
			return nil, fmt.Errorf("workspace FUSE system egress %s port is invalid", kind)
		}
		if len(result) == 0 || result[len(result)-1] != port {
			result = append(result, port)
		}
	}
	return result, nil
}

func containsCanonicalPort(ports []int32, wanted int32) bool {
	index := sort.Search(len(ports), func(i int) bool { return ports[i] >= wanted })
	return index < len(ports) && ports[index] == wanted
}

func canonicalStringSet(values []string) []string {
	canonical := append([]string(nil), values...)
	sort.Strings(canonical)
	result := canonical[:0]
	for _, value := range canonical {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func endpointHostAliases(hostname string, endpointIP bool, rawIPs []string, approvedNetworks []netip.Prefix) ([]corev1.HostAlias, error) {
	if len(rawIPs) == 0 {
		return nil, nil
	}
	if endpointIP {
		return nil, fmt.Errorf("workspace FUSE endpoint aliases require an FQDN endpoint")
	}
	ips := append([]string(nil), rawIPs...)
	sort.Strings(ips)
	aliases := make([]corev1.HostAlias, 0, len(ips))
	var previous string
	for _, rawIP := range ips {
		addr, err := netip.ParseAddr(rawIP)
		if err != nil || addr.Is4In6() || addr.Zone() != "" || addr.String() != rawIP {
			return nil, fmt.Errorf("workspace FUSE endpoint host alias must be a canonical literal IP")
		}
		if rawIP == previous {
			continue
		}
		approved := false
		for _, network := range approvedNetworks {
			approved = approved || network.Contains(addr)
		}
		if !approved {
			return nil, fmt.Errorf("workspace FUSE endpoint host alias is outside approved system egress CIDRs")
		}
		aliases = append(aliases, corev1.HostAlias{IP: rawIP, Hostnames: []string{hostname}})
		previous = rawIP
	}
	return aliases, nil
}

func preparedSandboxResources(spec runtime.SandboxSpec) (corev1.ResourceRequirements, error) {
	memoryLimit, err := parsePositiveQuantity("workspace FUSE sandbox memory limit", spec.Memory)
	if err != nil {
		return corev1.ResourceRequirements{}, err
	}
	memoryRequest := memoryLimit
	if spec.MemoryRequest != "" {
		memoryRequest, err = parsePositiveQuantity("workspace FUSE sandbox memory request", spec.MemoryRequest)
		if err != nil {
			return corev1.ResourceRequirements{}, err
		}
	}
	cpuLimit, err := parsePositiveQuantity("workspace FUSE sandbox CPU limit", spec.CPU)
	if err != nil {
		return corev1.ResourceRequirements{}, err
	}
	cpuRequest := cpuLimit
	if spec.CPURequest != "" {
		cpuRequest, err = parsePositiveQuantity("workspace FUSE sandbox CPU request", spec.CPURequest)
		if err != nil {
			return corev1.ResourceRequirements{}, err
		}
	}
	if memoryRequest.Cmp(memoryLimit) > 0 || cpuRequest.Cmp(cpuLimit) > 0 {
		return corev1.ResourceRequirements{}, fmt.Errorf("workspace FUSE sandbox resource request must not exceed its limit")
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: memoryRequest, corev1.ResourceCPU: cpuRequest},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: memoryLimit, corev1.ResourceCPU: cpuLimit},
	}, nil
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
	canonical := append([]string(nil), cidrs...)
	sort.Strings(canonical)
	nameservers := make([]string, 0, len(canonical))
	var previous string
	for _, cidr := range canonical {
		if cidr == previous {
			continue
		}
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil || prefix.Addr().Is4In6() || prefix.Addr().Zone() != "" || prefix.String() != cidr || prefix.Bits() != prefix.Addr().BitLen() {
			return nil, fmt.Errorf("workspace FUSE DNS CIDR %q must be a host-only /32 or /128 CIDR", cidr)
		}
		nameservers = append(nameservers, prefix.Addr().String())
		previous = cidr
	}
	if len(nameservers) < 1 || len(nameservers) > 3 {
		return nil, fmt.Errorf("workspace FUSE must configure between one and three unique DNS resolvers")
	}
	return nameservers, nil
}

func secretFilePath(key string) string {
	if key == "" {
		return ""
	}
	return path.Join(mounterSecretPath, key)
}

func preparedPoolKeyLabel(poolKey string) (string, error) {
	digest, err := hex.DecodeString(poolKey)
	if err != nil || len(digest) != 32 || poolKey != strings.ToLower(poolKey) {
		return "", fmt.Errorf("workspace FUSE PoolKey must be a canonical lowercase 64-character SHA-256 hex digest")
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
