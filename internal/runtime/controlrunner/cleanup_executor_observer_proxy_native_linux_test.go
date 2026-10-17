//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
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

type cleanupExecutorObserverStats struct {
	Terminate, Query, TerminalReplies int
	Withheld, PeerClosed, Joined      int
	Original                          p.NodeTerminationQuery
	ProofDigest                       [32]byte
	ConnectionLifetime                time.Duration
	Error                             string
}

// This fixture forwards the original bounded frames over two authenticated
// TLS connections. Only the first real terminal reply is withheld; this fixture
// does not create/sign a termination proof, manufacture an ACK, or cancel the
// worker context. Every proof comes from the actual upstream observer.
type cleanupExecutorObserverProxy struct {
	listener            net.Listener
	upstream            string
	server, client      *tls.Config
	credentials         [2]*p.NodeObserverTLSCredential
	verifier            *p.ManagementVerifier
	certificate, issuer []byte
	ctx                 context.Context
	cancel              context.CancelFunc
	served              chan struct{}
	handlers            sync.WaitGroup
	closeOnce           sync.Once
	stop                func() bool
	stopped             chan struct{}

	mu          sync.Mutex
	closed      bool
	accepted    int
	connections map[net.Conn]struct{}
	stats       cleanupExecutorObserverStats
}

func newCleanupExecutorObserverProxy(t *testing.T, ctx context.Context, s cleanupExecutorSetup, observerKey ed25519.PrivateKey) *cleanupExecutorObserverProxy {
	t.Helper()
	v, err := p.NewManagementVerifier(s.Binding, []ed25519.PublicKey{s.Root})
	require.NoError(t, err)
	serverCredential, err := p.NewNodeObserverTLSCredential(observerKey, s.ObserverCertificate, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, serverCredential.Close()) })
	clientCredential, err := p.NewNodeObserverTLSCredential(s.IssuerKey, s.IssuerCertificate, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, clientCredential.Close()) })
	server, err := p.NewNodeObserverServerTLSConfig(v, serverCredential, nativeFixtureClock{}, s.NodeUID, s.NodeBootID)
	require.NoError(t, err)
	client, err := p.NewNodeObserverClientTLSConfig(v, clientCredential, nativeFixtureClock{}, s.ObserverCertificate, s.NodeUID, s.NodeBootID)
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	owned, cancel := context.WithCancel(ctx)
	f := &cleanupExecutorObserverProxy{listener: listener, upstream: s.ObserverAddress, server: server, client: client, credentials: [2]*p.NodeObserverTLSCredential{serverCredential, clientCredential}, verifier: v, certificate: bytes.Clone(s.ObserverCertificate), issuer: bytes.Clone(s.IssuerCertificate), ctx: owned, cancel: cancel, served: make(chan struct{}), stopped: make(chan struct{}), connections: make(map[net.Conn]struct{})}
	f.stop = context.AfterFunc(owned, func() {
		defer close(f.stopped)
		f.shutdown()
	})
	go f.serve()
	t.Cleanup(func() { f.close(t) })
	return f
}

func (f *cleanupExecutorObserverProxy) snapshot() cleanupExecutorObserverStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats
}

func (f *cleanupExecutorObserverProxy) shutdown() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	_ = f.listener.Close()
	for connection := range f.connections {
		_ = connection.Close()
	}
}

// Stop accepting before waiting for handlers, so Wait cannot race a future
// Add. Both raw sockets are registered and closed even after a Fatal assertion;
// credentials are released only after every real handshake/IO has returned.
func (f *cleanupExecutorObserverProxy) close(t *testing.T) {
	t.Helper()
	f.closeOnce.Do(func() {
		f.cancel()
		if !f.stop() {
			<-f.stopped
		}
		f.shutdown()
		<-f.served
		f.handlers.Wait()
		for _, credential := range f.credentials {
			require.NoError(t, credential.Close())
		}
		t.Logf("cleanup observer TLS proxy actual Close joined: finalstats=%+v", f.snapshot())
	})
}

func (f *cleanupExecutorObserverProxy) serve() {
	defer close(f.served)
	for {
		raw, err := f.listener.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		// The case needs exactly one Terminate and one recovery Query. A small
		// fixed ceiling makes an accidental retry storm a bounded test failure.
		if f.closed || f.accepted >= 4 || len(f.connections) >= 4 {
			f.mu.Unlock()
			_ = raw.Close()
			continue
		}
		f.accepted++
		f.connections[raw] = struct{}{}
		f.handlers.Add(1)
		f.mu.Unlock()
		go func() {
			defer f.handlers.Done()
			defer f.remove(raw)
			if err := f.forward(raw); err != nil {
				f.mu.Lock()
				if f.stats.Error == "" && !f.closed {
					f.stats.Error = fmt.Sprintf("%.512s", err.Error())
				}
				f.mu.Unlock()
			}
		}()
	}
}

func (f *cleanupExecutorObserverProxy) remove(raw net.Conn) {
	_ = raw.Close()
	f.mu.Lock()
	delete(f.connections, raw)
	f.mu.Unlock()
}

func (f *cleanupExecutorObserverProxy) forward(raw net.Conn) error {
	started := time.Now()
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	if err := raw.SetDeadline(deadline); err != nil {
		return err
	}
	down := tls.Server(raw, f.server)
	if err := down.HandshakeContext(ctx); err != nil {
		return err
	}
	peer, err := p.NodeObserverTLSPeerCertificate(down.ConnectionState())
	if err != nil || !bytes.Equal(peer, f.issuer) {
		return fmt.Errorf("observer proxy issuer pin: %w", err)
	}
	wire, err := ct.ReadNodeObserverFrame(down)
	if err != nil {
		return err
	}
	request, err := ct.DecodeNodeTerminationRequest(wire)
	if err != nil {
		return err
	}
	query := request.Query
	terminate := request.Purpose == "task_runtime_terminate"
	if terminate {
		if !bytes.Equal(request.IssuerCertificate, peer) {
			return fmt.Errorf("observer proxy request/peer mismatch")
		}
		var ticket struct {
			Claims p.TaskRuntimeTerminationTicketClaims `json:"claims"`
		}
		if err := json.Unmarshal(request.Ticket, &ticket); err != nil {
			return err
		}
		c := ticket.Claims
		query = &p.NodeTerminationQuery{Version: 1, Purpose: "task_runtime_terminate_query", Partition: c.Partition, TaskID: c.Current.TaskID, CommandID: c.Current.CommandID, IntentCreateRevision: c.IntentCreateRevision, IntentDigest: c.IntentDigest, EnrollmentDigest: c.EnrollmentDigest}
	}
	f.mu.Lock()
	if terminate {
		f.stats.Terminate++
	} else {
		f.stats.Query++
	}
	f.mu.Unlock()

	upRaw, err := (&net.Dialer{}).DialContext(ctx, "tcp", f.upstream)
	if err != nil {
		return err
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		_ = upRaw.Close()
		return context.Canceled
	}
	f.connections[upRaw] = struct{}{}
	f.mu.Unlock()
	defer f.remove(upRaw)
	if err := upRaw.SetDeadline(deadline); err != nil {
		return err
	}
	up := tls.Client(upRaw, f.client)
	if err := up.HandshakeContext(ctx); err != nil {
		return err
	}
	if err := ct.WriteNodeObserverFrame(up, wire); err != nil {
		return err
	}
	reply, err := ct.ReadNodeObserverFrame(up)
	if err != nil {
		return err
	}
	result, err := p.DecodeNodeTerminationResult(reply)
	if err != nil || result.State != "terminal" || len(result.Proof) == 0 || !bytes.Equal(result.ObserverCertificate, f.certificate) {
		return errors.Join(fmt.Errorf("observer proxy requires actual terminal reply"), err)
	}
	if _, err := f.verifier.VerifyNodeTerminationProof(result.Proof, result.ObserverCertificate, *query); err != nil {
		return err
	}
	digest := sha256.Sum256(result.Proof)
	f.mu.Lock()
	f.stats.TerminalReplies++
	drop := terminate && f.stats.Withheld == 0
	if drop {
		f.stats.Withheld++
		f.stats.Original, f.stats.ProofDigest = *query, digest
	}
	original, originalProof := f.stats.Original, f.stats.ProofDigest
	f.mu.Unlock()
	if !drop {
		if !terminate && (*query != original || digest != originalProof) {
			return fmt.Errorf("observer Query changed original reservation/proof")
		}
		return ct.WriteNodeObserverFrame(down, reply)
	}

	// No reply and no caller cancellation. The real Destination's five-second
	// deadline closes the socket; our ten-second ceiling only detects a leak.
	var extra [1]byte
	n, readErr := down.Read(extra[:])
	var timeout net.Error
	if n != 0 || readErr == nil || errors.As(readErr, &timeout) && timeout.Timeout() {
		return fmt.Errorf("observer withheld ACK did not observe client close: n=%d err=%v", n, readErr)
	}
	f.remove(upRaw)
	f.remove(raw)
	f.mu.Lock()
	f.stats.PeerClosed++
	f.stats.ConnectionLifetime = time.Since(started)
	f.stats.Joined++
	f.mu.Unlock()
	return nil
}
