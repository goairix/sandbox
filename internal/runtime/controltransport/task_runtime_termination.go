package controltransport

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

type NodeTerminationRequest struct {
	Version           uint32                  `json:"version"`
	Purpose           string                  `json:"purpose"`
	Ticket            []byte                  `json:"ticket,omitempty"`
	IssuerCertificate []byte                  `json:"issuer_certificate,omitempty"`
	Query             *p.NodeTerminationQuery `json:"query,omitempty"`
}

func EncodeNodeTerminationRequest(r NodeTerminationRequest) ([]byte, error) {
	if r.Version != 1 {
		return nil, fmt.Errorf("invalid node termination request version")
	}
	switch r.Purpose {
	case "task_runtime_terminate":
		if len(r.Ticket) == 0 || len(r.Ticket) > 8192 || len(r.IssuerCertificate) == 0 || len(r.IssuerCertificate) > 4096 || r.Query != nil {
			return nil, fmt.Errorf("invalid fixed node termination request")
		}
	case "task_runtime_terminate_query":
		if r.Query == nil || len(r.Ticket)+len(r.IssuerCertificate) != 0 {
			return nil, fmt.Errorf("invalid fixed node termination query request")
		}
		if _, err := p.EncodeNodeTerminationQuery(*r.Query); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("invalid node termination request purpose")
	}
	wire, err := json.Marshal(r)
	if err != nil || len(wire) > p.NodeEnrollmentFrameBytes {
		return nil, fmt.Errorf("node termination request exceeds frame bound")
	}
	return wire, nil
}

func DecodeNodeTerminationRequest(wire []byte) (NodeTerminationRequest, error) {
	var r NodeTerminationRequest
	if len(wire) == 0 || len(wire) > p.NodeEnrollmentFrameBytes || json.Unmarshal(wire, &r) != nil {
		return r, fmt.Errorf("invalid node termination request frame")
	}
	canonical, err := EncodeNodeTerminationRequest(r)
	if err != nil || !bytes.Equal(canonical, wire) {
		return NodeTerminationRequest{}, fmt.Errorf("noncanonical node termination request")
	}
	return r, nil
}

type TerminationDestinationOptions struct {
	Address             string
	Binding             p.TrustBinding
	Roots               []ed25519.PublicKey
	Clock               p.AuthorityClock
	Credential          *p.NodeObserverTLSCredential
	ObserverCertificate []byte
	NodeUID             string
	NodeBootID          string
	EnrollmentDigest    string
}

type TerminationDestination struct {
	mu                sync.Mutex
	wg                sync.WaitGroup
	ctx               context.Context
	cancel            context.CancelFunc
	address           string
	config            *tls.Config
	credential        *p.NodeObserverTLSCredential // borrowed; caller owns its lifetime
	verifier          *p.ManagementVerifier
	certificate       []byte
	certificateDigest string
	nodeUID           string
	nodeBootID        string
	enrollment        string
	connections       map[net.Conn]struct{}
	active            uint32
	closed            bool
}

func NewTerminationDestination(o TerminationDestinationOptions) (*TerminationDestination, error) {
	if _, _, err := net.SplitHostPort(o.Address); err != nil || o.Credential == nil || len(o.EnrollmentDigest) != 64 {
		return nil, fmt.Errorf("invalid fixed termination destination")
	}
	v, err := p.NewManagementVerifier(o.Binding, o.Roots)
	if err != nil {
		return nil, err
	}
	config, err := p.NewNodeObserverClientTLSConfig(v, o.Credential, o.Clock, o.ObserverCertificate, o.NodeUID, o.NodeBootID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	sum := sha256.Sum256(o.ObserverCertificate)
	return &TerminationDestination{ctx: ctx, cancel: cancel, address: o.Address, config: config, credential: o.Credential, verifier: v, certificate: bytes.Clone(o.ObserverCertificate), certificateDigest: hex.EncodeToString(sum[:]), nodeUID: o.NodeUID, nodeBootID: o.NodeBootID, enrollment: o.EnrollmentDigest, connections: make(map[net.Conn]struct{})}, nil
}

// These unauthenticated fields only narrow the immutable destination. The
// observer authenticates the entire ticket against its concrete native guard.
func (d *TerminationDestination) matchTicketPins(ticket []byte) error {
	var envelope struct {
		Claims p.TaskRuntimeTerminationTicketClaims `json:"claims"`
	}
	if len(ticket) == 0 || len(ticket) > 8192 || json.Unmarshal(ticket, &envelope) != nil {
		return fmt.Errorf("invalid bounded termination ticket")
	}
	c := envelope.Claims
	if c.EnrollmentDigest != d.enrollment || c.ObserverCertificateDigest != d.certificateDigest || c.NodeUID != d.nodeUID || c.NodeBootID != d.nodeBootID {
		return fmt.Errorf("termination destination pin mismatch")
	}
	return nil
}

func (d *TerminationDestination) Terminate(ctx context.Context, ticket, issuerCertificate []byte) (p.NodeTerminationResult, error) {
	if d == nil || !d.credential.MatchesCertificate(issuerCertificate) {
		return p.NodeTerminationResult{}, fmt.Errorf("termination TLS issuer pin mismatch")
	}
	if err := d.matchTicketPins(ticket); err != nil {
		return p.NodeTerminationResult{}, err
	}
	return d.call(ctx, NodeTerminationRequest{Version: 1, Purpose: "task_runtime_terminate", Ticket: bytes.Clone(ticket), IssuerCertificate: bytes.Clone(issuerCertificate)})
}

func (d *TerminationDestination) Query(ctx context.Context, query p.NodeTerminationQuery) (p.NodeTerminationResult, error) {
	if d == nil || query.EnrollmentDigest != d.enrollment {
		return p.NodeTerminationResult{}, fmt.Errorf("termination query enrollment pin mismatch")
	}
	return d.call(ctx, NodeTerminationRequest{Version: 1, Purpose: "task_runtime_terminate_query", Query: &query})
}

func (d *TerminationDestination) call(ctx context.Context, request NodeTerminationRequest) (p.NodeTerminationResult, error) {
	if ctx == nil || d == nil || d.ctx == nil || d.config == nil || d.cancel == nil {
		return p.NodeTerminationResult{}, fmt.Errorf("missing termination context")
	}
	wire, err := EncodeNodeTerminationRequest(request)
	if err != nil {
		return p.NodeTerminationResult{}, err
	}
	d.mu.Lock()
	if d.closed || d.active >= 4 {
		d.mu.Unlock()
		return p.NodeTerminationResult{}, fmt.Errorf("termination destination closed or busy")
	}
	d.active++
	d.wg.Add(1)
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.active--
		d.mu.Unlock()
		d.wg.Done()
	}()
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	joined := make(chan struct{})
	stop := context.AfterFunc(d.ctx, func() { defer close(joined); cancel() })
	defer func() {
		if !stop() {
			<-joined
		}
	}()
	raw, err := (&net.Dialer{}).DialContext(bounded, "tcp", d.address)
	if err != nil {
		return p.NodeTerminationResult{}, err
	}
	defer raw.Close()
	d.mu.Lock()
	d.connections[raw] = struct{}{}
	closed := d.closed
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.connections, raw)
		d.mu.Unlock()
	}()
	if closed {
		return p.NodeTerminationResult{}, fmt.Errorf("termination destination closed")
	}
	ioJoined := make(chan struct{})
	stopIO := context.AfterFunc(bounded, func() { defer close(ioJoined); _ = raw.Close() })
	defer func() {
		if !stopIO() {
			<-ioJoined
		}
	}()
	deadline, _ := bounded.Deadline()
	if err := raw.SetDeadline(deadline); err != nil {
		return p.NodeTerminationResult{}, err
	}
	conn := tls.Client(raw, d.config)
	if err := conn.HandshakeContext(bounded); err != nil {
		return p.NodeTerminationResult{}, err
	}
	if err := WriteNodeObserverFrame(conn, wire); err != nil {
		return p.NodeTerminationResult{}, err
	}
	reply, err := ReadNodeObserverFrame(conn)
	if err != nil {
		return p.NodeTerminationResult{}, err
	}
	result, err := p.DecodeNodeTerminationResult(reply)
	if err != nil {
		return result, err
	}
	if result.State == "terminal" {
		if !bytes.Equal(result.ObserverCertificate, d.certificate) {
			return p.NodeTerminationResult{}, fmt.Errorf("terminal proof observer certificate pin mismatch")
		}
		query := request.Query
		if query == nil {
			var envelope struct {
				Claims p.TaskRuntimeTerminationTicketClaims `json:"claims"`
			}
			if json.Unmarshal(request.Ticket, &envelope) != nil {
				return p.NodeTerminationResult{}, fmt.Errorf("invalid original termination ticket")
			}
			c := envelope.Claims
			query = &p.NodeTerminationQuery{Version: 1, Purpose: "task_runtime_terminate_query", Partition: c.Partition, TaskID: c.Current.TaskID, CommandID: c.Current.CommandID, IntentCreateRevision: c.IntentCreateRevision, IntentDigest: c.IntentDigest, EnrollmentDigest: c.EnrollmentDigest}
		}
		if query.EnrollmentDigest != d.enrollment {
			return p.NodeTerminationResult{}, fmt.Errorf("terminal proof enrollment pin mismatch")
		}
		if _, err := d.verifier.VerifyNodeTerminationProof(result.Proof, result.ObserverCertificate, *query); err != nil {
			return p.NodeTerminationResult{}, err
		}
	}
	return result, bounded.Err()
}

func (d *TerminationDestination) Close() error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	d.closed = true
	if d.cancel != nil {
		d.cancel()
	}
	for conn := range d.connections {
		_ = conn.Close()
	}
	d.mu.Unlock()
	d.wg.Wait()
	return nil
}
