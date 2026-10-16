package controltransport

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"unicode"
	"unicode/utf8"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

const RuntimeHealthPurpose = "runtime_health_query"
const MaxRuntimeHealthFrame = 4096

// RuntimeHealthEnvelope requests a current scalar observation, never an effect.
type RuntimeHealthEnvelope struct {
	Version       uint32
	Purpose       string
	Identity      p.RuntimeIdentityContext
	DataGateEpoch int64
	Nonce         string
}

// RuntimeHealthResponse is metadata, not an execution or future reuse permit.
type RuntimeHealthResponse struct {
	Version       uint32
	Purpose       string
	Identity      p.RuntimeIdentityContext
	DataGateEpoch int64
	Nonce         string
	GateState     string
}

type runtimeHealthIdentity struct {
	SandboxID     string             `json:"sandbox_id"`
	WorkspaceHash string             `json:"workspace_hash"`
	Generation    int64              `json:"generation"`
	Runtime       p.RuntimeReference `json:"runtime"`
}

type runtimeHealthWire struct {
	Version       uint32                `json:"version"`
	Purpose       string                `json:"purpose"`
	Identity      runtimeHealthIdentity `json:"identity"`
	DataGateEpoch int64                 `json:"data_gate_epoch"`
	Nonce         string                `json:"nonce"`
	GateState     string                `json:"gate_state,omitempty"`
}

func healthIdentity(i p.RuntimeIdentityContext) runtimeHealthIdentity {
	return runtimeHealthIdentity{i.SandboxID, i.WorkspaceHash, i.Generation, i.Runtime}
}

func healthDigest(s string) bool {
	wire, err := hex.DecodeString(s)
	return err == nil && len(wire) == 32 && hex.EncodeToString(wire) == s
}

func healthOpaque(s string) bool {
	if s == "" || len(s) > 128 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (e runtimeHealthWire) validate(response bool) error {
	i := e.Identity
	if e.Version != 1 || !healthDigest(e.Nonce) || !healthDigest(i.WorkspaceHash) || !healthOpaque(i.SandboxID) || i.Generation <= 0 || !healthOpaque(i.Runtime.ID) || !healthOpaque(i.Runtime.UID) || !healthOpaque(i.Runtime.BootID) || e.DataGateEpoch <= 0 {
		return ErrFrame
	}
	if response {
		if e.Purpose != "runtime_health" || (e.GateState != "open" && e.GateState != "closed") {
			return ErrFrame
		}
	} else if e.Purpose != RuntimeHealthPurpose || e.GateState != "" {
		return ErrFrame
	}
	return nil
}

func WriteRuntimeHealthRequest(w io.Writer, e RuntimeHealthEnvelope) error {
	v := runtimeHealthWire{Version: e.Version, Purpose: e.Purpose, Identity: healthIdentity(e.Identity), DataGateEpoch: e.DataGateEpoch, Nonce: e.Nonce}
	if err := v.validate(false); err != nil {
		return err
	}
	return writePublicationFrame(w, v, MaxRuntimeHealthFrame-4)
}

func WriteRuntimeHealthResponse(w io.Writer, e RuntimeHealthResponse) error {
	v := runtimeHealthWire{Version: e.Version, Purpose: e.Purpose, Identity: healthIdentity(e.Identity), DataGateEpoch: e.DataGateEpoch, Nonce: e.Nonce, GateState: e.GateState}
	if err := v.validate(true); err != nil {
		return err
	}
	return writePublicationFrame(w, v, MaxRuntimeHealthFrame-4)
}

func readHealthHeader(r io.Reader, header []byte, response bool) (runtimeHealthWire, error) {
	var v runtimeHealthWire
	if len(header) == 0 || len(header)+4 > MaxRuntimeHealthFrame || !utf8.Valid(header) {
		return v, ErrFrame
	}
	decoder := json.NewDecoder(bytes.NewReader(header))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&v); err != nil {
		return runtimeHealthWire{}, err
	}
	canonical, err := json.Marshal(v)
	if err != nil || !bytes.Equal(canonical, header) {
		return runtimeHealthWire{}, ErrFrame
	}
	if err := v.validate(response); err != nil {
		return runtimeHealthWire{}, err
	}
	if err := requirePublicationEOF(r); err != nil {
		return runtimeHealthWire{}, err
	}
	return v, nil
}

func readRuntimeHealthHeader(r io.Reader, header []byte) (RuntimeHealthEnvelope, error) {
	v, err := readHealthHeader(r, header, false)
	if err != nil {
		return RuntimeHealthEnvelope{}, err
	}
	return RuntimeHealthEnvelope{Version: v.Version, Purpose: v.Purpose, Identity: p.RuntimeIdentityContext(v.Identity), DataGateEpoch: v.DataGateEpoch, Nonce: v.Nonce}, nil
}

func ReadRuntimeHealthResponse(r io.Reader) (RuntimeHealthResponse, error) {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return RuntimeHealthResponse{}, err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > MaxRuntimeHealthFrame-4 {
		return RuntimeHealthResponse{}, ErrFrame
	}
	header := make([]byte, n)
	if _, err := io.ReadFull(r, header); err != nil {
		return RuntimeHealthResponse{}, err
	}
	v, err := readHealthHeader(r, header, true)
	if err != nil {
		return RuntimeHealthResponse{}, err
	}
	return RuntimeHealthResponse{Version: v.Version, Purpose: v.Purpose, Identity: p.RuntimeIdentityContext(v.Identity), DataGateEpoch: v.DataGateEpoch, Nonce: v.Nonce, GateState: v.GateState}, nil
}
