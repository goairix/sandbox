package controlprotocol

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"fmt"
	"io"
	"time"
)

// ManagementTLSCredential owns a copied delegate key and custom certificate.
// It has no key getter and grants no operation or activation authority.
type ManagementTLSCredential struct {
	key                 ed25519.PrivateKey
	certificate         []byte
	role                string
	runtime             RuntimeIdentityContext
	notBefore, notAfter time.Time
}

type ManagementTLSOptions struct {
	Verifier        *ManagementVerifier
	Clock           AuthorityClock
	Credential      *ManagementTLSCredential
	PeerCertificate []byte
	// Client-only exact runtime tuple from original trusted state. The server's
	// selected issuer certificate must come from protected root activation.
	PeerRuntime *RuntimeIdentityContext
	Server      bool
}

// NewManagementTLSCredential proves key agreement and validates the custom
// certificate's shape. Root trust and freshness are checked at each handshake.
func NewManagementTLSCredential(key ed25519.PrivateKey, certificate []byte, role string) (*ManagementTLSCredential, error) {
	private, err := copyPrivateKey(key)
	if err != nil {
		return nil, err
	}
	c := &ManagementTLSCredential{key: private, certificate: append([]byte(nil), certificate...), role: role}
	var public []byte
	switch role {
	case "command_issuer":
		var e commandIssuerEnvelope
		if err := decodeWire(c.certificate, commandIssuerSchema, &e); err != nil {
			return nil, err
		}
		if err := validateCommandIssuerCertificate(e.Claims); err != nil {
			return nil, err
		}
		if len(e.Signature) != ed25519.SignatureSize {
			return nil, fmt.Errorf("invalid issuer signature length")
		}
		public = e.Claims.PublicKey
		c.notBefore = e.Claims.NotBefore
		c.notAfter = e.Claims.NotAfter
	case "runtime_receipt":
		var e runtimeIdentityEnvelope
		if err := decodeWire(c.certificate, runtimeIdentitySchema, &e); err != nil {
			return nil, err
		}
		if err := validateRuntimeIdentityCertificate(e.Claims); err != nil {
			return nil, err
		}
		if len(e.Signature) != ed25519.SignatureSize {
			return nil, fmt.Errorf("invalid runtime signature length")
		}
		public = e.Claims.PublicKey
		c.notBefore = e.Claims.NotBefore
		c.notAfter = e.Claims.NotAfter
		c.runtime = runtimeIdentityContext(e.Claims)
	default:
		return nil, fmt.Errorf("invalid management TLS role")
	}
	if !bytes.Equal(public, private[ed25519.SeedSize:]) {
		return nil, fmt.Errorf("management TLS key does not match custom certificate")
	}
	return c, nil
}

// tlsOnlySigner exposes possession proof solely in the role-specific TLS 1.3
// CertificateVerify domain. Neither key bytes nor general signing are exposed.
type tlsOnlySigner struct {
	key     ed25519.PrivateKey
	context string
}

func (s *tlsOnlySigner) Public() crypto.PublicKey {
	return append(ed25519.PublicKey(nil), s.key[ed25519.SeedSize:]...)
}
func (s *tlsOnlySigner) Sign(_ io.Reader, input []byte, opts crypto.SignerOpts) ([]byte, error) {
	if nilDependency(opts) || opts.HashFunc() != crypto.Hash(0) {
		return nil, fmt.Errorf("TLS signer requires direct Ed25519")
	}
	prefix := 64 + len(s.context)
	n := len(input) - prefix
	if (n != 32 && n != 48) || !bytes.Equal(input[:64], bytes.Repeat([]byte(" "), 64)) || string(input[64:prefix]) != s.context {
		return nil, fmt.Errorf("TLS signer rejects non-CertificateVerify input")
	}
	// ed25519.Options can request Ed25519ctx while still returning Hash(0).
	if o, ok := opts.(*ed25519.Options); ok && o.Context != "" {
		return nil, fmt.Errorf("TLS signer rejects Ed25519 context options")
	}
	return ed25519.Sign(s.key, input), nil
}
