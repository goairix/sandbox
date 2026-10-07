package controltransport

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
)

func taskFrame() TaskCloseEnvelope {
	return TaskCloseEnvelope{Version: 1, Purpose: "task_close_data", Context: p.TaskCloseDataContext{CommandID: "ef07e8e0-eef5-4cc7-ad85-ae343ddfbace"}, TicketDigest: strings.Repeat("a", 64), Ticket: []byte("ticket"), IssuerCertificate: []byte("issuer")}
}
func rawTaskFrame(header []byte) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.BigEndian, uint32(len(header)))
	b.Write(header)
	return b.Bytes()
}
func TestTaskCloseFrameDispatch(t *testing.T) {
	e := taskFrame()
	var b bytes.Buffer
	require.NoError(t, WriteTaskCloseRequest(&b, e))
	old, got, err := ReadControlRequest(&b)
	require.NoError(t, err)
	require.Equal(t, Envelope{}, old)
	require.Equal(t, e, *got)
	e.Purpose = "task_close_data_query"
	e.Ticket = nil
	e.IssuerCertificate = nil
	b.Reset()
	require.NoError(t, WriteTaskCloseRequest(&b, e))
	_, got, err = ReadControlRequest(&b)
	require.NoError(t, err)
	require.Equal(t, e, *got)
	// Existing exec serializer and decoder remain the exact bytes/semantics owner.
	exec := frameRequest(t)
	b.Reset()
	require.NoError(t, WriteRequest(&b, exec))
	wire := bytes.Clone(b.Bytes())
	old, got, err = ReadControlRequest(&b)
	require.NoError(t, err)
	require.Nil(t, got)
	var again bytes.Buffer
	require.NoError(t, WriteRequest(&again, old))
	require.Equal(t, wire, again.Bytes())
}
func TestTaskCloseFrameRejects(t *testing.T) {
	h, err := json.Marshal(taskFrame())
	require.NoError(t, err)
	cases := map[string][]byte{"trailing": append(rawTaskFrame(h), 0), "truncated": rawTaskFrame(h)[:12], "oversize": {0, 2, 0, 1}, "missing": rawTaskFrame(bytes.Replace(h, []byte(`"version":1,`), nil, 1)), "duplicate": rawTaskFrame(append([]byte(`{"version":1,`), h[1:]...)), "metadata": rawTaskFrame(append([]byte(`{"metadata":null,`), h[1:]...)), "context-null": rawTaskFrame(bytes.Replace(h, []byte(`"namespace":""`), []byte(`"namespace":null`), 1)), "surrogate": rawTaskFrame(bytes.Replace(h, []byte(`"namespace":""`), []byte(`"namespace":"\ud800"`), 1)), "purpose": rawTaskFrame(bytes.Replace(h, []byte(`task_close_data`), []byte(`task_close_all`), 1))}
	for name, wire := range cases {
		t.Run(name, func(t *testing.T) { _, _, err := ReadControlRequest(bytes.NewReader(wire)); require.Error(t, err) })
	}
	e := taskFrame()
	e.Ticket = make([]byte, 4097)
	require.Error(t, WriteTaskCloseRequest(&bytes.Buffer{}, e))
	e = taskFrame()
	e.Purpose = "task_close_data_query"
	require.Error(t, WriteTaskCloseRequest(&bytes.Buffer{}, e))
	require.Error(t, WriteTaskCloseRequest(shortWriter{}, taskFrame()))
}
