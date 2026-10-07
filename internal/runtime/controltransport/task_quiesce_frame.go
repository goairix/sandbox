package controltransport

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
)

func (e TaskQuiesceEnvelope) validateHeader() error {
	if e.Version != 1 || len(e.TicketDigest) != 64 || len(e.Ticket) > 8192 || len(e.IssuerCertificate) > 4096 || len(e.OriginalCloseDataReceipt) > 8192 {
		return ErrFrame
	}
	for _, c := range e.TicketDigest {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return ErrFrame
		}
	}
	switch e.Purpose {
	case "task_quiesce_users":
		if len(e.Ticket) == 0 || len(e.IssuerCertificate) == 0 || len(e.OriginalCloseDataReceipt) == 0 {
			return ErrFrame
		}
	case "task_quiesce_users_query":
		if e.Ticket != nil || e.IssuerCertificate != nil || e.OriginalCloseDataReceipt != nil {
			return ErrFrame
		}
	default:
		return ErrFrame
	}
	return nil
}

func WriteTaskQuiesceRequest(w io.Writer, e TaskQuiesceEnvelope) error {
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

func readTaskQuiesceHeader(r io.Reader, header []byte) (TaskQuiesceEnvelope, error) {
	var e TaskQuiesceEnvelope
	decoder := json.NewDecoder(bytes.NewReader(header))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&e); err != nil {
		return TaskQuiesceEnvelope{}, err
	}
	canonical, err := json.Marshal(e)
	if err != nil || !bytes.Equal(canonical, header) {
		return TaskQuiesceEnvelope{}, ErrFrame
	}
	if err = e.validateHeader(); err != nil {
		return TaskQuiesceEnvelope{}, err
	}
	var extra [1]byte
	n, err := r.Read(extra[:])
	if n != 0 || err != io.EOF {
		return TaskQuiesceEnvelope{}, ErrFrame
	}
	return e, nil
}
