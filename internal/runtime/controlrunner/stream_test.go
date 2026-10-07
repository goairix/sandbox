package controlrunner

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

// Reject attacker-controlled lengths before allocating a payload, and retain
// binary stdout exactly rather than passing it through JSON/string transforms.
func TestMonitorFrames(t *testing.T) {
	wire := []byte{1, 0, 0, 0, 4, 0, 255, 10, 128}
	kind, data, err := readMonitorFrame(bytes.NewReader(wire))
	if err != nil || kind != 1 || !bytes.Equal(data, []byte{0, 255, 10, 128}) {
		t.Fatalf("frame %d %x %v", kind, data, err)
	}
	var encoded bytes.Buffer
	if err = writeMonitorFrame(&encoded, 1, []byte{0, 255, 10, 128}); err != nil || !bytes.Equal(encoded.Bytes(), wire) {
		t.Fatalf("write %x %v", encoded.Bytes(), err)
	}
	for _, n := range []uint32{32769, 1048576, 0xffffffff} {
		var h [5]byte
		h[0] = 1
		binary.BigEndian.PutUint32(h[1:], n)
		if _, _, err = readMonitorFrame(bytes.NewReader(h[:])); err == nil {
			t.Fatalf("oversize %d accepted", n)
		}
	}
	for _, input := range [][]byte{{99, 0, 0, 0, 0}, {1, 0, 0, 0, 4, 1}, {1, 0, 0}} {
		if _, _, err = readMonitorFrame(bytes.NewReader(input)); err == nil {
			t.Fatal("malformed accepted")
		}
	}
	if _, _, err = readMonitorFrame(bytes.NewReader(nil)); err != io.EOF {
		t.Fatalf("clean EOF %v", err)
	}
	if err = writeMonitorFrame(&encoded, 1, make([]byte, 32769)); err == nil {
		t.Fatal("oversized write accepted")
	}
}
