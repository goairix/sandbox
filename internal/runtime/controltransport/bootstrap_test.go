package controltransport

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestBootstrapStrictFiniteRequest(t *testing.T) {
	var b bytes.Buffer
	e := BootstrapRequest{Version: 1, Purpose: "hello"}
	if err := WriteBootstrap(&b, e); err != nil {
		t.Fatal(err)
	}
	valid := bytes.Clone(b.Bytes())
	var got BootstrapRequest
	if err := ReadBootstrap(&b, &got); err != nil || got.Purpose != "hello" {
		t.Fatalf("%v %v", got, err)
	}
	for name, wire := range map[string][]byte{"trailing": append(bytes.Clone(valid), 0), "truncated": valid[:len(valid)-1], "large": {0, 0, 64, 0}, "duplicate": bootstrapBytes([]byte(`{"version":1,"version":1,"purpose":"hello","activation":null,"runtime_certificate":null,"issuer_certificate":null}`))} {
		t.Run(name, func(t *testing.T) {
			var x BootstrapRequest
			if err := ReadBootstrap(bytes.NewReader(wire), &x); err == nil {
				t.Fatal("invalid bootstrap accepted")
			}
		})
	}
}
func bootstrapBytes(p []byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint32(len(p)))
	b.Write(p)
	return b.Bytes()
}
