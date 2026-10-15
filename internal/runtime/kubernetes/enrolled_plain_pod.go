package kubernetes

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/goairix/sandbox/internal/imageref"
	"github.com/goairix/sandbox/internal/kubecontract"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type EnrolledPlainPodRequest struct {
	Spec                                            runtime.SandboxSpec
	NodeName, NodeUID, NodeBootID, CreationIntentID string
	Generation                                      int64
	Config                                          []byte
}

func BuildEnrolledPlainPod(namespace string, request EnrolledPlainPodRequest) (*corev1.Pod, error) {
	spec := request.Spec
	c, err := launcherconfig.Decode(request.Config)
	if err != nil {
		return nil, err
	}
	if spec.WorkspaceFUSE != nil || len(spec.Mounts) != 0 || spec.PidLimit != 0 || spec.PoolContract != "" || len(spec.NetworkWhitelist) != 0 || spec.NetworkBlockPrivate || spec.SeccompProfile != "" && spec.SeccompProfile != "RuntimeDefault" || !spec.ReadOnlyRootFS || spec.RunAsUser != int64(c.UID) || spec.NetworkEnabled != c.NetworkAllowed || request.NodeUID != c.Enrollment.NodeUID || request.NodeBootID != c.Enrollment.NodeBootID {
		return nil, fmt.Errorf("unsupported or unbound enrolled sandbox specification")
	}
	logicalID, err := ordinaryLogicalID(spec)
	if err != nil {
		return nil, err
	}
	labels := maps.Clone(spec.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	for key := range labels {
		if strings.HasPrefix(key, "sandbox.pool") || strings.HasPrefix(key, "sandbox.claim") {
			return nil, fmt.Errorf("pool identity is not an enrolled plain sandbox")
		}
	}
	labels["sandbox.id"], labels["sandbox.managed"], labels["sandbox.workspace.mode"] = logicalID, "true", "plain"
	resources := corev1.ResourceRequirements{Limits: corev1.ResourceList{}, Requests: corev1.ResourceList{}}
	for _, item := range []struct {
		name           corev1.ResourceName
		limit, request string
	}{{corev1.ResourceCPU, spec.CPU, spec.CPURequest}, {corev1.ResourceMemory, spec.Memory, spec.MemoryRequest}} {
		limit, err := resource.ParseQuantity(item.limit)
		if err != nil || limit.Sign() <= 0 {
			return nil, fmt.Errorf("positive bounded enrolled %s limit required", item.name)
		}
		requested := limit.DeepCopy()
		if item.request != "" {
			requested, err = resource.ParseQuantity(item.request)
		}
		if err != nil || requested.Sign() <= 0 || requested.Cmp(limit) > 0 {
			return nil, fmt.Errorf("invalid enrolled %s request", item.name)
		}
		resources.Limits[item.name], resources.Requests[item.name] = limit, requested
	}
	disk, err := resource.ParseQuantity(spec.Disk)
	if err != nil || disk.Sign() <= 0 {
		return nil, fmt.Errorf("positive workspace bound required")
	}
	tmp := spec.TmpDisk
	if tmp == "" {
		tmp = runtime.DefaultTmpDisk
	}
	tmpDisk, err := resource.ParseQuantity(tmp)
	if err != nil || tmpDisk.Sign() <= 0 {
		return nil, fmt.Errorf("positive tmp bound required")
	}
	if !imageref.IsRelease(spec.Image) || !strings.Contains(spec.Image, "@sha256:") {
		return nil, fmt.Errorf("enrolled image sha256 digest required")
	}
	imageDigest := strings.Split(spec.Image, "@sha256:")[1]
	digest := sha256.Sum256(request.Config)
	controlSize := resource.MustParse("1Mi")
	directory, socket := corev1.HostPathDirectory, corev1.HostPathSocket
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: spec.ID, Namespace: namespace, Labels: labels, Finalizers: []string{kubecontract.EnrolledRetainFinalizer}, Annotations: map[string]string{
		kubecontract.EnrolledProfileAnnotation:        kubecontract.EnrolledPlainProfile,
		kubecontract.EnrolledGenerationAnnotation:     strconv.FormatInt(request.Generation, 10),
		kubecontract.EnrolledNodeUIDAnnotation:        request.NodeUID,
		kubecontract.EnrolledNodeBootIDAnnotation:     request.NodeBootID,
		kubecontract.EnrolledConfigDigestAnnotation:   hex.EncodeToString(digest[:]),
		kubecontract.EnrolledImageDigestAnnotation:    imageDigest,
		kubecontract.EnrolledCreationIntentAnnotation: request.CreationIntentID,
	}}, Spec: kubecontract.EnrolledPlainPodSpec(request.NodeName, spec.Image, string(request.Config), resources, []corev1.Volume{
		{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &disk}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &tmpDisk}}},
		{Name: "control", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: &controlSize}}},
		{Name: "clock", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/run/sandbox-clock", Type: &directory}}},
		{Name: "enrollment", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: kubecontract.EnrolledEnrollmentSource, Type: &socket}}},
	})}
	if err := kubecontract.ValidateEnrolledPlainPod(pod); err != nil {
		return nil, err
	}
	return pod, nil
}
