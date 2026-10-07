package controlprotocol

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"
)

type SignedClockClientOptions struct {
	Binding   TrustBinding
	Audience  string
	PublicKey ed25519.PublicKey
	// Dial must honor context and transfer exclusive ownership of the connection.
	Dial func(context.Context) (net.Conn, error)
}
type SignedClockServerOptions struct {
	Binding        TrustBinding
	Key            ed25519.PrivateKey
	Source         AuthorityClock
	MaxConnections uint32
}

// SignedClockClient authenticates a separately operator-pinned dedicated clock
// key. Its use does not demonstrate correctness of the operator's time source.
type SignedClockClient struct{ options SignedClockClientOptions }

func NewSignedClockClient(o SignedClockClientOptions) (*SignedClockClient, error) {
	if err := validateBinding(o.Binding); err != nil {
		return nil, err
	}
	if !validID(o.Audience) || len(o.PublicKey) != ed25519.PublicKeySize || o.Dial == nil {
		return nil, fmt.Errorf("invalid signed clock client options")
	}
	o.PublicKey = append(ed25519.PublicKey(nil), o.PublicKey...)
	return &SignedClockClient{options: o}, nil
}
func (c *SignedClockClient) Observe(ctx context.Context) (ClockObservation, error) {
	if c == nil || nilDependency(ctx) || c.options.Dial == nil {
		return ClockObservation{}, fmt.Errorf("unconfigured clock client or context")
	}
	if err := ctx.Err(); err != nil {
		return ClockObservation{}, err
	}
	start := time.Now()
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	nonce, err := uuid.NewRandom()
	if err != nil {
		return ClockObservation{}, err
	}
	request := clockRequest{Version: 1, Purpose: clockObservationPurpose, Binding: trustBindingWire(c.options.Binding), Audience: c.options.Audience, Nonce: nonce.String()}
	wire, err := encodeWire(request, maxClockWireBytes)
	if err != nil {
		return ClockObservation{}, err
	}
	conn, err := c.options.Dial(bounded)
	if !nilDependency(conn) {
		defer conn.Close()
	}
	if err != nil {
		return ClockObservation{}, err
	}
	if nilDependency(conn) {
		return ClockObservation{}, fmt.Errorf("nil clock connection")
	}
	stop := closeOnContext(bounded, conn)
	defer stop()
	deadline, _ := bounded.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return ClockObservation{}, err
	}
	if err := bounded.Err(); err != nil {
		return ClockObservation{}, err
	}
	if err := writeClockFrame(conn, wire); err != nil {
		return ClockObservation{}, err
	}
	response, err := readClockFrame(conn)
	if err != nil {
		return ClockObservation{}, err
	}
	o, err := verifyClockResponse(response, request, c.options.PublicKey)
	if err != nil {
		return ClockObservation{}, err
	}
	if err := bounded.Err(); err != nil {
		return ClockObservation{}, err
	}
	return accountClockElapsed(o, time.Since(start))
}

// closeOnContext joins its cancellation callback before releasing connection
// ownership. No worker survives completion of an active call.
func closeOnContext(ctx context.Context, conn net.Conn) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(done); conn.Close() })
	return func() {
		if !stop() {
			<-done
		}
	}
}

// ServeSignedClock takes exclusive listener ownership, including on validation
// error, and closes/joins all accepted connections on stop or return. Source is
// borrowed and never closed; it must honor context. No source calls occur idle.
// Key custody and a correctly synchronized Source are operator responsibilities.
func ServeSignedClock(ctx context.Context, listener net.Listener, o SignedClockServerOptions) error {
	if nilDependency(listener) {
		return fmt.Errorf("nil clock listener")
	}
	defer listener.Close()
	if nilDependency(ctx) || nilDependency(o.Source) {
		return fmt.Errorf("missing clock context or source")
	}
	if err := validateBinding(o.Binding); err != nil {
		return err
	}
	key, err := copyPrivateKey(o.Key)
	if err != nil {
		return err
	}
	o.Key = key
	if o.MaxConnections == 0 {
		o.MaxConnections = 16
	}
	if o.MaxConnections > 16 {
		return fmt.Errorf("clock connection limit outside 1..16")
	}
	active, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	connections := make(map[net.Conn]struct{})
	var workers sync.WaitGroup
	stopped := make(chan struct{})
	stop := context.AfterFunc(active, func() {
		defer close(stopped)
		listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for conn := range connections {
			conn.Close()
		}
	})
	defer func() {
		cancel()
		if !stop() {
			<-stopped
		}
		mu.Lock()
		for conn := range connections {
			conn.Close()
		}
		mu.Unlock()
		workers.Wait()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if active.Err() != nil {
				return nil
			}
			return err
		}
		if nilDependency(conn) {
			return fmt.Errorf("listener returned nil connection")
		}
		mu.Lock()
		if active.Err() != nil || len(connections) >= int(o.MaxConnections) {
			mu.Unlock()
			conn.Close()
			continue
		}
		connections[conn] = struct{}{}
		workers.Add(1)
		mu.Unlock()
		go func() {
			defer workers.Done()
			defer func() { conn.Close(); mu.Lock(); delete(connections, conn); mu.Unlock() }()
			serveClockConnection(active, conn, o)
		}()
	}
}
func serveClockConnection(ctx context.Context, conn net.Conn, o SignedClockServerOptions) {
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	deadline, _ := bounded.Deadline()
	if conn.SetDeadline(deadline) != nil {
		return
	}
	wire, err := readClockFrame(conn)
	if err != nil {
		return
	}
	var request clockRequest
	if decodeWire(wire, clockRequestSchema, &request) != nil || validateClockRequest(request, o.Binding) != nil {
		return
	}
	observation, err := observeAuthorityClock(bounded, o.Source)
	if err != nil {
		return
	}
	claims := clockResponseClaims{clockRequest: request, UTC: observation.UTC, Uncertainty: observation.Uncertainty}
	signed, err := signingBytes(clockObservationDomain, claims)
	if err != nil {
		return
	}
	response, err := encodeWire(clockResponse{Claims: claims, Signature: ed25519.Sign(o.Key, signed)}, maxClockWireBytes)
	if err != nil {
		return
	}
	if bounded.Err() != nil {
		return
	}
	_ = writeClockFrame(conn, response)
}
