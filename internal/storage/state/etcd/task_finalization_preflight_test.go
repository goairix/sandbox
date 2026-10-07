package etcd

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// The absent client makes any accidentally dispatched KV call fail this host
// test. The evidence is real signature attribution, but no native capability.
func finalizationHostFixture(t *testing.T) (*Backend, *TaskClaim, *PreparedTaskQuiescence) {
	t.Helper()
	r := quiesceRecordFixture(t)
	var ticket struct {
		Claims p.TaskUserQuiescenceTicketClaims `json:"claims"`
	}
	require.NoError(t, json.Unmarshal(r.Ticket, &ticket))
	now := ticket.Claims.NotBefore.Add(time.Second)
	root := ed25519.NewKeyFromSeed(make([]byte, 32))
	rk := ed25519.NewKeyFromSeed([]byte("12345678901234567890123456789013"))
	cx := r.Context.Current
	v, err := p.NewManagementVerifier(p.TrustBinding{Namespace: cx.Namespace, AuthorityID: cx.AuthorityID, Target: cx.Target, RestoreEpoch: cx.RestoreEpoch}, []ed25519.PublicKey{root.Public().(ed25519.PublicKey)})
	require.NoError(t, err)
	cert, err := p.SignRuntimeIdentityCertificate(root, p.RuntimeIdentityCertificateClaims{Version: 1, Role: "runtime_receipt", CertificateID: uuid.NewString(), Namespace: cx.Namespace, AuthorityID: cx.AuthorityID, Target: cx.Target, RestoreEpoch: cx.RestoreEpoch, PublicKey: rk.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), SandboxID: cx.SandboxID, WorkspaceHash: cx.WorkspaceHash, Generation: cx.Generation, Runtime: cx.Runtime})
	require.NoError(t, err)
	wire, err := p.SignTaskUserQuiescenceReceipt(rk, p.TaskUserQuiescenceReceiptClaims{Version: 1, State: "users_quiesced", Context: r.Context, TicketDigest: r.TicketDigest, NotBefore: ticket.Claims.NotBefore, NotAfter: ticket.Claims.NotAfter, ExecutionSetDigest: strings.Repeat("a", 64)})
	require.NoError(t, err)
	receipt, err := v.VerifyTaskUserQuiescenceReceipt(wire, cert, r.Context, r.TicketDigest, ticket.Claims.NotBefore, ticket.Claims.NotAfter, now)
	require.NoError(t, err)
	b := &Backend{taskQuiescenceVerifier: v, authorityClock: testAuthorityClock(func(context.Context) (ClockObservation, error) { return ClockObservation{UTC: now}, nil })}
	c := &TaskClaim{origin: b, parentCtx: context.Background(), reference: r.Claim, deadline: time.Now().Add(time.Hour)}
	c.self = c
	prepared := &PreparedTaskQuiescence{origin: b, claim: c, claimReference: c.reference, deadline: c.deadline, receipt: receipt, emptyRevision: 50, pin: taskDualDrainPin{intent: taskFence{create: 50, mod: 50}, receipt: taskFence{create: 50, mod: 50}, reservation: taskQuiescenceReservationPin{revision: 49}}}
	prepared.self = prepared
	return b, c, prepared
}

func TestTaskFinalizationRejectsBeforeRPC(t *testing.T) {
	// Omitting any local-original/evidence validation would reach the nil client.
	for _, fault := range []string{"nil-context", "nil-backend", "nil-claim", "unsealed-claim", "nil-prepared", "empty-prepared", "copy", "backend", "claim", "reference", "revision", "intent-revision", "receipt-revision", "reservation-revision", "deadline", "receipt", "expired"} {
		t.Run(fault, func(t *testing.T) {
			b, c, prepared := finalizationHostFixture(t)
			ctx := context.Background()
			switch fault {
			case "nil-context":
				ctx = nil
			case "nil-backend":
				b = nil
			case "nil-claim":
				c = nil
			case "unsealed-claim":
				c.self = nil
			case "nil-prepared":
				prepared = nil
			case "empty-prepared":
				prepared = &PreparedTaskQuiescence{}
			case "copy":
				copy := *prepared
				prepared = &copy
			case "backend":
				prepared.origin = new(Backend)
			case "claim":
				prepared.claim = &TaskClaim{}
			case "reference":
				prepared.claimReference.CreateRevision++
			case "revision":
				prepared.emptyRevision = 0
			case "intent-revision":
				prepared.pin.intent.create = 0
			case "receipt-revision":
				prepared.pin.receipt.mod = 0
			case "reservation-revision":
				prepared.pin.reservation.revision = 0
			case "deadline":
				prepared.deadline = time.Time{}
			case "receipt":
				prepared.receipt = p.TaskUserQuiescenceReceiptEvidence{}
			case "expired":
				prepared.deadline = time.Now().Add(-time.Second)
			}
			require.Error(t, b.RevalidateTaskQuiescence(ctx, c, prepared, nil))
			if fault == "expired" {
				require.False(t, c.lost, "expired prerequisite must not poison live claim")
			}
		})
	}
}
