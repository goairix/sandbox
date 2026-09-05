package workspaceprobe

import (
	"bytes"
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
