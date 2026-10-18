package kubecontract

import (
	"encoding/json"
	"fmt"

	"github.com/goairix/sandbox/internal/fuseprotocol"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// EnrolledFUSEPodSpec renders an inactive, fixed structural template with owned
// output. The privileged helper is a trusted system component, not a confined
// user process: Kubernetes privileged mode ignores AppArmor confinement, so
// its policy name binds deployment intent only. This function does no I/O and
// grants no production admission;
// authenticated helper/runtime association, mount/flush evidence and cleanup
// authority remain prerequisites for any future production profile consumer.
func EnrolledFUSEPodSpec(input EnrolledFUSEPodSpecInput) (v1.PodSpec, error) {
	bootstrap, err := validateEnrolledFUSEInput(input)
	if err != nil {
		return v1.PodSpec{}, err
	}
	// validateEnrolledFUSEInput already decoded this bounded canonical config.
	// These public values locate the original operator identity at registration;
	// the observer must independently authenticate its kernel peer and API Pod.
	config, err := launcherconfig.Decode(input.LauncherConfig)
	if err != nil {
		return v1.PodSpec{}, err
	}
	// Reuse only the plain renderer's fixed launcher/container/PID boundary.
	// Plain validation and its production selector remain intentionally separate.
	spec := EnrolledPlainPodSpec(input.NodeName, input.LauncherImage, string(input.LauncherConfig), *input.LauncherResources.DeepCopy(), enrolledFUSEVolumes(input))
	toUser, toHost := v1.MountPropagationHostToContainer, v1.MountPropagationBidirectional
	spec.Containers[0].VolumeMounts[0].MountPropagation = &toUser
	grace := int64(90)
	spec.TerminationGracePeriodSeconds = &grace
	yes, root, restart := true, int64(0), v1.ContainerRestartPolicyAlways
	policy := input.MounterAppArmorProfile
	spec.InitContainers = []v1.Container{{
		Name: "workspace-mounter", Image: input.MounterImage, ImagePullPolicy: v1.PullIfNotPresent,
		Command: []string{fuseprotocol.MounterBinary, "supervise"}, WorkingDir: "/", RestartPolicy: &restart,
		TerminationMessagePath: v1.TerminationMessagePathDefault, TerminationMessagePolicy: v1.TerminationMessageReadFile,
		Resources: *input.MounterResources.DeepCopy(),
		SecurityContext: &v1.SecurityContext{
			Privileged: &yes, RunAsUser: &root, RunAsGroup: &root, ReadOnlyRootFilesystem: &yes,
			AppArmorProfile: &v1.AppArmorProfile{Type: v1.AppArmorProfileTypeLocalhost, LocalhostProfile: &policy},
		},
		Env: []v1.EnvVar{
			{Name: "SANDBOX_RUNTIME_UID", ValueFrom: &v1.EnvVarSource{FieldRef: &v1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.uid"}}},
			{Name: "SANDBOX_MOUNTER_BOOTSTRAP", Value: string(bootstrap)},
			{Name: "SANDBOX_MOUNTER_OBSERVER_SOCKET", Value: enrolledFUSEHealthSocket},
			{Name: "SANDBOX_MOUNTER_POD_NAME", ValueFrom: &v1.EnvVarSource{FieldRef: &v1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"}}},
			{Name: "SANDBOX_MOUNTER_NODE_UID", Value: config.Enrollment.NodeUID},
			{Name: "SANDBOX_MOUNTER_NODE_BOOT_ID", Value: config.Enrollment.NodeBootID},
			{Name: "SANDBOX_MOUNTER_CONTRACT_DIGEST", Value: config.ContractDigest},
		},
		VolumeMounts: []v1.VolumeMount{
			{Name: "workspace", MountPath: "/workspace", MountPropagation: &toHost},
			{Name: "fuse-cache", MountPath: "/var/cache/s3fs"},
			{Name: "dev-fuse", MountPath: "/dev/fuse"},
			{Name: "mounter-run", MountPath: "/run/s3fs"},
			{Name: "mounter-health", MountPath: enrolledFUSEHealthDirectory, ReadOnly: true},
		},
		StartupProbe: &v1.Probe{
			ProbeHandler:  v1.ProbeHandler{Exec: &v1.ExecAction{Command: []string{fuseprotocol.MounterBinary, "health", "prepared"}}},
			PeriodSeconds: 2, TimeoutSeconds: 1, SuccessThreshold: 1, FailureThreshold: 30,
		},
		ReadinessProbe: &v1.Probe{
			ProbeHandler:  v1.ProbeHandler{HTTPGet: &v1.HTTPGetAction{Path: fuseprotocol.MounterReadinessPath, Port: intstr.FromInt(fuseprotocol.MounterReadinessPort), Scheme: v1.URISchemeHTTP}},
			PeriodSeconds: 10, TimeoutSeconds: 1, SuccessThreshold: 1, FailureThreshold: 3,
		},
		// This best-effort lifecycle hook is not a durable flush/termination
		// receipt and cannot authorize releasing workspace ownership.
		Lifecycle: &v1.Lifecycle{PreStop: &v1.LifecycleHandler{Exec: &v1.ExecAction{Command: []string{fuseprotocol.MounterBinary, "shutdown"}}}},
	}}
	wire, err := json.Marshal(spec)
	if err != nil || len(wire) > enrolledFUSESpecBytes {
		return v1.PodSpec{}, fmt.Errorf("FUSE Pod exceeds canonical wire budget")
	}
	return *spec.DeepCopy(), nil
}

func enrolledFUSEVolumes(input EnrolledFUSEPodSpecInput) []v1.Volume {
	// The first five retain the plain renderer's exact mount positions. Only
	// workspace propagation differs; helper cache/run/device never enter launcher.
	directory, socket, device := v1.HostPathDirectory, v1.HostPathSocket, v1.HostPathCharDev
	return []v1.Volume{
		enrolledFUSEEmptyDir("workspace", v1.StorageMediumMemory, input.WorkspaceBytes),
		enrolledFUSEEmptyDir("tmp", v1.StorageMediumDefault, input.TmpBytes),
		enrolledFUSEEmptyDir("control", v1.StorageMediumMemory, 1<<20),
		{Name: "clock", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/run/sandbox-clock", Type: &directory}}},
		{Name: "enrollment", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: EnrolledEnrollmentSource, Type: &socket}}},
		enrolledFUSEEmptyDir("fuse-cache", v1.StorageMediumDefault, input.Bootstrap.CacheLimitBytes),
		enrolledFUSEEmptyDir("mounter-run", v1.StorageMediumMemory, 16<<20),
		{Name: "dev-fuse", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/dev/fuse", Type: &device}}},
		{Name: "mounter-health", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: enrolledFUSEHealthDirectory, Type: &directory}}},
	}
}

func enrolledFUSEEmptyDir(name string, medium v1.StorageMedium, bytes int64) v1.Volume {
	return v1.Volume{Name: name, VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{Medium: medium, SizeLimit: resource.NewQuantity(bytes, resource.BinarySI)}}}
}
