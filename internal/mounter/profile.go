package mounter

import (
	"fmt"
	"net/url"
	"regexp"

	"github.com/goairix/sandbox/internal/fuseprotocol"
)

var canonicalRegion = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

type MountParameterStatus string

const (
	MountParametersVerified   MountParameterStatus = "verified"
	MountParametersCandidate  MountParameterStatus = "candidate"
	MountParametersUnverified MountParameterStatus = "unverified"
)

type DurableFlushStatus string

const (
	DurableFlushVerified     DurableFlushStatus = "verified"
	DurableFlushPendingSpike DurableFlushStatus = "blocked-pending-flush-spike"
)

// ProfileDescriptor is the complete, non-executable audit identity of a
// compiled profile. Runtime argv is generated only by Profile.Options below;
// a packaged manifest can attest to this descriptor but cannot add options.
type ProfileDescriptor struct {
	ID               string               `json:"id"`
	Provider         string               `json:"provider"`
	MountParameters  MountParameterStatus `json:"mount_parameters"`
	DurableFlush     DurableFlushStatus   `json:"durable_flush"`
	TLSRequired      bool                 `json:"tls_required"`
	EndpointOption   string               `json:"endpoint_option"`
	RegionOption     string               `json:"region_option"`
	AddressingStyle  string               `json:"addressing_style"`
	SignatureVersion string               `json:"signature_version"`
}

type Profile struct {
	ID         string
	Provider   string
	Descriptor ProfileDescriptor
	Options    func(fuseprotocol.BootstrapConfig) ([]string, error)
	Flush      func(fuseprotocol.BootstrapConfig) []string
}

type ProfileRegistry interface {
	Lookup(id string) (Profile, bool)
}

type StaticProfiles map[string]Profile

func (p StaticProfiles) Lookup(id string) (Profile, bool) { profile, ok := p[id]; return profile, ok }

var compiledProfileCatalog = map[string]Profile{
	"minio-sigv4-path-style-v1": {
		ID: "minio-sigv4-path-style-v1", Provider: "minio",
		Descriptor: ProfileDescriptor{
			ID: "minio-sigv4-path-style-v1", Provider: "minio",
			MountParameters: MountParametersVerified, DurableFlush: DurableFlushPendingSpike,
			TLSRequired: true, EndpointOption: "url", RegionOption: "region",
			AddressingStyle: "path", SignatureVersion: "sigv4",
		},
		Options: minioOptions,
	},
	"huawei-obs-public-v1": {
		ID: "huawei-obs-public-v1", Provider: "obs",
		Descriptor: ProfileDescriptor{
			ID: "huawei-obs-public-v1", Provider: "obs",
			MountParameters: MountParametersCandidate, DurableFlush: DurableFlushPendingSpike,
			TLSRequired: true, EndpointOption: "url", RegionOption: "endpoint",
			AddressingStyle: "unverified", SignatureVersion: "sigv2",
		},
		// This records the vendor-documented public-cloud candidate contract.
		// It is deliberately unreachable from production Lookup until its
		// target-region provider spike promotes the descriptor to verified.
		Options: huaweiPublicCandidateOptions,
	},
	"huawei-obs-private-2023-v1": {
		ID: "huawei-obs-private-2023-v1", Provider: "obs",
		Descriptor: ProfileDescriptor{
			ID: "huawei-obs-private-2023-v1", Provider: "obs",
			MountParameters: MountParametersUnverified, DurableFlush: DurableFlushPendingSpike,
			TLSRequired: true, EndpointOption: "unverified", RegionOption: "unverified",
			AddressingStyle: "unverified", SignatureVersion: "unverified",
		},
	},
}

func minioOptions(config fuseprotocol.BootstrapConfig) ([]string, error) {
	if err := requireHTTPS(config.Endpoint); err != nil {
		return nil, err
	}
	if config.Provider != "" && config.Provider != "minio" {
		return nil, fmt.Errorf("minio profile provider mismatch")
	}
	if !canonicalRegion.MatchString(config.Region) {
		return nil, fmt.Errorf("minio profile requires a canonical SigV4 region")
	}
	return []string{"-o", "url=" + config.Endpoint, "-o", "region=" + config.Region, "-o", "use_path_request_style", "-o", "sigv4"}, nil
}

func huaweiPublicCandidateOptions(config fuseprotocol.BootstrapConfig) ([]string, error) {
	if err := requireHTTPS(config.Endpoint); err != nil {
		return nil, err
	}
	if config.Provider != "" && config.Provider != "obs" {
		return nil, fmt.Errorf("Huawei OBS profile provider mismatch")
	}
	if !canonicalRegion.MatchString(config.Region) {
		return nil, fmt.Errorf("Huawei OBS public candidate requires a region endpoint value")
	}
	return []string{"-o", "url=" + config.Endpoint, "-o", "endpoint=" + config.Region, "-o", "sigv2"}, nil
}

func requireHTTPS(raw string) error {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") {
		return fmt.Errorf("profile requires a canonical TLS HTTPS endpoint")
	}
	return nil
}

// InspectCompiledProfile exposes audit metadata, including blocked candidates.
// It must not be used to select a production mount profile.
func InspectCompiledProfile(id string) (Profile, bool) {
	profile, ok := compiledProfileCatalog[id]
	return profile, ok
}

// LookupCompiledProfile is the production mount lookup. Candidate and
// unverified entries, unknown IDs, and provider mismatches all fail closed.
func LookupCompiledProfile(provider, id string) (Profile, bool) {
	profile, ok := InspectCompiledProfile(id)
	if !ok || profile.Provider != provider || profile.Descriptor.MountParameters != MountParametersVerified || profile.Options == nil {
		return Profile{}, false
	}
	return profile, true
}

func CompiledProfiles() ProfileRegistry {
	profile, _ := LookupCompiledProfile("minio", "minio-sigv4-path-style-v1")
	return StaticProfiles{profile.ID: profile}
}

// BoundProfiles converts the constrained build-time profile ID into a
// single-entry runtime registry. Packaged JSON is never consulted here.
func BoundProfiles(id string) (ProfileRegistry, error) {
	profile, ok := InspectCompiledProfile(id)
	if !ok {
		return nil, fmt.Errorf("image profile binding is unknown")
	}
	if profile.Descriptor.MountParameters != MountParametersVerified || profile.Options == nil {
		return nil, fmt.Errorf("image profile binding is not mount-verified")
	}
	return StaticProfiles{id: profile}, nil
}

func CheckProductionProfile(provider, id string) error {
	profile, ok := InspectCompiledProfile(id)
	if !ok || profile.Provider != provider {
		return fmt.Errorf("profile is unknown or does not match provider")
	}
	switch profile.Descriptor.MountParameters {
	case MountParametersVerified:
		// Continue to the independent durability gate.
	case MountParametersCandidate:
		return fmt.Errorf("profile is blocked pending provider spike")
	default:
		return fmt.Errorf("profile is blocked pending provider spike")
	}
	if selectable, ok := LookupCompiledProfile(provider, id); !ok || selectable.Options == nil {
		return fmt.Errorf("profile has no verified mount implementation")
	}
	if profile.Descriptor.DurableFlush != DurableFlushVerified || profile.Flush == nil {
		return fmt.Errorf("profile is blocked pending durable-flush provider spike")
	}
	return nil
}
