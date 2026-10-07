package controlrunner

import (
	"encoding/binary"
	"fmt"
	"io"
)

// These are private monitor IPC frames, independent of Task4's TLS wire. The
// five-byte header and individual bounds are checked before allocating payloads.
const (
	monitorRequest byte = iota
	monitorStdout
	monitorStderr
	monitorResult
	monitorRenewAck
	monitorStdin
	monitorInputEnd
	monitorRenew
	monitorCancel
	monitorStarted
)

func monitorFrameLimit(kind byte) (uint32, error) {
	switch kind {
	case monitorRequest:
		return 131072, nil
	case monitorStdout, monitorStderr, monitorStdin:
		return 32768, nil
	case monitorResult, monitorStarted:
		return 8192, nil
	case monitorRenewAck, monitorRenew:
		return 64, nil
	case monitorInputEnd, monitorCancel:
		return 0, nil
	default:
		return 0, fmt.Errorf("invalid monitor frame kind")
	}
}
func readMonitorFrame(r io.Reader) (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	limit, err := monitorFrameLimit(h[0])
	if err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > limit {
		return 0, nil, fmt.Errorf("monitor frame exceeds bound")
	}
	data := make([]byte, n)
	if _, err = io.ReadFull(r, data); err != nil {
		return 0, nil, err
	}
	return h[0], data, nil
}
func writeMonitorFrame(w io.Writer, kind byte, data []byte) error {
	limit, err := monitorFrameLimit(kind)
	if err != nil {
		return err
	}
	if uint64(len(data)) > uint64(limit) {
		return fmt.Errorf("monitor frame exceeds bound")
	}
	var h [5]byte
	h[0] = kind
	binary.BigEndian.PutUint32(h[1:], uint32(len(data)))
	for _, b := range [][]byte{h[:], data} {
		for len(b) > 0 {
			n, e := w.Write(b)
			if e != nil {
				return e
			}
			if n <= 0 || n > len(b) {
				return io.ErrShortWrite
			}
			b = b[n:]
		}
	}
	return nil
}
