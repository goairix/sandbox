//go:build linux && (amd64 || arm64)

package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	transport "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const nativePrivate = "/run/sandbox-fixture/private"
const nativeEtcdSocket = "/run/sandbox-metadata/etcd.sock"
const nativeImage = "gcr.io/etcd-development/etcd:v3.6.15@sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1"

type nativeObservation struct {
	Version        uint32 `json:"version"`
	Project        string `json:"project"`
	TargetID       string `json:"target_id"`
	MetadataID     string `json:"metadata_id"`
	Image          string `json:"image"`
	ContractDigest string `json:"contract_digest"`
	MemberName     string `json:"member_name"`
}
type nativeSecret struct {
	Observation                      nativeObservation
	Root, Issuer, Clock, Publication ed25519.PrivateKey
	IssuerWire                       []byte
	Namespace                        string
}

func nativeEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("TEST_AUTHENTICATED_TARGET_NATIVE") != "1" {
		t.Skip("requires Root-owned authenticated target fixture")
	}
	require.Equal(t, 0, os.Geteuid())
	require.Equal(t, 1, os.Getpid())
}
func nativeWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	require.NoError(t, err)
	_, err = f.Write(data)
	require.NoError(t, err)
	require.NoError(t, f.Sync())
	require.NoError(t, f.Close())
}

// Root invokes this only with its independently retained exact inspect facts.
// Keys and protected files are created as actual root in uniquely owned volumes.
func TestAuthenticatedTargetInitialize(t *testing.T) {
	nativeEnabled(t)
	wire, err := io.ReadAll(io.LimitReader(os.Stdin, 16385))
	require.NoError(t, err)
	require.LessOrEqual(t, len(wire), 16384)
	var observation nativeObservation
	decoder := json.NewDecoder(bytes.NewReader(wire))
	decoder.DisallowUnknownFields()
	require.NoError(t, decoder.Decode(&observation))
	canonical, _ := json.Marshal(observation)
	require.Equal(t, canonical, wire)
	require.Equal(t, uint32(1), observation.Version)
	require.Regexp(t, `^sandbox-authenticated-[a-z0-9-]{8,80}$`, observation.Project)
	require.Regexp(t, `^[a-f0-9]{64}$`, observation.TargetID)
	require.Regexp(t, `^[a-f0-9]{64}$`, observation.MetadataID)
	require.NotEqual(t, observation.TargetID, observation.MetadataID)
	require.Equal(t, nativeImage, observation.Image)
	require.Regexp(t, `^[a-f0-9]{64}$`, observation.ContractDigest)
	require.Equal(t, "authenticated-metadata", observation.MemberName)
	for _, path := range []string{nativePrivate, transport.ControlDirectory, "/run/sandbox-clock", "/run/sandbox-metadata"} {
		require.NoError(t, os.MkdirAll(path, 0700))
		require.NoError(t, os.Chmod(path, 0700))
	}
	secret := nativeSecret{Observation: observation}
	_, secret.Root, err = ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, secret.Issuer, err = ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, secret.Clock, err = ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, secret.Publication, err = ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	n, err := NewNamespace("/codex-test", "authenticated", observation.Project)
	require.NoError(t, err)
	secret.Namespace = n.Root()
	now := time.Now().UTC()
	secret.IssuerWire, err = p.SignCommandIssuerCertificate(secret.Root, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: n.Root(), AuthorityID: "fixture-authority", Target: "docker", RestoreEpoch: "native-epoch", PublicKey: secret.Issuer.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(45 * time.Minute)})
	require.NoError(t, err)
	// Exact production schema and order; no target UID or boot/key is configured.
	cfg := struct {
		Version uint32 `json:"version"`
		Binding struct {
			Namespace    string `json:"namespace"`
			AuthorityID  string `json:"authority_id"`
			Target       string `json:"target"`
			RestoreEpoch string `json:"restore_epoch"`
		} `json:"binding"`
		Roots          []ed25519.PublicKey `json:"roots"`
		UID            uint32              `json:"uid"`
		GID            uint32              `json:"gid"`
		NetworkAllowed bool                `json:"network_allowed"`
		ContractDigest string              `json:"contract_digest"`
		ClockSocket    string              `json:"clock_socket"`
		ClockPublicKey ed25519.PublicKey   `json:"clock_public_key"`
		ClockAudience  string              `json:"clock_audience"`
		MaxActive      uint32              `json:"max_active"`
	}{Version: 1, Roots: []ed25519.PublicKey{secret.Root.Public().(ed25519.PublicKey)}, UID: 1000, GID: 1000, ContractDigest: observation.ContractDigest, ClockSocket: transport.ClockSocket, ClockPublicKey: secret.Clock.Public().(ed25519.PublicKey), ClockAudience: "task4-target", MaxActive: 4}
	cfg.Binding.Namespace = n.Root()
	cfg.Binding.AuthorityID = "fixture-authority"
	cfg.Binding.Target = "docker"
	cfg.Binding.RestoreEpoch = "native-epoch"
	encoded, err := json.Marshal(cfg)
	require.NoError(t, err)
	nativeWrite(t, transport.ControlDirectory+"/config.json", encoded)
	encoded, err = json.Marshal(secret)
	require.NoError(t, err)
	nativeWrite(t, nativePrivate+"/custody.json", encoded)
	fmt.Println("AUTHENTICATED_INIT_COMPLETE", observation.Project, observation.TargetID, observation.MetadataID)
}

type nativeClock struct {
	calls    atomic.Int64
	offsetNS atomic.Int64
}

func (c *nativeClock) Observe(ctx context.Context) (p.ClockObservation, error) {
	c.calls.Add(1)
	return p.ClockObservation{UTC: time.Now().UTC().Add(time.Duration(c.offsetNS.Load())), Uncertainty: time.Millisecond}, ctx.Err()
}

type nativeIssuer struct {
	secret   nativeSecret
	identity p.CommandIssuerIdentity
}

func (i *nativeIssuer) Certificate(ctx context.Context) ([]byte, error) {
	return bytes.Clone(i.secret.IssuerWire), ctx.Err()
}
func (i *nativeIssuer) SignStart(ctx context.Context, digest string, c p.ExecStartTicketClaims) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if digest != i.identity.Digest() {
		return nil, ErrInvalidRecord
	}
	return p.SignExecStartTicket(i.secret.Issuer, i.identity, c)
}
func (i *nativeIssuer) SignRenew(ctx context.Context, digest string, c p.ExecRenewTicketClaims) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if digest != i.identity.Digest() {
		return nil, ErrInvalidRecord
	}
	return p.SignExecRenewTicket(i.secret.Issuer, c)
}

// The only new metadata bridge lives in this named test-parent netnone namespace.
func nativeMetadataRelay(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var mu sync.Mutex
	connections := map[net.Conn]bool{}
	var workers sync.WaitGroup
	done := make(chan struct{})
	stopping := false
	go func() {
		defer close(done)
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			if stopping || len(connections) >= 16 {
				mu.Unlock()
				client.Close()
				continue
			}
			connections[client] = true
			workers.Add(1)
			mu.Unlock()
			go func() {
				defer workers.Done()
				defer func() { client.Close(); mu.Lock(); delete(connections, client); mu.Unlock() }()
				server, err := net.DialTimeout("unix", nativeEtcdSocket, time.Second)
				if err != nil {
					return
				}
				mu.Lock()
				if stopping {
					mu.Unlock()
					server.Close()
					return
				}
				connections[server] = true
				mu.Unlock()
				defer func() { server.Close(); mu.Lock(); delete(connections, server); mu.Unlock() }()
				a := make(chan struct{})
				go func() {
					defer close(a)
					io.CopyBuffer(server, client, make([]byte, 32768))
					server.Close()
					client.Close()
				}()
				io.CopyBuffer(client, server, make([]byte, 32768))
				server.Close()
				client.Close()
				<-a
			}()
		}
	}()
	t.Cleanup(func() {
		mu.Lock()
		stopping = true
		listener.Close()
		for conn := range connections {
			conn.Close()
		}
		mu.Unlock()
		<-done
		workers.Wait()
	})
	return "http://" + listener.Addr().String()
}
func nativeBootstrap(t *testing.T, request transport.BootstrapRequest, value any) {
	t.Helper()
	conn, err := net.DialTimeout("unix", transport.ControlSocket, time.Second)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	require.NoError(t, transport.WriteBootstrap(conn, request))
	require.NoError(t, conn.(*net.UnixConn).CloseWrite())
	require.NoError(t, transport.ReadBootstrap(conn, value))
}
func nativeSecretRead(t *testing.T) nativeSecret {
	t.Helper()
	require.NoError(t, transport.CheckProtectedPath(nativePrivate+"/custody.json", 0600))
	wire, err := os.ReadFile(nativePrivate + "/custody.json")
	require.NoError(t, err)
	var secret nativeSecret
	require.NoError(t, json.Unmarshal(wire, &secret))
	return secret
}

type nativeExecutionFixture struct {
	b            *Backend
	raw          *clientv3.Client
	destination  *transport.Destination
	input        AcquireIntentInput
	clock        *nativeClock
	secret       nativeSecret
	identity     p.RuntimeIdentityContext
	runtimeWire  []byte
	queryControl *ExecDeliveryHandle
}

func nativeExecutionSetup(t *testing.T) *nativeExecutionFixture {
	t.Helper()
	nativeEnabled(t)
	secret := nativeSecretRead(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	listener, err := net.Listen("unix", transport.ClockSocket)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(transport.ClockSocket, 0600))
	clock := &nativeClock{}
	clockDone := make(chan error, 1)
	binding := p.TrustBinding{Namespace: secret.Namespace, AuthorityID: "fixture-authority", Target: "docker", RestoreEpoch: "native-epoch"}
	go func() {
		clockDone <- p.ServeSignedClock(ctx, listener, p.SignedClockServerOptions{Binding: binding, Key: secret.Clock, Source: clock})
	}()
	t.Cleanup(func() { cancel(); require.NoError(t, <-clockDone) })
	fmt.Println("AUTHENTICATED_CLOCK_READY", secret.Observation.Project)
	deadline := time.Now().Add(40 * time.Second)
	for {
		_, err = os.Lstat(transport.ControlSocket)
		if err == nil {
			break
		}
		require.True(t, time.Now().Before(deadline), "production control socket never appeared")
		time.Sleep(20 * time.Millisecond)
	}
	var hello transport.BootstrapResponse
	nativeBootstrap(t, transport.BootstrapRequest{Version: 1, Purpose: "hello"}, &hello)
	require.Equal(t, "closed_hello", hello.Purpose)
	require.NotNil(t, hello.Birth)
	require.Equal(t, uint32(1000), hello.Birth.UID)
	require.Equal(t, uint32(1000), hello.Birth.GID)
	require.False(t, hello.Birth.NetworkAllowed)
	require.Equal(t, secret.Observation.ContractDigest, hello.Birth.ContractDigest)
	require.NotEmpty(t, hello.Birth.BootID)
	var snapshot json.RawMessage
	nativeBootstrap(t, transport.BootstrapRequest{Version: 1, Purpose: "inspect"}, &snapshot)
	t.Logf("PRODUCTION_BOOTSTRAP_COST %s", snapshot)
	endpoint := nativeMetadataRelay(t)
	raw, err := clientv3.New(clientv3.Config{Endpoints: []string{endpoint}, DialTimeout: 3 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	members, err := raw.MemberList(ctx)
	require.NoError(t, err)
	require.NotNil(t, members.Header)
	require.Len(t, members.Members, 1)
	require.Equal(t, secret.Observation.MemberName, members.Members[0].Name)
	require.Equal(t, members.Members[0].ID, members.Header.MemberId)
	require.NotZero(t, members.Header.ClusterId)
	t.Logf("ACTUAL_OWNED_METADATA uid=%s member=%x cluster=%x name=%s", secret.Observation.MetadataID, members.Members[0].ID, members.Header.ClusterId, members.Members[0].Name)
	n, err := NewNamespace("/codex-test", "authenticated", secret.Observation.Project)
	require.NoError(t, err)
	require.Equal(t, secret.Namespace, n.Root())
	identity := Identity{SchemaVersion: 1, Prefix: n.prefix, AuthorityID: n.scope, Cell: n.cell, ClusterID: members.Header.ClusterId, StorageID: "test-storage", RuntimeID: "fixture-authority", RestoreEpoch: "native-epoch"}
	wire, err := json.Marshal(identity)
	require.NoError(t, err)
	identityKey, _ := n.Key("meta", "identity")
	restoreKey, _ := n.Key("meta", "restore_epoch")
	seed, err := raw.Txn(ctx).If(clientv3.Compare(clientv3.CreateRevision(identityKey), "=", 0), clientv3.Compare(clientv3.CreateRevision(restoreKey), "=", 0)).Then(clientv3.OpPut(identityKey, string(wire)), clientv3.OpPut(restoreKey, identity.RestoreEpoch)).Commit()
	require.NoError(t, err)
	require.True(t, seed.Succeeded, "fixture namespace already existed")
	t.Cleanup(func() {
		bounded, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := raw.Delete(bounded, n.Root(), clientv3.WithPrefix())
		require.NoError(t, err)
	})
	issuer := &nativeIssuer{secret: secret}
	b, err := New(ctx, Options{Endpoints: []string{endpoint}, Namespace: n, Identity: identity, AllowInsecureLoopback: true, RequestTimeout: 3 * time.Second, Clock: clock, ExecIssuer: issuer, PublicationTrust: &RuntimePublicationTrust{AuthorityID: binding.AuthorityID, Target: binding.Target, Roots: []ed25519.PublicKey{secret.Root.Public().(ed25519.PublicKey)}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	issuer.identity, err = b.execVerifier.VerifyCommandIssuerCertificate(secret.IssuerWire, time.Now().UTC())
	require.NoError(t, err)
	input := acquisitionInput(t, "authenticated-native")
	input.TTL = time.Hour
	acquire, err := b.AcquireIntent(ctx, input)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, acquire.Outcome)
	claim, err := b.ClaimCreation(ctx, input.Workspace, input.IntentID, "native-attester", 30*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.ReleaseCreationClaim(context.Background(), claim) })
	dispatch, err := b.DeclareRuntimeDispatch(ctx, claim, RuntimeDispatchInput{Kind: RuntimeDispatchCreate, Target: "docker", Payload: json.RawMessage(`{"fixture":"actual-production-target"}`)})
	require.NoError(t, err)
	require.NotNil(t, dispatch.Entry)
	record := dispatch.Entry.Record
	runtimeRef := p.RuntimeReference{ID: secret.Observation.TargetID, UID: secret.Observation.TargetID, BootID: hello.Birth.BootID}
	expected := p.RuntimeIdentityContext{SandboxID: record.SandboxID, WorkspaceHash: record.WorkspaceHash, Generation: record.Generation, Runtime: runtimeRef}
	now := time.Now().UTC()
	publicationCert, err := p.SignRuntimeCertificate(secret.Root, p.RuntimeCertificateClaims{Version: 1, Namespace: n.Root(), AuthorityID: binding.AuthorityID, Target: "docker", RestoreEpoch: binding.RestoreEpoch, IntentID: record.IntentID, SandboxID: record.SandboxID, WorkspaceHash: record.WorkspaceHash, Generation: record.Generation, OperationID: record.OperationID, PayloadDigest: record.PayloadDigest, Snapshot: p.SnapshotReference(record.Snapshot), ExpiresAt: record.ExpiresAt, Runtime: runtimeRef, WorkspaceMode: "plain", PublicKey: secret.Publication.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Minute), NotAfter: record.ExpiresAt})
	require.NoError(t, err)
	bind, err := b.BindRuntime(ctx, claim, publicationCert)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, bind.Outcome)
	mount, err := b.ConsumeRuntimeMount(ctx, claim)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, mount.Outcome)
	require.Zero(t, mount.Mount.Record.MountAttempt)
	runtimeWire, err := p.SignRuntimeIdentityCertificate(secret.Root, p.RuntimeIdentityCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "runtime_receipt", Namespace: n.Root(), AuthorityID: binding.AuthorityID, Target: "docker", RestoreEpoch: binding.RestoreEpoch, PublicKey: hello.Birth.RuntimePublicKey, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(35 * time.Minute), SandboxID: expected.SandboxID, WorkspaceHash: expected.WorkspaceHash, Generation: expected.Generation, Runtime: expected.Runtime})
	require.NoError(t, err)
	hash := func(w []byte) string { h := sha256.Sum256(w); return hex.EncodeToString(h[:]) }
	activation, err := p.SignTargetActivation(secret.Root, p.TargetActivationClaims{Version: 1, Role: "target_activation", ActivationID: uuid.NewString(), Binding: binding, Identity: expected, DataGateEpoch: claim.control.DataGateEpoch, UID: 1000, GID: 1000, NetworkAllowed: false, ContractDigest: secret.Observation.ContractDigest, RuntimeCertificateDigest: hash(runtimeWire), IssuerCertificateDigest: hash(secret.IssuerWire), NotBefore: now.Add(-3 * time.Second), NotAfter: now.Add(20 * time.Minute)})
	require.NoError(t, err)
	var activated transport.BootstrapResponse
	nativeBootstrap(t, transport.BootstrapRequest{Version: 1, Purpose: "activate", Activation: activation, RuntimeCertificate: runtimeWire, IssuerCertificate: secret.IssuerWire}, &activated)
	require.Equal(t, "activated", activated.Purpose)
	credential, err := p.NewManagementTLSCredential(secret.Issuer, secret.IssuerWire, "command_issuer")
	require.NoError(t, err)
	destination, err := transport.NewDestination(transport.DestinationOptions{Identity: expected, RuntimeCertificate: runtimeWire, Credential: credential, Verifier: b.execVerifier, Clock: clock, Dial: func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", transport.ControlSocket)
	}})
	require.NoError(t, err)
	f := &nativeExecutionFixture{b: b, raw: raw, destination: destination, input: input, clock: clock, secret: secret, identity: expected, runtimeWire: runtimeWire}
	snap := f.inspect(t, "PRODUCTION_ACTIVATED_IDLE_COST")
	require.Equal(t, "open", snap["gate"])
	require.Equal(t, float64(0), snap["active"])
	idleCalls := clock.calls.Load()
	time.Sleep(500 * time.Millisecond)
	require.Equal(t, idleCalls, clock.calls.Load(), "idle PID1 called controlled clock")
	// Fixture-owned legacy delegate attests only the actually observed activated
	// plain gate. Its key is intentionally distinct from PID1's fresh TLS key.
	bundle, err := b.loadRuntimePreparation(ctx, claim.workspace, claim.reference.IntentID, "")
	require.NoError(t, err)
	now = time.Now().UTC()
	ready, err := p.SignReadyReceipt(secret.Publication, publicationCert, p.ReadyReceiptClaims{Version: 1, CertificateDigest: bundle.Binding.Record.CertificateDigest, Claim: p.ClaimReference{ClaimID: claim.reference.ClaimID, CreateRevision: claim.reference.CreateRevision, LeaseID: claim.reference.LeaseID}, DataGateEpoch: claim.control.DataGateEpoch, GateState: "open", MountAttempt: 0, ObservedAt: now, ValidUntil: now.Add(5 * time.Second)})
	require.NoError(t, err)
	published, err := b.PublishRuntime(ctx, claim, ready)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, published.Outcome)
	t.Logf("EXACT_CERT_ROLE_MAPPING runtime=%+v management_cert=%s publication_cert=%s issuer=%s", expected.Runtime, hash(runtimeWire), hash(publicationCert), issuer.identity.Digest())
	return f
}
func (f *nativeExecutionFixture) inspect(t *testing.T, label string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := f.destination.Open(ctx)
	require.NoError(t, err)
	defer s.Close()
	require.NoError(t, s.Send(ctx, transport.Envelope{Version: 1, Purpose: "exec_inspect"}))
	kind, wire, err := s.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, transport.EventDiagnostics, kind)
	require.LessOrEqual(t, len(wire), 16384)
	var snap map[string]any
	require.NoError(t, json.Unmarshal(wire, &snap))
	require.Equal(t, true, snap["complete"])
	require.Equal(t, float64(1), snap["pid"])
	require.Equal(t, f.identity.Runtime.BootID, snap["boot_id"])
	t.Logf("%s %s", label, wire)
	return snap
}
func (f *nativeExecutionFixture) prepare(t *testing.T, request p.ExecutionRequest) (*OperationCapability, *PreparedExecEffect) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	admission, err := f.b.BeginOperation(ctx, BeginOperationInput{SandboxID: f.input.SandboxID, RequestID: uuid.NewString(), Kind: OperationData})
	require.NoError(t, err)
	require.NotNil(t, admission.Capability)
	prepared, err := f.b.PrepareExecEffect(ctx, admission.Capability, request)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, prepared.Outcome)
	require.NotNil(t, prepared.Prepared)
	require.Equal(t, f.identity.Runtime, p.RuntimeReference(admission.Capability.record.Runtime))
	return admission.Capability, prepared.Prepared
}
func TestAuthenticatedTargetNativeSuccess(t *testing.T) {
	f := nativeExecutionSetup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	request := p.ExecutionRequest{Argv: []string{"/native/user", "probe"}, Env: map[string]string{"PATH": "/native", "GOMAXPROCS": "2", "TOKEN": "exact-user-value"}, UID: 1000, GID: 1000, WorkDir: "/", TimeoutSeconds: 10, Stdin: []byte{0, 1, 255, 10}}
	_, prepared := f.prepare(t, request)
	sent := time.Now()
	h, err := f.b.DeliverExecEffect(ctx, prepared, f.destination)
	require.NoError(t, err)
	require.NotNil(t, h)
	defer h.Close()
	ack := time.Since(sent)
	var stdout, stderr bytes.Buffer
	receipt, err := h.Wait(ctx, &stdout, &stderr)
	require.NoError(t, err)
	require.Equal(t, "local_terminal", receipt.State())
	require.True(t, receipt.DrainConfirmed())
	require.Zero(t, receipt.RootWaitStatus())
	require.Greater(t, receipt.RootPID(), 1)
	require.Contains(t, stderr.String(), "STDERR_MARKER")
	var actual struct {
		UID, GID int
		Stdin    []byte
		Env      []string
		Cwd      string
		FDs      []string
		TTY      [3]bool
	}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &actual))
	require.Equal(t, 1000, actual.UID)
	require.Equal(t, 1000, actual.GID)
	require.Equal(t, request.Stdin, actual.Stdin)
	require.ElementsMatch(t, []string{"PATH=/native", "GOMAXPROCS=2", "TOKEN=exact-user-value"}, actual.Env)
	require.Equal(t, "/", actual.Cwd)
	require.Equal(t, [3]bool{}, actual.TTY)
	for _, fd := range actual.FDs {
		require.NotContains(t, fd, "sandbox-control")
		require.NotContains(t, fd, "sandbox-clock")
		require.NotContains(t, fd, "custody")
	}
	require.Error(t, f.b.RenewExecDelivery(ctx, h))
	t.Logf("ACTUAL_DELIVERY_ACK=%s COMPLETE=%s RECEIPT=%s STDOUT=%s STDERR=%s", ack, time.Since(sent), receipt.Wire(), stdout.Bytes(), stderr.Bytes())
	f.activeRenewal(t, ctx)
	f.inspect(t, "PRODUCTION_COMPLETED_COST")
}

type nativeReadyWriter struct {
	bytes.Buffer
	ready chan struct{}
	once  sync.Once
}

func (w *nativeReadyWriter) Write(b []byte) (int, error) {
	n, err := w.Buffer.Write(b)
	if bytes.Contains(w.Bytes(), []byte("USER_READY")) {
		w.once.Do(func() { close(w.ready) })
	}
	return n, err
}
func (f *nativeExecutionFixture) activeRenewal(t *testing.T, ctx context.Context) {
	request := p.ExecutionRequest{Argv: []string{"/native/user", "hold"}, Env: map[string]string{"GOMAXPROCS": "2"}, UID: 1000, GID: 1000, WorkDir: "/", TimeoutSeconds: 8}
	capability, prepared := f.prepare(t, request)
	start := time.Now()
	h, err := f.b.DeliverExecEffect(ctx, prepared, f.destination)
	require.NoError(t, err)
	defer h.Close()
	require.True(t, capability.mu.TryLock(), "accepted ACK retained original capability mutex")
	capability.mu.Unlock()
	output := &nativeReadyWriter{ready: make(chan struct{})}
	type result struct {
		receipt p.LocalExecReceiptEvidence
		err     error
	}
	done := make(chan result, 1)
	go func() { receipt, err := h.Wait(ctx, output, io.Discard); done <- result{receipt, err} }()
	select {
	case <-output.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("accepted user never became ready")
	}
	snapshot := f.inspect(t, "PRODUCTION_ONE_ACTIVE_COST")
	require.Equal(t, float64(1), snapshot["active"])
	time.Sleep(time.Second)
	require.NoError(t, f.b.RenewExecDelivery(ctx, h))
	select {
	case result := <-done:
		require.NoError(t, result.err)
		require.Equal(t, "local_terminal", result.receipt.State())
		require.True(t, result.receipt.DrainConfirmed())
		require.NotZero(t, result.receipt.RootWaitStatus())
		t.Logf("ACTUAL_ACCEPTED_RENEWAL_AND_TIMEOUT elapsed=%s receipt=%s", time.Since(start), result.receipt.Wire())
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.Error(t, f.b.RenewExecDelivery(ctx, h))
}
