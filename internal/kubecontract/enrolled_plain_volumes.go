package kubecontract

import (
	"fmt"
	"reflect"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func validateEnrolledVolumes(pod *v1.Pod) error {
	if len(pod.Spec.Volumes) != 5 {
		return fmt.Errorf("five enrolled volumes required")
	}
	volumes := pod.Spec.Volumes
	for i, name := range []string{"workspace", "tmp", "control"} {
		volume := volumes[i]
		medium := v1.StorageMediumDefault
		if i == 2 {
			medium = v1.StorageMediumMemory
		}
		if volume.EmptyDir == nil || volume.EmptyDir.SizeLimit == nil || volume.EmptyDir.SizeLimit.Sign() <= 0 || i == 2 && volume.EmptyDir.SizeLimit.Cmp(resource.MustParse("1Mi")) > 0 || !reflect.DeepEqual(volume, v1.Volume{Name: name, VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{Medium: medium, SizeLimit: volume.EmptyDir.SizeLimit}}}) {
			return fmt.Errorf("invalid enrolled bounded EmptyDir")
		}
	}
	directory, socket := v1.HostPathDirectory, v1.HostPathSocket
	expectedHosts := []v1.Volume{
		{Name: "clock", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/run/sandbox-clock", Type: &directory}}},
		{Name: "enrollment", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: EnrolledEnrollmentSource, Type: &socket}}},
	}
	if !reflect.DeepEqual(volumes[3:], expectedHosts) {
		return fmt.Errorf("invalid enrolled fixed host socket sources")
	}
	expectedMounts := []v1.VolumeMount{
		{Name: "workspace", MountPath: "/workspace"}, {Name: "tmp", MountPath: "/tmp"}, {Name: "control", MountPath: "/run/sandbox-control"},
		{Name: "clock", MountPath: "/run/sandbox-clock", ReadOnly: true}, {Name: "enrollment", MountPath: "/run/sandbox-control/enrollment.sock", ReadOnly: true},
	}
	if !reflect.DeepEqual(pod.Spec.Containers[0].VolumeMounts, expectedMounts) {
		return fmt.Errorf("invalid enrolled fixed volume mounts")
	}
	return nil
}
