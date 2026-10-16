package controltransport

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

type healthRecordInput struct {
	net.Conn
	input *bytes.Reader
	last  error
	sizes []int
}

func (c *healthRecordInput) Read(p []byte) (int, error) {
	c.sizes = append(c.sizes, len(p))
	n, err := c.input.Read(p)
	if n > 0 && c.input.Len() == 0 && c.last != nil {
		err = c.last
	}
	return n, err
}

func TestRuntimeHealthTLSFirstReadBoundaries(t *testing.T) {
	// Two records and a tail are available from the first read, including
	// the handshake record. No application-purpose switch can repair this later.
	raw := &healthRecordInput{input: bytes.NewReader([]byte{22, 3, 3, 0, 3, 1, 2, 3, 23, 3, 3, 0, 2, 4, 5, 255})}
	conn := NewFiniteTLSRecordConn(raw)
	buffer := make([]byte, 64)
	for _, want := range []int{5, 3, 5, 2} {
		n, err := conn.Read(buffer)
		require.NoError(t, err)
		require.Equal(t, want, n)
	}
	require.Equal(t, []int{5, 3, 5, 2}, raw.sizes)
	require.Equal(t, 1, raw.input.Len(), "tail never entered TLS private buffering")
}

func TestRuntimeHealthTLSFinalReadErrors(t *testing.T) {
	failure := errors.New("original adapter failure")
	for _, last := range []error{io.EOF, io.ErrUnexpectedEOF, failure} {
		t.Run(last.Error(), func(t *testing.T) {
			raw := &healthRecordInput{input: bytes.NewReader([]byte{23, 3, 3, 0, 2, 1, 2}), last: last}
			conn := NewFiniteTLSRecordConn(raw)
			buffer := make([]byte, 64)
			n, err := conn.Read(buffer)
			require.Equal(t, 5, n)
			require.NoError(t, err)
			n, err = conn.Read(buffer)
			require.Equal(t, 2, n)
			if last == io.EOF {
				require.NoError(t, err, "complete close_notify bytes may carry raw EOF")
			} else {
				require.ErrorIs(t, err, last, "non-EOF adapter failure must not be discarded")
			}
			n, err = conn.Read(buffer)
			require.Zero(t, n)
			if last == failure {
				require.ErrorIs(t, err, failure)
			} else {
				require.ErrorIs(t, err, io.ErrUnexpectedEOF)
				require.NotEqual(t, io.ErrUnexpectedEOF, err, "Go TLS must not normalize truncation to EOF")
			}
		})
	}
}
