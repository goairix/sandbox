package controltransport

import (
	"bytes"
	"context"
	"io"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

// ReadEnrolledPublication returns copied diagnostics over the pinned original
// management TLS destination. The caller must still apply its original enrolled
// publication verifier and current native guards before any publication write.
func (d *Destination) ReadEnrolledPublication(ctx context.Context, certificate []byte, claim p.ClaimReference) (p.EnrolledRuntimeEvidence, []byte, error) {
	if d == nil || d.self != d || isNil(ctx) {
		return p.EnrolledRuntimeEvidence{}, nil, ErrDestination
	}
	e := RuntimePublicationEnvelope{Version: 1, Purpose: RuntimeEnrolledPublicationPurpose, PublicationCertificate: certificate, Claim: claim}
	if err := e.validate(); err != nil {
		return p.EnrolledRuntimeEvidence{}, nil, err
	}
	e.PublicationCertificate = bytes.Clone(certificate)
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	s, err := d.Open(bounded)
	if err != nil {
		return p.EnrolledRuntimeEvidence{}, nil, err
	}
	defer s.Close()
	if err := s.sendEnrolledPublication(bounded, e); err != nil {
		return p.EnrolledRuntimeEvidence{}, nil, err
	}
	response, err := s.readEnrolledPublication(bounded)
	if err != nil {
		return p.EnrolledRuntimeEvidence{}, nil, err
	}
	return response.Enrollment, response.Ready, nil
}

func (s *Session) sendEnrolledPublication(ctx context.Context, e RuntimePublicationEnvelope) error {
	return s.sendMode(ctx, requestPublication, func(w io.Writer) error { return WriteRuntimePublicationRequest(w, e) })
}

func (s *Session) readEnrolledPublication(ctx context.Context) (RuntimePublicationResponse, error) {
	if s == nil || s.self != s || isNil(ctx) {
		return RuntimePublicationResponse{}, ErrDestination
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.sent || s.mode != requestPublication || s.publicationRead {
		return RuntimePublicationResponse{}, ErrFrame
	}
	// A failed read cannot be retried to discard a bad prefix or trailing bytes.
	s.publicationRead = true
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := bounded.Err(); err != nil {
		return RuntimePublicationResponse{}, err
	}
	stop := closeOnContext(bounded, s.conn)
	defer stop()
	deadline, _ := bounded.Deadline()
	if err := s.conn.SetReadDeadline(deadline); err != nil {
		return RuntimePublicationResponse{}, err
	}
	response, err := ReadRuntimePublicationResponse(s.conn)
	if err == nil {
		// TLS close_notify authenticates the response, but a finite adapter may
		// still owe its terminal status. Preserve the same deadline and context
		// owner until the original transport confirms EOF before Session.Close.
		// Bypass only the strict TLS wrapper: authenticated EOF was checked above.
		var extra [1]byte
		n, terminalErr := s.archiveTransport.Conn.Read(extra[:])
		if n != 0 || terminalErr == nil {
			err = ErrFrame
		} else if terminalErr != io.EOF {
			err = terminalErr
		}
	}
	if err == nil {
		err = bounded.Err()
	}
	if err != nil {
		return RuntimePublicationResponse{}, err
	}
	return response, nil
}
