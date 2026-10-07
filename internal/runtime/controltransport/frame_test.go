package controltransport

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"testing"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

func frameRequest(t *testing.T) Envelope {
	t.Helper()
	d, err := controlprotocol.NewExecutionDescriptor(controlprotocol.ExecutionRequest{Argv: []string{"/bin/cat"}, Env: map[string]string{"PATH": "/bin"}, UID: 1, GID: 1, WorkDir: "/", TimeoutSeconds: 10, Stdin: []byte{0, 1, 255, 10}})
	if err != nil {
		t.Fatal(err)
	}
	return StartEnvelope(controlprotocol.ExecStartContext{}, d, []byte("ticket"), []byte("issuer"))
}
func TestFrameExactBinaryAndEOF(t *testing.T) {
	e := frameRequest(t)
	var b bytes.Buffer
	if err := WriteRequest(&b, e); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRequest(&b)
	if err != nil {
		t.Fatal(err)
	}
	d, err := got.Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(d.Request().Stdin, []byte{0, 1, 255, 10}) {
		t.Fatalf("stdin changed: %v", d.Request().Stdin)
	}
}
func TestFrameRejectsMalformedBeforeAcceptance(t *testing.T) {
	e := frameRequest(t)
	var b bytes.Buffer
	if err := WriteRequest(&b, e); err != nil {
		t.Fatal(err)
	}
	valid := b.Bytes()
	n := int(binary.BigEndian.Uint32(valid[:4]))
	header := valid[4 : 4+n]
	body := valid[4+n:]
	cases := map[string][]byte{"truncated_header": valid[:6], "truncated_stdin": valid[:len(valid)-1], "trailing_byte": append(bytes.Clone(valid), 0), "oversized_header": {0, 2, 0, 1}, "zero_header": {0, 0, 0, 0}}
	edits := map[string][]byte{"duplicate": append([]byte(`{"version":1,`), header[1:]...), "unknown": append([]byte(`{"unknown":0,`), header[1:]...), "purpose": bytes.Replace(header, []byte(`"exec_start"`), []byte(`"exec_end"`), 1), "stdin_length": bytes.Replace(header, []byte(`"stdin_length":4`), []byte(`"stdin_length":1048577`), 1), "digest": bytes.Replace(header, []byte(`"stdin_digest":"`), []byte(`"stdin_digest":"0`), 1)}
	for name, h := range edits {
		var x bytes.Buffer
		binary.Write(&x, binary.BigEndian, uint32(len(h)))
		x.Write(h)
		x.Write(body)
		cases[name] = x.Bytes()
	}
	for name, wire := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadRequest(bytes.NewReader(wire)); err == nil {
				t.Fatal("accepted malformed frame")
			}
		})
	}
}
func TestOutputFramesBoundedAndShortWrites(t *testing.T) {
	var b bytes.Buffer
	if err := WriteEvent(&b, EventStdout, bytes.Repeat([]byte{42}, 32768)); err != nil {
		t.Fatal(err)
	}
	k, p, err := ReadEvent(&b)
	if err != nil || k != EventStdout || len(p) != 32768 {
		t.Fatalf("%d %d %v", k, len(p), err)
	}
	if err := WriteEvent(&b, EventStdout, make([]byte, 32769)); err == nil {
		t.Fatal("oversized event accepted")
	}
	if err := WriteEvent(shortWriter{}, EventStdout, []byte{1}); err != io.ErrShortWrite {
		t.Fatalf("short writer: %v", err)
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return 0, nil }
func TestFrameRejectsNonCanonicalMetadata(t *testing.T) {
	e := frameRequest(t)
	var b bytes.Buffer
	WriteRequest(&b, e)
	wire := b.Bytes()
	n := int(binary.BigEndian.Uint32(wire[:4]))
	var obj any
	json.Unmarshal(wire[4:4+n], &obj)
	h, _ := json.MarshalIndent(obj, "", " ")
	var bad bytes.Buffer
	binary.Write(&bad, binary.BigEndian, uint32(len(h)))
	bad.Write(h)
	bad.Write(wire[4+n:])
	if _, err := ReadRequest(&bad); err == nil {
		t.Fatal("noncanonical header accepted")
	}
}
