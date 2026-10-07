package etcd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	transport "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// This fixture exercises only receipt reconciliation, never original metadata
// admission or delivery authority. Real etcd fences belong to the native gate.
func receiptAttributionFixture(t *testing.T) (*ExecDeliveryHandle, func(string, time.Time) []byte) {
	t.Helper()
	root, rk, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, ik, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	runtimePublic, runtimeKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	now := time.Now().UTC()
	binding := p.TrustBinding{Namespace: "/sandbox/control/authority/", AuthorityID: "authority", Target: "docker", RestoreEpoch: "epoch"}
	verifier, err := p.NewManagementVerifier(binding, []ed25519.PublicKey{root})
	require.NoError(t, err)
	issuerWire, err := p.SignCommandIssuerCertificate(rk, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: ik.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Minute)})
	require.NoError(t, err)
	issuer, err := verifier.VerifyCommandIssuerCertificate(issuerWire, now)
	require.NoError(t, err)
	c := p.ExecStartContext{Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, IssuerCertificateID: issuer.CertificateID(), IssuerCertificateDigest: issuer.Digest(), CommandID: uuid.NewString(), OperationID: uuid.NewString(), RequestID: "request", OperationDigest: strings.Repeat("a", 64), SandboxID: "sandbox", WorkspaceHash: strings.Repeat("b", 64), Generation: 1, DataGateEpoch: 1, ControlRevision: 1, AdmissionRevision: 2, LeaseID: 3, Runtime: p.RuntimeReference{ID: "runtime", UID: "actual-uid", BootID: uuid.NewString()}, ExpiresAt: now.Add(time.Minute)}
	descriptor, err := p.NewExecutionDescriptor(p.ExecutionRequest{Argv: []string{"/bin/true"}, Env: map[string]string{}, UID: 1, GID: 1, WorkDir: "/", TimeoutSeconds: 10})
	require.NoError(t, err)
	claims := p.ExecStartTicketClaims{Version: 1, Purpose: "operation_exec_start", Context: c, DescriptorDigest: descriptor.Digest(), NotBefore: now.Add(-2 * time.Second), NotAfter: now.Add(20 * time.Second)}
	ticketWire, err := p.SignExecStartTicket(ik, issuer, claims)
	require.NoError(t, err)
	ticket, err := verifier.VerifyExecStartTicket(ticketWire, issuerWire, c, descriptor, now)
	require.NoError(t, err)
	identity := p.RuntimeIdentityContext{SandboxID: c.SandboxID, WorkspaceHash: c.WorkspaceHash, Generation: c.Generation, Runtime: c.Runtime}
	runtimeWire, err := p.SignRuntimeIdentityCertificate(rk, p.RuntimeIdentityCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "runtime_receipt", Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch, PublicKey: runtimePublic, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Minute), SandboxID: c.SandboxID, WorkspaceHash: c.WorkspaceHash, Generation: c.Generation, Runtime: c.Runtime})
	require.NoError(t, err)
	credential, err := p.NewManagementTLSCredential(ik, issuerWire, "command_issuer")
	require.NoError(t, err)
	clock := testAuthorityClock(func(context.Context) (ClockObservation, error) { return ClockObservation{UTC: now}, nil })
	destination, err := transport.NewDestination(transport.DestinationOptions{Identity: identity, RuntimeCertificate: runtimeWire, Credential: credential, Verifier: verifier, Clock: clock, Dial: func(context.Context) (net.Conn, error) { return nil, ErrInvalidRecord }})
	require.NoError(t, err)
	publication, err := p.NewPublicationVerifier(binding, []ed25519.PublicKey{root})
	require.NoError(t, err)
	b := &Backend{execVerifier: verifier, publicationVerifier: publication, authorityClock: clock, requestTimeout: time.Second}
	draft := &execEffectDraft{descriptor: descriptor, claims: claims, ticket: ticket}
	capability := &OperationCapability{origin: b, parentCtx: context.Background(), execDraft: draft}
	h := &ExecDeliveryHandle{origin: b, capability: capability, draft: draft, destination: destination, accepted: true, deadline: claims.NotAfter}
	h.self = h
	draft.delivery = h
	sign := func(state string, deadline time.Time) []byte {
		claims := p.LocalExecReceiptClaims{Version: 1, State: state, Context: c, DescriptorDigest: descriptor.Digest(), TicketDigest: ticket.Digest(), NotBefore: claims.NotBefore, NotAfter: claims.NotAfter, AuthorityDeadline: deadline}
		if state == "local_terminal" {
			claims.RootPID = 23
			claims.DrainConfirmed = true
			claims.Reason = "completed"
		}
		if state == "unknown" {
			claims.Reason = "transport_lost"
		}
		wire, err := p.SignLocalExecReceipt(runtimeKey, claims)
		require.NoError(t, err)
		return wire
	}
	return h, sign
}
func TestDeliveryPendingRenewalRequiresCurrentOwnerConfirmation(t *testing.T) {
	h, sign := receiptAttributionFixture(t)
	old := h.deadline
	pending := old.Add(4 * time.Second)
	h.pending = pending
	// Query arrives before an already-issued delayed renewal reaches the target.
	_, err := h.observeReceipt(context.Background(), transport.EventAccepted, sign("accepted", old))
	require.NoError(t, err)
	require.Equal(t, pending, h.pending)
	require.Equal(t, old, h.deadline)
	// Even durable pending history cannot stand in for monitor ACK/live-owner proof.
	_, err = h.observeReceipt(context.Background(), transport.EventReceipt, sign("accepted", pending))
	require.NoError(t, err)
	require.Equal(t, pending, h.pending)
	require.Equal(t, old, h.deadline)
	// Only the exact independently retained pending deadline plus current-owner
	// event can reconcile after the delayed original renewal actually completes.
	_, err = h.observeReceipt(context.Background(), transport.EventAccepted, sign("accepted", pending))
	require.NoError(t, err)
	require.True(t, h.pending.IsZero())
	require.Equal(t, pending, h.deadline)
}
func TestDeliveryPendingRefusesFurtherRenewAndArbitraryDeadline(t *testing.T) {
	h, sign := receiptAttributionFixture(t)
	h.pending = h.deadline.Add(3 * time.Second)
	require.ErrorIs(t, h.origin.RenewExecDelivery(context.Background(), h), ErrConflict)
	_, err := h.observeReceipt(context.Background(), transport.EventAccepted, sign("accepted", h.pending.Add(time.Second)))
	require.ErrorIs(t, err, ErrDeliveryUnknown)
	require.False(t, h.pending.IsZero())
	for _, state := range []string{"local_terminal", "unknown"} {
		t.Run(state, func(t *testing.T) {
			handle, sign := receiptAttributionFixture(t)
			handle.pending = handle.deadline.Add(3 * time.Second)
			_, err := handle.observeReceipt(context.Background(), transport.EventReceipt, sign(state, handle.pending))
			require.NoError(t, err)
			require.False(t, handle.accepted)
			require.ErrorIs(t, handle.origin.RenewExecDelivery(context.Background(), handle), ErrConflict)
		})
	}
}
func TestDeliveryCopiedHandleAndCloseRemainCleanupOnly(t *testing.T) {
	h, _ := receiptAttributionFixture(t)
	var copy ExecDeliveryHandle
	reflect.ValueOf(&copy).Elem().Set(reflect.ValueOf(h).Elem())
	require.False(t, copy.valid())
	require.ErrorIs(t, copy.Close(), ErrInvalidRecord)
	require.NoError(t, h.Close())
	require.True(t, h.closed)
	require.False(t, h.terminal)
	require.ErrorIs(t, h.origin.RenewExecDelivery(context.Background(), h), ErrConflict)
}
