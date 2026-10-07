package controlprotocol

import (
	"crypto/ed25519"
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

const clockObservationDomain = "sandbox-authority-clock-observation:v1\x00"
const clockObservationPurpose = "authority_clock_observation"
const maxClockWireBytes = 4096

type clockRequest struct {
	Version  uint32           `json:"version"`
	Purpose  string           `json:"purpose"`
	Binding  trustBindingWire `json:"binding"`
	Audience string           `json:"audience"`
	Nonce    string           `json:"nonce"`
}
type clockResponseClaims struct {
	clockRequest
	UTC         time.Time     `json:"utc"`
	Uncertainty time.Duration `json:"uncertainty"`
}
type clockResponse struct {
	Claims    clockResponseClaims `json:"claims"`
	Signature []byte              `json:"signature"`
}

var clockRequestSchema = &wireSchema{kind: 'o', maxBytes: maxClockWireBytes, fields: map[string]*wireSchema{"version": numberField, "purpose": stringField, "binding": trustBindingSchema, "audience": stringField, "nonce": stringField}}
var clockResponseSchema = &wireSchema{kind: 'o', maxBytes: maxClockWireBytes, fields: map[string]*wireSchema{"claims": objectSchema(map[string]*wireSchema{"version": numberField, "purpose": stringField, "binding": trustBindingSchema, "audience": stringField, "nonce": stringField, "utc": utcField, "uncertainty": numberField}), "signature": stringField}}

func validateClockRequest(r clockRequest, binding TrustBinding) error {
	if r.Version != 1 || r.Purpose != clockObservationPurpose || TrustBinding(r.Binding) != binding || !validID(r.Audience) || !validUUID(r.Nonce) {
		return fmt.Errorf("invalid clock challenge")
	}
	return nil
}
func readClockFrame(r io.Reader) ([]byte, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(prefix[:])
	if n == 0 || n > maxClockWireBytes {
		return nil, fmt.Errorf("invalid clock frame length")
	}
	wire := make([]byte, int(n))
	if _, err := io.ReadFull(r, wire); err != nil {
		return nil, err
	}
	return wire, nil
}
func writeClockFrame(w io.Writer, wire []byte) error {
	if len(wire) == 0 || len(wire) > maxClockWireBytes {
		return fmt.Errorf("invalid clock frame size")
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(wire)))
	for _, b := range [][]byte{prefix[:], wire} {
		for len(b) > 0 {
			n, err := w.Write(b)
			if err != nil {
				return err
			}
			if n <= 0 || n > len(b) {
				return io.ErrShortWrite
			}
			b = b[n:]
		}
	}
	return nil
}
func verifyClockResponse(wire []byte, request clockRequest, key ed25519.PublicKey) (ClockObservation, error) {
	var r clockResponse
	if err := decodeWire(wire, clockResponseSchema, &r); err != nil {
		return ClockObservation{}, err
	}
	if r.Claims.clockRequest != request || len(r.Signature) != ed25519.SignatureSize {
		return ClockObservation{}, fmt.Errorf("clock response does not match challenge")
	}
	signed, err := signingBytes(clockObservationDomain, r.Claims)
	if err != nil {
		return ClockObservation{}, err
	}
	if !ed25519.Verify(key, signed, r.Signature) {
		return ClockObservation{}, fmt.Errorf("invalid clock signature")
	}
	return accountClockElapsed(ClockObservation{UTC: r.Claims.UTC, Uncertainty: r.Claims.Uncertainty}, 0)
}
