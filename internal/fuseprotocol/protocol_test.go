package fuseprotocol

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestControlDTOsUseOneStrictVersionedSchema(t *testing.T) {
	auth := AuthorizeRequest{
		Version: Version, RuntimeUID: "uid-a", PoolKey: strings.Repeat("a", 64), WorkspaceHash: strings.Repeat("b", 64),
		Prefix: "workspaces/a/", LeaseGeneration: 7, MountAttempt: 1,
	}
	raw, err := json.Marshal(auth)
	require.NoError(t, err)
	var decoded AuthorizeRequest
	require.NoError(t, DecodeExact(raw, &decoded))
	assert.Equal(t, auth, decoded)

	for _, invalid := range [][]byte{
		[]byte(`{"version":1,"runtime_uid":"uid-a","pool_key":"key","workspace_hash":"hash","prefix":"a/","lease_generation":1,"mount_attempt":1,"unknown":true}`),
		[]byte(`{"version":1,"version":1,"runtime_uid":"uid-a","pool_key":"key","workspace_hash":"hash","prefix":"a/","lease_generation":1,"mount_attempt":1}`),
		[]byte(`{"version":1,"runtime_uid":null,"pool_key":"key","workspace_hash":"hash","prefix":"a/","lease_generation":1,"mount_attempt":1}`),
		[]byte(`{"version":1,"runtime_uid":"uid-a","pool_key":"key","workspace_hash":"hash","prefix":"a/","lease_generation":1}`),
		append(raw, []byte(` {}`)...),
		[]byte(strings.Repeat("x", MaxJSONBytes+1)),
		append([]byte(`{"version":1,"runtime_uid":"`), []byte{0xff, '"', '}'}...),
	} {
		require.Error(t, DecodeExact(invalid, &decoded))
	}
}

func TestFixedCommandAllowlistsMatchRuntimeAndBinaries(t *testing.T) {
	assert.True(t, AllowedMounterCommand([]string{MounterBinary, "authorize"}))
	assert.True(t, AllowedMounterCommand([]string{MounterBinary, "flush"}))
	assert.True(t, AllowedMounterCommand([]string{MounterBinary, "shutdown"}))
	assert.True(t, AllowedMounterCommand([]string{MounterBinary, "health", "prepared"}))
	assert.True(t, AllowedMounterCommand([]string{MounterBinary, "health", "ready"}))
	assert.False(t, AllowedMounterCommand([]string{MounterBinary, "bootstrap"}), "Kubernetes private exec never bootstraps a running sidecar")

	assert.True(t, AllowedProbeCommand([]string{ProbeBinary, "quiesce", "--runtime-uid", "uid-a", "--generation", "7"}))
	assert.True(t, AllowedProbeCommand([]string{ProbeBinary, "resume", "--runtime-uid", "uid-a", "--generation", "7", "--token-stdin"}))
	for _, invalidGeneration := range []string{"", "0", "-1", "01", "x"} {
		assert.False(t, AllowedProbeCommand([]string{ProbeBinary, "write-read-delete", "--runtime-uid", "uid-a", "--generation", invalidGeneration}))
	}
}

func TestBootstrapAllowsOnlyOptionalRuntimeUIDRegionAndCA(t *testing.T) {
	bootstrap := BootstrapConfig{
		Version: Version, Provider: "minio", Bucket: "bucket-a", Endpoint: "https://minio.example.com", Profile: "minio-sigv4-path-style-v1",
		AccessKeyFile: "/run/secrets/workspace/accessKey", SecretKeyFile: "/run/secrets/workspace/secretKey", PasswdFile: "/run/s3fs/passwd-s3fs",
		CacheDir: "/var/cache/s3fs", MountPath: "/workspace", PoolKey: strings.Repeat("a", 64), CacheLimitBytes: 2 << 30,
		MountTimeoutSeconds: 30, FlushTimeoutSeconds: 30, UnmountTimeoutSeconds: 15,
	}
	raw, err := json.Marshal(bootstrap)
	require.NoError(t, err)
	var decoded BootstrapConfig
	require.NoError(t, Decode(raw, &decoded))
	assert.Equal(t, bootstrap, decoded)

	require.Error(t, Decode([]byte(`{"version":1,"provider":"minio","unknown":"secret"}`), &decoded))
}

func TestPublicCLIGrammarsAreStrictAndDistinct(t *testing.T) {
	for _, argv := range [][]string{
		{MounterBinary, "supervise"},
		{MounterBinary, "bootstrap"},
		{MounterBinary, "authorize"},
		{MounterBinary, "health", "prepared"},
		{MounterBinary, "health", "ready"},
		{MounterBinary, "health", "prepared", "--self-check-image"},
		{MounterBinary, "health", "prepared", "--release-check-image"},
		{MounterBinary, "flush"},
		{MounterBinary, "shutdown"},
	} {
		assert.True(t, AllowedMounterCLI(argv), argv)
	}
	assert.False(t, AllowedMounterCLI([]string{MounterBinary, "broker"}))
	assert.False(t, AllowedMounterCLI([]string{MounterBinary, "health", "ready", "--release-check-image"}))
	assert.False(t, AllowedMounterCommand([]string{MounterBinary, "bootstrap"}), "Kubernetes private exec must not bootstrap")
	assert.True(t, AllowedDockerMounterCommand([]string{MounterBinary, "bootstrap"}))
	assert.False(t, AllowedDockerMounterCommand([]string{MounterBinary, "supervise"}))

	assert.True(t, AllowedProbeCLI([]string{ProbeBinary, "self-check"}))
	assert.True(t, AllowedProbeCLI([]string{ProbeBinary, "write-read-delete", "--runtime-uid", "uid-a", "--generation", "1"}))
	assert.False(t, AllowedProbeCLI([]string{ProbeBinary, "hidden-broker"}))
	assert.False(t, AllowedProbeCLI([]string{ProbeBinary, "write-read-delete", "/arbitrary"}))
}

func TestSocketEnvelopeHasExactBoundedSchema(t *testing.T) {
	request := SocketRequest{Version: Version, Command: "authorize", Input: []byte(`{"version":1}`)}
	raw, err := json.Marshal(request)
	require.NoError(t, err)
	assert.Equal(t, `{"version":1,"command":"authorize","input":{"version":1}}`, string(raw))
	var decoded SocketRequest
	require.NoError(t, DecodeExact(raw, &decoded))
	assert.Equal(t, request, decoded)

	response := SocketResponse{Version: Version, OK: true, Output: []byte(`{}`), ErrorCode: ""}
	raw, err = json.Marshal(response)
	require.NoError(t, err)
	var decodedResponse SocketResponse
	require.NoError(t, DecodeExact(raw, &decodedResponse))
	assert.Equal(t, response, decodedResponse)
}

func TestDeriveProbeObjectNameIsStableOpaqueAndVersioned(t *testing.T) {
	first, err := DeriveProbeObjectName("runtime-secret-a", 7)
	require.NoError(t, err)
	second, err := DeriveProbeObjectName("runtime-secret-a", 7)
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.True(t, IsReservedProbeObjectName(first))
	assert.NotContains(t, first, "runtime-secret-a")
	assert.Regexp(t, `^\.workspace-probe-v1-[0-9a-f]{64}$`, first)
}

func TestDeriveProbeObjectNameSeparatesIdentityAndCannotInjectPath(t *testing.T) {
	base, err := DeriveProbeObjectName("runtime-a", 7)
	require.NoError(t, err)
	otherRuntime, err := DeriveProbeObjectName("runtime-b", 7)
	require.NoError(t, err)
	otherGeneration, err := DeriveProbeObjectName("runtime-a", 8)
	require.NoError(t, err)
	injectedIdentity, err := DeriveProbeObjectName("../runtime-a", 7)
	require.NoError(t, err)
	assert.NotEqual(t, base, otherRuntime)
	assert.NotEqual(t, base, otherGeneration)
	assert.True(t, IsReservedProbeObjectName(injectedIdentity))
	assert.NotContains(t, injectedIdentity, "..")
	assert.NotContains(t, injectedIdentity, "/")
	_, err = DeriveProbeObjectName("runtime-a", 0)
	require.Error(t, err)
	assert.False(t, IsReservedProbeObjectName(".workspace-probe-v1-../../user"))
}
