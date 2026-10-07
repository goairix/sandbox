package etcd

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	j "github.com/goairix/sandbox/internal/runtime/controltarget"
	tr "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func newTaskDeliveryFixture(t *testing.T, expired bool) *taskCloseFixture {
	t.Helper()
	// Construct the original published identity with the live target's UUID
	// birth contract; do not rewrite any already-frozen destroy/claim evidence.
	ownedFixtureContainers(t)
	base, raw, input, creation, certificate, publicationRoot := preparationFixture(t, "plain")
	var original struct {
		Claims p.RuntimeCertificateClaims `json:"claims"`
	}
	require.NoError(t, json.Unmarshal(certificate, &original))
	original.Claims.Runtime.BootID = uuid.NewString()
	certificate, e := p.SignRuntimeCertificate(publicationRoot, original.Claims)
	require.NoError(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	bound, e := base.BindRuntime(ctx, creation, certificate)
	require.NoError(t, e)
	require.Equal(t, OutcomeCommitted, bound.Outcome)
	mounted, e := base.ConsumeRuntimeMount(ctx, creation)
	require.NoError(t, e)
	require.Equal(t, OutcomeCommitted, mounted.Outcome)
	published, e := base.PublishRuntime(ctx, creation, publicationProof(t, base, creation, certificate, nil))
	require.NoError(t, e)
	require.Equal(t, OutcomeCommitted, published.Outcome)
	controlKey, _, e := base.namespace.sandboxKeys(creation.workspace.Partition(), input.SandboxID, "read")
	require.NoError(t, e)
	got, e := raw.Get(ctx, controlKey)
	require.NoError(t, e)
	require.Len(t, got.Kvs, 1)
	in := BeginDestroyInput{SandboxID: input.SandboxID, RequestID: "destroy-request", ExpectedControlRevision: got.Kvs[0].ModRevision}
	if expired {
		r, e := raw.Get(ctx, controlKey)
		require.NoError(t, e)
		var c SandboxControlRecord
		require.NoError(t, decodeDomainRecord(r.Kvs[0], &c))
		c.ExpiresAt = time.Now().UTC().Add(-time.Hour)
		v, e := encodeDomainRecord(c)
		require.NoError(t, e)
		put, e := raw.Put(ctx, controlKey, v)
		require.NoError(t, e)
		in.ExpectedControlRevision = put.Header.Revision
	}
	s, ref := taskDestroyPrepare(t, base, ctx, in)
	out, e := base.CommitStage(ctx, s)
	require.NoError(t, e)
	require.Equal(t, OutcomeCommitted, out)
	now := time.Now().UTC()
	root := ed25519.NewKeyFromSeed(make([]byte, 32))
	key := ed25519.NewKeyFromSeed([]byte("12345678901234567890123456789012"))
	wire, e := p.SignCommandIssuerCertificate(root, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: base.namespace.Root(), AuthorityID: base.publicationAuthorityID, Target: base.publicationTarget, RestoreEpoch: base.restoreEpoch, PublicKey: key.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Minute)})
	require.NoError(t, e)
	p := &taskCloseProvider{wire: wire, key: key}
	id, e := decodeIdentity(base.identityValue, base.namespace)
	require.NoError(t, e)
	id.RestoreEpoch = base.restoreEpoch
	clockCalls := 0
	b, e := New(ctx, Options{Endpoints: raw.Endpoints(), Namespace: base.namespace, Identity: id, AllowInsecureLoopback: true, RequestTimeout: 3 * time.Second, TaskIssuer: p, Clock: testAuthorityClock(func(context.Context) (ClockObservation, error) { clockCalls++; return ClockObservation{UTC: now}, nil }), PublicationTrust: &RuntimePublicationTrust{AuthorityID: base.publicationAuthorityID, Target: base.publicationTarget, Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}})
	require.NoError(t, e)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	require.Zero(t, clockCalls)
	require.Zero(t, p.certCalls)
	require.Zero(t, p.signCalls)
	p.identity, e = b.taskVerifier.VerifyCommandIssuerCertificate(wire, now)
	require.NoError(t, e)
	c, e := b.ClaimTask(ctx, ref, "close-worker", 30*time.Second)
	require.NoError(t, e)
	t.Cleanup(func() { taskReleaseClaim(t, b, c) })
	return &taskCloseFixture{b, raw, c, p, now, ctx}
}

type taskDeliveryClock struct{ now atomic.Int64 }

func (c *taskDeliveryClock) Observe(ctx context.Context) (p.ClockObservation, error) {
	return p.ClockObservation{UTC: time.Unix(0, c.now.Load()).UTC()}, ctx.Err()
}

type taskDeliveryTarget struct {
	destination     *tr.Destination
	journal         *j.Journal
	clock           *taskDeliveryClock
	closes, queries atomic.Int32
	lose            atomic.Bool
	dial            func(context.Context) (net.Conn, error)
}

// Actual mTLS, frame codecs, signature verifier, and protected journal APIs are
// used here. The separate PID1 native gate covers the Supervisor/monitor owner;
// this metadata gate does not pretend that this handler is a protected PID1.
func newTaskDeliveryTarget(t *testing.T, f *taskCloseFixture, prepared *PreparedTaskCloseData) *taskDeliveryTarget {
	t.Helper()
	c := prepared.draft.claims.Context
	root := ed25519.NewKeyFromSeed(make([]byte, 32))
	runtimeKey := ed25519.NewKeyFromSeed([]byte("12345678901234567890123456789013"))
	clock := &taskDeliveryClock{}
	clock.now.Store(f.now.UnixNano())
	binding := p.TrustBinding{Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch}
	identity := p.RuntimeIdentityContext{SandboxID: c.SandboxID, WorkspaceHash: c.WorkspaceHash, Generation: c.Generation, Runtime: c.Runtime}
	birth := p.BirthContext{BootID: c.Runtime.BootID, RuntimePublicKey: runtimeKey.Public().(ed25519.PublicKey), UID: 1000, GID: 1000, ContractDigest: strings.Repeat("e", 64)}
	runtimeWire, err := p.SignRuntimeIdentityCertificate(root, p.RuntimeIdentityCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "runtime_receipt", Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch, PublicKey: birth.RuntimePublicKey, NotBefore: f.now.Add(-time.Minute), NotAfter: f.now.Add(time.Hour), SandboxID: c.SandboxID, WorkspaceHash: c.WorkspaceHash, Generation: c.Generation, Runtime: c.Runtime})
	require.NoError(t, err)
	runtimeDigest, err := snapshotDigest(runtimeWire)
	require.NoError(t, err)
	activationWire, err := p.SignTargetActivation(root, p.TargetActivationClaims{Version: 1, Role: "target_activation", ActivationID: uuid.NewString(), Binding: binding, Identity: identity, DataGateEpoch: c.DataGateEpoch, UID: 1000, GID: 1000, ContractDigest: birth.ContractDigest, RuntimeCertificateDigest: runtimeDigest, IssuerCertificateDigest: c.IssuerCertificateDigest, NotBefore: f.now.Add(-time.Minute), NotAfter: f.now.Add(time.Minute)})
	require.NoError(t, err)
	activation, err := f.b.taskVerifier.VerifyTargetActivation(activationWire, runtimeWire, f.p.wire, birth, f.now)
	require.NoError(t, err)
	directory, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.Chmod(directory, 0700))
	journal, err := j.CreateClosedJournal(f.ctx, j.JournalOptions{Directory: filepath.Join(directory, "journal"), Identity: j.JournalIdentity{Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch, SandboxID: c.SandboxID, WorkspaceHash: c.WorkspaceHash, Generation: c.Generation, Runtime: c.Runtime}, DataGateEpoch: c.DataGateEpoch, ManagementUID: uint32(os.Geteuid()), Birth: &birth, Verifier: f.b.taskVerifier, Clock: clock})
	require.NoError(t, err)
	require.NoError(t, journal.InstallActivation(f.ctx, activation))
	runtimeCredential, err := p.NewManagementTLSCredential(runtimeKey, runtimeWire, "runtime_receipt")
	require.NoError(t, err)
	config, err := p.NewManagementTLSConfig(p.ManagementTLSOptions{Verifier: f.b.taskVerifier, Clock: clock, Credential: runtimeCredential, PeerCertificate: f.p.wire, Server: true})
	require.NoError(t, err)
	issuerCredential, err := p.NewManagementTLSCredential(f.p.key, f.p.wire, "command_issuer")
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	target := &taskDeliveryTarget{journal: journal, clock: clock}
	target.dial = func(ctx context.Context) (net.Conn, error) {
		var dial net.Dialer
		return dial.DialContext(ctx, "tcp", listener.Addr().String())
	}
	target.destination, err = tr.NewDestination(tr.DestinationOptions{Identity: identity, RuntimeCertificate: runtimeWire, Credential: issuerCredential, Verifier: f.b.taskVerifier, Clock: clock, Dial: func(ctx context.Context) (net.Conn, error) { return target.dial(ctx) }})
	require.NoError(t, err)
	done := make(chan struct{})
	stop, cancel := context.WithCancel(context.Background())
	go func() {
		defer close(done)
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer raw.Close()
				ctx, finish := context.WithTimeout(stop, 4*time.Second)
				defer finish()
				_ = raw.SetDeadline(time.Now().Add(4 * time.Second))
				conn := tls.Server(raw, config)
				if conn.HandshakeContext(ctx) != nil {
					return
				}
				_, e, err := tr.ReadControlRequest(conn)
				if err != nil || e == nil {
					return
				}
				switch e.Purpose {
				case "task_close_data":
					target.closes.Add(1)
					var ev p.TaskCloseDataEvidence
					ev, err = f.b.taskVerifier.VerifyTaskCloseDataTicket(e.Ticket, e.IssuerCertificate, e.Context, time.Unix(0, clock.now.Load()).UTC())
					if err == nil && ev.Digest() != e.TicketDigest {
						err = errors.New("digest mismatch")
					}
					if err == nil {
						_, err = journal.CloseData(ctx, ev)
					}
				case "task_close_data_query":
					target.queries.Add(1)
				default:
					err = tr.ErrFrame
				}
				if err != nil {
					_ = tr.WriteEvent(conn, tr.EventError, []byte("unknown"))
					return
				}
				wire, err := journal.SignDataCloseReceipt(ctx, e.Context, e.TicketDigest, runtimeKey)
				if err != nil {
					_ = tr.WriteEvent(conn, tr.EventError, []byte("unknown"))
					return
				}
				if target.lose.Load() {
					return
				}
				_ = tr.WriteEvent(conn, tr.EventReceipt, wire)
			}()
		}
	}()
	t.Cleanup(func() {
		cancel()
		listener.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("task target handler did not join")
		}
		require.NoError(t, journal.Close())
	})
	return target
}
