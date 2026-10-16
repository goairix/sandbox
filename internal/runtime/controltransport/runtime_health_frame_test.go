package controltransport

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
)

func healthEnvelope() RuntimeHealthEnvelope {
	return RuntimeHealthEnvelope{Version: 1, Purpose: "runtime_health_query", Identity: p.RuntimeIdentityContext{SandboxID: "sandbox", WorkspaceHash: strings.Repeat("a", 64), Generation: 1, Runtime: p.RuntimeReference{ID: "pod", UID: "uid", BootID: "boot"}}, DataGateEpoch: 2, Nonce: strings.Repeat("b", 64)}
}

func healthTestFrame(wire []byte) []byte {
	frame := make([]byte, 4, len(wire)+4)
	binary.BigEndian.PutUint32(frame, uint32(len(wire)))
	return append(frame, wire...)
}

// Removing canonical/finite parsing would accept one of these ambiguous frames.
func TestRuntimeHealthFrame(t *testing.T) {
	e := healthEnvelope()
	var frame bytes.Buffer
	require.NoError(t, WriteRuntimeHealthRequest(&frame, e))
	require.LessOrEqual(t, frame.Len(), 4096)
	require.Contains(t, frame.String(), `"identity":{"sandbox_id":"sandbox","workspace_hash":`)
	r, err := ReadRuntimeRequest(bytes.NewReader(frame.Bytes()))
	require.NoError(t, err)
	require.Equal(t, &e, r.Health)
	require.Nil(t, r.Publication)
	response := RuntimeHealthResponse{Version: 1, Purpose: "runtime_health", Identity: e.Identity, DataGateEpoch: e.DataGateEpoch, Nonce: e.Nonce, GateState: "open"}
	var output bytes.Buffer
	require.NoError(t, WriteRuntimeHealthResponse(&output, response))
	got, err := ReadRuntimeHealthResponse(bytes.NewReader(output.Bytes()))
	require.NoError(t, err)
	require.Equal(t, response, got)
	header := frame.Bytes()[4:]
	for name, bad := range map[string][]byte{
		"duplicate":        bytes.Replace(header, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
		"unknown":          bytes.Replace(header, []byte(`"version":1`), []byte(`"version":1,"unknown":0`), 1),
		"nested duplicate": bytes.Replace(header, []byte(`"sandbox_id":"sandbox"`), []byte(`"sandbox_id":"sandbox","sandbox_id":"sandbox"`), 1),
		"whitespace":       append([]byte(" "), header...),
		"nonce":            bytes.Replace(header, []byte(e.Nonce), []byte(strings.Repeat("B", 64)), 1),
		"zero gate":        bytes.Replace(header, []byte(`"data_gate_epoch":2`), []byte(`"data_gate_epoch":0`), 1),
		"oversize":         bytes.Repeat([]byte(" "), 4093),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ReadRuntimeRequest(bytes.NewReader(healthTestFrame(bad)))
			require.Error(t, err)
		})
	}
	for _, bad := range [][]byte{append(bytes.Clone(frame.Bytes()), 0), frame.Bytes()[:frame.Len()-1], {0, 0, 0, 0}, {0, 0, 16, 0}} {
		_, err := ReadRuntimeRequest(bytes.NewReader(bad))
		require.Error(t, err)
	}
	for _, bad := range [][]byte{append(bytes.Clone(output.Bytes()), 0), output.Bytes()[:output.Len()-1], healthTestFrame([]byte(`{}`))} {
		_, err := ReadRuntimeHealthResponse(bytes.NewReader(bad))
		require.Error(t, err)
	}
	response.GateState = "unknown"
	require.Error(t, WriteRuntimeHealthResponse(&bytes.Buffer{}, response))
}
