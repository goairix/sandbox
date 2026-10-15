package controlprotocol

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEnrolledManagementTLSCredentialLifetime(t *testing.T) {
	f := newActivationFixture(t)
	original := bytes.Clone(f.runtimeKey)
	c, err := NewManagementTLSCredential(f.runtimeKey, f.runtimeWire, "runtime_receipt")
	require.NoError(t, err)
	options := ManagementTLSOptions{Verifier: f.verifier, Clock: clockFunc(func(context.Context) (ClockObservation, error) {
		return ClockObservation{UTC: f.now, Uncertainty: time.Millisecond}, nil
	}), Credential: c, PeerCertificate: f.issuerWire, Server: true}
	config, err := NewManagementTLSConfig(options)
	require.NoError(t, err)
	signer := config.Certificates[0].PrivateKey.(crypto.Signer)
	input := append(bytes.Repeat([]byte{32}, 64), []byte("TLS 1.3, server CertificateVerify\x00")...)
	input = append(input, bytes.Repeat([]byte{1}, 32)...)
	signature, err := signer.Sign(nil, input, crypto.Hash(0))
	require.NoError(t, err)
	require.True(t, ed25519.Verify(original[32:], input, signature))
	var workers sync.WaitGroup
	for n := 0; n < 8; n++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := 0; i < 10; i++ {
				_, _ = signer.Sign(nil, input, crypto.Hash(0))
				_ = signer.Public()
				_, _ = NewManagementTLSConfig(options)
			}
		}()
	}
	require.NoError(t, c.Close())
	workers.Wait()
	require.NoError(t, c.Close())
	require.Equal(t, original, []byte(f.runtimeKey))
	require.Equal(t, make([]byte, ed25519.PrivateKeySize), []byte(c.key))
	_, err = signer.Sign(nil, input, crypto.Hash(0))
	require.Error(t, err)
	_, err = NewManagementTLSConfig(options)
	require.Error(t, err)
	var absent *ManagementTLSCredential
	require.NoError(t, absent.Close())
}
