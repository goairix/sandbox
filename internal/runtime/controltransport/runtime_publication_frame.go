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

// Publication objects have a fixed canonical field order. Read only raw values
// until every object and blob has passed preflight: struct unmarshaling would
// merge duplicate objects and hide earlier values behind their final occurrence.
func publicationRawFields(wire []byte, names ...string) ([]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(wire))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return nil, ErrFrame
	}
	fields := make([]json.RawMessage, len(names))
	canonical := []byte{'{'}
	for i, name := range names {
		key, err := d.Token()
		if err != nil || key != name || d.Decode(&fields[i]) != nil {
			return nil, ErrFrame
		}
		if i != 0 {
			canonical = append(canonical, ',')
		}
		canonical = append(canonical, '"')
		canonical = append(canonical, name...)
		canonical = append(canonical, '"', ':')
		canonical = append(canonical, fields[i]...)
	}
	end, err := d.Token()
	if err != nil || end != json.Delim('}') {
		return nil, ErrFrame
	}
	canonical = append(canonical, '}')
	if !bytes.Equal(wire, canonical) {
		return nil, ErrFrame
	}
	return fields, nil
}

func validateRuntimePublicationRequestRaw(wire []byte) error {
	if len(wire) == 0 || len(wire) > MaxRuntimePublicationRequest {
		return ErrFrame
	}
	fields, err := publicationRawFields(wire, "version", "purpose", "publication_certificate", "claim")
	if err != nil || !publicationBlobBound(fields[2], 4096) {
		return ErrFrame
	}
	if _, err := publicationRawFields(fields[3], "claim_id", "create_revision", "lease_id"); err != nil {
		return ErrFrame
	}
	return nil
}

func readRuntimePublicationHeader(r io.Reader, wire []byte) (RuntimePublicationEnvelope, error) {
	var e RuntimePublicationEnvelope
	if err := validateRuntimePublicationRequestRaw(wire); err != nil {
		return e, err
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
	if err := validateRuntimePublicationResponseRaw(wire); err != nil {
		return e, err
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

func validateRuntimePublicationResponseRaw(wire []byte) error {
	if len(wire) == 0 || len(wire) > MaxRuntimePublicationResponse {
		return ErrFrame
	}
	fields, err := publicationRawFields(wire, "version", "purpose", "enrollment", "ready")
	if err != nil || !publicationBlobBound(fields[3], 8192) {
		return ErrFrame
	}
	enrollment, err := publicationRawFields(fields[2], "Request", "ObserverCertificate", "Receipt")
	if err != nil || len(enrollment[0]) > p.NodeEnrollmentFrameBytes || !publicationBlobBound(enrollment[1], 2048) || !publicationBlobBound(enrollment[2], 4096) {
		return ErrFrame
	}
	request, err := publicationRawFields(enrollment[0], "version", "purpose", "runtime_certificate", "activation", "issuer_certificate")
	if err != nil || !publicationBlobBound(request[2], 4096) || !publicationBlobBound(request[3], 16384) || !publicationBlobBound(request[4], 4096) {
		return ErrFrame
	}
	return nil
}
