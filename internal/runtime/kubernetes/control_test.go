package kubernetes

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordedPodCommand struct {
	pod       string
	container string
	argv      []string
	stdin     []byte
}

type recordingPodExecutor struct {
	commands []recordedPodCommand
	stdout   []byte
	err      error
}

func TestControlStreamOptionsMatchesExecStdinFlag(t *testing.T) {
	withoutInput := controlStreamOptions(nil, &boundedBuffer{}, &boundedBuffer{})
	assert.Nil(t, withoutInput.Stdin)

	withInput := controlStreamOptions([]byte(`{"version":1}`), &boundedBuffer{}, &boundedBuffer{})
	require.NotNil(t, withInput.Stdin)
}

func (e *recordingPodExecutor) Exec(_ context.Context, pod, container string, argv []string, stdin []byte) ([]byte, error) {
	e.commands = append(e.commands, recordedPodCommand{pod: pod, container: container, argv: append([]string(nil), argv...), stdin: append([]byte(nil), stdin...)})
	return append([]byte(nil), e.stdout...), e.err
}

func TestExecControlOnlyTargetsMounterAndPreservesArgv(t *testing.T) {
	executor := &recordingPodExecutor{stdout: []byte(`{"version":1,"accepted":true,"runtime_uid":"uid-a","generation":7}`)}
	rt := &Runtime{controlExecutor: executor}

	_, err := rt.execControl(context.Background(), "pod-a", "sandbox", []string{"anything"}, nil)
	require.ErrorIs(t, err, ErrInvalidControlContainer)
	_, err = rt.execControl(context.Background(), "pod-a", workspaceMounterContainer, []string{mounterBinary, "authorize"}, []byte(`{"safe":true}`))
	require.NoError(t, err)
	require.Len(t, executor.commands, 1)
	assert.Equal(t, workspaceMounterContainer, executor.commands[0].container)
	assert.Equal(t, []string{mounterBinary, "authorize"}, executor.commands[0].argv)
	assert.Equal(t, []byte(`{"safe":true}`), executor.commands[0].stdin)
}

func TestPrivateExecutorsRejectOversizedStdinBeforeDispatch(t *testing.T) {
	executor := &recordingPodExecutor{}
	rt := &Runtime{controlExecutor: executor}
	oversized := []byte(strings.Repeat("x", maxControlJSONBytes+1))
	_, err := rt.execControl(context.Background(), "pod-a", workspaceMounterContainer, []string{mounterBinary, "authorize"}, oversized)
	require.Error(t, err)
	_, err = rt.execSandboxProbe(context.Background(), "pod-a", []string{workspaceProbeBinary, "resume", "--runtime-uid", "uid-a", "--generation", "1", "--token-stdin"}, oversized)
	require.Error(t, err)
	assert.Empty(t, executor.commands)
}

func TestProbeAllowlistRequiresCanonicalPositiveGeneration(t *testing.T) {
	valid := []string{workspaceProbeBinary, "quiesce", "--runtime-uid", "uid-a", "--generation", "1"}
	assert.True(t, allowedProbeCommand(valid))
	for _, generation := range []string{"", "0", "-1", "01", "not-a-number"} {
		argv := append([]string(nil), valid...)
		argv[5] = generation
		assert.False(t, allowedProbeCommand(argv), generation)
	}
	argv := append([]string(nil), valid...)
	argv[3] = ""
	assert.False(t, allowedProbeCommand(argv))
}

func TestDecodeMounterStatusIsStrictBoundedAndVersioned(t *testing.T) {
	valid := `{"version":1,"state":"ready","runtime_uid":"uid-a","pool_key":"pool-a","mount_type":"fuse","generation":7,"restart_detected":false,"cache_bytes":0,"cache_limit_bytes":2147483648,"cache_exceeded":false}`
	status, err := decodeMounterStatus([]byte(valid))
	require.NoError(t, err)
	assert.Equal(t, int64(7), status.Generation)

	invalid := []string{
		`{"version":2,"state":"ready","runtime_uid":"uid-a","pool_key":"pool-a","mount_type":"fuse","generation":7,"restart_detected":false,"cache_bytes":0,"cache_limit_bytes":2147483648,"cache_exceeded":false}`,
		`{"version":1,"state":"ready","runtime_uid":"uid-a","pool_key":"pool-a","mount_type":"fuse","generation":7}`,
		`{"version":1,"state":"ready","runtime_uid":"uid-a","pool_key":"pool-a","mount_type":"fuse","generation":7,"restart_detected":null,"cache_bytes":0,"cache_limit_bytes":2147483648,"cache_exceeded":false}`,
		`{"version":1,"version":1,"state":"ready","runtime_uid":"uid-a","pool_key":"pool-a","mount_type":"fuse","generation":7,"restart_detected":false,"cache_bytes":0,"cache_limit_bytes":2147483648,"cache_exceeded":false}`,
		valid + ` {}`,
		`{"version":1,"state":"ready","runtime_uid":"uid-a","pool_key":"pool-a","mount_type":"fuse","generation":7,"restart_detected":false,"cache_bytes":0,"cache_limit_bytes":2147483648,"cache_exceeded":false,"secret":"must-not-appear"}`,
	}
	for _, raw := range invalid {
		_, err := decodeMounterStatus([]byte(raw))
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "must-not-appear")
	}
	_, err = decodeMounterStatus([]byte(strings.Repeat(" ", maxControlJSONBytes+1)))
	require.Error(t, err)
}

func TestDecodeControlAcknowledgementsRejectMalformedFields(t *testing.T) {
	_, err := decodeControlAck([]byte(`{"version":1,"accepted":true,"runtime_uid":"uid-a","generation":7}`))
	require.NoError(t, err)
	_, err = decodeControlAck([]byte(`{"version":1,"accepted":true,"runtime_uid":null,"generation":7}`))
	require.Error(t, err)

	_, err = decodeShutdownAck([]byte(`{"version":1,"runtime_uid":"uid-a","generation":7,"graceful_unmount":true}`))
	require.NoError(t, err)
	_, err = decodeShutdownAck([]byte(`{"version":1,"runtime_uid":"uid-a","generation":7,"graceful_unmount":true,"process_exited":true}`))
	require.Error(t, err)

	_, err = decodeProbeStatus([]byte(`{"version":1,"runtime_uid":"uid-a","generation":7,"ok":true,"token":"opaque"}`))
	require.NoError(t, err)
	_, err = decodeProbeStatus([]byte(`{"version":1,"runtime_uid":"uid-a","generation":7,"ok":true,"token":"opaque","x":1}`))
	require.Error(t, err)
}

func TestKubernetesControlErrorAcceptsOnlySafeDiagnosticToken(t *testing.T) {
	err := kubernetesControlExecError(errors.New("stream failed"), []byte("workspace-mounter-error:storage-auth\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "storage-auth")

	err = kubernetesControlExecError(errors.New("stream failed"), []byte("workspace-mounter-error:storage-auth\nAKIA-DO-NOT-LEAK\n"))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "storage-auth")
	assert.NotContains(t, err.Error(), "AKIA-DO-NOT-LEAK")
}
