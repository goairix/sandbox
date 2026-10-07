package etcd

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	tr "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"net"
	"strings"
	"testing"
	"time"
)

// This exercises genuine pinned mTLS, finite EOF, and backend-owned independent
// verification. Protocol-created receipts here are not PID1/namespace proof.
func TestTaskQuiesceTransportBackendAuthenticatesResponse(t *testing.T) {
	for _, kind := range []byte{tr.EventAccepted, tr.EventReceipt, tr.EventError} {
		t.Run(string([]byte{kind}), func(t *testing.T) {
			r := quiesceRecordFixture(t)
			var ticket struct {
				Claims p.TaskUserQuiescenceTicketClaims `json:"claims"`
			}
			require.NoError(t, json.Unmarshal(r.Ticket, &ticket))
			now := ticket.Claims.NotBefore.Add(3 * time.Second)
			root := ed25519.NewKeyFromSeed(make([]byte, 32))
			ik := ed25519.NewKeyFromSeed([]byte("12345678901234567890123456789012"))
			rk := ed25519.NewKeyFromSeed([]byte("12345678901234567890123456789013"))
			c := r.Context
			binding := p.TrustBinding{Namespace: c.Current.Namespace, AuthorityID: c.Current.AuthorityID, Target: c.Current.Target, RestoreEpoch: c.Current.RestoreEpoch}
			v, err := p.NewManagementVerifier(binding, []ed25519.PublicKey{root.Public().(ed25519.PublicKey)})
			require.NoError(t, err)
			iw, err := p.SignCommandIssuerCertificate(root, p.CommandIssuerCertificateClaims{Version: 1, Role: "command_issuer", CertificateID: uuid.NewString(), Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: ik.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)})
			require.NoError(t, err)
			rw, err := p.SignRuntimeIdentityCertificate(root, p.RuntimeIdentityCertificateClaims{Version: 1, Role: "runtime_receipt", CertificateID: uuid.NewString(), Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: rk.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), SandboxID: c.Current.SandboxID, WorkspaceHash: c.Current.WorkspaceHash, Generation: c.Current.Generation, Runtime: c.Current.Runtime})
			require.NoError(t, err)
			ic, err := p.NewManagementTLSCredential(ik, iw, "command_issuer")
			require.NoError(t, err)
			rc, err := p.NewManagementTLSCredential(rk, rw, "runtime_receipt")
			require.NoError(t, err)
			clock := &taskDeliveryClock{}
			clock.now.Store(now.UnixNano())
			cfg, err := p.NewManagementTLSConfig(p.ManagementTLSOptions{Verifier: v, Clock: clock, Credential: rc, PeerCertificate: iw, Server: true})
			require.NoError(t, err)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer listener.Close()
			d, err := tr.NewDestination(tr.DestinationOptions{Identity: p.RuntimeIdentityContext{SandboxID: c.Current.SandboxID, WorkspaceHash: c.Current.WorkspaceHash, Generation: c.Current.Generation, Runtime: c.Current.Runtime}, RuntimeCertificate: rw, Credential: ic, Verifier: v, Clock: clock, Dial: func(ctx context.Context) (net.Conn, error) {
				var dial net.Dialer
				return dial.DialContext(ctx, "tcp", listener.Addr().String())
			}})
			require.NoError(t, err)
			wire := []byte("unknown")
			if kind == tr.EventAccepted {
				wire, err = p.SignTaskUserQuiescenceAccepted(rk, p.TaskUserQuiescenceAcceptedClaims{Version: 1, State: "quiescence_accepted", Context: c, TicketDigest: r.TicketDigest, NotBefore: ticket.Claims.NotBefore, NotAfter: ticket.Claims.NotAfter})
			} else if kind == tr.EventReceipt {
				wire, err = p.SignTaskUserQuiescenceReceipt(rk, p.TaskUserQuiescenceReceiptClaims{Version: 1, State: "users_quiesced", Context: c, TicketDigest: r.TicketDigest, NotBefore: ticket.Claims.NotBefore, NotAfter: ticket.Claims.NotAfter, ExecutionSetDigest: strings.Repeat("a", 64)})
			}
			require.NoError(t, err)
			done := make(chan error, 1)
			go func() {
				raw, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer raw.Close()
				raw.SetDeadline(time.Now().Add(2 * time.Second))
				conn := tls.Server(raw, cfg)
				if err = conn.Handshake(); err == nil {
					_, err = tr.ReadRuntimeRequest(conn)
				}
				if err == nil {
					err = tr.WriteEvent(conn, kind, wire)
				}
				done <- err
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			s, err := d.Open(ctx)
			require.NoError(t, err)
			defer s.Close()
			require.NoError(t, s.SendTaskQuiesce(ctx, tr.TaskQuiesceEnvelope{Version: 1, Purpose: "task_quiesce_users_query", Context: c, TicketDigest: r.TicketDigest}))
			publication, err := p.NewPublicationVerifier(binding, []ed25519.PublicKey{root.Public().(ed25519.PublicKey)})
			require.NoError(t, err)
			b := &Backend{taskQuiescenceVerifier: v, publicationVerifier: publication, requestTimeout: time.Second, authorityClock: testAuthorityClock(func(context.Context) (ClockObservation, error) { return ClockObservation{UTC: now}, nil })}
			ref := TaskQuiescenceReference{Task: r.Task.Reference, CommandID: c.Current.CommandID}
			got, err := b.readTaskQuiescenceResult(ctx, s, d, ref, c, r.TicketDigest, ticket.Claims.NotBefore, ticket.Claims.NotAfter)
			if kind == tr.EventError {
				require.ErrorIs(t, err, ErrDeliveryUnknown)
				require.Nil(t, got.Accepted)
				require.Nil(t, got.Receipt)
			} else {
				require.NoError(t, err)
				if kind == tr.EventAccepted {
					require.NotNil(t, got.Accepted)
					require.Nil(t, got.Receipt)
				} else {
					require.NotNil(t, got.Receipt)
					require.Nil(t, got.Accepted)
				}
			}
			require.Equal(t, ref, got.Reference)
			require.NoError(t, <-done)
		})
	}
}
