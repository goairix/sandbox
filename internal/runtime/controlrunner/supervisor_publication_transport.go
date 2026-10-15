package controlrunner

import (
	"context"
	"crypto/tls"

	t "github.com/goairix/sandbox/internal/runtime/controltransport"
)

// The real management TLS handler owns this read-only dispatch. Signing and
// gate checks remain in the original enrolled Supervisor methods; snapshots
// cannot create enrollment or extend publication authority after close.
func (s *Supervisor) dispatchEnrolledPublication(ctx context.Context, conn *tls.Conn, request t.RuntimePublicationEnvelope) error {
	enrollment, err := s.Enrollment()
	if err != nil {
		return err
	}
	ready, err := s.EnrolledPublicationProof(ctx, request.PublicationCertificate, request.Claim)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.WriteRuntimePublicationResponse(conn, t.RuntimePublicationResponse{Version: 1, Purpose: t.RuntimeEnrolledPublicationPurpose, Enrollment: enrollment, Ready: ready}); err != nil {
		return err
	}
	return conn.CloseWrite()
}
