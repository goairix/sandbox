package workspaceprobe

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/goairix/sandbox/internal/fuseprotocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLIAcceptsOnlyExactGrammar(t *testing.T) {
	probe, _, _ := testProbe(t)
	for _, argv := range [][]string{
		{"workspace-probe", "unknown"},
		{"workspace-probe", "write-read-delete", "/tmp"},
		{"workspace-probe", "resume", "--runtime-uid", "runtime-a", "--generation", "7"},
		{"workspace-probe", "quiesce", "--runtime-uid", "runtime-a", "--generation", "07"},
	} {
		var stdout, stderr bytes.Buffer
		assert.NotZero(t, runCLI(probe, argv, bytes.NewReader(nil), &stdout, &stderr), argv)
		assert.Empty(t, stdout.String())
	}
}

func TestCleanupQuiesceCanRepeatWithoutCreatingResumeBroker(t *testing.T) {
	probe, processes, broker := testProbe(t)
	p10 := processIdentity{PID: 10, UID: 1000, StartTime: 101, State: 'T'}
	processes.states[10] = p10
	processes.snapshots = [][]processIdentity{{p10}, {p10}}
	broker.state = brokerState{RuntimeUID: "existing-cycle", Generation: 3}
	for i := 0; i < 2; i++ {
		var stdout, stderr bytes.Buffer
		code := runCLI(probe, []string{fuseprotocol.ProbeBinary, "quiesce", "--runtime-uid", "runtime-a", "--generation", "7", "--cleanup"}, bytes.NewReader(nil), &stdout, &stderr)
		require.Zero(t, code, stderr.String())
		var status fuseprotocol.ProbeStatus
		require.NoError(t, fuseprotocol.DecodeExact(bytes.TrimSpace(stdout.Bytes()), &status))
		require.True(t, status.OK)
		require.Equal(t, "runtime-a", status.RuntimeUID)
		require.Equal(t, int64(7), status.Generation)
		require.NotEmpty(t, status.Token)
		require.Equal(t, "existing-cycle", broker.state.RuntimeUID, "cleanup must not create or replace a resume broker")
		require.Equal(t, i+1, broker.cleanupCalls)
	}
}

func TestCleanupQuiesceRefusesWritableWorkspaceDescriptor(t *testing.T) {
	probe, processes, _ := testProbe(t)
	p10 := processIdentity{PID: 10, UID: 1000, StartTime: 101, State: 'T'}
	processes.states[10] = p10
	processes.snapshots = [][]processIdentity{{p10}, {p10}}
	processes.fds[10] = []descriptor{{Target: probe.workspace + "/business", Flags: 1}}
	var stdout, stderr bytes.Buffer
	code := runCLI(probe, []string{fuseprotocol.ProbeBinary, "quiesce", "--runtime-uid", "runtime-a", "--generation", "7", "--cleanup"}, bytes.NewReader(nil), &stdout, &stderr)
	require.NotZero(t, code)
	require.Empty(t, stdout.String())
	require.Contains(t, stderr.String(), "writable workspace descriptor")
}

func TestCLIResumeStrictlyDecodesAndEmitsStatus(t *testing.T) {
	probe, _, broker := testProbe(t)
	broker.state = brokerState{RuntimeUID: "runtime-a", Generation: 7}
	input, err := json.Marshal(fuseprotocol.ProbeResumeRequest{
		Version: fuseprotocol.Version, RuntimeUID: "runtime-a", Generation: 7, Token: broker.token,
	})
	require.NoError(t, err)
	var stdout, stderr bytes.Buffer
	code := runCLI(probe, []string{fuseprotocol.ProbeBinary, "resume", "--runtime-uid", "runtime-a", "--generation", "7", "--token-stdin"}, bytes.NewReader(input), &stdout, &stderr)
	require.Zero(t, code, stderr.String())
	var status fuseprotocol.ProbeStatus
	require.NoError(t, fuseprotocol.DecodeExact(bytes.TrimSpace(stdout.Bytes()), &status))
	assert.Equal(t, fuseprotocol.ProbeStatus{Version: 1, RuntimeUID: "runtime-a", Generation: 7, OK: true, Token: ""}, status)
}

func TestCLIResumeRejectsDuplicateOrMismatchedEnvelope(t *testing.T) {
	probe, _, broker := testProbe(t)
	broker.state = brokerState{RuntimeUID: "runtime-a", Generation: 7}
	inputs := [][]byte{
		[]byte(`{"version":1,"runtime_uid":"runtime-a","runtime_uid":"runtime-a","generation":7,"token":"opaque-token"}`),
		[]byte(`{"version":1,"runtime_uid":"runtime-b","generation":7,"token":"opaque-token"}`),
		append(bytes.Repeat([]byte("x"), fuseprotocol.MaxJSONBytes), 'x'),
	}
	for _, input := range inputs {
		var stdout, stderr bytes.Buffer
		code := runCLI(probe, []string{fuseprotocol.ProbeBinary, "resume", "--runtime-uid", "runtime-a", "--generation", "7", "--token-stdin"}, bytes.NewReader(input), &stdout, &stderr)
		assert.NotZero(t, code)
		assert.Empty(t, stdout.String())
	}
}

func TestCleanupQuiesceRequiresAcknowledgedFenceBeforeStoppingProcesses(t *testing.T) {
	probe, processes, broker := testProbe(t)
	p10 := processIdentity{PID: 10, UID: 1000, StartTime: 101, State: 'R'}
	processes.states[10] = p10
	processes.snapshots = [][]processIdentity{{p10}}
	broker.cleanupErr = ErrInvalidToken
	var stdout, stderr bytes.Buffer
	code := runCLI(probe, []string{fuseprotocol.ProbeBinary, "quiesce", "--runtime-uid", "runtime-a", "--generation", "7", "--cleanup"}, bytes.NewReader(nil), &stdout, &stderr)
	require.NotZero(t, code)
	require.Empty(t, stdout.String())
	require.Zero(t, processes.scans)
	require.Empty(t, processes.stopped)
}

func TestCLIKubernetesQuiesceRejectsForgedControlBeforeStopping(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x6a}, ed25519.SeedSize))
	t.Setenv(PID1EnvironmentName, PID1EnvironmentValue)
	t.Setenv(fuseprotocol.ProbeControlPublicKeyEnv, base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)))
	valid, err := fuseprotocol.SignProbeControl(key, "cleanup", "runtime-a", 7)
	require.NoError(t, err)
	wrongGeneration, err := fuseprotocol.SignProbeControl(key, "cleanup", "runtime-a", 999)
	require.NoError(t, err)
	wrongCommand, err := fuseprotocol.SignProbeControl(key, "register", "runtime-a", 7)
	require.NoError(t, err)
	wrongUID, err := fuseprotocol.SignProbeControl(key, "cleanup", "runtime-b", 7)
	require.NoError(t, err)
	otherKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x7b}, ed25519.SeedSize))
	forged, err := fuseprotocol.SignProbeControl(otherKey, "cleanup", "runtime-a", 7)
	require.NoError(t, err)
	for _, request := range []fuseprotocol.ProbeControlRequest{{}, wrongGeneration, wrongCommand, wrongUID, forged} {
		probe, processes, broker := testProbe(t)
		input, err := json.Marshal(request)
		require.NoError(t, err)
		var stdout, stderr bytes.Buffer
		code := runCLI(probe, []string{fuseprotocol.ProbeBinary, "quiesce", "--runtime-uid", "runtime-a", "--generation", "7", "--cleanup"}, bytes.NewReader(input), &stdout, &stderr)
		require.NotZero(t, code)
		require.Empty(t, stdout.String())
		require.Empty(t, processes.stopped)
		require.Zero(t, processes.scans)
		require.Zero(t, broker.cleanupCalls)
	}
	probe, _, broker := testProbe(t)
	input, err := json.Marshal(valid)
	require.NoError(t, err)
	var stdout, stderr bytes.Buffer
	require.Zero(t, runCLI(probe, []string{fuseprotocol.ProbeBinary, "quiesce", "--runtime-uid", "runtime-a", "--generation", "7", "--cleanup"}, bytes.NewReader(input), &stdout, &stderr), stderr.String())
	require.Equal(t, 1, broker.cleanupCalls)
}
