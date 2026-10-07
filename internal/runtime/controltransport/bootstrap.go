package controltransport

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
)

const MaxBootstrap = 16384

// BootstrapRequest conveys only closed Hello or independently Root-signed
// activation. The owner chooses whether bootstrap is still permitted.
type BootstrapRequest struct {
	Version            uint32 `json:"version"`
	Purpose            string `json:"purpose"`
	Activation         []byte `json:"activation"`
	RuntimeCertificate []byte `json:"runtime_certificate"`
	IssuerCertificate  []byte `json:"issuer_certificate"`
}
type Birth struct {
	BootID           string `json:"boot_id"`
	RuntimePublicKey []byte `json:"runtime_public_key"`
	UID              uint32 `json:"uid"`
	GID              uint32 `json:"gid"`
	NetworkAllowed   bool   `json:"network_allowed"`
	ContractDigest   string `json:"contract_digest"`
}
type BootstrapResponse struct {
	Version uint32 `json:"version"`
	Purpose string `json:"purpose"`
	Birth   *Birth `json:"birth"`
}

func WriteBootstrap(w io.Writer, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(b) == 0 || len(b)+4 > MaxBootstrap {
		return ErrFrame
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(b)))
	if err = writeExact(w, size[:]); err != nil {
		return err
	}
	return writeExact(w, b)
}
func ReadBootstrap(r io.Reader, value any) error {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > MaxBootstrap-4 {
		return ErrFrame
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	canonical, err := json.Marshal(value)
	if err != nil || !bytes.Equal(canonical, b) {
		return ErrFrame
	}
	var extra [1]byte
	count, err := r.Read(extra[:])
	if count != 0 || err != io.EOF {
		return ErrFrame
	}
	return nil
}
