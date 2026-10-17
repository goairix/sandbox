package kubecontract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/distribution/reference"
	"github.com/goairix/sandbox/internal/fuseprotocol"
	"github.com/goairix/sandbox/internal/imageref"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// EnrolledFUSEProfile names an inactive structural contract. Rendering or
// validating it grants no enrollment, mount, flush, cleanup or user admission.
// Existing production selectors deliberately do not accept this profile.
const EnrolledFUSEProfile = "sandbox-enrolled-fuse/v1"

const (
	// Public configuration budgets precede JSON encoding. The complete returned
	// PodSpec also has a 64 KiB wire cap, independent of Kubernetes API limits.
	enrolledFUSEBootstrapBytes = 4096
	enrolledFUSESpecBytes      = 65536
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
// AppArmor policy name and this fixed template version. It does not attest that
// the kernel loaded a policy or that either image/process is currently running.
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
	digest := sha256.Sum256([]byte("goairix/sandbox/enrolled-fuse-contract/v1\x00" + EnrolledFUSEProfile + "\x00" + launcher + "\x00" + mounter + "\x00" + appArmorProfile))
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
func ValidateEnrolledFUSEPodSpec(actual v1.PodSpec, original EnrolledFUSEPodSpecInput) error {
	expected, err := EnrolledFUSEPodSpec(original)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf("FUSE Pod differs from original fixed structural intent")
	}
	return nil
}
