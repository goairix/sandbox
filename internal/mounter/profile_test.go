package mounter

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/fuseprotocol"
)

func TestCompiledMinIOProfileHasExactVerifiedMountContract(t *testing.T) {
	profile, ok := LookupCompiledProfile("minio", "minio-sigv4-path-style-v1")
	require.True(t, ok)
	assert.Equal(t, MountParametersVerified, profile.Descriptor.MountParameters)
	assert.Equal(t, DurableFlushVerified, profile.Descriptor.DurableFlush)
	assert.True(t, profile.Descriptor.TLSRequired)
	assert.Equal(t, "endpoint", profile.Descriptor.RegionOption)

	options, err := profile.Options(fuseprotocol.BootstrapConfig{
		Provider: "minio", Endpoint: "https://minio.example.com:9000", Region: "us-east-1",
	})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"-o", "url=https://minio.example.com:9000",
		"-o", "endpoint=us-east-1",
		"-o", "use_path_request_style",
		"-o", "sigv4",
	}, options)
	require.NotNil(t, profile.Flush)
	assert.Equal(t, []string{"/bin/sync", "-f", "--", "/workspace"}, profile.Flush(fuseprotocol.BootstrapConfig{MountPath: "/workspace"}))

	_, err = profile.Options(fuseprotocol.BootstrapConfig{Provider: "minio", Endpoint: "http://minio.example.com", Region: "us-east-1"})
	require.ErrorContains(t, err, "TLS")
	for _, region := range []string{"", "US East 1", "../us-east-1", "cn_north_4"} {
		_, err = profile.Options(fuseprotocol.BootstrapConfig{Provider: "minio", Endpoint: "https://minio.example.com", Region: region})
		require.ErrorContains(t, err, "canonical")
	}
}

func TestCompiledCatalogRecordsBlockedOBSProfilesWithoutMakingThemSelectable(t *testing.T) {
	public, ok := InspectCompiledProfile("huawei-obs-public-v1")
	require.True(t, ok)
	assert.Equal(t, MountParametersCandidate, public.Descriptor.MountParameters)
	assert.Equal(t, "url", public.Descriptor.EndpointOption)
	assert.Equal(t, "endpoint", public.Descriptor.RegionOption)
	assert.Equal(t, "sigv2", public.Descriptor.SignatureVersion)
	assert.Equal(t, DurableFlushPendingSpike, public.Descriptor.DurableFlush)

	private, ok := InspectCompiledProfile("huawei-obs-private-2023-v1")
	require.True(t, ok)
	assert.Equal(t, MountParametersUnverified, private.Descriptor.MountParameters)
	assert.Equal(t, "unverified", private.Descriptor.EndpointOption)
	assert.Nil(t, private.Options)

	_, ok = LookupCompiledProfile("obs", public.Descriptor.ID)
	assert.False(t, ok, "candidate profiles must not be selectable")
	_, ok = LookupCompiledProfile("minio", "huawei-obs-public-v1")
	assert.False(t, ok, "provider mismatch must fail closed")
	_, ok = LookupCompiledProfile("obs", "unknown")
	assert.False(t, ok)
}

func TestBoundProfilesContainsExactlyOneMountVerifiedProfile(t *testing.T) {
	registry, err := BoundProfiles("minio-sigv4-path-style-v1")
	require.NoError(t, err)
	profile, ok := registry.Lookup("minio-sigv4-path-style-v1")
	require.True(t, ok)
	assert.Equal(t, "minio", profile.Provider)
	_, ok = registry.Lookup("huawei-obs-public-v1")
	assert.False(t, ok)

	_, err = BoundProfiles("huawei-obs-public-v1")
	require.ErrorContains(t, err, "not mount-verified")
	_, err = BoundProfiles(strings.Repeat("x", 1024))
	require.Error(t, err)
}

func TestProductionProfileReadinessRequiresDurableFlushEvidence(t *testing.T) {
	err := CheckProductionProfile("minio", "minio-sigv4-path-style-v1")
	require.NoError(t, err)
	err = CheckProductionProfile("obs", "huawei-obs-public-v1")
	require.ErrorContains(t, err, "pending provider spike")
}

func TestProductionProfileReadinessRequiresMountImplementation(t *testing.T) {
	const id = "test-verified-without-options"
	compiledProfileCatalog[id] = Profile{
		ID: id, Provider: "minio",
		Descriptor: ProfileDescriptor{
			ID: id, Provider: "minio", MountParameters: MountParametersVerified,
			DurableFlush: DurableFlushVerified, TLSRequired: true,
			EndpointOption: "url", RegionOption: "region", AddressingStyle: "path", SignatureVersion: "sigv4",
		},
		Flush: func(fuseprotocol.BootstrapConfig) []string { return []string{"verified-flush"} },
	}
	t.Cleanup(func() { delete(compiledProfileCatalog, id) })

	require.ErrorContains(t, CheckProductionProfile("minio", id), "no verified mount implementation")
}
