package controltransport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"time"
)

// QueryRuntimeHealth binds a fresh random nonce to this exact pinned TLS peer.
// The original adapter must also finish successfully before an observation returns.
func (d *Destination) QueryRuntimeHealth(ctx context.Context, gate int64) (RuntimeHealthResponse, error) {
	if d == nil || d.self != d || isNil(ctx) || gate <= 0 {
		return RuntimeHealthResponse{}, ErrDestination
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return RuntimeHealthResponse{}, err
	}
	e := RuntimeHealthEnvelope{Version: 1, Purpose: RuntimeHealthPurpose, Identity: d.identity, DataGateEpoch: gate, Nonce: hex.EncodeToString(nonce[:])}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	s, err := d.Open(bounded)
	if err != nil {
		return RuntimeHealthResponse{}, err
	}
	defer s.Close()
	if err := s.sendMode(bounded, requestHealth, func(w io.Writer) error { return WriteRuntimeHealthRequest(w, e) }); err != nil {
		return RuntimeHealthResponse{}, err
	}
	response, err := s.readRuntimeHealth(bounded)
	if err != nil {
		return RuntimeHealthResponse{}, err
	}
	if response.Identity != e.Identity || response.DataGateEpoch != e.DataGateEpoch || response.Nonce != e.Nonce {
		return RuntimeHealthResponse{}, ErrFrame
	}
	return response, nil
}

func (s *Session) readRuntimeHealth(ctx context.Context) (RuntimeHealthResponse, error) {
	if s == nil || s.self != s || isNil(ctx) {
		return RuntimeHealthResponse{}, ErrDestination
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.sent || s.mode != requestHealth || s.healthRead {
		return RuntimeHealthResponse{}, ErrFrame
	}
	s.healthRead = true
	if err := ctx.Err(); err != nil {
		return RuntimeHealthResponse{}, err
	}
	stop := closeOnContext(ctx, s.conn)
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := s.conn.SetReadDeadline(deadline); err != nil {
		return RuntimeHealthResponse{}, err
	}
	response, err := ReadRuntimeHealthResponse(s.conn)
	if err == nil {
		var extra [1]byte
		n, terminalErr := s.archiveTransport.Conn.Read(extra[:])
		if n != 0 || terminalErr == nil {
			err = ErrFrame
		} else if terminalErr != io.EOF {
			err = terminalErr
		}
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return RuntimeHealthResponse{}, err
	}
	return response, nil
}
