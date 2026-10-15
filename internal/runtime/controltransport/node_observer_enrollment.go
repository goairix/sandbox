package controltransport

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

// NodeEnrollmentSession exclusively owns one original kernel-peer connection.
// No response may be forwarded into a second session; the signed challenge
// binds its random nonce and exact request bytes. Caller must verify the root
// observer identity before its Supervisor signs the returned challenge.
type NodeEnrollmentSession struct {
	mu     sync.Mutex
	conn   *net.UnixConn
	ctx    context.Context
	cancel context.CancelFunc
	stop   func() bool
	joined chan struct{}
	used   bool
}

func WriteNodeObserverFrame(w io.Writer, wire []byte) error {
	if len(wire) == 0 || len(wire) > p.NodeEnrollmentFrameBytes {
		return fmt.Errorf("node observer frame exceeds fixed bound")
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(wire)))
	if err := writeExact(w, size[:]); err != nil {
		return err
	}
	return writeExact(w, wire)
}

func ReadNodeObserverFrame(r io.Reader) ([]byte, error) {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > p.NodeEnrollmentFrameBytes {
		return nil, fmt.Errorf("node observer frame exceeds fixed bound")
	}
	wire := make([]byte, n)
	_, err := io.ReadFull(r, wire)
	return wire, err
}

func OpenNodeEnrollment(ctx context.Context, conn *net.UnixConn, req p.NodeEnrollmentRequest) (*NodeEnrollmentSession, p.NodeEnrollmentChallenge, error) {
	fail := func(err error) (*NodeEnrollmentSession, p.NodeEnrollmentChallenge, error) {
		if conn != nil {
			_ = conn.Close()
		}
		return nil, p.NodeEnrollmentChallenge{}, err
	}
	if ctx == nil || conn == nil {
		return fail(fmt.Errorf("missing node enrollment context/socket"))
	}
	wire, err := p.EncodeNodeEnrollmentRequest(req)
	if err != nil {
		return fail(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	s := &NodeEnrollmentSession{conn: conn, ctx: bounded, cancel: cancel, joined: make(chan struct{})}
	s.stop = context.AfterFunc(bounded, func() { defer close(s.joined); _ = conn.Close() })
	deadline, _ := bounded.Deadline()
	if err = conn.SetDeadline(deadline); err == nil {
		err = WriteNodeObserverFrame(conn, wire)
	}
	if err != nil {
		_ = s.Close()
		return fail(err)
	}
	reply, err := ReadNodeObserverFrame(conn)
	if err != nil {
		_ = s.Close()
		return fail(err)
	}
	challenge, err := p.DecodeNodeEnrollmentChallenge(reply)
	sum := sha256.Sum256(wire)
	if err != nil || challenge.RequestDigest != hex.EncodeToString(sum[:]) || bounded.Err() != nil {
		_ = s.Close()
		return fail(fmt.Errorf("node enrollment challenge request mismatch: %v", err))
	}
	return s, challenge, nil
}

func (s *NodeEnrollmentSession) finish() {
	_ = s.conn.Close()
	s.cancel()
	if !s.stop() {
		<-s.joined
	}
}

func (s *NodeEnrollmentSession) Close() error {
	if s == nil || s.conn == nil {
		return nil
	}
	_ = s.conn.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.used {
		s.used = true
		s.finish()
	}
	return nil
}

func (s *NodeEnrollmentSession) Enroll(ctx context.Context, response p.NodeEnrollmentResponse) (p.NodeEnrollmentReceipt, error) {
	if s == nil || ctx == nil || s.conn == nil || s.ctx == nil || s.cancel == nil || s.stop == nil {
		return p.NodeEnrollmentReceipt{}, fmt.Errorf("missing node enrollment session/context")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used || s.ctx.Err() != nil || ctx.Err() != nil {
		return p.NodeEnrollmentReceipt{}, fmt.Errorf("node enrollment session expired or consumed")
	}
	s.used = true
	defer s.finish()
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(closed); _ = s.conn.Close() })
	defer func() {
		if !stop() {
			<-closed
		}
	}()
	wire, err := p.EncodeNodeEnrollmentResponse(response)
	if err != nil {
		return p.NodeEnrollmentReceipt{}, err
	}
	if err := WriteNodeObserverFrame(s.conn, wire); err != nil {
		return p.NodeEnrollmentReceipt{}, err
	}
	reply, err := ReadNodeObserverFrame(s.conn)
	if err != nil {
		return p.NodeEnrollmentReceipt{}, err
	}
	return p.DecodeNodeEnrollmentReceipt(reply)
}
