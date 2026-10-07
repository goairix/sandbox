// Package controltransport provides bounded byte transport. Decoding an envelope
// never grants execution authority; only the owning supervisor authenticates it.
package controltransport

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

const MaxHeader = 131072
const MaxStdin = 1048576
const MaxEvent = 32768
const (
	EventAccepted byte = 1 + iota
	EventStdout
	EventStderr
	EventReceipt
	EventError
)

var ErrFrame = errors.New("invalid control frame")

// Metadata contains the complete execution metadata. Stdin follows the header
// as exact binary bytes and is bound by length and SHA-256 before acceptance.
type Metadata struct {
	Argv            []string          `json:"argv"`
	Env             map[string]string `json:"env"`
	UID             uint32            `json:"uid"`
	GID             uint32            `json:"gid"`
	WorkDir         string            `json:"work_dir"`
	TimeoutSeconds  uint32            `json:"timeout_seconds"`
	StdinLength     uint32            `json:"stdin_length"`
	StdinDigest     string            `json:"stdin_digest"`
	TTY             bool              `json:"tty"`
	RequiresNetwork bool              `json:"requires_network"`
}

// Envelope is untrusted wire data, with no dialing, key or trust selection.
type Envelope struct {
	Version           uint32                           `json:"version"`
	Purpose           string                           `json:"purpose"`
	Context           controlprotocol.ExecStartContext `json:"context"`
	Metadata          *Metadata                        `json:"metadata"`
	DescriptorDigest  string                           `json:"descriptor_digest"`
	Ticket            []byte                           `json:"ticket"`
	IssuerCertificate []byte                           `json:"issuer_certificate"`
	Stdin             []byte                           `json:"-"`
}

func StartEnvelope(c controlprotocol.ExecStartContext, d controlprotocol.ExecutionDescriptor, ticket, issuer []byte) Envelope {
	r := d.Request()
	digest := sha256.Sum256(r.Stdin)
	return Envelope{Version: 1, Purpose: "exec_start", Context: c, Metadata: &Metadata{Argv: r.Argv, Env: r.Env, UID: r.UID, GID: r.GID, WorkDir: r.WorkDir, TimeoutSeconds: r.TimeoutSeconds, StdinLength: uint32(len(r.Stdin)), StdinDigest: hex.EncodeToString(digest[:]), TTY: r.TTY, RequiresNetwork: r.RequiresNetwork}, DescriptorDigest: d.Digest(), Ticket: bytes.Clone(ticket), IssuerCertificate: bytes.Clone(issuer), Stdin: r.Stdin}
}
func (e Envelope) Descriptor() (controlprotocol.ExecutionDescriptor, error) {
	m := e.Metadata
	if m == nil || m.StdinLength > MaxStdin || int(m.StdinLength) != len(e.Stdin) {
		return controlprotocol.ExecutionDescriptor{}, ErrFrame
	}
	sum := sha256.Sum256(e.Stdin)
	if hex.EncodeToString(sum[:]) != m.StdinDigest {
		return controlprotocol.ExecutionDescriptor{}, ErrFrame
	}
	d, err := controlprotocol.NewExecutionDescriptor(controlprotocol.ExecutionRequest{Argv: m.Argv, Env: m.Env, UID: m.UID, GID: m.GID, WorkDir: m.WorkDir, TimeoutSeconds: m.TimeoutSeconds, Stdin: e.Stdin, TTY: m.TTY, RequiresNetwork: m.RequiresNetwork})
	if err != nil {
		return d, err
	}
	if d.Digest() != e.DescriptorDigest {
		return controlprotocol.ExecutionDescriptor{}, ErrFrame
	}
	return d, nil
}
func (e Envelope) validateHeader() error {
	if e.Version != 1 || len(e.Ticket) > 4096 || len(e.IssuerCertificate) > 4096 {
		return ErrFrame
	}
	switch e.Purpose {
	case "exec_start":
		if e.Metadata == nil || e.Metadata.StdinLength > MaxStdin || len(e.Ticket) == 0 || len(e.IssuerCertificate) == 0 {
			return ErrFrame
		}
	case "exec_query":
		if e.Metadata != nil || len(e.Ticket) != 0 || len(e.IssuerCertificate) != 0 {
			return ErrFrame
		}
	case "exec_renew":
		if e.Metadata != nil || len(e.Ticket) == 0 || len(e.IssuerCertificate) == 0 {
			return ErrFrame
		}
	default:
		return ErrFrame
	}
	return nil
}
func WriteRequest(w io.Writer, e Envelope) error {
	if err := e.validateHeader(); err != nil {
		return err
	}
	if e.Purpose == "exec_start" {
		if _, err := e.Descriptor(); err != nil {
			return err
		}
	} else if len(e.Stdin) != 0 {
		return ErrFrame
	}
	header, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if len(header) == 0 || len(header) > MaxHeader {
		return ErrFrame
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(header)))
	for _, b := range [][]byte{size[:], header, e.Stdin} {
		if err := writeExact(w, b); err != nil {
			return err
		}
	}
	return nil
}

// ReadRequest requires EOF after the declared body. TLS callers half-close the
// request direction; a trailing byte or missing body fails before dispatch.
func ReadRequest(r io.Reader) (Envelope, error) {
	var e Envelope
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return e, err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > MaxHeader {
		return e, ErrFrame
	}
	header := make([]byte, n)
	if _, err := io.ReadFull(r, header); err != nil {
		return e, err
	}
	decoder := json.NewDecoder(bytes.NewReader(header))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&e); err != nil {
		return e, err
	}
	canonical, err := json.Marshal(e)
	if err != nil || !bytes.Equal(canonical, header) {
		return e, fmt.Errorf("%w: noncanonical header", ErrFrame)
	}
	if err := e.validateHeader(); err != nil {
		return e, err
	}
	if e.Metadata != nil {
		e.Stdin = make([]byte, e.Metadata.StdinLength)
		if _, err := io.ReadFull(r, e.Stdin); err != nil {
			return Envelope{}, err
		}
		if _, err := e.Descriptor(); err != nil {
			return Envelope{}, err
		}
	}
	var extra [1]byte
	nextra, err := r.Read(extra[:])
	if nextra != 0 || err != io.EOF {
		return Envelope{}, fmt.Errorf("%w: request requires exact EOF", ErrFrame)
	}
	return e, nil
}
func WriteEvent(w io.Writer, kind byte, data []byte) error {
	if kind < EventAccepted || kind > EventError || len(data) > MaxEvent {
		return ErrFrame
	}
	var header [5]byte
	header[0] = kind
	binary.BigEndian.PutUint32(header[1:], uint32(len(data)))
	if err := writeExact(w, header[:]); err != nil {
		return err
	}
	return writeExact(w, data)
}
func ReadEvent(r io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(header[1:])
	if header[0] < EventAccepted || header[0] > EventError || n > MaxEvent {
		return 0, nil, ErrFrame
	}
	data := make([]byte, n)
	_, err := io.ReadFull(r, data)
	return header[0], data, err
}
func writeExact(w io.Writer, p []byte) error {
	n, err := w.Write(p)
	if err == nil && n != len(p) {
		return io.ErrShortWrite
	}
	return err
}
