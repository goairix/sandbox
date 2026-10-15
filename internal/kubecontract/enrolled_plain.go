package kubecontract

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/distribution/reference"
	"github.com/goairix/sandbox/internal/imageref"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	"github.com/google/uuid"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	EnrolledPlainProfile             = "sandbox-enrolled-plain/v1"
	EnrolledProfileAnnotation        = MetadataPrefix + "/sandbox-launcher-profile"
	EnrolledGenerationAnnotation     = MetadataPrefix + "/sandbox-launcher-generation"
	EnrolledNodeUIDAnnotation        = MetadataPrefix + "/sandbox-launcher-node-uid"
	EnrolledNodeBootIDAnnotation     = MetadataPrefix + "/sandbox-launcher-node-boot-id"
	EnrolledConfigDigestAnnotation   = MetadataPrefix + "/sandbox-launcher-config-digest"
	EnrolledImageDigestAnnotation    = MetadataPrefix + "/sandbox-launcher-image-digest"
	EnrolledCreationIntentAnnotation = "sandbox.creation.intent"
	EnrolledRetainFinalizer          = MetadataPrefix + "/sandbox-runtime-termination"
	EnrolledLauncherPath             = "/usr/local/bin/sandbox-launcher"
	EnrolledEnrollmentSource         = "/run/sandbox-node-observer/enrollment.sock"
)

// EnrolledPlainPodIdentity is copied structural diagnostic data, never authority.
type EnrolledPlainPodIdentity struct {
	Config                                           launcherconfig.Config
	LogicalID, CreationIntentID, NodeUID, NodeBootID string
	Generation                                       int64
}

// SelectEnrolledPlainPodProfile must run before either consumer's legacy branch.
// Removing a marker does not convert a managed launcher into the older profile.
func SelectEnrolledPlainPodProfile(pod *v1.Pod) (bool, error) {
	if pod == nil {
		return false, fmt.Errorf("missing enrolled profile Pod")
	}
	if value, ok := pod.Annotations[EnrolledProfileAnnotation]; ok {
		if value != EnrolledPlainProfile {
			return false, fmt.Errorf("unknown launcher profile")
		}
		return true, nil
	}
	for key := range pod.Annotations {
		if strings.HasPrefix(key, MetadataPrefix+"/sandbox-launcher-") {
			return false, fmt.Errorf("launcher metadata without profile")
		}
	}
	for _, c := range append(append([]v1.Container(nil), pod.Spec.Containers...), pod.Spec.InitContainers...) {
		for _, arg := range append(append([]string(nil), c.Command...), c.Args...) {
			if arg == EnrolledLauncherPath || arg == "serve-provisioned" {
				return false, fmt.Errorf("launcher command without profile")
			}
		}
		for _, env := range c.Env {
			if env.Name == launcherconfig.Environment {
				return false, fmt.Errorf("launcher configuration without profile")
			}
		}
		for _, mount := range c.VolumeMounts {
			if enrolledReservedMount(mount.Name, mount.MountPath) {
				return false, fmt.Errorf("launcher mount without profile")
			}
		}
	}
	for _, volume := range pod.Spec.Volumes {
		if enrolledReservedMount(volume.Name, "") || volume.HostPath != nil && enrolledReservedMount("", volume.HostPath.Path) {
			return false, fmt.Errorf("launcher volume without profile")
		}
	}
	return false, nil
}

func enrolledReservedMount(name, path string) bool {
	if name == "control" || name == "clock" || name == "enrollment" {
		return true
	}
	for _, base := range []string{"/run/sandbox-control", "/run/sandbox-clock", "/run/sandbox-node-observer"} {
		if path == base || strings.HasPrefix(path, base+"/") {
			return true
		}
	}
	return false
}

// InspectEnrolledPlainPod checks structure and public configuration consistency.
// Consumers must still compare this copy against their verified original tuple.
func InspectEnrolledPlainPod(pod *v1.Pod) (EnrolledPlainPodIdentity, error) {
	var result EnrolledPlainPodIdentity
	selected, err := SelectEnrolledPlainPodProfile(pod)
	if err != nil {
		return result, err
	}
	if !selected {
		return result, fmt.Errorf("enrolled profile required")
	}
	if len(validation.IsDNS1123Label(pod.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(pod.Name)) != 0 || len(pod.Name) > 128 || pod.GenerateName != "" || len(pod.OwnerReferences) != 0 || len(pod.Finalizers) > 1 || len(pod.Finalizers) == 1 && pod.Finalizers[0] != EnrolledRetainFinalizer || len(validation.IsDNS1123Subdomain(pod.Spec.NodeName)) != 0 {
		return result, fmt.Errorf("invalid enrolled Pod identity")
	}
	result.LogicalID = pod.Labels["sandbox.id"]
	if len(validation.IsDNS1123Subdomain(result.LogicalID)) != 0 || len(result.LogicalID) > 128 || pod.Labels["sandbox.managed"] != "true" || pod.Labels["sandbox.workspace.mode"] != "plain" {
		return result, fmt.Errorf("invalid enrolled logical identity")
	}
	for key, value := range pod.Labels {
		if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 || strings.HasPrefix(key, "sandbox.pool") || strings.HasPrefix(key, "sandbox.claim") {
			return result, fmt.Errorf("invalid enrolled label")
		}
	}
	keys := []string{EnrolledProfileAnnotation, EnrolledGenerationAnnotation, EnrolledNodeUIDAnnotation, EnrolledNodeBootIDAnnotation, EnrolledConfigDigestAnnotation, EnrolledImageDigestAnnotation, EnrolledCreationIntentAnnotation}
	if len(pod.Annotations) != len(keys) {
		return result, fmt.Errorf("unknown enrolled annotation")
	}
	for _, key := range keys {
		if pod.Annotations[key] == "" {
			return result, fmt.Errorf("missing enrolled annotation")
		}
	}
	result.Generation, err = strconv.ParseInt(pod.Annotations[EnrolledGenerationAnnotation], 10, 64)
	if err != nil || result.Generation <= 0 || strconv.FormatInt(result.Generation, 10) != pod.Annotations[EnrolledGenerationAnnotation] {
		return result, fmt.Errorf("invalid enrolled generation")
	}
	result.CreationIntentID, result.NodeUID, result.NodeBootID = pod.Annotations[EnrolledCreationIntentAnnotation], pod.Annotations[EnrolledNodeUIDAnnotation], pod.Annotations[EnrolledNodeBootIDAnnotation]
	for _, value := range []string{result.CreationIntentID, result.NodeUID, result.NodeBootID} {
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil || id.String() != value {
			return result, fmt.Errorf("invalid enrolled UUID")
		}
	}
	if len(pod.Spec.Containers) != 1 {
		return result, fmt.Errorf("enrolled single container required")
	}
	c := &pod.Spec.Containers[0]
	if len(c.Env) != 1 || c.Env[0].Name != launcherconfig.Environment || c.Env[0].ValueFrom != nil {
		return result, fmt.Errorf("fixed public launcher configuration required")
	}
	result.Config, err = launcherconfig.Decode([]byte(c.Env[0].Value))
	if err != nil {
		return result, err
	}
	if result.Config.Enrollment.NodeUID != result.NodeUID || result.Config.Enrollment.NodeBootID != result.NodeBootID {
		return result, fmt.Errorf("enrolled node and configuration differ")
	}
	digest := sha256.Sum256([]byte(c.Env[0].Value))
	if pod.Annotations[EnrolledConfigDigestAnnotation] != hex.EncodeToString(digest[:]) || !imageref.IsRelease(c.Image) || !strings.Contains(c.Image, "@sha256:") || pod.Annotations[EnrolledImageDigestAnnotation] != strings.Split(c.Image, "@sha256:")[1] {
		return result, fmt.Errorf("enrolled configuration or image digest differs")
	}
	contract, err := EnrolledPlainContractDigest(c.Image)
	if err != nil || result.Config.ContractDigest != contract {
		return result, fmt.Errorf("enrolled image differs from original launcher contract")
	}
	if err := validateEnrolledVolumes(pod); err != nil {
		return result, err
	}
	if err := validateEnrolledSecurity(pod); err != nil {
		return result, err
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != "sandbox" || status.RestartCount != 0 {
			return result, fmt.Errorf("restarted or foreign enrolled container status")
		}
	}
	if len(pod.Status.InitContainerStatuses) != 0 || len(pod.Status.EphemeralContainerStatuses) != 0 {
		return result, fmt.Errorf("foreign enrolled container status")
	}
	return result, nil
}
func ValidateEnrolledPlainPod(pod *v1.Pod) error { _, err := InspectEnrolledPlainPod(pod); return err }

// EnrolledPlainContractDigest binds immutable executable image content and the
// fixed profile version into the original activation's ContractDigest. Node and
// per-request resources are independently checked, not part of this static pin.
func EnrolledPlainContractDigest(image string) (string, error) {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return "", fmt.Errorf("invalid enrolled image")
	}
	digested, ok := named.(reference.Digested)
	if !ok || !imageref.IsRelease(image) {
		return "", fmt.Errorf("enrolled sha256 image required")
	}
	digest := sha256.Sum256([]byte("goairix/sandbox/enrolled-plain-contract/v1\x00" + EnrolledPlainProfile + "\x00" + digested.Digest().String()))
	return hex.EncodeToString(digest[:]), nil
}
