package kubernetes

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
	corev1 "k8s.io/api/core/v1"
)

var immutableAppArmorProfile = regexp.MustCompile(`^sandbox-fuse-[a-f0-9]{64}$`)

// verifyMounterAppArmor is deliberately private: tenant execution must never
// gain access to the privileged sidecar. Both identity reads are bounded by
// the same deadline, and no credentials are marshalled before this succeeds.
func (r *Runtime) verifyMounterAppArmor(ctx context.Context, ref runtime.RuntimeRef) error {
	if r.appArmorProfile == "" {
		return nil
	}
	if !immutableAppArmorProfile.MatchString(r.appArmorProfile) {
		return fmt.Errorf("invalid immutable AppArmor profile")
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	pod, err := r.getExactPod(checkCtx, ref)
	if err != nil {
		return err
	}
	if err := validateAppArmorMounterPod(pod, ref, r.appArmorProfile); err != nil {
		return err
	}
	raw, err := r.execControl(checkCtx, ref.ID, workspaceMounterContainer, []string{"/bin/cat", "/proc/1/attr/current"}, nil)
	if err != nil {
		return fmt.Errorf("cannot verify mounter AppArmor enforcement")
	}
	if len(raw) > 512 || strings.TrimSpace(string(raw)) != r.appArmorProfile+" (enforce)" {
		return fmt.Errorf("mounter is not using the requested enforce AppArmor profile")
	}
	pod, err = r.getExactPod(checkCtx, ref)
	if err != nil {
		return err
	}
	return validateAppArmorMounterPod(pod, ref, r.appArmorProfile)
}

func validateAppArmorMounterPod(pod *corev1.Pod, ref runtime.RuntimeRef, profile string) error {
	invalid := func() error { return fmt.Errorf("mounter AppArmor security contract does not match") }
	if _, err := exactFUSEPodBootstrap(pod, ref); err != nil {
		return err
	}
	if err := validatePreparedContainerState(pod); err != nil {
		return err
	}
	if len(pod.Spec.InitContainers) != 1 || len(pod.Spec.Containers) != 1 || len(pod.Spec.EphemeralContainers) != 0 ||
		pod.Spec.HostNetwork || pod.Spec.HostPID || pod.Spec.HostIPC ||
		pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken ||
		pod.Spec.ShareProcessNamespace == nil || *pod.Spec.ShareProcessNamespace {
		return invalid()
	}
	mounter := pod.Spec.InitContainers[0]
	if mounter.Name != workspaceMounterContainer || !reflect.DeepEqual(mounter.Command, []string{mounterBinary, "supervise"}) || len(mounter.Args) != 0 ||
		mounter.RestartPolicy == nil || *mounter.RestartPolicy != corev1.ContainerRestartPolicyAlways || mounter.SecurityContext == nil {
		return invalid()
	}
	security := mounter.SecurityContext.DeepCopy()
	annotation := pod.Annotations[corev1.DeprecatedAppArmorBetaContainerAnnotationKeyPrefix+workspaceMounterContainer]
	if annotation != "" && annotation != "localhost/"+profile {
		return invalid()
	}
	if security.AppArmorProfile != nil {
		p := security.AppArmorProfile
		if p.Type != corev1.AppArmorProfileTypeLocalhost || p.LocalhostProfile == nil || *p.LocalhostProfile != profile {
			return invalid()
		}
	} else if annotation != "localhost/"+profile {
		return invalid()
	}
	security.AppArmorProfile = nil // exact equivalent legacy/native representation
	yes := true
	root := int64(0)
	expected := &corev1.SecurityContext{Privileged: &yes, RunAsUser: &root, ReadOnlyRootFilesystem: &yes}
	if !reflect.DeepEqual(security, expected) {
		return invalid()
	}
	// No extra privileged bind mounts can be smuggled into the trusted sidecar.
	propagation := corev1.MountPropagationBidirectional
	expectedMounts := []corev1.VolumeMount{
		{Name: "workspace", MountPath: workspaceMountPath, MountPropagation: &propagation},
		{Name: "fuse-cache", MountPath: mounterCachePath},
		{Name: "dev-fuse", MountPath: "/dev/fuse"},
		{Name: "mounter-run", MountPath: mounterRunPath},
	}
	if len(mounter.VolumeMounts) == 5 {
		expectedMounts = append(expectedMounts, corev1.VolumeMount{Name: "workspace-ca", MountPath: mounterSecretPath, ReadOnly: true})
	}
	if !reflect.DeepEqual(mounter.VolumeMounts, expectedMounts) {
		return invalid()
	}
	if pod.Spec.SecurityContext == nil || !reflect.DeepEqual(pod.Spec.SecurityContext,
		&corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}) {
		return invalid()
	}
	no := false
	tenantUID := int64(1000)
	tenant := pod.Spec.Containers[0]
	if tenant.Name != sandboxContainer || !reflect.DeepEqual(tenant.SecurityContext, &corev1.SecurityContext{
		RunAsNonRoot: &yes, RunAsUser: &tenantUID, RunAsGroup: &tenantUID,
		AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes,
		Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}) {
		return invalid()
	}
	expectedVolumeNames := []string{"workspace", "fuse-cache", "dev-fuse", "mounter-run", "tmp"}
	if len(expectedMounts) == 5 {
		expectedVolumeNames = append(expectedVolumeNames, "workspace-ca")
	}
	if len(pod.Spec.Volumes) != len(expectedVolumeNames) {
		return invalid()
	}
	for i, volume := range pod.Spec.Volumes {
		if volume.Name != expectedVolumeNames[i] {
			return invalid()
		}
		source := volume.VolumeSource.DeepCopy()
		switch volume.Name {
		case "dev-fuse":
			device := corev1.HostPathCharDev
			if !reflect.DeepEqual(source, &corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev/fuse", Type: &device}}) {
				return invalid()
			}
		case "workspace-ca":
			if source.Secret == nil || source.Secret.SecretName == "" || len(source.Secret.Items) == 0 {
				return invalid()
			}
			if !reflect.DeepEqual(source, &corev1.VolumeSource{Secret: source.Secret}) {
				return invalid()
			}
		default:
			if source.EmptyDir == nil {
				return invalid()
			}
			source.EmptyDir.SizeLimit = nil // limits are validated when building the Pod
			medium := corev1.StorageMediumDefault
			if volume.Name == "workspace" || volume.Name == "mounter-run" {
				medium = corev1.StorageMediumMemory
			}
			if !reflect.DeepEqual(source, &corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: medium}}) {
				return invalid()
			}
		}
	}
	return nil
}
