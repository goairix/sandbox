// Package sandboxsnapshot stores validated creation configuration without
// runtime authority or deployment credentials.
package sandboxsnapshot

import (
	"errors"
	"math"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/pkg/types"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Version is the business snapshot schema version.
const Version = 1

// MaxBytes is the maximum canonical business snapshot wire size.
const MaxBytes = 60 * 1024

var errInvalid = errors.New("invalid sandbox business snapshot")

// Defaults are resolved only when Build first captures the request.
type Defaults struct {
	TimeoutSeconds     int
	Resources          types.ResourceLimits
	WorkspaceMountMode types.WorkspaceMountMode
}

// TTL distinguishes a finite lifetime from an explicitly permanent request.
type TTL struct {
	Kind    string `json:"kind"`
	Seconds int64  `json:"seconds"`
}

// Resolved contains only values whose defaults must survive deployment changes.
type Resolved struct {
	TTL                TTL                      `json:"ttl"`
	Resources          types.ResourceLimits     `json:"resources"`
	WorkspaceMountMode types.WorkspaceMountMode `json:"workspace_mount_mode"`
}

// Snapshot preserves the normalized request and its first resolved defaults.
type Snapshot struct {
	Version         int                        `json:"version"`
	Requested       types.CreateSandboxRequest `json:"requested"`
	Resolved        Resolved                   `json:"resolved"`
	RequestedDigest string                     `json:"requested_digest"`
}

// Build captures an owned, validated creation request and first-use defaults.
func Build(request types.CreateSandboxRequest, defaults Defaults) (*Snapshot, error) {
	request = normalize(request)
	resolved := Resolved{Resources: defaults.Resources}
	if request.Resources != nil {
		for _, pair := range resourcePairs(request.Resources, &resolved.Resources) {
			if *pair[0] != "" {
				*pair[1] = *pair[0]
			}
		}
	}
	if resolved.Resources.TmpDisk == "" {
		resolved.Resources.TmpDisk = runtime.DefaultTmpDisk
	}
	seconds := request.Timeout
	if seconds == 0 {
		seconds = defaults.TimeoutSeconds
	}
	if seconds == -1 {
		resolved.TTL = TTL{Kind: "permanent"}
	} else {
		resolved.TTL = TTL{Kind: "finite", Seconds: int64(seconds)}
	}
	if request.WorkspacePath != "" {
		resolved.WorkspaceMountMode = request.WorkspaceMountMode
		if resolved.WorkspaceMountMode == "" {
			resolved.WorkspaceMountMode = defaults.WorkspaceMountMode
		}
		if resolved.WorkspaceMountMode == "" {
			resolved.WorkspaceMountMode = types.WorkspaceMountSync
		}
	}
	// No clone, quantity parser, digest or JSON encoder sees unbounded input.
	if err := bounded(request, resolved); err != nil {
		return nil, err
	}
	if err := validateFields(request, resolved); err != nil {
		return nil, err
	}
	snapshot := &Snapshot{Version: Version, Requested: cloneRequest(request), Resolved: resolved}
	snapshot.Resolved.Resources = cloneResources(resolved.Resources)
	snapshot.Resolved.WorkspaceMountMode = types.WorkspaceMountMode(strings.Clone(string(resolved.WorkspaceMountMode)))
	snapshot.RequestedDigest = requestedDigest(snapshot.Requested)
	if _, err := Encode(snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func normalize(request types.CreateSandboxRequest) types.CreateSandboxRequest {
	if request.Resources != nil && *request.Resources == (types.ResourceLimits{}) {
		request.Resources = nil
	}
	if request.Network != nil && !request.Network.Enabled && !request.Network.BlockPrivate && len(request.Network.Whitelist) == 0 {
		request.Network = nil
	}
	if len(request.Dependencies) == 0 {
		request.Dependencies = nil
	}
	if len(request.WorkspaceSyncExclude) == 0 {
		request.WorkspaceSyncExclude = nil
	}
	return request
}

func validateFields(request types.CreateSandboxRequest, resolved Resolved) error {
	if request.Mode != "ephemeral" && request.Mode != "persistent" || request.Timeout < -1 || int64(request.Timeout) > math.MaxInt64/int64(time.Second) {
		return errInvalid
	}
	if resolved.TTL.Kind != "finite" && resolved.TTL.Kind != "permanent" || resolved.TTL.Kind == "finite" && (resolved.TTL.Seconds <= 0 || resolved.TTL.Seconds > math.MaxInt64/int64(time.Second)) || resolved.TTL.Kind == "permanent" && resolved.TTL.Seconds != 0 {
		return errInvalid
	}
	if request.Timeout == -1 && resolved.TTL.Kind != "permanent" || request.Timeout > 0 && (resolved.TTL.Kind != "finite" || resolved.TTL.Seconds != int64(request.Timeout)) {
		return errInvalid
	}
	if request.WorkspacePath == "" {
		if request.WorkspaceMountMode != "" || resolved.WorkspaceMountMode != "" {
			return errInvalid
		}
	} else if resolved.WorkspaceMountMode != types.WorkspaceMountSync && resolved.WorkspaceMountMode != types.WorkspaceMountFUSE || request.WorkspaceMountMode != "" && request.WorkspaceMountMode != resolved.WorkspaceMountMode {
		return errInvalid
	}
	for _, dependency := range request.Dependencies {
		if dependency.Name == "" || dependency.Manager != "pip" && dependency.Manager != "npm" {
			return errInvalid
		}
	}
	requestedResources := types.ResourceLimits{}
	if request.Resources != nil {
		requestedResources = *request.Resources
	}
	actual := resolved.Resources
	for _, pair := range resourcePairs(&requestedResources, &actual) {
		quantity, err := resource.ParseQuantity(*pair[1])
		if err != nil || quantity.Sign() <= 0 {
			return errInvalid
		}
		if *pair[0] != "" {
			explicit, err := resource.ParseQuantity(*pair[0])
			if err != nil || explicit.Sign() <= 0 || explicit.Cmp(quantity) != 0 {
				return errInvalid
			}
		}
	}
	return nil
}

func resourcePairs(first, second *types.ResourceLimits) [4][2]*string {
	return [4][2]*string{{&first.CPU, &second.CPU}, {&first.Memory, &second.Memory}, {&first.Disk, &second.Disk}, {&first.TmpDisk, &second.TmpDisk}}
}

// bounded computes a lower bound on the final wire before any copying or
// encoding. Even worst-case escaping and struct/slice overhead are thereby
// bounded by a fixed multiple of MaxBytes. The exact wire limit follows later.
func bounded(request types.CreateSandboxRequest, resolved Resolved) error {
	remaining := MaxBytes
	add := func(value string) bool {
		if remaining < 2 || len(value) > remaining-2 {
			return false
		}
		remaining -= len(value) + 2
		return utf8.ValidString(value)
	}
	optional := func(value string) bool { return value == "" || add(value) }
	if !add(request.Mode) || !optional(request.WorkspacePath) || !optional(string(request.WorkspaceMountMode)) || !optional(string(resolved.WorkspaceMountMode)) {
		return errInvalid
	}
	for _, limits := range []*types.ResourceLimits{request.Resources, &resolved.Resources} {
		if limits != nil && (!optional(limits.CPU) || !optional(limits.Memory) || !optional(limits.Disk) || !optional(limits.TmpDisk)) {
			return errInvalid
		}
	}
	var whitelist []string
	if request.Network != nil {
		whitelist = request.Network.Whitelist
	}
	for _, values := range [][]string{whitelist, request.WorkspaceSyncExclude} {
		if len(values) > remaining/2 {
			return errInvalid
		}
		for _, value := range values {
			if !add(value) {
				return errInvalid
			}
		}
	}
	if len(request.Dependencies) > remaining/4 {
		return errInvalid
	}
	for _, dependency := range request.Dependencies {
		if !add(dependency.Name) || !add(dependency.Manager) || !optional(dependency.Version) {
			return errInvalid
		}
	}
	return nil
}

func normalized(request types.CreateSandboxRequest) bool {
	if !reflect.DeepEqual(request, normalize(request)) {
		return false
	}
	return request.Network == nil || request.Network.Whitelist == nil || len(request.Network.Whitelist) > 0
}

func cloneResources(value types.ResourceLimits) types.ResourceLimits {
	return types.ResourceLimits{CPU: strings.Clone(value.CPU), Memory: strings.Clone(value.Memory), Disk: strings.Clone(value.Disk), TmpDisk: strings.Clone(value.TmpDisk)}
}

func cloneRequest(request types.CreateSandboxRequest) types.CreateSandboxRequest {
	request.Mode, request.WorkspacePath = strings.Clone(request.Mode), strings.Clone(request.WorkspacePath)
	request.WorkspaceMountMode = types.WorkspaceMountMode(strings.Clone(string(request.WorkspaceMountMode)))
	if request.Resources != nil {
		limits := cloneResources(*request.Resources)
		request.Resources = &limits
	}
	if request.Network != nil {
		network := *request.Network
		network.Whitelist = cloneStrings(network.Whitelist)
		request.Network = &network
	}
	request.Dependencies = slices.Clone(request.Dependencies)
	for i := range request.Dependencies {
		dependency := &request.Dependencies[i]
		dependency.Name, dependency.Version, dependency.Manager = strings.Clone(dependency.Name), strings.Clone(dependency.Version), strings.Clone(dependency.Manager)
	}
	request.WorkspaceSyncExclude = cloneStrings(request.WorkspaceSyncExclude)
	return request
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	cloned := make([]string, len(values))
	for i := range values {
		cloned[i] = strings.Clone(values[i])
	}
	return cloned
}
