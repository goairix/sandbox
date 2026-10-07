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
	"sync"
	"syscall"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	transport "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
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
	errors     []error
	errorCount uint64
	stopOnce   sync.Once
}

func newCombinedBridge(ctx context.Context) *combinedBridge {
	return &combinedBridge{ctx: ctx, pairs: map[net.Conn]net.Conn{}}
}
func (b *combinedBridge) listen(listener net.Listener, network, address string) {
	b.mu.Lock()
	b.listeners = append(b.listeners, listener)
	b.workers.Add(1)
	b.mu.Unlock()
	go func() {
		defer b.workers.Done()
		for {
			c, err := listener.Accept()
			if err != nil {
				b.note(err)
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
func (b *combinedBridge) note(err error) {
	if err == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.stopping {
		b.errorCount++
		// Keep bounded detail while retaining the complete observed count.
		if len(b.errors) < 16 {
			b.errors = append(b.errors, err)
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
		b.note(err)
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
	done := make(chan error, 1)
	go func() {
		_, err := io.CopyBuffer(s, c, make([]byte, 32768))
		s.Close()
		c.Close()
		done <- err
	}()
	_, err = io.CopyBuffer(c, s, make([]byte, 32768))
	s.Close()
	c.Close()
	reverseErr := <-done
	b.note(err)
	b.note(reverseErr)
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
		t.Logf("TASK5_BRIDGE_IO before-stop error=%v", err)
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
