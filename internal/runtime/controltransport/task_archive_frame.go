package controltransport

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"reflect"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

const (
	ArchiveData     byte = 0xa1
	ArchiveComplete byte = 0xa2
)

// TaskWorkspaceArchiveEnvelope carries untrusted attribution without a path,
// user override, routing authority, or live producer capability.
type TaskWorkspaceArchiveEnvelope struct {
	Version                   uint32                                      `json:"version"`
	Purpose                   string                                      `json:"purpose"`
	Context                   controlprotocol.TaskWorkspaceArchiveContext `json:"context"`
	TicketDigest              string                                      `json:"ticket_digest"`
	Ticket                    []byte                                      `json:"ticket"`
	IssuerCertificate         []byte                                      `json:"issuer_certificate"`
	OriginalQuiescenceReceipt []byte                                      `json:"original_quiescence_receipt"`
}

func (e TaskWorkspaceArchiveEnvelope) validateHeader() error {
	if e.Version != 1 || len(e.TicketDigest) != 64 || len(e.Ticket) > 8192 || len(e.IssuerCertificate) > 4096 || len(e.OriginalQuiescenceReceipt) > 12288 {
		return ErrFrame
	}
	for _, c := range e.TicketDigest {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return ErrFrame
		}
	}
	switch e.Purpose {
	case "task_workspace_archive":
		if len(e.Ticket) == 0 || len(e.IssuerCertificate) == 0 || len(e.OriginalQuiescenceReceipt) == 0 {
			return ErrFrame
		}
	case "task_workspace_archive_query":
		if e.Ticket != nil || e.IssuerCertificate != nil || e.OriginalQuiescenceReceipt != nil {
			return ErrFrame
		}
	default:
		return ErrFrame
	}
	return nil
}

func archiveNilIO(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	}
	return false
}

func archiveWriteExact(w io.Writer, p []byte) error {
	n, err := w.Write(p)
	if err != nil {
		return err
	}
	if n != len(p) {
		return io.ErrShortWrite
	}
	return nil
}

func WriteTaskWorkspaceArchiveRequest(w io.Writer, e TaskWorkspaceArchiveEnvelope) error {
	if archiveNilIO(w) {
		return ErrFrame
	}
	if err := e.validateHeader(); err != nil {
		return err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if len(b) == 0 || len(b) > MaxHeader {
		return ErrFrame
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(b)))
	if err = archiveWriteExact(w, size[:]); err != nil {
		return err
	}
	return archiveWriteExact(w, b)
}

// ReadTaskWorkspaceArchiveRequest requires canonical JSON and actual EOF.
// Authentication and context semantics belong to the protocol verifier.
func ReadTaskWorkspaceArchiveRequest(r io.Reader) (TaskWorkspaceArchiveEnvelope, error) {
	fail := func(err error) (TaskWorkspaceArchiveEnvelope, error) { return TaskWorkspaceArchiveEnvelope{}, err }
	if archiveNilIO(r) {
		return fail(ErrFrame)
	}
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return fail(err)
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > MaxHeader {
		return fail(ErrFrame)
	}
	header := make([]byte, n)
	if _, err := io.ReadFull(r, header); err != nil {
		return fail(err)
	}
	return readTaskWorkspaceArchiveHeader(r, header)
}

func readTaskWorkspaceArchiveHeader(r io.Reader, header []byte) (TaskWorkspaceArchiveEnvelope, error) {
	fail := func(err error) (TaskWorkspaceArchiveEnvelope, error) { return TaskWorkspaceArchiveEnvelope{}, err }
	var e TaskWorkspaceArchiveEnvelope
	d := json.NewDecoder(bytes.NewReader(header))
	d.DisallowUnknownFields()
	if err := d.Decode(&e); err != nil {
		return fail(err)
	}
	canonical, err := json.Marshal(e)
	if err != nil || !bytes.Equal(canonical, header) {
		return fail(ErrFrame)
	}
	if err = e.validateHeader(); err != nil {
		return fail(err)
	}
	var extra [1]byte
	if n, err := r.Read(extra[:]); n != 0 || err != io.EOF {
		return fail(ErrFrame)
	}
	return e, nil
}

func validArchiveFrame(kind byte, size uint64) bool {
	return size > 0 && (kind == ArchiveData && size <= 32768 || kind == ArchiveComplete && size <= 16384)
}

// WriteTaskWorkspaceArchiveFrame writes one frame; it makes no assertion about
// ordering, total bytes, digest, authentication, or response EOF.
func WriteTaskWorkspaceArchiveFrame(w io.Writer, kind byte, data []byte) error {
	if archiveNilIO(w) || !validArchiveFrame(kind, uint64(len(data))) {
		return ErrFrame
	}
	var header [5]byte
	header[0] = kind
	binary.BigEndian.PutUint32(header[1:], uint32(len(data)))
	if err := archiveWriteExact(w, header[:]); err != nil {
		return err
	}
	return archiveWriteExact(w, data)
}

// ReadTaskWorkspaceArchiveFrame reads one bounded frame, leaving any next frame
// untouched. A future receiver must enforce stream completion and actual EOF.
func ReadTaskWorkspaceArchiveFrame(r io.Reader) (byte, []byte, error) {
	if archiveNilIO(r) {
		return 0, nil, ErrFrame
	}
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(header[1:])
	if !validArchiveFrame(header[0], uint64(n)) {
		return 0, nil, ErrFrame
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		return 0, nil, err
	}
	return header[0], data, nil
}
