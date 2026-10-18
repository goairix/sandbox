package controltransport

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

const (
	// RuntimeRenewalPurpose routes one root-certified install on the existing
	// runtime; RuntimeRenewalQueryPurpose carries only its exact retained pair.
	RuntimeRenewalPurpose      = "runtime_activation_renew"
	RuntimeRenewalQueryPurpose = "runtime_activation_renew_query"
	// MaxRuntimeRenewalRequest is bytes including the four-byte frame prefix.
	// It accommodates the existing 16 KiB aggregate journal activation bundle
	// plus explicit identity, nonce and pair attribution; nested caps stay intact.
	MaxRuntimeRenewalRequest = 32768
	// MaxRuntimeRenewalResponse bounds one scalar diagnostic frame in bytes,
	// including its four-byte prefix. No certificates or execution data return.
	MaxRuntimeRenewalResponse = 4096
)

// RuntimeRenewalEnvelope selects one predecessor/candidate pair on the exact
// existing runtime. Only install carries root-signed certification; query has
// no payload and never authorizes another installation or opens a data gate.
type RuntimeRenewalEnvelope struct {
	Version         uint32
	Purpose         string
	Identity        p.RuntimeIdentityContext
	DataGateEpoch   int64
	Nonce           string
	PreviousDigest  string
	CandidateDigest string

	Activation         []byte
	RuntimeCertificate []byte
	IssuerCertificate  []byte
}

// RuntimeRenewalResponse is a nonce-bound diagnostic from an authenticated
// finite exchange. A copied value grants no metadata commit or execution right.
type RuntimeRenewalResponse struct {
	Version         uint32
	Purpose         string
	Identity        p.RuntimeIdentityContext
	DataGateEpoch   int64
	Nonce           string
	PreviousDigest  string
	CandidateDigest string
	Installed       bool
	// Rejected is a completed original install refusal before journal effects.
	// It is never an answer to a Query or proof inferred from candidate absence.
	Rejected  bool
	GateState string
}

type runtimeRenewalWire struct {
	Version         uint32                `json:"version"`
	Purpose         string                `json:"purpose"`
	Identity        runtimeHealthIdentity `json:"identity"`
	DataGateEpoch   int64                 `json:"data_gate_epoch"`
	Nonce           string                `json:"nonce"`
	PreviousDigest  string                `json:"previous_digest"`
	CandidateDigest string                `json:"candidate_digest"`

	Activation         []byte `json:"activation,omitempty"`
	RuntimeCertificate []byte `json:"runtime_certificate,omitempty"`
	IssuerCertificate  []byte `json:"issuer_certificate,omitempty"`
	Installed          bool   `json:"installed,omitempty"`
	Rejected           bool   `json:"rejected,omitempty"`
	GateState          string `json:"gate_state,omitempty"`
}

func (v runtimeRenewalWire) validate(response bool) error {
	identity := runtimeHealthWire{Version: v.Version, Purpose: RuntimeHealthPurpose, Identity: v.Identity, DataGateEpoch: v.DataGateEpoch, Nonce: v.Nonce}
	if identity.validate(false) != nil || !healthDigest(v.PreviousDigest) || !healthDigest(v.CandidateDigest) || v.PreviousDigest == v.CandidateDigest {
		return ErrFrame
	}
	if response {
		if v.Purpose != "runtime_activation_renewed" || v.Installed == v.Rejected || (v.GateState != "open" && v.GateState != "closed") || len(v.Activation) != 0 || len(v.RuntimeCertificate) != 0 || len(v.IssuerCertificate) != 0 {
			return ErrFrame
		}
		return nil
	}
	if v.Installed || v.Rejected || v.GateState != "" {
		return ErrFrame
	}
	switch v.Purpose {
	case RuntimeRenewalPurpose:
		if len(v.Activation) == 0 || len(v.Activation) > 16384 || len(v.RuntimeCertificate) == 0 || len(v.RuntimeCertificate) > 8192 || len(v.IssuerCertificate) == 0 || len(v.IssuerCertificate) > 8192 {
			return ErrFrame
		}
	case RuntimeRenewalQueryPurpose:
		if len(v.Activation) != 0 || len(v.RuntimeCertificate) != 0 || len(v.IssuerCertificate) != 0 {
			return ErrFrame
		}
	default:
		return ErrFrame
	}
	return nil
}

func renewalRequestWire(e RuntimeRenewalEnvelope) runtimeRenewalWire {
	return runtimeRenewalWire{Version: e.Version, Purpose: e.Purpose, Identity: healthIdentity(e.Identity), DataGateEpoch: e.DataGateEpoch, Nonce: e.Nonce, PreviousDigest: e.PreviousDigest, CandidateDigest: e.CandidateDigest, Activation: e.Activation, RuntimeCertificate: e.RuntimeCertificate, IssuerCertificate: e.IssuerCertificate}
}

// WriteRuntimeRenewalRequest encodes one finite structural request. The owning
// supervisor still authenticates install certification before touching journal.
func WriteRuntimeRenewalRequest(w io.Writer, e RuntimeRenewalEnvelope) error {
	v := renewalRequestWire(e)
	if err := v.validate(false); err != nil {
		return err
	}
	return writePublicationFrame(w, v, MaxRuntimeRenewalRequest-4)
}

// WriteRuntimeRenewalResponse encodes exactly one installed/refused result.
// Callers must finish authenticated EOF and the original transport afterwards.
func WriteRuntimeRenewalResponse(w io.Writer, e RuntimeRenewalResponse) error {
	v := runtimeRenewalWire{Version: e.Version, Purpose: e.Purpose, Identity: healthIdentity(e.Identity), DataGateEpoch: e.DataGateEpoch, Nonce: e.Nonce, PreviousDigest: e.PreviousDigest, CandidateDigest: e.CandidateDigest, Installed: e.Installed, Rejected: e.Rejected, GateState: e.GateState}
	if err := v.validate(true); err != nil {
		return err
	}
	return writePublicationFrame(w, v, MaxRuntimeRenewalResponse-4)
}

func readRuntimeRenewalHeader(r io.Reader, header []byte) (RuntimeRenewalEnvelope, error) {
	v, err := decodeRenewalHeader(r, header, false)
	if err != nil {
		return RuntimeRenewalEnvelope{}, err
	}
	return RuntimeRenewalEnvelope{Version: v.Version, Purpose: v.Purpose, Identity: p.RuntimeIdentityContext(v.Identity), DataGateEpoch: v.DataGateEpoch, Nonce: v.Nonce, PreviousDigest: v.PreviousDigest, CandidateDigest: v.CandidateDigest, Activation: v.Activation, RuntimeCertificate: v.RuntimeCertificate, IssuerCertificate: v.IssuerCertificate}, nil
}

func decodeRenewalHeader(r io.Reader, header []byte, response bool) (runtimeRenewalWire, error) {
	var v runtimeRenewalWire
	limit := MaxRuntimeRenewalRequest
	if response {
		limit = MaxRuntimeRenewalResponse
	}
	if len(header) == 0 || len(header)+4 > limit || canonicalPublicationJSON(header, &v) != nil || v.validate(response) != nil {
		return runtimeRenewalWire{}, ErrFrame
	}
	if err := requirePublicationEOF(r); err != nil {
		return runtimeRenewalWire{}, err
	}
	return v, nil
}

// ReadRuntimeRenewalResponse rejects noncanonical, unbounded and trailing wire.
// Destination additionally binds nonce/tuple/pair and checks concrete TLS and
// adapter completion; this parser alone produces no installation authority.
func ReadRuntimeRenewalResponse(r io.Reader) (RuntimeRenewalResponse, error) {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return RuntimeRenewalResponse{}, err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > MaxRuntimeRenewalResponse-4 {
		return RuntimeRenewalResponse{}, ErrFrame
	}
	header := make([]byte, n)
	if _, err := io.ReadFull(r, header); err != nil {
		return RuntimeRenewalResponse{}, err
	}
	v, err := decodeRenewalHeader(r, header, true)
	if err != nil {
		return RuntimeRenewalResponse{}, err
	}
	return RuntimeRenewalResponse{Version: v.Version, Purpose: v.Purpose, Identity: p.RuntimeIdentityContext(v.Identity), DataGateEpoch: v.DataGateEpoch, Nonce: v.Nonce, PreviousDigest: v.PreviousDigest, CandidateDigest: v.CandidateDigest, Installed: v.Installed, Rejected: v.Rejected, GateState: v.GateState}, nil
}

// RenewRuntimeActivation sends one selected candidate once. A failed exchange
// is uncertain; callers retain this pair and use Query rather than re-signing
// or interpreting transport failure as a failed runtime installation.
func (d *Destination) RenewRuntimeActivation(ctx context.Context, previousDigest string, next p.TargetActivationEvidence) (RuntimeRenewalResponse, error) {
	if d == nil || d.self != d || next.Identity() != d.identity {
		return RuntimeRenewalResponse{}, ErrDestination
	}
	e := RuntimeRenewalEnvelope{Version: 1, Purpose: RuntimeRenewalPurpose, Identity: d.identity, DataGateEpoch: next.DataGateEpoch(), PreviousDigest: previousDigest, CandidateDigest: next.Digest(), Activation: next.Wire(), RuntimeCertificate: next.RuntimeCertificate(), IssuerCertificate: next.IssuerCertificate()}
	return d.exchangeRuntimeRenewal(ctx, e)
}

// QueryRuntimeActivationRenewal asks only about the exact original candidate.
// It is independent of business activation expiry and open-gate admission, but
// still requires fresh pinned management TLS and the original adapter terminal.
func (d *Destination) QueryRuntimeActivationRenewal(ctx context.Context, gate int64, previousDigest, candidateDigest string) (RuntimeRenewalResponse, error) {
	if d == nil || d.self != d {
		return RuntimeRenewalResponse{}, ErrDestination
	}
	e := RuntimeRenewalEnvelope{Version: 1, Purpose: RuntimeRenewalQueryPurpose, Identity: d.identity, DataGateEpoch: gate, PreviousDigest: previousDigest, CandidateDigest: candidateDigest}
	return d.exchangeRuntimeRenewal(ctx, e)
}

func (d *Destination) exchangeRuntimeRenewal(ctx context.Context, e RuntimeRenewalEnvelope) (response RuntimeRenewalResponse, err error) {
	if isNil(ctx) {
		return response, ErrDestination
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return response, err
	}
	e.Nonce = hex.EncodeToString(nonce[:])
	if err := renewalRequestWire(e).validate(false); err != nil {
		return response, err
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	s, err := d.open(bounded, true)
	if err != nil {
		return response, err
	}
	defer func() {
		err = errors.Join(err, s.Close(), bounded.Err())
		if err != nil {
			response = RuntimeRenewalResponse{}
		}
	}()
	if err = s.sendMode(bounded, requestRenewal, func(w io.Writer) error { return WriteRuntimeRenewalRequest(w, e) }); err != nil {
		return response, err
	}
	response, err = s.readRuntimeRenewal(bounded)
	if err == nil && (response.Identity != e.Identity || response.DataGateEpoch != e.DataGateEpoch || response.Nonce != e.Nonce || response.PreviousDigest != e.PreviousDigest || response.CandidateDigest != e.CandidateDigest) {
		err = ErrFrame
	}
	if err == nil && e.Purpose == RuntimeRenewalQueryPurpose && response.Rejected {
		err = ErrFrame
	}
	return response, err
}

func (s *Session) readRuntimeRenewal(ctx context.Context) (RuntimeRenewalResponse, error) {
	if s == nil || s.self != s || isNil(ctx) {
		return RuntimeRenewalResponse{}, ErrDestination
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.sent || s.mode != requestRenewal || s.renewalRead {
		return RuntimeRenewalResponse{}, ErrFrame
	}
	s.renewalRead = true
	stop := closeOnContext(ctx, s.conn)
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := s.conn.SetReadDeadline(deadline); err != nil {
		return RuntimeRenewalResponse{}, err
	}
	response, err := ReadRuntimeRenewalResponse(s.conn)
	if err == nil {
		// Authenticated close_notify is followed by the concrete adapter's
		// original terminal EOF, before closing its Session or returning a DTO.
		var extra [1]byte
		n, terminalErr := s.archiveTransport.Conn.Read(extra[:])
		if n != 0 || terminalErr == nil {
			err = ErrFrame
		} else if terminalErr != io.EOF {
			err = terminalErr
		}
	}
	if err != nil {
		return RuntimeRenewalResponse{}, err
	}
	return response, ctx.Err()
}
