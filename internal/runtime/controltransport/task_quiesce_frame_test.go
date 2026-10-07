package controltransport

import (
	"bytes"
	"encoding/json"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func quiesceFrame() TaskQuiesceEnvelope {
	return TaskQuiesceEnvelope{Version: 1, Purpose: "task_quiesce_users", Context: p.TaskUserQuiescenceContext{}, TicketDigest: strings.Repeat("a", 64), Ticket: []byte("ticket"), IssuerCertificate: []byte("issuer"), OriginalCloseDataReceipt: []byte("receipt")}
}

// A missing fixed branch must fail on a real serialized request, not a symbol.
func TestTaskQuiesceTransportRoutesCanonicalFiniteRequest(t *testing.T) {
	e := quiesceFrame()
	h, err := json.Marshal(e)
	require.NoError(t, err)
	r, err := ReadRuntimeRequest(bytes.NewReader(rawTaskFrame(h)))
	require.NoError(t, err)
	require.Equal(t, e, *r.Quiesce)
	require.Nil(t, r.CloseData)
	for _, e := range []TaskQuiesceEnvelope{e, {Version: 1, Purpose: "task_quiesce_users_query", TicketDigest: strings.Repeat("a", 64)}} {
		h, err = json.Marshal(e)
		require.NoError(t, err)
		_, err = ReadRuntimeRequest(bytes.NewReader(append(rawTaskFrame(h), 0)))
		require.Error(t, err, "requires real EOF")
	}
}

func TestTaskQuiesceTransportStrictBoundsAndOldWire(t *testing.T) {
	e := quiesceFrame()
	h, err := json.Marshal(e)
	require.NoError(t, err)
	for name, wire := range map[string][]byte{
		"duplicate": rawTaskFrame(append([]byte(`{"version":1,`), h[1:]...)),
		"missing":   rawTaskFrame(bytes.Replace(h, []byte(`"version":1,`), nil, 1)),
		"null":      rawTaskFrame(bytes.Replace(h, []byte(`"current":{`), []byte(`"current":null,"ignored":{`), 1)),
		"unknown":   rawTaskFrame(append([]byte(`{"extra":false,`), h[1:]...)),
		"truncated": rawTaskFrame(h)[:13], "oversize": {0, 2, 0, 1},
	} {
		t.Run(name, func(t *testing.T) { _, err := ReadRuntimeRequest(bytes.NewReader(wire)); require.Error(t, err) })
	}
	for _, field := range []string{"ticket", "issuer", "receipt", "query"} {
		bad := e
		switch field {
		case "ticket":
			bad.Ticket = make([]byte, 8193)
		case "issuer":
			bad.IssuerCertificate = make([]byte, 4097)
		case "receipt":
			bad.OriginalCloseDataReceipt = make([]byte, 8193)
		case "query":
			bad.Purpose = "task_quiesce_users_query"
		}
		require.Error(t, WriteTaskQuiesceRequest(&bytes.Buffer{}, bad))
	}
	for _, old := range []bool{false, true} {
		var b bytes.Buffer
		if old {
			require.NoError(t, WriteTaskCloseRequest(&b, taskFrame()))
		} else {
			require.NoError(t, WriteRequest(&b, frameRequest(t)))
		}
		wire := bytes.Clone(b.Bytes())
		r, err := ReadRuntimeRequest(&b)
		require.NoError(t, err)
		require.Nil(t, r.Quiesce)
		var again bytes.Buffer
		if old {
			require.NoError(t, WriteTaskCloseRequest(&again, *r.CloseData))
		} else {
			require.NoError(t, WriteRequest(&again, r.Exec))
		}
		require.Equal(t, wire, again.Bytes())
	}
	// The public old signature remains deliberately unable to accept new frames.
	_, _, err = ReadControlRequest(bytes.NewReader(rawTaskFrame(h)))
	require.Error(t, err)
}
