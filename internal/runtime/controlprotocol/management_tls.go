package controlprotocol

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"time"
)

const managementALPN = "sandbox-control/v1"
const maxManagementTLSLeafBytes = 8192

// NewManagementTLSConfig authenticates fixed complementary roles and exact
// delegate possession. It does not authorize delivery or any execution effect.
// Every actual connection obtains fresh controlled time and re-verifies both
// custom certificates; local wall time is never an authentication fallback.
func NewManagementTLSConfig(o ManagementTLSOptions) (*tls.Config, error) {
	if o.Verifier == nil || len(o.Verifier.roots) == 0 || nilDependency(o.Clock) || o.Credential == nil {
		return nil, fmt.Errorf("management TLS requires pinned trust, clock and credential")
	}
	c := o.Credential
	expectedRole := "command_issuer"
	if o.Server {
		expectedRole = "runtime_receipt"
	}
	if c.role != expectedRole || len(c.key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("management TLS local role mismatch")
	}
	peer := append([]byte(nil), o.PeerCertificate...)
	var peerRuntime RuntimeIdentityContext
	if o.Server {
		if o.PeerRuntime != nil {
			return nil, fmt.Errorf("server peer must be command issuer")
		}
		var e commandIssuerEnvelope
		if err := decodeWire(peer, commandIssuerSchema, &e); err != nil {
			return nil, err
		}
		if err := validateCommandIssuerCertificate(e.Claims); err != nil {
			return nil, err
		}
	} else {
		if o.PeerRuntime == nil {
			return nil, fmt.Errorf("missing trusted peer runtime")
		}
		peerRuntime = *o.PeerRuntime
		if err := validateRuntimeIdentityContext(peerRuntime); err != nil {
			return nil, err
		}
		var e runtimeIdentityEnvelope
		if err := decodeWire(peer, runtimeIdentitySchema, &e); err != nil {
			return nil, err
		}
		if err := validateRuntimeIdentityCertificate(e.Claims); err != nil {
			return nil, err
		}
	}
	leaf, err := managementTLSLeaf(c)
	if err != nil {
		return nil, err
	}
	config := &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, NextProtos: []string{managementALPN}, Certificates: []tls.Certificate{leaf}, SessionTicketsDisabled: true}
	if o.Server {
		config.ClientAuth = tls.RequireAnyClientCert
	} else {
		// Complete independent pin/role/signature/time checks in VerifyConnection
		// replace CA/hostname verification. No trust is selected by peer wire.
		config.InsecureSkipVerify = true
	}
	verifier, clock, server := o.Verifier, o.Clock, o.Server
	config.VerifyConnection = func(state tls.ConnectionState) error {
		if state.Version != tls.VersionTLS13 || state.NegotiatedProtocol != managementALPN || state.DidResume {
			return fmt.Errorf("management TLS protocol or resumption mismatch")
		}
		if len(state.PeerCertificates) != 1 || len(state.PeerCertificates[0].Raw) == 0 || len(state.PeerCertificates[0].Raw) > maxManagementTLSLeafBytes {
			return fmt.Errorf("management TLS requires one bounded leaf")
		}
		observation, err := observeAuthorityClock(context.Background(), clock)
		if err != nil {
			return err
		}
		now := observation.UTC
		var localKey, peerKey ed25519.PublicKey
		var peerBefore, peerAfter time.Time
		if server {
			local, err := verifier.VerifyRuntimeIdentityCertificate(c.certificate, c.runtime, now)
			if err != nil {
				return err
			}
			localKey = local.publicKey
			remote, err := verifier.VerifyCommandIssuerCertificate(peer, now)
			if err != nil {
				return err
			}
			peerKey = remote.publicKey
			peerBefore = remote.notBefore
			peerAfter = remote.notAfter
		} else {
			local, err := verifier.VerifyCommandIssuerCertificate(c.certificate, now)
			if err != nil {
				return err
			}
			localKey = local.publicKey
			remote, err := verifier.VerifyRuntimeIdentityCertificate(peer, peerRuntime, now)
			if err != nil {
				return err
			}
			peerKey = remote.publicKey
			peerBefore = remote.notBefore
			peerAfter = remote.notAfter
		}
		if !bytes.Equal(localKey, c.key[ed25519.SeedSize:]) || bytes.Equal(localKey, peerKey) {
			return fmt.Errorf("management TLS delegates must match and be independent")
		}
		return verifyManagementTLSLeaf(state.PeerCertificates[0], peerKey, server, peerBefore, peerAfter, now)
	}
	return config, nil
}

func managementTLSLeaf(c *ManagementTLSCredential) (tls.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	serial.Add(serial, big.NewInt(1))
	notBefore := c.notBefore.Truncate(time.Second)
	if notBefore.Before(c.notBefore) {
		notBefore = notBefore.Add(time.Second)
	}
	notAfter := c.notAfter.Truncate(time.Second)
	if !notBefore.Before(notAfter) {
		return tls.Certificate{}, fmt.Errorf("TLS leaf interval too short")
	}
	eku := x509.ExtKeyUsageClientAuth
	role := "client"
	if c.role == "runtime_receipt" {
		eku = x509.ExtKeyUsageServerAuth
		role = "server"
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: c.role}, NotBefore: notBefore, NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{eku}, BasicConstraintsValid: true, SignatureAlgorithm: x509.PureEd25519}
	// Leaf creation uses the private key before installing the restricted signer.
	der, err := x509.CreateCertificate(rand.Reader, template, template, c.key.Public(), c.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	if len(der) > maxManagementTLSLeafBytes {
		return tls.Certificate{}, fmt.Errorf("TLS leaf exceeds limit")
	}
	signer := &tlsOnlySigner{key: append(ed25519.PrivateKey(nil), c.key...), context: "TLS 1.3, " + role + " CertificateVerify\x00"}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: signer}, nil
}
func verifyManagementTLSLeaf(leaf *x509.Certificate, key ed25519.PublicKey, peerIsClient bool, notBefore, notAfter, now time.Time) error {
	public, ok := leaf.PublicKey.(ed25519.PublicKey)
	eku := x509.ExtKeyUsageServerAuth
	if peerIsClient {
		eku = x509.ExtKeyUsageClientAuth
	}
	if !ok || !bytes.Equal(public, key) || leaf.PublicKeyAlgorithm != x509.Ed25519 || leaf.SignatureAlgorithm != x509.PureEd25519 || leaf.IsCA || !leaf.BasicConstraintsValid || leaf.KeyUsage != x509.KeyUsageDigitalSignature || len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != eku || len(leaf.UnknownExtKeyUsage) != 0 || len(leaf.UnhandledCriticalExtensions) != 0 || !bytes.Equal(leaf.RawSubject, leaf.RawIssuer) {
		return fmt.Errorf("management TLS leaf key or role invalid")
	}
	if leaf.NotBefore.Before(notBefore) || leaf.NotAfter.After(notAfter) || now.Add(-time.Second).Before(leaf.NotBefore) || !now.Add(time.Second).Before(leaf.NotAfter) {
		return fmt.Errorf("management TLS leaf outside authenticated interval")
	}
	if err := leaf.CheckSignature(x509.PureEd25519, leaf.RawTBSCertificate, leaf.Signature); err != nil {
		return fmt.Errorf("management TLS leaf not self-signed: %w", err)
	}
	return nil
}
