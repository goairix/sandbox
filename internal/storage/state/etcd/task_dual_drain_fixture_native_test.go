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
	"os/signal"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	transport "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const combinedPrivate = "/run/sandbox-fixture/private/custody.json"
const combinedImage = "gcr.io/etcd-development/etcd:v3.6.15@sha256:5ed4e32061aa061f970d6500629213e8579ef17571dcf9789074966acb9afad1"
const combinedClockAudience = "task-quiesce-combined"
const combinedLifetime = 180 * time.Second

// Root supplies this exact canonical, nonsecret schema on initializer stdin.
// Its daemon observations remain independently audited by Root, not attested
// merely by accepting this file. No caller-selected path or endpoint exists.
type combinedMemberObservation struct {
	Name        string `json:"name"`
	ContainerID string `json:"container_id"`
	NetworkIP   string `json:"network_ip"`
}
type combinedObservation struct {
	Version        uint32                      `json:"version"`
	Project        string                      `json:"project"`
	TargetID       string                      `json:"target_id"`
	BackendID      string                      `json:"backend_id"`
	RelayID        string                      `json:"relay_id"`
	Image          string                      `json:"image"`
	ContractDigest string                      `json:"contract_digest"`
	Members        []combinedMemberObservation `json:"members"`
}
type combinedSecret struct {
	Observation                      combinedObservation `json:"observation"`
	Root, Issuer, Clock, Publication ed25519.PrivateKey
	IssuerWire                       []byte
	Namespace                        string
}
type combinedConfig struct {
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
}
type combinedPhase struct {
	Version uint32 `json:"version"`
	Project string `json:"project"`
	Role    string `json:"role"`
	Phase   string `json:"phase"`
	Member  string `json:"member,omitempty"`
	Path    string `json:"path,omitempty"`
}

func combinedEnabled(t *testing.T, role string) context.Context {
	t.Helper()
	if os.Getenv("SANDBOX_TASK5_NATIVE") != "1" {
		t.Skip("requires Root-owned combined physical fixture; host SKIP is not native PASS")
	}
	require.Equal(t, "linux", runtime.GOOS)
	require.Equal(t, 0, os.Geteuid())
	require.Equal(t, 1, os.Getpid())
	require.Equal(t, role, os.Getenv("SANDBOX_TASK5_ROLE"))
	require.Regexp(t, `^sandbox-quiesce-combined-[a-f0-9]{12}$`, os.Getenv("SANDBOX_TASK5_PROJECT"))
	ctx, cancel := context.WithTimeout(context.Background(), combinedLifetime)
	t.Cleanup(cancel)
	return ctx
}
func combinedEmit(t *testing.T, role, phase, member, path string) {
	t.Helper()
	wire, err := json.Marshal(combinedPhase{1, os.Getenv("SANDBOX_TASK5_PROJECT"), role, phase, member, path})
	require.NoError(t, err)
	fmt.Printf("TASK5_FIXTURE %s\n", wire)
}
func combinedCanonical(t *testing.T, wire []byte, value any) {
	t.Helper()
	require.NotEmpty(t, wire)
	require.LessOrEqual(t, len(wire), 16384)
	d := json.NewDecoder(bytes.NewReader(wire))
	d.DisallowUnknownFields()
	require.NoError(t, d.Decode(value))
	canonical, err := json.Marshal(value)
	require.NoError(t, err)
	require.Equal(t, canonical, wire)
}
func combinedValidateObservation(t *testing.T, o combinedObservation) {
	t.Helper()
	require.Equal(t, uint32(1), o.Version)
	require.Equal(t, os.Getenv("SANDBOX_TASK5_PROJECT"), o.Project)
	require.Equal(t, combinedImage, o.Image)
	require.Regexp(t, `^[a-f0-9]{64}$`, o.ContractDigest)
	seen := map[string]bool{}
	for _, id := range []string{o.TargetID, o.BackendID, o.RelayID} {
		require.Regexp(t, `^[a-f0-9]{64}$`, id)
		require.False(t, seen[id])
		seen[id] = true
	}
	require.Len(t, o.Members, 3)
	for i, member := range o.Members {
		require.Equal(t, fmt.Sprintf("etcd-%d", i+1), member.Name)
		require.Regexp(t, `^[a-f0-9]{64}$`, member.ContainerID)
		require.False(t, seen[member.ContainerID])
		seen[member.ContainerID] = true
		require.NotNil(t, net.ParseIP(member.NetworkIP))
	}
}
func combinedWrite(t *testing.T, path string, wire []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	require.NoError(t, err)
	n, writeErr := f.Write(wire)
	syncErr, closeErr := f.Sync(), f.Close()
	require.NoError(t, writeErr)
	require.Equal(t, len(wire), n)
	require.NoError(t, syncErr)
	require.NoError(t, closeErr)
	require.NoError(t, transport.CheckProtectedPath(path, 0600))
}
func TestTaskDualDrainFixtureInitialize(t *testing.T) {
	ctx := combinedEnabled(t, "initialize")
	wire, err := io.ReadAll(io.LimitReader(os.Stdin, 16385))
	require.NoError(t, err)
	var observation combinedObservation
	combinedCanonical(t, wire, &observation)
	combinedValidateObservation(t, observation)
	for _, path := range []string{"/run/sandbox-fixture/private", transport.ControlDirectory, "/run/sandbox-clock", "/run/sandbox-metadata"} {
		require.NoError(t, os.MkdirAll(path, 0700))
		require.NoError(t, os.Chmod(path, 0700))
		require.NoError(t, transport.CheckProtectedPath(path, os.ModeDir|0700))
	}
	s := combinedSecret{Observation: observation}
	for _, key := range []*ed25519.PrivateKey{&s.Root, &s.Issuer, &s.Clock, &s.Publication} {
		_, *key, err = ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
	}
	n, err := NewNamespace("/codex-test", "combined", observation.Project)
	require.NoError(t, err)
	s.Namespace = n.Root()
	now := time.Now().UTC()
	s.IssuerWire, err = p.SignCommandIssuerCertificate(s.Root, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: n.Root(), AuthorityID: "fixture-authority", Target: "docker", RestoreEpoch: "native-epoch", PublicKey: s.Issuer.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(45 * time.Minute)})
	require.NoError(t, err)
	cfg := combinedConfig{Version: 1, Roots: []ed25519.PublicKey{s.Root.Public().(ed25519.PublicKey)}, UID: 1000, GID: 1000, ContractDigest: observation.ContractDigest, ClockSocket: transport.ClockSocket, ClockPublicKey: s.Clock.Public().(ed25519.PublicKey), ClockAudience: combinedClockAudience, MaxActive: 4}
	cfg.Binding.Namespace, cfg.Binding.AuthorityID, cfg.Binding.Target, cfg.Binding.RestoreEpoch = n.Root(), "fixture-authority", "docker", "native-epoch"
	wire, err = json.Marshal(cfg)
	require.NoError(t, err)
	combinedWrite(t, transport.ControlDirectory+"/config.json", wire)
	wire, err = json.Marshal(s)
	require.NoError(t, err)
	require.LessOrEqual(t, len(wire), 16384)
	combinedWrite(t, combinedPrivate, wire)
	require.NoError(t, ctx.Err())
	combinedEmit(t, "initialize", "initialized", "", transport.ControlDirectory+"/config.json")
}

// One pair budget is shared across the three fixed listeners. Stop, admission,
// and WaitGroup Add are serialized; stop joins both copy owners of every pair.
type combinedBridge struct {
	ctx        context.Context
	mu         sync.Mutex
	stopping   bool
	listeners  []net.Listener
	pairs      map[net.Conn]net.Conn
	workers    sync.WaitGroup
	errors     []string
	errorCount uint64
	stopOnce   sync.Once
}

func newCombinedBridge(ctx context.Context) *combinedBridge {
	return &combinedBridge{ctx: ctx, pairs: map[net.Conn]net.Conn{}}
}
func (b *combinedBridge) listen(listener net.Listener, network, address string) {
	b.mu.Lock()
	if b.stopping {
		b.mu.Unlock()
		listener.Close()
		return
	}
	b.listeners = append(b.listeners, listener)
	b.workers.Add(1)
	b.mu.Unlock()
	go func() {
		defer b.workers.Done()
		for {
			c, err := listener.Accept()
			if err != nil {
				b.note("accept", err)
				return
			}
			b.mu.Lock()
			if b.stopping || len(b.pairs) >= 16 {
				b.mu.Unlock()
				c.Close()
				continue
			}
			b.pairs[c] = nil
			b.workers.Add(1)
			b.mu.Unlock()
			go b.copyPair(c, network, address)
		}
	}()
}
func (b *combinedBridge) note(origin string, err error) {
	if err == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.stopping {
		b.errorCount++
		// Keep bounded detail while retaining the complete observed count.
		if len(b.errors) < 16 {
			b.errors = append(b.errors, fmt.Sprintf("origin=%s error=%v", origin, err))
		}
	}
}
func (b *combinedBridge) copyPair(c net.Conn, network, address string) {
	defer b.workers.Done()
	defer func() { c.Close(); b.mu.Lock(); delete(b.pairs, c); b.mu.Unlock() }()
	dialCtx, cancel := context.WithTimeout(b.ctx, time.Second)
	s, err := (&net.Dialer{}).DialContext(dialCtx, network, address)
	cancel()
	if err != nil {
		b.note("fixed_outgoing_dial", err)
		return
	}
	b.mu.Lock()
	if b.stopping {
		b.mu.Unlock()
		s.Close()
		return
	}
	b.pairs[c] = s
	b.mu.Unlock()
	defer s.Close()
	// Each copy reports before it causes the pair close. The reverse error
	// following that close is explicitly attributed, not hidden as shutdown.
	done := make(chan error, 1)
	var closeOnce sync.Once
	closePair := func(origin string, err error) {
		first := false
		closeOnce.Do(func() { first = true; s.Close(); c.Close() })
		if first {
			if err == nil {
				b.note(origin+"_EOF", io.EOF)
			} else {
				b.note(origin+"_first_failure", err)
			}
		} else {
			b.note(origin+"_after_pair_close", err)
		}
	}
	go func() {
		_, err := io.CopyBuffer(s, c, make([]byte, 32768))
		closePair("client_to_member", err)
		done <- err
	}()
	_, err = io.CopyBuffer(c, s, make([]byte, 32768))
	closePair("member_to_client", err)
	<-done
}
func (b *combinedBridge) close(t *testing.T) {
	t.Helper()
	b.stopOnce.Do(func() {
		b.mu.Lock()
		b.stopping = true
		for _, l := range b.listeners {
			l.Close()
		}
		for c, s := range b.pairs {
			c.Close()
			if s != nil {
				s.Close()
			}
		}
		b.mu.Unlock()
		b.workers.Wait()
	})
	b.mu.Lock()
	defer b.mu.Unlock()
	require.Empty(t, b.pairs, "bridge retained pair after join")
	for _, err := range b.errors {
		t.Logf("TASK5_BRIDGE_IO before-stop %s", err)
	}
	t.Logf("TASK5_BRIDGE_JOIN pairs=%d listeners=%d errors=%d retained=%d", len(b.pairs), len(b.listeners), b.errorCount, len(b.errors))
}
func combinedSocket(i int) string { return fmt.Sprintf("/run/sandbox-metadata/member-%d.sock", i+1) }
func TestTaskDualDrainFixtureRelay(t *testing.T) {
	lifetime := combinedEnabled(t, "relay")
	ctx, cancel := signal.NotifyContext(lifetime, syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	b := newCombinedBridge(ctx)
	defer b.close(t)
	require.NoError(t, transport.CheckProtectedPath("/run/sandbox-metadata", os.ModeDir|0700))
	for i := 0; i < 3; i++ {
		path := combinedSocket(i)
		_, err := os.Lstat(path)
		require.True(t, os.IsNotExist(err), "refuse stale metadata socket: %s", path)
		l, err := net.Listen("unix", path)
		require.NoError(t, err)
		require.NoError(t, os.Chmod(path, 0600))
		require.NoError(t, transport.CheckProtectedPath(path, os.ModeSocket|0600))
		b.listen(l, "tcp", fmt.Sprintf("etcd-%d:2379", i+1))
		combinedEmit(t, "relay", "member_socket_ready", fmt.Sprintf("etcd-%d", i+1), path)
	}
	<-ctx.Done()
	require.NoError(t, lifetime.Err(), "relay absolute lifetime expired before Root teardown")
}
func combinedHash(wire []byte) string { h := sha256.Sum256(wire); return hex.EncodeToString(h[:]) }

type combinedClock struct{ calls atomic.Uint64 }

func (c *combinedClock) Observe(ctx context.Context) (p.ClockObservation, error) {
	c.calls.Add(1)
	return p.ClockObservation{UTC: time.Now().UTC(), Uncertainty: time.Millisecond}, ctx.Err()
}

type combinedIssuer struct {
	secret   combinedSecret
	identity p.CommandIssuerIdentity
}

func (i *combinedIssuer) Certificate(ctx context.Context) ([]byte, error) {
	return bytes.Clone(i.secret.IssuerWire), ctx.Err()
}
func (i *combinedIssuer) SignStart(ctx context.Context, digest string, c p.ExecStartTicketClaims) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if digest != i.identity.Digest() {
		return nil, ErrInvalidRecord
	}
	return p.SignExecStartTicket(i.secret.Issuer, i.identity, c)
}
func (i *combinedIssuer) SignRenew(ctx context.Context, digest string, c p.ExecRenewTicketClaims) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if digest != i.identity.Digest() {
		return nil, ErrInvalidRecord
	}
	return p.SignExecRenewTicket(i.secret.Issuer, c)
}
func (i *combinedIssuer) SignCloseData(ctx context.Context, digest string, c p.TaskCloseDataTicketClaims) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if digest != i.identity.Digest() {
		return nil, ErrInvalidRecord
	}
	return p.SignTaskCloseDataTicket(i.secret.Issuer, i.identity, c)
}
func (i *combinedIssuer) SignQuiesceUsers(ctx context.Context, digest string, c p.TaskUserQuiescenceTicketClaims) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if digest != i.identity.Digest() {
		return nil, ErrInvalidRecord
	}
	return p.SignTaskUserQuiescenceTicket(i.secret.Issuer, i.identity, c)
}

type combinedFixture struct {
	ctx           context.Context
	b             *Backend
	raw           *clientv3.Client
	endpoints     []string
	secret        combinedSecret
	clock         *combinedClock // independent backend observation source
	identity      p.RuntimeIdentityContext
	runtimeWire   []byte
	credential    *p.ManagementTLSCredential
	destination   *transport.Destination
	input         AcquireIntentInput
	creationLease int64
}

func combinedBootstrap(t *testing.T, ctx context.Context, request transport.BootstrapRequest, value any) {
	t.Helper()
	require.NoError(t, transport.CheckProtectedPath(transport.ControlSocket, os.ModeSocket|0600))
	c, err := (&net.Dialer{}).DialContext(ctx, "unix", transport.ControlSocket)
	require.NoError(t, err)
	defer c.Close()
	require.NoError(t, c.SetDeadline(time.Now().Add(5*time.Second)))
	require.NoError(t, transport.WriteBootstrap(c, request))
	require.NoError(t, c.(*net.UnixConn).CloseWrite())
	require.NoError(t, transport.ReadBootstrap(c, value))
}
func combinedReadSecret(t *testing.T) combinedSecret {
	t.Helper()
	require.NoError(t, transport.CheckProtectedPath(combinedPrivate, 0600))
	f, err := os.Open(combinedPrivate)
	require.NoError(t, err)
	wire, readErr := io.ReadAll(io.LimitReader(f, 16385))
	closeErr := f.Close()
	require.NoError(t, readErr)
	require.NoError(t, closeErr)
	var s combinedSecret
	// Private custody validation never formats key-containing wire in failures.
	require.True(t, len(wire) > 0 && len(wire) <= 16384, "private custody size outside bound")
	d := json.NewDecoder(bytes.NewReader(wire))
	d.DisallowUnknownFields()
	require.NoError(t, d.Decode(&s))
	canonical, err := json.Marshal(s)
	require.NoError(t, err)
	require.True(t, bytes.Equal(canonical, wire), "noncanonical private custody")
	combinedValidateObservation(t, s.Observation)
	for _, key := range []ed25519.PrivateKey{s.Root, s.Issuer, s.Clock, s.Publication} {
		require.Equal(t, ed25519.PrivateKeySize, len(key), "invalid private key length")
	}
	return s
}
func combinedSetup(t *testing.T) *combinedFixture {
	t.Helper()
	ctx := combinedEnabled(t, "backend")
	s := combinedReadSecret(t)
	f := &combinedFixture{ctx: ctx, secret: s, clock: &combinedClock{}}
	binding := p.TrustBinding{Namespace: s.Namespace, AuthorityID: "fixture-authority", Target: "docker", RestoreEpoch: "native-epoch"}
	// Target clock source and backend clock are independent current observations.
	source := &combinedClock{}
	require.NoError(t, transport.CheckProtectedPath("/run/sandbox-clock", os.ModeDir|0700))
	_, err := os.Lstat(transport.ClockSocket)
	require.True(t, os.IsNotExist(err), "refuse stale clock socket")
	l, err := net.Listen("unix", transport.ClockSocket)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(transport.ClockSocket, 0600))
	require.NoError(t, transport.CheckProtectedPath(transport.ClockSocket, os.ModeSocket|0600))
	clockCtx, clockCancel := context.WithCancel(ctx)
	clockDone := make(chan error, 1)
	go func() {
		clockDone <- p.ServeSignedClock(clockCtx, l, p.SignedClockServerOptions{Binding: binding, Key: s.Clock, Source: source, MaxConnections: 16})
	}()
	t.Cleanup(func() { clockCancel(); require.NoError(t, <-clockDone) })
	combinedEmit(t, "backend", "clock_ready", "", transport.ClockSocket)
	bridge := newCombinedBridge(ctx)
	t.Cleanup(func() { bridge.close(t) })
	for i := 0; i < 3; i++ {
		require.NoError(t, transport.CheckProtectedPath(combinedSocket(i), os.ModeSocket|0600))
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		bridge.listen(listener, "unix", combinedSocket(i))
		f.endpoints = append(f.endpoints, "http://"+listener.Addr().String())
	}
	f.raw, err = clientv3.New(clientv3.Config{Endpoints: f.endpoints, DialTimeout: 3 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, f.raw.Close()) })
	cluster := f.members(t, "before")
	n, err := NewNamespace("/codex-test", "combined", s.Observation.Project)
	require.NoError(t, err)
	require.Equal(t, s.Namespace, n.Root())
	id := Identity{SchemaVersion: 1, Prefix: n.prefix, AuthorityID: n.scope, Cell: n.cell, ClusterID: cluster, StorageID: "test-storage", RuntimeID: binding.AuthorityID, RestoreEpoch: binding.RestoreEpoch}
	wire, err := json.Marshal(id)
	require.NoError(t, err)
	identityKey, _ := n.Key("meta", "identity")
	restoreKey, _ := n.Key("meta", "restore_epoch")
	request, cancel := context.WithTimeout(ctx, 3*time.Second)
	seed, err := f.raw.Txn(request).If(clientv3.Compare(clientv3.CreateRevision(identityKey), "=", 0), clientv3.Compare(clientv3.CreateRevision(restoreKey), "=", 0)).Then(clientv3.OpPut(identityKey, string(wire)), clientv3.OpPut(restoreKey, id.RestoreEpoch)).Commit()
	cancel()
	require.NoError(t, err)
	require.True(t, seed.Succeeded, "namespace was not fresh")
	issuer := &combinedIssuer{secret: s}
	f.b, err = New(ctx, Options{Endpoints: f.endpoints, Namespace: n, Identity: id, AllowInsecureLoopback: true, DialTimeout: 3 * time.Second, RequestTimeout: 3 * time.Second, Clock: f.clock, ExecIssuer: issuer, TaskIssuer: issuer, TaskQuiescenceIssuer: issuer, PublicationTrust: &RuntimePublicationTrust{AuthorityID: binding.AuthorityID, Target: binding.Target, Roots: []ed25519.PublicKey{s.Root.Public().(ed25519.PublicKey)}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, f.b.Close()) })
	require.Equal(t, f.endpoints, f.b.client.Endpoints())
	issuer.identity, err = f.b.execVerifier.VerifyCommandIssuerCertificate(s.IssuerWire, time.Now().UTC())
	require.NoError(t, err)
	deadline := time.Now().Add(40 * time.Second)
	for {
		_, err = os.Lstat(transport.ControlSocket)
		if err == nil {
			break
		}
		require.True(t, os.IsNotExist(err))
		require.True(t, time.Now().Before(deadline), "production target socket never appeared")
		select {
		case <-ctx.Done():
			require.NoError(t, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	var hello transport.BootstrapResponse
	combinedBootstrap(t, ctx, transport.BootstrapRequest{Version: 1, Purpose: "hello"}, &hello)
	require.Equal(t, "closed_hello", hello.Purpose)
	require.NotNil(t, hello.Birth)
	require.Equal(t, uint32(1000), hello.Birth.UID)
	require.Equal(t, uint32(1000), hello.Birth.GID)
	require.False(t, hello.Birth.NetworkAllowed)
	require.Equal(t, s.Observation.ContractDigest, hello.Birth.ContractDigest)
	require.NotEmpty(t, hello.Birth.BootID)
	require.Len(t, hello.Birth.RuntimePublicKey, ed25519.PublicKeySize)
	var bootstrap json.RawMessage
	combinedBootstrap(t, ctx, transport.BootstrapRequest{Version: 1, Purpose: "inspect"}, &bootstrap)
	t.Logf("TASK5_ORIGINAL_BOOTSTRAP %s", bootstrap)
	f.input = acquisitionInput(t, "combined-native")
	f.input.TTL = time.Hour
	acquire, err := f.b.AcquireIntent(ctx, f.input)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, acquire.Outcome)
	claim, err := f.b.ClaimCreation(ctx, f.input.Workspace, f.input.IntentID, "combined-attester", 30*time.Second)
	require.NoError(t, err)
	f.creationLease = claim.reference.LeaseID
	dispatch, err := f.b.DeclareRuntimeDispatch(ctx, claim, RuntimeDispatchInput{Kind: RuntimeDispatchCreate, Target: "docker", Payload: json.RawMessage(`{"fixture":"actual-production-target"}`)})
	require.NoError(t, err)
	require.NotNil(t, dispatch.Entry)
	r := dispatch.Entry.Record
	runtimeRef := p.RuntimeReference{ID: s.Observation.TargetID, UID: s.Observation.TargetID, BootID: hello.Birth.BootID}
	f.identity = p.RuntimeIdentityContext{SandboxID: r.SandboxID, WorkspaceHash: r.WorkspaceHash, Generation: r.Generation, Runtime: runtimeRef}
	now := time.Now().UTC()
	publication, err := p.SignRuntimeCertificate(s.Root, p.RuntimeCertificateClaims{Version: 1, Namespace: n.Root(), AuthorityID: binding.AuthorityID, Target: "docker", RestoreEpoch: binding.RestoreEpoch, IntentID: r.IntentID, SandboxID: r.SandboxID, WorkspaceHash: r.WorkspaceHash, Generation: r.Generation, OperationID: r.OperationID, PayloadDigest: r.PayloadDigest, Snapshot: p.SnapshotReference(r.Snapshot), ExpiresAt: r.ExpiresAt, Runtime: runtimeRef, WorkspaceMode: "plain", PublicKey: s.Publication.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Minute), NotAfter: r.ExpiresAt})
	require.NoError(t, err)
	bound, err := f.b.BindRuntime(ctx, claim, publication)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, bound.Outcome)
	mount, err := f.b.ConsumeRuntimeMount(ctx, claim)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, mount.Outcome)
	require.Zero(t, mount.Mount.Record.MountAttempt)
	f.runtimeWire, err = p.SignRuntimeIdentityCertificate(s.Root, p.RuntimeIdentityCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "runtime_receipt", Namespace: n.Root(), AuthorityID: binding.AuthorityID, Target: "docker", RestoreEpoch: binding.RestoreEpoch, PublicKey: hello.Birth.RuntimePublicKey, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(35 * time.Minute), SandboxID: f.identity.SandboxID, WorkspaceHash: f.identity.WorkspaceHash, Generation: f.identity.Generation, Runtime: runtimeRef})
	require.NoError(t, err)
	activation, err := p.SignTargetActivation(s.Root, p.TargetActivationClaims{Version: 1, Role: "target_activation", ActivationID: uuid.NewString(), Binding: binding, Identity: f.identity, DataGateEpoch: claim.control.DataGateEpoch, UID: 1000, GID: 1000, NetworkAllowed: false, ContractDigest: s.Observation.ContractDigest, RuntimeCertificateDigest: combinedHash(f.runtimeWire), IssuerCertificateDigest: combinedHash(s.IssuerWire), NotBefore: now.Add(-3 * time.Second), NotAfter: now.Add(20 * time.Minute)})
	require.NoError(t, err)
	var activated transport.BootstrapResponse
	combinedBootstrap(t, ctx, transport.BootstrapRequest{Version: 1, Purpose: "activate", Activation: activation, RuntimeCertificate: f.runtimeWire, IssuerCertificate: s.IssuerWire}, &activated)
	require.Equal(t, "activated", activated.Purpose)
	f.credential, err = p.NewManagementTLSCredential(s.Issuer, s.IssuerWire, "command_issuer")
	require.NoError(t, err)
	f.destination = f.newDestination(t, f.dial)
	snap := f.inspect(t, "activated_idle")
	require.Equal(t, "open", snap["gate"])
	require.Equal(t, float64(0), snap["active"])
	// The public creation delegate attests only the observed plain activated gate.
	actualBinding, err := f.b.LoadRuntimeBinding(ctx, f.input.Workspace, f.input.IntentID)
	require.NoError(t, err)
	require.NotNil(t, actualBinding)
	now = time.Now().UTC()
	ready, err := p.SignReadyReceipt(s.Publication, publication, p.ReadyReceiptClaims{Version: 1, CertificateDigest: actualBinding.Record.CertificateDigest, Claim: p.ClaimReference{ClaimID: claim.reference.ClaimID, CreateRevision: claim.reference.CreateRevision, LeaseID: claim.reference.LeaseID}, DataGateEpoch: claim.control.DataGateEpoch, GateState: "open", MountAttempt: 0, ObservedAt: now, ValidUntil: now.Add(5 * time.Second)})
	require.NoError(t, err)
	published, err := f.b.PublishRuntime(ctx, claim, ready)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, published.Outcome)
	t.Logf("TASK5_ACTIVATION identity=%+v runtime_cert=%s publication_cert=%s issuer=%s activation=%s", f.identity, combinedHash(f.runtimeWire), combinedHash(publication), issuer.identity.Digest(), combinedHash(activation))
	return f
}
func (f *combinedFixture) dial(ctx context.Context) (net.Conn, error) {
	if err := transport.CheckProtectedPath(transport.ControlSocket, os.ModeSocket|0600); err != nil {
		return nil, err
	}
	return (&net.Dialer{}).DialContext(ctx, "unix", transport.ControlSocket)
}
func (f *combinedFixture) newDestination(t *testing.T, dial func(context.Context) (net.Conn, error)) *transport.Destination {
	t.Helper()
	d, err := transport.NewDestination(transport.DestinationOptions{Identity: f.identity, RuntimeCertificate: f.runtimeWire, Credential: f.credential, Verifier: f.b.execVerifier, Clock: f.clock, Dial: dial})
	require.NoError(t, err)
	return d
}
func (f *combinedFixture) inspect(t *testing.T, phase string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
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
	t.Logf("TASK5_TARGET_SAMPLE phase=%s diagnostic_connection_overhead=true %s", phase, wire)
	return snap
}
func (f *combinedFixture) members(t *testing.T, phase string) uint64 {
	t.Helper()
	var cluster uint64
	for i, endpoint := range f.endpoints {
		ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
		status, err := f.raw.Status(ctx, endpoint)
		cancel()
		require.NoError(t, err)
		require.NotNil(t, status.Header)
		require.NotZero(t, status.Header.ClusterId)
		if cluster == 0 {
			cluster = status.Header.ClusterId
		}
		require.Equal(t, cluster, status.Header.ClusterId)
		one, err := clientv3.New(clientv3.Config{Endpoints: []string{endpoint}, DialTimeout: 3 * time.Second})
		require.NoError(t, err)
		ctx, cancel = context.WithTimeout(f.ctx, 3*time.Second)
		members, err := one.MemberList(ctx)
		cancel()
		require.NoError(t, err)
		require.Len(t, members.Members, 3)
		require.NotNil(t, members.Header)
		require.Equal(t, cluster, members.Header.ClusterId)
		seen := map[uint64]bool{}
		found := false
		for _, member := range members.Members {
			require.NotZero(t, member.ID)
			require.False(t, seen[member.ID])
			seen[member.ID] = true
			require.False(t, member.IsLearner)
			index := slices.Index([]string{"etcd-1", "etcd-2", "etcd-3"}, member.Name)
			require.GreaterOrEqual(t, index, 0)
			require.Equal(t, []string{"http://" + member.Name + ":2379"}, member.ClientURLs)
			require.Equal(t, []string{"http://" + member.Name + ":2380"}, member.PeerURLs)
			if member.Name == fmt.Sprintf("etcd-%d", i+1) {
				require.Equal(t, member.ID, status.Header.MemberId)
				found = true
			}
			t.Logf("TASK5_MEMBER phase=%s endpoint=%s cluster=%x member=%x alias=%s container=%s ip=%s image=%s", phase, endpoint, cluster, member.ID, member.Name, f.secret.Observation.Members[index].ContainerID, f.secret.Observation.Members[index].NetworkIP, combinedImage)
		}
		require.True(t, found)
		require.NoError(t, one.Close())
	}
	// A quorum-acknowledged unleased write and linearizable reads through every
	// separate endpoint show replication; this nonce is not dual-drain proof.
	key := f.secret.Namespace + "/fixture-health"
	nonce := uuid.NewString()
	ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
	put, err := f.raw.Put(ctx, key, nonce)
	cancel()
	require.NoError(t, err)
	require.Equal(t, cluster, put.Header.ClusterId)
	for _, endpoint := range f.endpoints {
		one, err := clientv3.New(clientv3.Config{Endpoints: []string{endpoint}, DialTimeout: 3 * time.Second})
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
		got, err := one.Get(ctx, key)
		cancel()
		require.NoError(t, err)
		require.Len(t, got.Kvs, 1)
		require.Equal(t, nonce, string(got.Kvs[0].Value))
		require.Zero(t, got.Kvs[0].Lease)
		require.NoError(t, one.Close())
	}
	t.Logf("TASK5_QUORUM_REPLICATION phase=%s cluster=%x revision=%d nonce=%s", phase, cluster, put.Header.Revision, nonce)
	return cluster
}
func (f *combinedFixture) snapshot(t *testing.T) map[string]*mvccpb.KeyValue {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
	defer cancel()
	r, err := f.raw.Get(ctx, f.secret.Namespace+"/", clientv3.WithPrefix(), clientv3.WithLimit(129))
	require.NoError(t, err)
	require.False(t, r.More)
	require.LessOrEqual(t, len(r.Kvs), 128)
	result := map[string]*mvccpb.KeyValue{}
	totalBytes := 0
	for _, kv := range r.Kvs {
		totalBytes += len(kv.Key) + len(kv.Value)
		require.LessOrEqual(t, totalBytes, 256*1024, "native snapshot exceeds fixed bound")
		wire, err := json.Marshal(kv)
		require.NoError(t, err)
		t.Logf("TASK5_NATIVE_RECORD %s", wire)
		result[string(kv.Key)] = kv
	}
	return result
}
