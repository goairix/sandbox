package controlprotocol

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"fmt"
	"io"
	"math/big"
	"sync"
	"time"
)

const nodeObserverALPN = "sandbox-node-observer/v1"

var nodeObserverCertificateOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 2}

// NodeObserverTLSCredential is restricted to the two observer transport roles.
// Its signer accepts only the exact role's TLS1.3 CertificateVerify domain.
// Close clears its sole copied key after the caller has joined all connections.
type NodeObserverTLSCredential struct {
	mu                  sync.RWMutex
	key                 ed25519.PrivateKey
	certificate         []byte
	server              bool
	notBefore, notAfter time.Time
}

func NewNodeObserverTLSCredential(key ed25519.PrivateKey, certificate []byte, server bool) (*NodeObserverTLSCredential, error) {
	private, err := copyPrivateKey(key)
	if err != nil {
		return nil, err
	}
	c := &NodeObserverTLSCredential{key: private, certificate: bytes.Clone(certificate), server: server}
	var public []byte
	if server {
		var e nodeObserverCertificateEnvelope
		if err = decodeWire(c.certificate, nodeObserverCertificateSchema, &e); err == nil {
			err = validateNodeObserverCertificate(e.Claims)
		}
		public, c.notBefore, c.notAfter = e.Claims.PublicKey, e.Claims.NotBefore, e.Claims.NotAfter
	} else {
		var e commandIssuerEnvelope
		if err = decodeWire(c.certificate, commandIssuerSchema, &e); err == nil {
			err = validateCommandIssuerCertificate(e.Claims)
		}
		public, c.notBefore, c.notAfter = e.Claims.PublicKey, e.Claims.NotBefore, e.Claims.NotAfter
	}
	if err != nil || !bytes.Equal(public, private[ed25519.SeedSize:]) {
		clear(private)
		return nil, fmt.Errorf("invalid observer TLS credential role/key: %v", err)
	}
	return c, nil
}

func (c *NodeObserverTLSCredential) Public() crypto.PublicKey {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.key) != ed25519.PrivateKeySize {
		return ed25519.PublicKey(nil)
	}
	return ed25519.PublicKey(bytes.Clone(c.key[ed25519.SeedSize:]))
}

func (c *NodeObserverTLSCredential) Sign(_ io.Reader, input []byte, opts crypto.SignerOpts) ([]byte, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	role := "client"
	if c.server {
		role = "server"
	}
	prefix := append(bytes.Repeat([]byte(" "), 64), []byte("TLS 1.3, "+role+" CertificateVerify\x00")...)
	if len(c.key) != ed25519.PrivateKeySize || nilDependency(opts) || opts.HashFunc() != crypto.Hash(0) || (len(input) != len(prefix)+32 && len(input) != len(prefix)+48) || !bytes.HasPrefix(input, prefix) {
		return nil, fmt.Errorf("observer TLS signer rejects non-CertificateVerify input")
	}
	if o, ok := opts.(*ed25519.Options); ok && o.Context != "" {
		return nil, fmt.Errorf("observer TLS signer rejects context option")
	}
	return ed25519.Sign(c.key, input), nil
}

func (c *NodeObserverTLSCredential) Close() error {
	if c != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		clear(c.key)
		c.key = nil
	}
	return nil
}

func (c *NodeObserverTLSCredential) leaf() (tls.Certificate, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.key) != ed25519.PrivateKeySize {
		return tls.Certificate{}, fmt.Errorf("closed observer TLS credential")
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	serial.Add(serial, big.NewInt(1))
	before := c.notBefore.Truncate(time.Second)
	if before.Before(c.notBefore) {
		before = before.Add(time.Second)
	}
	after := c.notAfter.Truncate(time.Second)
	if !before.Before(after) {
		return tls.Certificate{}, fmt.Errorf("observer TLS interval too short")
	}
	role, eku := "command_issuer", x509.ExtKeyUsageClientAuth
	if c.server {
		role, eku = "node_observer", x509.ExtKeyUsageServerAuth
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: role}, NotBefore: before, NotAfter: after, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{eku}, BasicConstraintsValid: true, SignatureAlgorithm: x509.PureEd25519, ExtraExtensions: []pkix.Extension{{Id: nodeObserverCertificateOID, Value: bytes.Clone(c.certificate)}}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, c.key.Public(), c.key)
	if err != nil || len(der) > maxManagementTLSLeafBytes {
		return tls.Certificate{}, fmt.Errorf("observer TLS leaf invalid: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: c}, nil
}

// NodeObserverTLSPeerCertificate only extracts the fixed custom certificate.
// Callers must first complete a handshake with the observer TLS config below.
func NodeObserverTLSPeerCertificate(state tls.ConnectionState) ([]byte, error) {
	if len(state.PeerCertificates) != 1 || len(state.PeerCertificates[0].Raw) > maxManagementTLSLeafBytes {
		return nil, fmt.Errorf("observer TLS requires one bounded leaf")
	}
	var wire []byte
	for _, extension := range state.PeerCertificates[0].Extensions {
		if extension.Id.Equal(nodeObserverCertificateOID) {
			if wire != nil || extension.Critical || len(extension.Value) == 0 || len(extension.Value) > maxManagementCertificateWireBytes {
				return nil, fmt.Errorf("invalid observer TLS certificate extension")
			}
			wire = bytes.Clone(extension.Value)
		}
	}
	if len(wire) == 0 {
		return nil, fmt.Errorf("missing observer TLS role certificate")
	}
	return wire, nil
}

func NewNodeObserverServerTLSConfig(v *ManagementVerifier, credential *NodeObserverTLSCredential, clock AuthorityClock, nodeUID, nodeBootID string) (*tls.Config, error) {
	return nodeObserverTLSConfig(v, credential, clock, nil, nodeUID, nodeBootID, true)
}

func NewNodeObserverClientTLSConfig(v *ManagementVerifier, credential *NodeObserverTLSCredential, clock AuthorityClock, observerCertificate []byte, nodeUID, nodeBootID string) (*tls.Config, error) {
	return nodeObserverTLSConfig(v, credential, clock, bytes.Clone(observerCertificate), nodeUID, nodeBootID, false)
}

func nodeObserverTLSConfig(v *ManagementVerifier, c *NodeObserverTLSCredential, clock AuthorityClock, observerWire []byte, nodeUID, nodeBootID string, server bool) (*tls.Config, error) {
	if v == nil || len(v.roots) == 0 || nilDependency(clock) || c == nil || c.server != server || !validUUID(nodeUID) || !validUUID(nodeBootID) {
		return nil, fmt.Errorf("invalid observer TLS configuration")
	}
	leaf, err := c.leaf()
	if err != nil {
		return nil, err
	}
	if !server && len(observerWire) == 0 {
		return nil, fmt.Errorf("observer TLS requires pinned observer certificate")
	}
	config := &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, NextProtos: []string{nodeObserverALPN}, Certificates: []tls.Certificate{leaf}, SessionTicketsDisabled: true}
	if server {
		config.ClientAuth = tls.RequireAnyClientCert
	} else {
		// Fixed root-certified role, identity and delegate possession checks below
		// replace CA/hostname selection. The peer cannot select the roots.
		config.InsecureSkipVerify = true
	}
	config.VerifyConnection = func(state tls.ConnectionState) error {
		if state.Version != tls.VersionTLS13 || state.NegotiatedProtocol != nodeObserverALPN || state.DidResume {
			return fmt.Errorf("observer TLS protocol/resumption mismatch")
		}
		peer, err := NodeObserverTLSPeerCertificate(state)
		if err != nil {
			return err
		}
		observation, err := observeAuthorityClock(context.Background(), clock)
		if err != nil {
			return err
		}
		var localKey, peerKey ed25519.PublicKey
		var peerBefore, peerAfter time.Time
		role := "node_observer"
		if server {
			local, err := v.VerifyNodeObserverCertificate(c.certificate, nodeUID, nodeBootID, observation.UTC)
			if err != nil {
				return err
			}
			remote, err := v.VerifyCommandIssuerCertificate(peer, observation.UTC)
			if err != nil {
				return err
			}
			localKey, peerKey = local.PublicKey(), remote.PublicKey()
			peerBefore, peerAfter, role = remote.NotBefore(), remote.NotAfter(), "command_issuer"
		} else {
			if !bytes.Equal(observerWire, peer) {
				return fmt.Errorf("observer TLS certificate pin mismatch")
			}
			local, err := v.VerifyCommandIssuerCertificate(c.certificate, observation.UTC)
			if err != nil {
				return err
			}
			remote, err := v.VerifyNodeObserverCertificate(peer, nodeUID, nodeBootID, observation.UTC)
			if err != nil {
				return err
			}
			localKey, peerKey = local.PublicKey(), remote.PublicKey()
			peerBefore, peerAfter = remote.NotBefore(), remote.NotAfter()
		}
		if !bytes.Equal(localKey, c.Public().(ed25519.PublicKey)) || bytes.Equal(localKey, peerKey) || state.PeerCertificates[0].Subject.CommonName != role {
			return fmt.Errorf("observer TLS delegate/role mismatch")
		}
		return verifyManagementTLSLeaf(state.PeerCertificates[0], peerKey, server, peerBefore, peerAfter, observation.UTC)
	}
	return config, nil
}
