package controlprotocol

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type observerTestClock struct{ now time.Time }

func (c observerTestClock) Observe(ctx context.Context) (ClockObservation, error) {
	return ClockObservation{UTC: c.now, Uncertainty: time.Millisecond}, ctx.Err()
}

func TestNodeObserverProtocolTLS(t *testing.T) {
	f, key, cert, wire := nodeObserverCertificateFixture(t)
	server, err := NewNodeObserverTLSCredential(key, wire, true)
	require.NoError(t, err)
	defer server.Close()
	client, err := NewNodeObserverTLSCredential(f.issuerKey, f.issuerWire, false)
	require.NoError(t, err)
	defer client.Close()
	clock := observerTestClock{f.now}
	st, err := NewNodeObserverServerTLSConfig(f.verifier, server, clock, cert.NodeUID, cert.NodeBootID)
	require.NoError(t, err)
	ct, err := NewNodeObserverClientTLSConfig(f.verifier, client, clock, wire, cert.NodeUID, cert.NodeBootID)
	require.NoError(t, err)
	x, y := net.Pipe()
	s, c := tls.Server(x, st), tls.Client(y, ct)
	defer x.Close()
	defer y.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.HandshakeContext(ctx) }()
	require.NoError(t, c.HandshakeContext(ctx))
	require.NoError(t, <-done)
	peer, err := NodeObserverTLSPeerCertificate(s.ConnectionState())
	require.NoError(t, err)
	require.Equal(t, f.issuerWire, peer)
	peer, err = NodeObserverTLSPeerCertificate(c.ConnectionState())
	require.NoError(t, err)
	require.Equal(t, wire, peer)
	require.NoError(t, server.Close())
	require.NotEmpty(t, key)
	_, err = NewNodeObserverServerTLSConfig(f.verifier, server, clock, cert.NodeUID, cert.NodeBootID)
	require.Error(t, err)
	_, err = NewNodeObserverTLSCredential(key, f.runtimeWire, true)
	require.Error(t, err)
}
