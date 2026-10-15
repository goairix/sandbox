package controltransport

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
)

const RuntimeEnrolledPublicationPurpose = "runtime_enrolled_publication"
const MaxRuntimePublicationRequest = 32768
const MaxRuntimePublicationResponse = 65536

// RuntimePublicationEnvelope requests only an observation of existing enrolled
// authority. Neither the request nor its copied response grants publication.
type RuntimePublicationEnvelope struct {
	Version                uint32           `json:"version"`
	Purpose                string           `json:"purpose"`
	PublicationCertificate []byte           `json:"publication_certificate"`
	Claim                  p.ClaimReference `json:"claim"`
}

type RuntimePublicationResponse struct {
	Version    uint32                    `json:"version"`
	Purpose    string                    `json:"purpose"`
	Enrollment p.EnrolledRuntimeEvidence `json:"enrollment"`
	Ready      []byte                    `json:"ready"`
}

func (e RuntimePublicationEnvelope) validate() error {
	id, err := uuid.Parse(e.Claim.ClaimID)
	if e.Version != 1 || e.Purpose != RuntimeEnrolledPublicationPurpose || len(e.PublicationCertificate) == 0 || len(e.PublicationCertificate) > 4096 || err != nil || id == uuid.Nil || id.String() != e.Claim.ClaimID || e.Claim.CreateRevision <= 0 || e.Claim.LeaseID <= 0 {
		return ErrFrame
	}
	return nil
}

func (e RuntimePublicationResponse) validate() error {
	if e.Version != 1 || e.Purpose != RuntimeEnrolledPublicationPurpose || len(e.Ready) == 0 || len(e.Ready) > 8192 || len(e.Enrollment.ObserverCertificate) == 0 || len(e.Enrollment.ObserverCertificate) > 2048 || len(e.Enrollment.Receipt) == 0 || len(e.Enrollment.Receipt) > 4096 {
		return ErrFrame
	}
	// Preserve the original enrollment request's aggregate and nested caps.
	_, err := p.EncodeNodeEnrollmentRequest(e.Enrollment.Request)
	return err
}

func writePublicationFrame(w io.Writer, value any, limit int) error {
	wire, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(wire) == 0 || len(wire) > limit {
		return ErrFrame
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(wire)))
	if err := writeExact(w, size[:]); err != nil {
		return err
	}
	return writeExact(w, wire)
}

func WriteRuntimePublicationRequest(w io.Writer, e RuntimePublicationEnvelope) error {
	if err := e.validate(); err != nil {
		return err
	}
	return writePublicationFrame(w, e, MaxRuntimePublicationRequest)
}

func WriteRuntimePublicationResponse(w io.Writer, e RuntimePublicationResponse) error {
	if err := e.validate(); err != nil {
		return err
	}
	return writePublicationFrame(w, e, MaxRuntimePublicationResponse)
}

func canonicalPublicationJSON(wire []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(wire))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	canonical, err := json.Marshal(out)
	if err != nil || !bytes.Equal(canonical, wire) {
		return ErrFrame
	}
	return nil
}

// Check raw base64 length before decoding allocates nested byte slices. Escaped
// base64 and null are never canonical; decoded length includes exact padding.
func publicationBlobBound(raw json.RawMessage, limit int) bool {
	if len(raw) < 3 || len(raw) > base64.StdEncoding.EncodedLen(limit)+2 || raw[0] != '"' || raw[len(raw)-1] != '"' || bytes.ContainsRune(raw, '\\') {
		return false
	}
	body := raw[1 : len(raw)-1]
	n := base64.StdEncoding.DecodedLen(len(body))
	if body[len(body)-1] == '=' {
		n--
	}
	if len(body) > 1 && body[len(body)-2] == '=' {
		n--
	}
	return n > 0 && n <= limit
}

func requirePublicationEOF(r io.Reader) error {
	var extra [1]byte
	n, err := r.Read(extra[:])
	if n != 0 || err != io.EOF {
		return ErrFrame
	}
	return nil
}

func readRuntimePublicationHeader(r io.Reader, wire []byte) (RuntimePublicationEnvelope, error) {
	var e RuntimePublicationEnvelope
	var raw struct {
		Certificate json.RawMessage `json:"publication_certificate"`
	}
	if len(wire) == 0 || len(wire) > MaxRuntimePublicationRequest || json.Unmarshal(wire, &raw) != nil || !publicationBlobBound(raw.Certificate, 4096) {
		return e, ErrFrame
	}
	if err := canonicalPublicationJSON(wire, &e); err != nil {
		return RuntimePublicationEnvelope{}, err
	}
	if err := e.validate(); err != nil {
		return RuntimePublicationEnvelope{}, err
	}
	if err := requirePublicationEOF(r); err != nil {
		return RuntimePublicationEnvelope{}, err
	}
	return e, nil
}

// ReadRuntimePublicationResponse consumes one independently bounded response
// and EOF. In a Session the reader additionally requires TLS close_notify.
func ReadRuntimePublicationResponse(r io.Reader) (RuntimePublicationResponse, error) {
	var e RuntimePublicationResponse
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return e, err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > MaxRuntimePublicationResponse {
		return e, ErrFrame
	}
	wire := make([]byte, n)
	if _, err := io.ReadFull(r, wire); err != nil {
		return e, err
	}
	var raw struct {
		Enrollment struct {
			Request             json.RawMessage
			ObserverCertificate json.RawMessage
			Receipt             json.RawMessage
		} `json:"enrollment"`
		Ready json.RawMessage `json:"ready"`
	}
	if json.Unmarshal(wire, &raw) != nil || len(raw.Enrollment.Request) > p.NodeEnrollmentFrameBytes || !publicationBlobBound(raw.Ready, 8192) || !publicationBlobBound(raw.Enrollment.ObserverCertificate, 2048) || !publicationBlobBound(raw.Enrollment.Receipt, 4096) {
		return e, ErrFrame
	}
	var request struct {
		RuntimeCertificate json.RawMessage `json:"runtime_certificate"`
		Activation         json.RawMessage `json:"activation"`
		IssuerCertificate  json.RawMessage `json:"issuer_certificate"`
	}
	if json.Unmarshal(raw.Enrollment.Request, &request) != nil || !publicationBlobBound(request.RuntimeCertificate, 4096) || !publicationBlobBound(request.Activation, 16384) || !publicationBlobBound(request.IssuerCertificate, 4096) {
		return e, ErrFrame
	}
	if err := canonicalPublicationJSON(wire, &e); err != nil {
		return RuntimePublicationResponse{}, err
	}
	if err := e.validate(); err != nil {
		return RuntimePublicationResponse{}, err
	}
	if err := requirePublicationEOF(r); err != nil {
		return RuntimePublicationResponse{}, err
	}
	return e, nil
}
