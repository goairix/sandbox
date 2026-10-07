package controltransport

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

// TaskCloseEnvelope is a separate canonical request: execution metadata and
// stdin are not part of this protocol. Decoding conveys no command authority.
type TaskCloseEnvelope struct {
	Version           uint32                 `json:"version"`
	Purpose           string                 `json:"purpose"`
	Context           p.TaskCloseDataContext `json:"context"`
	TicketDigest      string                 `json:"ticket_digest"`
	Ticket            []byte                 `json:"ticket"`
	IssuerCertificate []byte                 `json:"issuer_certificate"`
}

func (e TaskCloseEnvelope) validateHeader() error {
	if e.Version != 1 || len(e.TicketDigest) != 64 || len(e.Ticket) > 4096 || len(e.IssuerCertificate) > 4096 {
		return ErrFrame
	}
	for _, c := range e.TicketDigest {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return ErrFrame
		}
	}
	switch e.Purpose {
	case "task_close_data":
		if len(e.Ticket) == 0 || len(e.IssuerCertificate) == 0 {
			return ErrFrame
		}
	case "task_close_data_query":
		if e.Ticket != nil || e.IssuerCertificate != nil {
			return ErrFrame
		}
	default:
		return ErrFrame
	}
	return nil
}
func WriteTaskCloseRequest(w io.Writer, e TaskCloseEnvelope) error {
	if err := e.validateHeader(); err != nil {
		return err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if len(b) > MaxHeader {
		return ErrFrame
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(b)))
	if err = writeExact(w, size[:]); err != nil {
		return err
	}
	return writeExact(w, b)
}

// ReadControlRequest routes a bounded header without changing the original exec
// codec or its bytes. Both paths consume exactly one request and require EOF.
func ReadControlRequest(r io.Reader) (Envelope, *TaskCloseEnvelope, error) {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return Envelope{}, nil, err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > MaxHeader {
		return Envelope{}, nil, ErrFrame
	}
	header := make([]byte, n)
	if _, err := io.ReadFull(r, header); err != nil {
		return Envelope{}, nil, err
	}
	var purpose struct {
		Purpose string `json:"purpose"`
	}
	if err := json.Unmarshal(header, &purpose); err != nil {
		return Envelope{}, nil, err
	}
	if purpose.Purpose != "task_close_data" && purpose.Purpose != "task_close_data_query" {
		e, err := ReadRequest(io.MultiReader(bytes.NewReader(size[:]), bytes.NewReader(header), r))
		return e, nil, err
	}
	var e TaskCloseEnvelope
	decoder := json.NewDecoder(bytes.NewReader(header))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&e); err != nil {
		return Envelope{}, nil, err
	}
	canonical, err := json.Marshal(e)
	if err != nil || !bytes.Equal(canonical, header) {
		return Envelope{}, nil, ErrFrame
	}
	if err = e.validateHeader(); err != nil {
		return Envelope{}, nil, err
	}
	var extra [1]byte
	got, err := r.Read(extra[:])
	if got != 0 || err != io.EOF {
		return Envelope{}, nil, ErrFrame
	}
	return Envelope{}, &e, nil
}
