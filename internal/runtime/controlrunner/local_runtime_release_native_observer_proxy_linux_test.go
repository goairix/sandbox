//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/stretchr/testify/require"
)

func TestLocalRuntimeReleaseNativeObserverACK(t *testing.T) {
	runLocalRuntimeReleaseNative(t, "observer-send-ack")
}

// The proxy has the same fixture-owned TLS roles on both connections. It only
// forwards actual bounded frames; no proof/result is produced by this fixture.
type localReleaseObserverProxy struct {
	ctx                                          context.Context
	listener                                     net.Listener
	upstream                                     string
	serverTLS, clientTLS                         *tls.Config
	verifier                                     *p.ManagementVerifier
	observerCertificate, issuerCertificate       []byte
	mu                                           sync.Mutex
	connections                                  [2]net.Conn
	closed                                       bool
	terminate, query, terminalResponses, dropped int
	drop                                         bool
	done                                         chan error
}

func newLocalReleaseObserverProxy(t *testing.T, ctx context.Context, s localReleaseNativeSetup, observerKey ed25519.PrivateKey) (string, *localReleaseObserverProxy) {
	t.Helper()
	v, err := p.NewManagementVerifier(s.Binding, []ed25519.PublicKey{s.Root})
	require.NoError(t, err)
	serverCredential, err := p.NewNodeObserverTLSCredential(observerKey, s.ObserverCertificate, true)
	require.NoError(t, err)
	clientCredential, err := p.NewNodeObserverTLSCredential(s.IssuerKey, s.IssuerCertificate, false)
	require.NoError(t, err)
	serverTLS, err := p.NewNodeObserverServerTLSConfig(v, serverCredential, nativeFixtureClock{}, s.NodeUID, s.NodeBootID)
	require.NoError(t, err)
	clientTLS, err := p.NewNodeObserverClientTLSConfig(v, clientCredential, nativeFixtureClock{}, s.ObserverCertificate, s.NodeUID, s.NodeBootID)
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	proxy := &localReleaseObserverProxy{ctx: ctx, listener: listener, upstream: s.ObserverAddress, serverTLS: serverTLS, clientTLS: clientTLS, verifier: v, observerCertificate: bytes.Clone(s.ObserverCertificate), issuerCertificate: bytes.Clone(s.IssuerCertificate), drop: true, done: make(chan error, 1)}
	go func() { proxy.done <- proxy.serve() }()
	t.Cleanup(func() {
		proxy.mu.Lock()
		proxy.closed = true
		connections := proxy.connections
		proxy.mu.Unlock()
		require.NoError(t, listener.Close())
		for _, conn := range connections {
			if conn != nil {
				_ = conn.Close()
			}
		}
		require.NoError(t, <-proxy.done)
		require.NoError(t, serverCredential.Close())
		require.NoError(t, clientCredential.Close())
		t.Log("original observer proxy accept/forward IO joined before owned TLS credentials closed")
	})
	return listener.Addr().String(), proxy
}

func (p *localReleaseObserverProxy) serve() error {
	for {
		conn, err := p.listener.Accept()
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := p.forward(conn); err != nil {
			p.mu.Lock()
			closed := p.closed
			p.mu.Unlock()
			if closed {
				return nil
			}
			return err
		}
	}
}

func (p *localReleaseObserverProxy) track(index int, conn net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.connections[index] = conn
	return true
}

func (proxy *localReleaseObserverProxy) forward(raw net.Conn) error {
	defer raw.Close()
	if !proxy.track(0, raw) {
		return net.ErrClosed
	}
	defer func() { proxy.mu.Lock(); proxy.connections = [2]net.Conn{}; proxy.mu.Unlock() }()
	deadline := time.Now().Add(30 * time.Second)
	if err := raw.SetDeadline(deadline); err != nil {
		return err
	}
	downstream := tls.Server(raw, proxy.serverTLS)
	if err := downstream.HandshakeContext(proxy.ctx); err != nil {
		return err
	}
	peer, err := p.NodeObserverTLSPeerCertificate(downstream.ConnectionState())
	if err != nil || !bytes.Equal(peer, proxy.issuerCertificate) {
		return fmt.Errorf("observer proxy issuer mismatch: %w", err)
	}
	wire, err := ct.ReadNodeObserverFrame(downstream)
	if err != nil {
		return err
	}
	request, err := ct.DecodeNodeTerminationRequest(wire)
	if err != nil {
		return err
	}
	upRaw, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(proxy.ctx, "tcp", proxy.upstream)
	if err != nil {
		return err
	}
	defer upRaw.Close()
	if !proxy.track(1, upRaw) {
		return net.ErrClosed
	}
	if err := upRaw.SetDeadline(deadline); err != nil {
		return err
	}
	upstream := tls.Client(upRaw, proxy.clientTLS)
	if err := upstream.HandshakeContext(proxy.ctx); err != nil {
		return err
	}
	if err := ct.WriteNodeObserverFrame(upstream, wire); err != nil {
		return err
	}
	proxy.mu.Lock()
	if request.Purpose == "task_runtime_terminate" {
		proxy.terminate++
	} else {
		proxy.query++
	}
	proxy.mu.Unlock()
	reply, err := ct.ReadNodeObserverFrame(upstream)
	if err != nil {
		return err
	}
	result, err := p.DecodeNodeTerminationResult(reply)
	if err != nil {
		return err
	}
	if result.State != "terminal" || !bytes.Equal(result.ObserverCertificate, proxy.observerCertificate) {
		return fmt.Errorf("actual observer did not produce pinned terminal proof")
	}
	query := request.Query
	if query == nil {
		var ticket struct {
			Claims p.TaskRuntimeTerminationTicketClaims `json:"claims"`
		}
		if err := json.Unmarshal(request.Ticket, &ticket); err != nil {
			return err
		}
		c := ticket.Claims
		query = &p.NodeTerminationQuery{Version: 1, Purpose: "task_runtime_terminate_query", Partition: c.Partition, TaskID: c.Current.TaskID, CommandID: c.Current.CommandID, IntentCreateRevision: c.IntentCreateRevision, IntentDigest: c.IntentDigest, EnrollmentDigest: c.EnrollmentDigest}
	}
	if _, err := proxy.verifier.VerifyNodeTerminationProof(result.Proof, result.ObserverCertificate, *query); err != nil {
		return err
	}
	proxy.mu.Lock()
	proxy.terminalResponses++
	drop := proxy.drop && request.Purpose == "task_runtime_terminate" && proxy.dropped == 0
	if drop {
		proxy.dropped++
	}
	proxy.mu.Unlock()
	if drop {
		return nil
	} // original downstream closes only after real proof verification
	return ct.WriteNodeObserverFrame(downstream, reply)
}

func (proxy *localReleaseObserverProxy) verify(t *testing.T) {
	t.Helper()
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	require.Equal(t, 1, proxy.terminate, "all actual forwarded Terminate frames, including retries")
	require.Equal(t, 1, proxy.query)
	require.Equal(t, 2, proxy.terminalResponses)
	require.Equal(t, 1, proxy.dropped)
	t.Logf("actual observer TLS frames Terminate=%d Query=%d independently verified real terminal responses=%d dropped Terminate response=%d", proxy.terminate, proxy.query, proxy.terminalResponses, proxy.dropped)
}
