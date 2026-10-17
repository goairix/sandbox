//go:build linux

package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sandboxruntime "github.com/goairix/sandbox/internal/runtime"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	k "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
)

// This component fixture models only exact Kubernetes API observations and an
// explicitly Unknown observer. Its Pod is built through the formal plain
// builder from the real signed publication. Durable provider/termination KVs
// can only be produced by the production entry; no physical proof is issued.
type detachedTerminationProviderFixture struct {
	f                      *cleanupDetachedNoEffectNativeFixture
	mu                     sync.Mutex
	pod                    *v1.Pod
	node                   *v1.Node
	gets, deletes, patches int
	issuer                 *detachedTerminationFixtureIssuer
	terminates, queries    atomic.Int64
	destination            *ct.TerminationDestination
}

type detachedTerminationFixtureIssuer struct {
	key                      ed25519.PrivateKey
	wire                     []byte
	identity                 p.CommandIssuerIdentity
	certificates, signatures atomic.Int64
	// Tests observe actual borrowed callback contexts; this never changes a
	// certificate, signed claim, authority tuple or production clock/deadline.
	observe func(context.Context, string, time.Time)
}

func (s *detachedTerminationFixtureIssuer) Certificate(ctx context.Context) ([]byte, error) {
	s.certificates.Add(1)
	if s.observe != nil {
		s.observe(ctx, "Certificate", time.Time{})
	}
	return bytes.Clone(s.wire), ctx.Err()
}

func (s *detachedTerminationFixtureIssuer) SignRuntimeTermination(ctx context.Context, digest string, claims p.TaskRuntimeTerminationTicketClaims) ([]byte, error) {
	s.signatures.Add(1)
	if s.observe != nil {
		s.observe(ctx, "SignRuntimeTermination", claims.NotAfter)
	}
	if ctx.Err() != nil || digest != s.identity.Digest() {
		return nil, fmt.Errorf("fixture issuer binding refused")
	}
	return p.SignTaskRuntimeTerminationTicket(s.key, s.identity, claims)
}

func newDetachedTerminationProviderFixture(t *testing.T, claimTTL ...time.Duration) *detachedTerminationProviderFixture {
	t.Helper()
	h := &detachedTerminationProviderFixture{}
	api := httptest.NewTLSServer(http.HandlerFunc(h.serveKube))
	t.Cleanup(api.Close)
	ttl := 30 * time.Second
	if len(claimTTL) != 0 {
		ttl = claimTTL[0]
	}
	h.f = newCleanupDetachedNoEffectNativeFixtureWithClaimTTL(t, ttl, func(o *Options, enrolled p.EnrolledRuntimeEvidence) {
		h.configure(t, o, enrolled, api)
	})
	h.startUnknownObserver(t)
	return h
}

// All optional dependencies and their native budget configuration are installed
// before New; this hook never patches backend dependencies or creates archive,
// source, E/S, provider or termination authority records.
func (h *detachedTerminationProviderFixture) configure(t *testing.T, o *Options, enrolled p.EnrolledRuntimeEvidence, api *httptest.Server) {
	t.Helper()
	observation, err := o.Clock.Observe(context.Background())
	require.NoError(t, err)
	binding := p.TrustBinding{Namespace: o.Namespace.Root(), AuthorityID: o.PublicationTrust.AuthorityID, Target: o.PublicationTrust.Target, RestoreEpoch: o.Identity.RestoreEpoch}
	verifier, err := p.NewManagementVerifier(binding, o.PublicationTrust.Roots)
	require.NoError(t, err)
	activation, err := verifier.VerifyNodeEnrollmentRequest(enrolled.Request, observation.UTC)
	require.NoError(t, err)
	receipt, err := p.DecodeNodeEnrollmentReceipt(enrolled.Receipt)
	require.NoError(t, err)
	receipt, err = verifier.VerifyNodeEnrollmentReceipt(enrolled.Receipt, enrolled.ObserverCertificate, receipt.Claims, observation.UTC)
	require.NoError(t, err)
	identity, birth := activation.Identity(), activation.Birth()
	require.Equal(t, identity.Runtime, receipt.Claims.Runtime)
	require.Equal(t, identity.SandboxID, receipt.Claims.SandboxID)
	require.Equal(t, identity.WorkspaceHash, receipt.Claims.WorkspaceHash)
	require.Equal(t, identity.Generation, receipt.Claims.Generation)
	require.Equal(t, activation.Digest(), receipt.Claims.ActivationDigest)
	require.Equal(t, o.EnrolledPublication.ContractDigest, birth.ContractDigest)

	clockKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{5}, ed25519.SeedSize))
	config := launcherconfig.Config{Version: 2, Binding: launcherconfig.Binding(binding), Roots: o.PublicationTrust.Roots, UID: birth.UID, GID: birth.GID, NetworkAllowed: birth.NetworkAllowed, ContractDigest: birth.ContractDigest, ClockSocket: ct.ClockSocket, ClockPublicKey: clockKey.Public().(ed25519.PublicKey), ClockAudience: "launcher", MaxActive: 4, Enrollment: launcherconfig.Enrollment{NodeUID: receipt.Claims.NodeUID, NodeBootID: receipt.Claims.NodeBootID}}
	wire, err := json.Marshal(config)
	require.NoError(t, err)
	h.pod, err = k.BuildEnrolledPlainPod("runtimes", k.EnrolledPlainPodRequest{Spec: sandboxruntime.SandboxSpec{ID: identity.Runtime.ID, Image: detachedCleanupPlainImageForTest, CPU: "1", Memory: "128Mi", Disk: "1Mi", TmpDisk: "1Mi", ReadOnlyRootFS: true, RunAsUser: int64(birth.UID), NetworkEnabled: birth.NetworkAllowed, Labels: map[string]string{"sandbox.id": identity.SandboxID}}, NodeName: "node-a", NodeUID: receipt.Claims.NodeUID, NodeBootID: receipt.Claims.NodeBootID, CreationIntentID: receipt.Claims.CreationIntentID, Generation: identity.Generation, Config: wire})
	require.NoError(t, err)
	h.pod.UID, h.pod.ResourceVersion = types.UID(identity.Runtime.UID), "1"
	h.node = &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", UID: types.UID(receipt.Claims.NodeUID)}, Status: v1.NodeStatus{NodeInfo: v1.NodeSystemInfo{BootID: receipt.Claims.NodeBootID, OperatingSystem: "linux", Architecture: runtime.GOARCH}}}

	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw})
	provider, err := k.NewTaskRuntimeCleanupClient(k.TaskRuntimeCleanupOptions{Config: &rest.Config{Host: api.URL, TLSClientConfig: rest.TLSClientConfig{CAData: ca}}, Namespace: "runtimes", RequestTimeout: time.Second, MaxActive: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, provider.Close()) })
	issuerKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{8}, ed25519.SeedSize))
	issuerIdentity, err := verifier.VerifyCommandIssuerCertificate(enrolled.Request.IssuerCertificate, observation.UTC)
	require.NoError(t, err)
	h.issuer = &detachedTerminationFixtureIssuer{key: issuerKey, wire: bytes.Clone(enrolled.Request.IssuerCertificate), identity: issuerIdentity}
	o.TaskRuntimeTerminationIssuer = h.issuer
	o.LocalRuntimeRelease = &LocalRuntimeReleaseOptions{MaxActive: 1, Timeout: 20 * time.Second, Provider: provider}
	recovery := recoveryTestOptions(archiveTestDirectory(t), 1)
	o.TaskWorkspaceArchiveRecovery = &recovery
	budget := &TaskWorkspaceSyncBudgetOptions{BudgetID: uuid.NewString(), SlotsPerPartition: 1, MaxIntentBytes: 4096}
	o.TaskWorkspaceSyncRecyclingBudget = budget
	key, err := o.Namespace.Key("meta", "workspace_sync_recycling_budget")
	require.NoError(t, err)
	value, err := encodeOperationMetadata(taskWorkspaceSyncRecyclingBudgetRecord{2, *budget}, 1024)
	require.NoError(t, err)
	raw, err := clientv3.New(clientv3.Config{Endpoints: o.Endpoints, TLS: o.TLS, DialTimeout: time.Second})
	require.NoError(t, err)
	defer raw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = raw.Put(ctx, key, value)
	require.NoError(t, err)
}

func (h *detachedTerminationProviderFixture) serveKube(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet && r.URL.RawQuery == "" {
		h.gets++
		switch r.URL.Path {
		case "/api/v1/nodes/node-a":
			_ = json.NewEncoder(w).Encode(h.node)
			return
		case "/api/v1/namespaces/runtimes/pods/" + h.pod.Name:
			_ = json.NewEncoder(w).Encode(h.pod)
			return
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(metav1.Status{Status: "Failure", Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound})
			return
		}
	}
	if r.Method == http.MethodPatch {
		h.patches++
	}
	if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/namespaces/runtimes/pods/"+h.pod.Name || h.pod.DeletionTimestamp != nil {
		http.Error(w, "unexpected component effect", http.StatusConflict)
		return
	}
	var request metav1.DeleteOptions
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&request) != nil || request.Preconditions == nil || request.Preconditions.UID == nil || request.Preconditions.ResourceVersion == nil || *request.Preconditions.UID != h.pod.UID || *request.Preconditions.ResourceVersion != h.pod.ResourceVersion {
		http.Error(w, "UID/RV precondition refused", http.StatusConflict)
		return
	}
	// An actual received DELETE must follow the production durable intent, not
	// a test-seeded provider KV. Its immutable sender/effect/UID/RV are checked
	// before this API model acknowledges or changes its copied Pod state.
	_, key, _, err := h.f.b.namespace.localRuntimeReleaseKeys(h.f.ref)
	if err != nil {
		http.Error(w, "provider key refused", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	points, err := h.f.raw.Get(ctx, key)
	if err != nil || len(points.Kvs) != 1 {
		http.Error(w, "durable provider intent missing", http.StatusConflict)
		return
	}
	provider, err := decodeTaskRuntimeProviderCleanup(points.Kvs[0])
	slot := provider.Slots[1]
	if err != nil || provider.Slots[0].State != "complete" || slot.State != "intent" || slot.Claim == nil || *slot.Claim != localRuntimeClaimWire(h.f.job.claim) || slot.UID != string(h.pod.UID) || slot.ResourceVersion != h.pod.ResourceVersion || slot.EffectID == "" {
		http.Error(w, "durable provider intent binding refused", http.StatusConflict)
		return
	}
	h.deletes++
	now := metav1.Now()
	h.pod.DeletionTimestamp, h.pod.ResourceVersion = &now, "2"
	_ = json.NewEncoder(w).Encode(h.pod)
}

// The peer authenticates the real ticket against its actual reserved native
// intent and returns only Unknown. It never registers PID1, executes a kill or
// signs a terminal proof. Listener/connection work is joined by fixture cleanup.
func (h *detachedTerminationProviderFixture) startUnknownObserver(t *testing.T) {
	t.Helper()
	f := h.f
	receipt, err := p.DecodeNodeEnrollmentReceipt(f.publication.Enrollment.Receipt)
	require.NoError(t, err)
	serverKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{6}, ed25519.SeedSize))
	serverCredential, err := p.NewNodeObserverTLSCredential(serverKey, f.publication.Enrollment.ObserverCertificate, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, serverCredential.Close()) })
	config, err := p.NewNodeObserverServerTLSConfig(f.b.enrolledVerifier, serverCredential, f.b.authorityClock, receipt.Claims.NodeUID, receipt.Claims.NodeBootID)
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var joined sync.WaitGroup
	joined.Add(1)
	go func() {
		defer joined.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				secure := tls.Server(conn, config)
				if err := h.unknownRequest(secure); err != nil {
					t.Errorf("actual Unknown observer request: %v", err)
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		joined.Wait()
	})
	clientCredential, err := p.NewNodeObserverTLSCredential(h.issuer.key, h.issuer.wire, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, clientCredential.Close()) })
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	binding := p.TrustBinding{Namespace: f.b.namespace.Root(), AuthorityID: f.b.publicationAuthorityID, Target: f.b.publicationTarget, RestoreEpoch: f.b.restoreEpoch}
	h.destination, err = ct.NewTerminationDestination(ct.TerminationDestinationOptions{Address: listener.Addr().String(), Binding: binding, Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}, Clock: f.b.authorityClock, Credential: clientCredential, ObserverCertificate: f.publication.Enrollment.ObserverCertificate, NodeUID: receipt.Claims.NodeUID, NodeBootID: receipt.Claims.NodeBootID, EnrollmentDigest: receipt.Claims.EnrollmentDigest})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.destination.Close()) })
}

func (h *detachedTerminationProviderFixture) unknownRequest(conn net.Conn) error {
	wire, err := ct.ReadNodeObserverFrame(conn)
	if err != nil {
		return err
	}
	request, err := ct.DecodeNodeTerminationRequest(wire)
	if err == nil && request.Purpose == "task_runtime_terminate_query" {
		h.queries.Add(1)
	}
	if err != nil || request.Purpose != "task_runtime_terminate" || !bytes.Equal(request.IssuerCertificate, h.issuer.wire) {
		return fmt.Errorf("unexpected observer request: %w", err)
	}
	f := h.f
	key, _, _, err := f.b.namespace.localRuntimeReleaseKeys(f.ref)
	if err != nil {
		return err
	}
	actual, err := f.raw.Get(f.ctx, key)
	if err != nil || len(actual.Kvs) != 1 {
		return fmt.Errorf("actual reserved intent missing: %w", err)
	}
	intent, err := decodeLocalRuntimeTermination(actual.Kvs[0])
	if err != nil || intent.Version != 2 || intent.Phase != "reserved" {
		return fmt.Errorf("actual reserved intent refused: %w", err)
	}
	var envelope struct {
		Claims p.TaskRuntimeTerminationTicketClaims `json:"claims"`
	}
	if json.Unmarshal(request.Ticket, &envelope) != nil {
		return fmt.Errorf("ticket window missing")
	}
	claims := p.TaskRuntimeTerminationTicketClaims{Version: 1, Purpose: "task_runtime_terminate", Current: intent.Current, Partition: intent.Partition, IntentCreateRevision: actual.Kvs[0].CreateRevision, IntentModRevision: actual.Kvs[0].ModRevision, IntentDigest: workspaceSyncIntentDigest(string(actual.Kvs[0].Value)), EnrollmentDigest: intent.EnrollmentDigest, ObserverCertificateDigest: intent.ObserverCertificateDigest, NodeUID: intent.NodeUID, NodeBootID: intent.NodeBootID, NotBefore: envelope.Claims.NotBefore, NotAfter: envelope.Claims.NotAfter}
	observation, err := f.b.authorityClock.Observe(f.ctx)
	if err != nil {
		return err
	}
	if _, err := f.b.enrolledVerifier.VerifyTaskRuntimeTerminationTicket(request.Ticket, request.IssuerCertificate, claims, observation.UTC); err != nil {
		return err
	}
	h.terminates.Add(1)
	reply, err := p.EncodeNodeTerminationResult(p.NodeTerminationResult{Version: 1, Purpose: "task_runtime_termination_result", State: "unknown"})
	if err != nil {
		return err
	}
	return ct.WriteNodeObserverFrame(conn, reply)
}
