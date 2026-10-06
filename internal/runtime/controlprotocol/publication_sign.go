package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const (
	certificateDomain = "sandbox-runtime-certificate:v1\x00"
	readyDomain       = "sandbox-runtime-ready:v1\x00"
)

func wireDigest(wire []byte) string { sum := sha256.Sum256(wire); return hex.EncodeToString(sum[:]) }

// SignRuntimeCertificate is for a future trusted issuer that certifies the
// immutable snapshot's workspace mode and supplies an independent delegate key.
// The root key ID is always derived from the validated issuer private key.
func SignRuntimeCertificate(key ed25519.PrivateKey, claims RuntimeCertificateClaims) ([]byte, error) {
	private, err := copyPrivateKey(key)
	if err != nil {
		return nil, err
	}
	claims.PublicKey = append([]byte(nil), claims.PublicKey...)
	claims.RootKeyID = wireDigest(private[ed25519.SeedSize:])
	if err := validateCertificate(claims); err != nil {
		return nil, err
	}
	signed, err := signingBytes(certificateDomain, claims)
	if err != nil {
		return nil, err
	}
	return encodeWire(certificateEnvelope{Claims: claims, Signature: ed25519.Sign(private, signed)}, maxCertificateWireBytes)
}

// SignReadyReceipt binds a delegate's statement to the canonical certificate.
// It does not authenticate an authority root; the pinned verifier does that.
// A trusted producer must observe the real durable gate and mount first.
func SignReadyReceipt(key ed25519.PrivateKey, certificate []byte, claims ReadyReceiptClaims) ([]byte, error) {
	private, err := copyPrivateKey(key)
	if err != nil {
		return nil, err
	}
	var envelope certificateEnvelope
	if err := decodeWire(certificate, certificateSchema, &envelope); err != nil {
		return nil, err
	}
	if err := validateCertificate(envelope.Claims); err != nil {
		return nil, err
	}
	if len(envelope.Signature) != ed25519.SignatureSize {
		return nil, fmt.Errorf("invalid certificate signature length")
	}
	if !bytes.Equal(private[ed25519.SeedSize:], envelope.Claims.PublicKey) {
		return nil, fmt.Errorf("delegate key does not match certificate")
	}
	normalized, err := encodeWire(envelope, maxCertificateWireBytes)
	if err != nil {
		return nil, err
	}
	if err := validateReady(claims, envelope.Claims); err != nil {
		return nil, err
	}
	if claims.CertificateDigest != wireDigest(normalized) {
		return nil, fmt.Errorf("ready certificate digest mismatch")
	}
	signed, err := signingBytes(readyDomain, claims)
	if err != nil {
		return nil, err
	}
	return encodeWire(readyEnvelope{Certificate: envelope, Claims: claims, Signature: ed25519.Sign(private, signed)}, maxReadyWireBytes)
}

func copyPrivateKey(key ed25519.PrivateKey) (ed25519.PrivateKey, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid publication private key length")
	}
	private := append(ed25519.PrivateKey(nil), key...)
	derived := ed25519.NewKeyFromSeed(private[:ed25519.SeedSize])
	if !bytes.Equal(private, derived) {
		return nil, fmt.Errorf("publication private key has inconsistent public suffix")
	}
	return private, nil
}

func signingBytes(domain string, claims any) ([]byte, error) {
	wire, err := json.Marshal(claims)
	if err != nil {
		return nil, fmt.Errorf("encode publication claims: %w", err)
	}
	return append([]byte(domain), wire...), nil
}
