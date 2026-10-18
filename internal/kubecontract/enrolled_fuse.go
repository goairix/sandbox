package kubecontract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/distribution/reference"
	"github.com/goairix/sandbox/internal/fuseprotocol"
	"github.com/goairix/sandbox/internal/imageref"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	"github.com/google/uuid"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// EnrolledFUSEProfile names an inactive structural contract. Rendering or
// validating it grants no enrollment, mount, flush, cleanup or user admission.
// Business creation selectors deliberately do not activate this profile. The
// Observer's explicit diagnostic branch still requires original enrollment and
// independently authenticated kernel peers; inspection alone grants nothing.
const EnrolledFUSEProfile = "sandbox-enrolled-fuse/v2"

const (
	// Public configuration budgets precede JSON encoding. The complete returned
	// PodSpec also has a 64 KiB wire cap, independent of Kubernetes API limits.
	enrolledFUSEBootstrapBytes = 4096
	enrolledFUSESpecBytes      = 65536
	// Mount the directory, not one socket inode: an observer restart can bind
	// its replacement socket without remounting the trusted mounter container.
	enrolledFUSEHealthDirectory = "/run/sandbox-node-observer/mounter-health"
	enrolledFUSEHealthSocket    = enrolledFUSEHealthDirectory + "/roothealth.sock"
)

// EnrolledFUSEPodSpecInput is caller-retained original structural intent, not
// authority. The two immutable images and AppArmor policy name bind the public
// launcher contract; remaining fields must be compared against the same original
// input. Neither container may obtain additional mounts, commands or env fields.
type EnrolledFUSEPodSpecInput struct {
	NodeName               string
	LauncherImage          string
	MounterImage           string
	MounterAppArmorProfile string
	LauncherConfig         []byte
	Bootstrap              fuseprotocol.BootstrapConfig
	LauncherResources      v1.ResourceRequirements
	MounterResources       v1.ResourceRequirements
	// WorkspaceBytes and TmpBytes bound the EmptyDir volumes in bytes. The
	// disk-backed helper cache limit comes from Bootstrap.CacheLimitBytes.
	WorkspaceBytes int64
	TmpBytes       int64
}

// EnrolledFUSEContractDigest binds executable image content, the immutable
// AppArmor deployment policy name and this fixed template version. Kubernetes
// privileged containers ignore AppArmor confinement; this name is intent only,
// not a kernel security assertion. Neither the digest nor the template attests
// that either image/process is currently running.
func EnrolledFUSEContractDigest(launcherImage, mounterImage, appArmorProfile string) (string, error) {
	launcher, err := enrolledFUSEImageDigest(launcherImage)
	if err != nil {
		return "", err
	}
	mounter, err := enrolledFUSEImageDigest(mounterImage)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(appArmorProfile, "sandbox-fuse-") || !enrolledFUSEHex(strings.TrimPrefix(appArmorProfile, "sandbox-fuse-")) {
		return "", fmt.Errorf("immutable FUSE AppArmor policy name required")
	}
	digest := sha256.Sum256([]byte("goairix/sandbox/enrolled-fuse-contract/v2\x00" + EnrolledFUSEProfile + "\x00" + launcher + "\x00" + mounter + "\x00" + appArmorProfile))
	return hex.EncodeToString(digest[:]), nil
}

func enrolledFUSEImageDigest(image string) (string, error) {
	if len(image) == 0 || len(image) > 1024 || !imageref.IsRelease(image) {
		return "", fmt.Errorf("bounded pinned FUSE image required")
	}
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return "", fmt.Errorf("invalid FUSE image")
	}
	pinned, ok := named.(reference.Digested)
	if !ok || !strings.HasPrefix(pinned.Digest().String(), "sha256:") || !enrolledFUSEHex(strings.TrimPrefix(pinned.Digest().String(), "sha256:")) {
		return "", fmt.Errorf("FUSE image sha256 digest required")
	}
	return pinned.Digest().String(), nil
}

func enrolledFUSEHex(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validateEnrolledFUSEInput(input EnrolledFUSEPodSpecInput) ([]byte, error) {
	if len(input.NodeName) > 128 || len(validation.IsDNS1123Subdomain(input.NodeName)) != 0 || input.WorkspaceBytes <= 0 || input.TmpBytes <= 0 {
		return nil, fmt.Errorf("bounded FUSE node and volumes required")
	}
	contract, err := EnrolledFUSEContractDigest(input.LauncherImage, input.MounterImage, input.MounterAppArmorProfile)
	if err != nil {
		return nil, err
	}
	config, err := launcherconfig.Decode(input.LauncherConfig)
	// Current compiled s3fs argv pins uid/gid=1000. A different launcher user
	// would not represent the same user-visible mount ownership contract.
	if err != nil || config.UID != 1000 || config.GID != 1000 || config.ContractDigest != contract {
		return nil, fmt.Errorf("FUSE launcher configuration differs from fixed contract")
	}
	if err := validateEnrolledFUSEResources(input.LauncherResources); err != nil {
		return nil, err
	}
	if err := validateEnrolledFUSEResources(input.MounterResources); err != nil {
		return nil, err
	}
	return enrolledFUSEBootstrap(input.Bootstrap)
}

func validateEnrolledFUSEResources(resources v1.ResourceRequirements) error {
	if len(resources.Claims) != 0 || len(resources.Limits) != 2 || len(resources.Requests) != 2 {
		return fmt.Errorf("FUSE CPU and memory limits/requests required")
	}
	for _, name := range []v1.ResourceName{v1.ResourceCPU, v1.ResourceMemory} {
		limit, ok := resources.Limits[name]
		request, exists := resources.Requests[name]
		if !ok || !exists || limit.Sign() <= 0 || request.Sign() <= 0 || request.Cmp(limit) > 0 || len(limit.String()) > 64 || len(request.String()) > 64 {
			return fmt.Errorf("invalid bounded FUSE resource quantity")
		}
	}
	return nil
}

func enrolledFUSEBootstrap(bootstrap fuseprotocol.BootstrapConfig) ([]byte, error) {
	if bootstrap.Version != fuseprotocol.Version || bootstrap.RuntimeUID != "" || bootstrap.Provider != "minio" || bootstrap.Profile != "minio-sigv4-path-style-v1" || !enrolledFUSEHex(bootstrap.PoolKey) {
		return nil, fmt.Errorf("fixed FUSE bootstrap identity required")
	}
	if len(bootstrap.Bucket) < 3 || len(bootstrap.Bucket) > 63 || len(validation.IsDNS1123Label(bootstrap.Bucket)) != 0 || len(bootstrap.Region) > 63 || len(validation.IsDNS1123Label(bootstrap.Region)) != 0 || len(bootstrap.Endpoint) > 1024 {
		return nil, fmt.Errorf("bounded FUSE storage parameters required")
	}
	// URL parsing accepts some Unicode controls and invalid UTF-8 in hosts.
	// Reject them before JSON can replace bytes or disagree with helper input.
	if !utf8.ValidString(bootstrap.Endpoint) {
		return nil, fmt.Errorf("FUSE endpoint must contain valid UTF-8")
	}
	for _, char := range bootstrap.Endpoint {
		if unicode.IsControl(char) {
			return nil, fmt.Errorf("FUSE endpoint contains a control character")
		}
	}
	endpoint, err := url.Parse(bootstrap.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || endpoint.Opaque != "" || endpoint.Path != "" && endpoint.Path != "/" {
		return nil, fmt.Errorf("FUSE HTTPS origin required")
	}
	if bootstrap.PasswdFile != "/run/s3fs/passwd-s3fs" || bootstrap.CacheDir != "/var/cache/s3fs" || bootstrap.MountPath != "/workspace" || bootstrap.CAFile != "" || bootstrap.CacheLimitBytes <= 0 {
		return nil, fmt.Errorf("fixed FUSE private paths and cache bound required")
	}
	for _, seconds := range []int64{bootstrap.MountTimeoutSeconds, bootstrap.FlushTimeoutSeconds, bootstrap.UnmountTimeoutSeconds} {
		if seconds < 1 || seconds > 3600 {
			return nil, fmt.Errorf("FUSE timeout must be 1..3600 seconds")
		}
	}
	wire, err := json.Marshal(bootstrap)
	if err != nil || len(wire) > enrolledFUSEBootstrapBytes {
		return nil, fmt.Errorf("FUSE bootstrap exceeds canonical wire budget")
	}
	return wire, nil
}

// ValidateEnrolledFUSEPodSpec compares every field with retained original
// structural intent. It does not infer that intent from observed fields, accept
// admission mutations, mutate either input, or attest running/kernel state.
// Only the five EmptyDir byte limits ignore Quantity's JSON representation;
// their numeric values must be exact. All other fields retain strict equality.
func ValidateEnrolledFUSEPodSpec(actual v1.PodSpec, original EnrolledFUSEPodSpecInput) error {
	expected, err := EnrolledFUSEPodSpec(original)
	if err != nil {
		return err
	}
	if len(actual.Volumes) != len(expected.Volumes) {
		return fmt.Errorf("FUSE Pod differs from original fixed structural intent")
	}
	// NewQuantity and API JSON decoding can represent the same byte count
	// differently internally. Normalize only these known template fields in
	// an owned copy after exact Cmp, never after Value's integer rounding.
	// Do not use global API semantic equality: nil/empty fields and every
	// other structural/security difference must still fail closed below.
	checked := actual.DeepCopy()
	for _, index := range []int{0, 1, 2, 5, 6} {
		volume := checked.Volumes[index].EmptyDir
		limit := expected.Volumes[index].EmptyDir.SizeLimit
		if volume == nil || volume.SizeLimit == nil || volume.SizeLimit.Cmp(*limit) != 0 {
			return fmt.Errorf("FUSE Pod differs from original fixed structural intent")
		}
		volume.SizeLimit = limit
	}
	if !reflect.DeepEqual(*checked, expected) {
		return fmt.Errorf("FUSE Pod differs from original fixed structural intent")
	}
	return nil
}

// EnrolledFUSEPodIdentity contains copied structural diagnostics. Config's root
// list and contract are public assertions, not independently trusted authority.
// Consumers must bind these fields to their original verified registration and
// authenticate the actual helper kernel peer before consuming its observations.
type EnrolledFUSEPodIdentity struct {
	Config    launcherconfig.Config
	Bootstrap fuseprotocol.BootstrapConfig

	LogicalID, CreationIntentID, NodeUID, NodeBootID string
	Generation                                       int64
	MounterImage, MounterAppArmorProfile             string
}

// InspectEnrolledFUSEPod checks the complete Version2 shape and its internal
// public configuration consistency. Reconstructing a render input below only
// detects extra/malformed observed fields; it does not recover original intent
// or grant the supplied config roots, Pod, helper or image any effect authority.
func InspectEnrolledFUSEPod(pod *v1.Pod) (EnrolledFUSEPodIdentity, error) {
	var result EnrolledFUSEPodIdentity
	if pod == nil || pod.Annotations[EnrolledProfileAnnotation] != EnrolledFUSEProfile {
		return result, fmt.Errorf("enrolled FUSE Version2 profile required")
	}
	if len(validation.IsDNS1123Label(pod.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(pod.Name)) != 0 || len(pod.Name) > 128 || pod.GenerateName != "" || len(pod.OwnerReferences) != 0 || len(pod.Finalizers) > 1 || len(pod.Finalizers) == 1 && pod.Finalizers[0] != EnrolledRetainFinalizer {
		return result, fmt.Errorf("invalid enrolled FUSE Pod identity")
	}
	result.LogicalID = pod.Labels["sandbox.id"]
	if len(validation.IsDNS1123Subdomain(result.LogicalID)) != 0 || len(result.LogicalID) > 128 || pod.Labels["sandbox.managed"] != "true" || pod.Labels["sandbox.workspace.mode"] != "fuse" {
		return result, fmt.Errorf("invalid enrolled FUSE logical identity")
	}
	for key, value := range pod.Labels {
		if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 || strings.HasPrefix(key, "sandbox.pool") || strings.HasPrefix(key, "sandbox.claim") {
			return result, fmt.Errorf("invalid enrolled FUSE label")
		}
	}
	keys := []string{EnrolledProfileAnnotation, EnrolledGenerationAnnotation, EnrolledNodeUIDAnnotation, EnrolledNodeBootIDAnnotation, EnrolledConfigDigestAnnotation, EnrolledImageDigestAnnotation, EnrolledCreationIntentAnnotation}
	if len(pod.Annotations) != len(keys) {
		return result, fmt.Errorf("unknown enrolled FUSE annotation")
	}
	for _, key := range keys {
		if pod.Annotations[key] == "" {
			return result, fmt.Errorf("missing enrolled FUSE annotation")
		}
	}
	var err error
	result.Generation, err = strconv.ParseInt(pod.Annotations[EnrolledGenerationAnnotation], 10, 64)
	if err != nil || result.Generation <= 0 || strconv.FormatInt(result.Generation, 10) != pod.Annotations[EnrolledGenerationAnnotation] {
		return result, fmt.Errorf("invalid enrolled FUSE generation")
	}
	result.CreationIntentID, result.NodeUID, result.NodeBootID = pod.Annotations[EnrolledCreationIntentAnnotation], pod.Annotations[EnrolledNodeUIDAnnotation], pod.Annotations[EnrolledNodeBootIDAnnotation]
	for _, value := range []string{result.CreationIntentID, result.NodeUID, result.NodeBootID} {
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil || id.String() != value {
			return result, fmt.Errorf("invalid enrolled FUSE UUID")
		}
	}

	if len(pod.Spec.Containers) != 1 || len(pod.Spec.InitContainers) != 1 || len(pod.Spec.Volumes) != 9 {
		return result, fmt.Errorf("fixed enrolled FUSE containers and volumes required")
	}
	launcher, helper := &pod.Spec.Containers[0], &pod.Spec.InitContainers[0]
	if len(launcher.Env) != 1 || launcher.Env[0].Name != launcherconfig.Environment || launcher.Env[0].ValueFrom != nil || len(helper.Env) != 7 || helper.Env[1].Name != "SANDBOX_MOUNTER_BOOTSTRAP" || helper.Env[1].ValueFrom != nil || helper.SecurityContext == nil || helper.SecurityContext.AppArmorProfile == nil || helper.SecurityContext.AppArmorProfile.LocalhostProfile == nil {
		return result, fmt.Errorf("fixed enrolled FUSE public configuration required")
	}
	result.Config, err = launcherconfig.Decode([]byte(launcher.Env[0].Value))
	if err != nil {
		return result, err
	}
	if result.Config.Enrollment.NodeUID != result.NodeUID || result.Config.Enrollment.NodeBootID != result.NodeBootID {
		return result, fmt.Errorf("enrolled FUSE node and configuration differ")
	}
	if err := fuseprotocol.Decode([]byte(helper.Env[1].Value), &result.Bootstrap); err != nil {
		return result, err
	}
	result.MounterImage, result.MounterAppArmorProfile = helper.Image, *helper.SecurityContext.AppArmorProfile.LocalhostProfile
	configDigest := sha256.Sum256([]byte(launcher.Env[0].Value))
	imageDigest, err := enrolledFUSEImageDigest(launcher.Image)
	if err != nil || pod.Annotations[EnrolledConfigDigestAnnotation] != hex.EncodeToString(configDigest[:]) || pod.Annotations[EnrolledImageDigestAnnotation] != strings.TrimPrefix(imageDigest, "sha256:") {
		return result, fmt.Errorf("enrolled FUSE configuration or image digest differs")
	}
	for _, index := range []int{0, 1} {
		volume := pod.Spec.Volumes[index]
		if volume.EmptyDir == nil || volume.EmptyDir.SizeLimit == nil {
			return result, fmt.Errorf("bounded enrolled FUSE volume required")
		}
	}
	input := EnrolledFUSEPodSpecInput{
		NodeName: pod.Spec.NodeName, LauncherImage: launcher.Image, MounterImage: helper.Image,
		MounterAppArmorProfile: result.MounterAppArmorProfile, LauncherConfig: []byte(launcher.Env[0].Value), Bootstrap: result.Bootstrap,
		LauncherResources: *launcher.Resources.DeepCopy(), MounterResources: *helper.Resources.DeepCopy(),
		WorkspaceBytes: pod.Spec.Volumes[0].EmptyDir.SizeLimit.Value(), TmpBytes: pod.Spec.Volumes[1].EmptyDir.SizeLimit.Value(),
	}
	if err := ValidateEnrolledFUSEPodSpec(pod.Spec, input); err != nil {
		return result, err
	}

	// Pending status may be absent during creation. A live channel separately
	// requires the exact running init ContainerID and actual kernel association;
	// structural inspection never treats an API status as a process proof.
	if len(pod.Status.ContainerStatuses) > 1 || len(pod.Status.InitContainerStatuses) > 1 || len(pod.Status.EphemeralContainerStatuses) != 0 {
		return result, fmt.Errorf("foreign enrolled FUSE container status")
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != "sandbox" || status.RestartCount != 0 {
			return result, fmt.Errorf("restarted or foreign enrolled FUSE launcher")
		}
	}
	for _, status := range pod.Status.InitContainerStatuses {
		if status.Name != "workspace-mounter" || status.RestartCount != 0 {
			return result, fmt.Errorf("restarted or foreign enrolled FUSE helper")
		}
	}
	return result, nil
}
