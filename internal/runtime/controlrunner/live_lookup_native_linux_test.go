//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/provisioning"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	kube "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestLiveLookupNative(t *testing.T) {
	if os.Getenv("SANDBOX_LIVE_LOOKUP_NATIVE") != "1" {
		t.Skip("requires exact owned live Lookup Linux fixture")
	}
	for _, business := range []bool{false, true} {
		t.Run(fmt.Sprintf("v%d", map[bool]int{false: 1, true: 2}[business]), func(t *testing.T) {
			// v2 uses a real detached identity; the by-ID reader cannot recreate
			// that authority from its persisted workspace hash.
			runColdCreateServiceVariantNative(t, false, business, business, false, true)
		})
	}
}

type liveLookupNativeClock struct {
	after atomic.Int32
	calls atomic.Int32
}

func (c *liveLookupNativeClock) Observe(ctx context.Context) (p.ClockObservation, error) {
	o, err := (nativeFixtureClock{}).Observe(ctx)
	if after := c.after.Load(); after > 0 && c.calls.Add(1) >= after {
		o.UTC = o.UTC.Add(2 * time.Minute)
	}
	return o, err
}

type liveLookupNativeBridgeAction struct {
	before  func() error
	discard bool
	started chan *startupBridgeFixture
}

type liveLookupNativeHooks struct {
	next   atomic.Pointer[liveLookupNativeBridgeAction]
	mu     sync.Mutex
	failed map[*startupBridgeFixture]bool
}

func (h *liveLookupNativeHooks) beforeBridge(b *startupBridgeFixture) error {
	a := h.next.Swap(nil)
	if a == nil {
		return nil
	}
	if a.before != nil {
		if err := a.before(); err != nil {
			return err
		}
	}
	if a.discard {
		b.discardStdout = true
		b.stdoutObserved = make(chan struct{})
		h.mu.Lock()
		if h.failed == nil {
			h.failed = make(map[*startupBridgeFixture]bool)
		}
		h.failed[b] = true
		h.mu.Unlock()
	}
	if a.started != nil {
		a.started <- b
	}
	return nil
}

func (h *liveLookupNativeHooks) expectedFailure(b *startupBridgeFixture) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.failed[b]
}

func requireLiveLookupFailedBridgeJoined(t *testing.T, b *startupBridgeFixture) {
	t.Helper()
	select {
	case <-b.done:
	case <-time.After(3 * time.Second):
		t.Fatal("original failed bridge handler did not join")
	}
	b.wg.Wait()
	require.True(t, b.waited, "original Cmd.Wait called after original pipe owners joined")
	require.Error(t, b.result, "actual discarded TLS response cannot become successful bridge")
	t.Logf("live lookup negative original bridge Cmd.Wait=%v; original relay IO joined", b.waitErr)
}

func exerciseLiveLookupNative(t *testing.T, ctx context.Context, service *provisioning.PlainColdService, backend *state.Backend, admin *clientv3.Client, backendOptions state.Options, options provisioning.PlainColdOptions, ns state.Namespace, w state.WorkspaceIdentity, request provisioning.PlainColdRequest, created provisioning.PlainColdResult, hooks *liveLookupNativeHooks, clock *liveLookupNativeClock) {
	t.Helper()
	before, err := admin.Get(ctx, ns.Root(), clientv3.WithPrefix())
	require.NoError(t, err)
	got, err := service.Lookup(ctx, request.SandboxID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, request.RequestID, got.RequestID)
	require.Equal(t, request.IntentID, got.IntentID)
	require.Equal(t, request.SandboxID, got.SandboxID)
	require.Equal(t, w.Hash(), got.WorkspaceHash)
	require.Equal(t, created.Runtime, got.Runtime)
	require.Equal(t, request.Business, got.Business)
	dispatch, err := backend.LoadRuntimeDispatch(ctx, w, request.IntentID)
	require.NoError(t, err)
	var original struct {
		CreatedAt time.Time `json:"created_at"`
	}
	require.NoError(t, json.Unmarshal(dispatch.Payload, &original))
	require.Equal(t, original.CreatedAt, got.CreatedAt, "immutable dispatch is the original creation timestamp")
	after, err := admin.Get(ctx, ns.Root(), clientv3.WithPrefix())
	require.NoError(t, err)
	require.Equal(t, before.Kvs, after.Kvs, "full successful Lookup has no etcd writes")
	if got.Business != nil {
		wantCPU := request.Business.Resolved.Resources.CPU
		got.Business.Resolved.Resources.CPU = "mutated"
		again, err := service.Lookup(ctx, request.SandboxID)
		require.NoError(t, err)
		require.Equal(t, wantCPU, again.Business.Resolved.Resources.CPU, "owned returned configuration cannot mutate future reads")
	}
	missing, err := service.Lookup(ctx, uuid.NewString())
	require.ErrorIs(t, err, provisioning.ErrPlainColdNotFound)
	require.Nil(t, missing)
	otherOptions := options
	otherOptions.Disk = "2Mi"
	if options.Business != nil {
		otherBusiness := *options.Business
		otherBusiness.Defaults.Resources.Disk = "2Mi"
		otherOptions.Business = &otherBusiness
	}
	otherProfile, err := provisioning.NewPlainColdService(ctx, otherOptions)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, otherProfile.Close()) })
	wrongProfile, err := otherProfile.Lookup(ctx, request.SandboxID)
	require.ErrorIs(t, err, provisioning.ErrPlainColdProfile, "fixed deployment resources cannot migrate an original sandbox profile")
	require.Nil(t, wrongProfile)
	require.NoError(t, otherProfile.Close())
	view, err := backend.ReadSandboxView(ctx, request.SandboxID)
	require.NoError(t, err)
	copy := *view
	for _, bad := range []*state.SandboxReadView{nil, {}, &copy} {
		value, err := backend.ReadSandboxRuntime(ctx, bad)
		require.ErrorIs(t, err, state.ErrInvalidRecord)
		require.Nil(t, value)
	}
	foreign, err := state.New(ctx, backendOptions)
	require.NoError(t, err)
	value, err := foreign.ReadSandboxRuntime(ctx, view)
	require.ErrorIs(t, err, state.ErrInvalidRecord)
	require.Nil(t, value)
	require.NoError(t, foreign.Close())
	_, err = backend.LoadRuntimePublicationEnrolled(ctx, state.WorkspaceIdentity{}, request.IntentID)
	require.Error(t, err)
	_, err = backend.LoadRuntimeDispatch(ctx, state.WorkspaceIdentity{}, request.IntentID)
	require.Error(t, err)
	read, err := backend.ReadSandboxRuntime(ctx, view)
	require.NoError(t, err)
	require.Equal(t, dispatch.Payload, read.Dispatch.Payload)
	read.Dispatch.Payload[0] = 'x'
	certificateBefore := bytes.Clone(read.Publication.Enrollment.Request.RuntimeCertificate)
	read.Publication.Enrollment.Request.RuntimeCertificate[0] = 'x'
	read, err = backend.ReadSandboxRuntime(ctx, view)
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(read.Dispatch.Payload, []byte("{")))
	require.Equal(t, certificateBefore, read.Publication.Enrollment.Request.RuntimeCertificate)
	controlKey := ns.Root() + fmt.Sprintf("p/%02x/controls/%s", w.Partition(), request.SandboxID)
	controlKV, err := admin.Get(ctx, controlKey)
	require.NoError(t, err)
	require.Len(t, controlKV.Kvs, 1)
	originalControl := bytes.Clone(controlKV.Kvs[0].Value)
	putControl := func(wire []byte) error { _, err := admin.Put(ctx, controlKey, string(wire)); return err }
	require.NoError(t, putControl(originalControl))
	value, err = backend.ReadSandboxRuntime(ctx, view)
	require.ErrorIs(t, err, state.ErrConflict)
	require.Nil(t, value)
	var control state.SandboxControlRecord
	require.NoError(t, json.Unmarshal(originalControl, &control))
	control.ExpiresAt = control.ExpiresAt.Add(10 * time.Second)
	renewed, err := json.Marshal(control)
	require.NoError(t, err)
	require.NoError(t, putControl(renewed))
	got, err = service.Lookup(ctx, request.SandboxID)
	require.NoError(t, err)
	require.Equal(t, original.CreatedAt, got.CreatedAt)
	require.Equal(t, control.ExpiresAt, got.ExpiresAt, "current expiry is independent from original publication expiry")
	control.ExpiresAt = time.Now().UTC().Add(500 * time.Millisecond)
	expired, err := json.Marshal(control)
	require.NoError(t, err)
	require.NoError(t, putControl(expired))
	got, err = service.Lookup(ctx, request.SandboxID)
	require.ErrorIs(t, err, provisioning.ErrPlainColdUnavailable)
	require.Nil(t, got)
	require.NoError(t, putControl(renewed))
	// Observe #1 is prehealth TTL, #2 authenticates client TLS, and #3 is
	// the posthealth TTL. This controlled service clock never changes target time.
	clock.calls.Store(0)
	clock.after.Store(3)
	got, err = service.Lookup(ctx, request.SandboxID)
	clock.after.Store(0)
	require.ErrorIs(t, err, provisioning.ErrPlainColdUnavailable)
	require.Nil(t, got)
	require.GreaterOrEqual(t, clock.calls.Load(), int32(3))
	control.Phase = state.PhaseCleanupPending
	control.ExpiresAt = time.Now().UTC().Add(time.Minute)
	inactive, err := json.Marshal(control)
	require.NoError(t, err)
	require.NoError(t, putControl(inactive))
	got, err = service.Lookup(ctx, request.SandboxID)
	require.ErrorIs(t, err, provisioning.ErrPlainColdUnavailable)
	require.Nil(t, got)
	require.NoError(t, putControl(renewed))
	hooks.next.Store(&liveLookupNativeBridgeAction{before: func() error { return putControl(renewed) }})
	got, err = service.Lookup(ctx, request.SandboxID)
	require.ErrorIs(t, err, state.ErrConflict, "same-value metadata rewrite during actual health bridge invalidates final view")
	require.Nil(t, got)
	for _, mode := range []string{"cancel-broken-bridge", "service-close"} {
		secondary, err := provisioning.NewPlainColdService(ctx, options)
		require.NoError(t, err)
		started := make(chan *startupBridgeFixture, 1)
		hooks.next.Store(&liveLookupNativeBridgeAction{discard: true, started: started})
		callCtx, cancel := context.WithCancel(ctx)
		type lookupAnswer struct {
			value *provisioning.PlainColdLookupResult
			err   error
		}
		done := make(chan lookupAnswer, 1)
		go func() {
			value, err := secondary.Lookup(callCtx, request.SandboxID)
			done <- lookupAnswer{value: value, err: err}
		}()
		var joined sync.Once
		var answer lookupAnswer
		join := func() { joined.Do(func() { answer = <-done }) }
		t.Cleanup(func() { cancel(); require.NoError(t, secondary.Close()); join() })
		var b *startupBridgeFixture
		select {
		case b = <-started:
		case <-ctx.Done():
			t.Fatal("actual broken bridge not entered")
		}
		select {
		case <-b.stdoutObserved:
		case <-ctx.Done():
			t.Fatal("original bridge child never emitted real TLS stdout")
		}
		if mode == "cancel-broken-bridge" {
			cancel()
		} else {
			require.NoError(t, secondary.Close())
		}
		join()
		require.Error(t, answer.err, "canceled or closed original service cannot return new success")
		require.Nil(t, answer.value, "canceled or closed original service cannot return metadata as success")
		cancel()
		require.NoError(t, secondary.Close())
		requireLiveLookupFailedBridgeJoined(t, b)
	}
	rejectLiveLookupUnauthenticatedEOF(t, ctx, options, read.Publication, control, created.Runtime)
	closeLiveLookupNativeGate(t, ctx, options, read.Publication, control, created.Runtime)
	got, err = service.Lookup(ctx, request.SandboxID)
	require.ErrorIs(t, err, provisioning.ErrPlainColdUnavailable, "actual target gate closure overrides active etcd metadata")
	require.Nil(t, got)
	t.Log("live Lookup actual v1/v2 metadata, owned history/config, TTL before/after health, final state fence, target gate and Close/IO join verified")
}

// This calls the original protected Supervisor through the original bridge.
// A correct authenticated request frame must still send TLS close_notify.
func rejectLiveLookupUnauthenticatedEOF(t *testing.T, ctx context.Context, o provisioning.PlainColdOptions, publication *state.RuntimeEnrolledPublicationEntry, control state.SandboxControlRecord, runtime p.RuntimeReference) {
	t.Helper()
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cfg, err := launcherconfig.Decode(o.LauncherConfig)
	require.NoError(t, err)
	verifier, err := p.NewManagementVerifier(cfg.TrustBinding(), cfg.Roots)
	require.NoError(t, err)
	credential, err := p.NewManagementTLSCredential(o.IssuerPrivateKey, o.IssuerCertificate, "command_issuer")
	require.NoError(t, err)
	defer credential.Close()
	identity := p.RuntimeIdentityContext{SandboxID: control.SandboxID, WorkspaceHash: control.WorkspaceHash, Generation: control.Generation, Runtime: runtime}
	config, err := p.NewManagementTLSConfig(p.ManagementTLSOptions{Verifier: verifier, Clock: nativeFixtureClock{}, Credential: credential, PeerCertificate: publication.Enrollment.Request.RuntimeCertificate, PeerRuntime: &identity})
	require.NoError(t, err)
	bridge, err := kube.NewEnrolledBridgeClient(o.API)
	require.NoError(t, err)
	defer bridge.Close()
	target := kube.EnrolledBridgeTarget{PodName: runtime.ID, PodUID: runtime.UID, NodeName: o.NodeName, NodeUID: o.NodeUID, NodeBootID: o.NodeBootID, LogicalID: control.SandboxID, CreationIntentID: control.IntentID, Generation: control.Generation, Authority: cfg.TrustBinding(), ContractDigest: cfg.ContractDigest, UID: cfg.UID, GID: cfg.GID, NetworkAllowed: cfg.NetworkAllowed}
	raw, err := bridge.Dial(bounded, target)
	require.NoError(t, err)
	defer raw.Close()
	deadline, _ := bounded.Deadline()
	require.NoError(t, raw.SetDeadline(deadline))
	conn := tls.Client(raw, config)
	defer conn.Close()
	require.NoError(t, conn.HandshakeContext(bounded))
	var nonce [32]byte
	_, err = rand.Read(nonce[:])
	require.NoError(t, err)
	request := ct.RuntimeHealthEnvelope{Version: 1, Purpose: ct.RuntimeHealthPurpose, Identity: identity, DataGateEpoch: control.DataGateEpoch, Nonce: hex.EncodeToString(nonce[:])}
	require.NoError(t, ct.WriteRuntimeHealthRequest(conn, request))
	half, ok := raw.(interface{ CloseWrite() error })
	require.True(t, ok)
	// Deliberately omit conn.CloseWrite: only unauthenticated raw FIN is sent.
	require.NoError(t, half.CloseWrite())
	response, healthErr := ct.ReadRuntimeHealthResponse(conn)
	// Finish the original adapter/status even on the old invalid-success path,
	// so the RED cause cannot become an auxiliary connection broken pipe.
	var extra [1]byte
	n, terminalErr := raw.Read(extra[:])
	require.Zero(t, n)
	require.ErrorIs(t, terminalErr, io.EOF)
	if healthErr == nil {
		require.Equal(t, identity, response.Identity)
		require.Equal(t, request.Nonce, response.Nonce)
		require.Equal(t, "open", response.GateState)
		t.Log("original protected handler returned valid open health after raw FIN without TLS close_notify")
	}
	require.Error(t, healthErr, "original protected health handler must reject unauthenticated request EOF")
	require.Equal(t, ct.RuntimeHealthResponse{}, response)
}

func closeLiveLookupNativeGate(t *testing.T, ctx context.Context, o provisioning.PlainColdOptions, publication *state.RuntimeEnrolledPublicationEntry, control state.SandboxControlRecord, runtime p.RuntimeReference) {
	t.Helper()
	cfg, err := launcherconfig.Decode(o.LauncherConfig)
	require.NoError(t, err)
	verifier, err := p.NewManagementVerifier(cfg.TrustBinding(), cfg.Roots)
	require.NoError(t, err)
	credential, err := p.NewManagementTLSCredential(o.IssuerPrivateKey, o.IssuerCertificate, "command_issuer")
	require.NoError(t, err)
	defer credential.Close()
	bridge, err := kube.NewEnrolledBridgeClient(o.API)
	require.NoError(t, err)
	defer bridge.Close()
	identity := p.RuntimeIdentityContext{SandboxID: control.SandboxID, WorkspaceHash: control.WorkspaceHash, Generation: control.Generation, Runtime: runtime}
	target := kube.EnrolledBridgeTarget{PodName: runtime.ID, PodUID: runtime.UID, NodeName: o.NodeName, NodeUID: o.NodeUID, NodeBootID: o.NodeBootID, LogicalID: control.SandboxID, CreationIntentID: control.IntentID, Generation: control.Generation, Authority: cfg.TrustBinding(), ContractDigest: cfg.ContractDigest, UID: cfg.UID, GID: cfg.GID, NetworkAllowed: cfg.NetworkAllowed}
	var originalRaw net.Conn
	destination, err := ct.NewDestination(ct.DestinationOptions{Identity: identity, RuntimeCertificate: publication.Enrollment.Request.RuntimeCertificate, Credential: credential, Verifier: verifier, Clock: nativeFixtureClock{}, Dial: func(ctx context.Context) (net.Conn, error) {
		raw, err := bridge.Dial(ctx, target)
		originalRaw = raw
		return raw, err
	}})
	require.NoError(t, err)
	now := time.Now().UTC()
	issuer, err := verifier.VerifyCommandIssuerCertificate(o.IssuerCertificate, now)
	require.NoError(t, err)
	binding := cfg.TrustBinding()
	c := p.TaskCloseDataContext{Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, IssuerCertificateID: issuer.CertificateID(), IssuerCertificateDigest: issuer.Digest(), CommandID: uuid.NewString(), TaskID: uuid.NewString(), TaskDigest: strings.Repeat("a", 64), ClaimID: uuid.NewString(), WorkerID: "live-lookup", SandboxID: control.SandboxID, WorkspaceHash: control.WorkspaceHash, Generation: control.Generation, DataGateEpoch: control.DataGateEpoch, ControlRevision: 100, ClaimCreateRevision: 101, LeaseID: 1, Runtime: runtime}
	before, after := now.Add(-time.Second), now.Add(10*time.Second)
	ticket, err := p.SignTaskCloseDataTicket(o.IssuerPrivateKey, issuer, p.TaskCloseDataTicketClaims{Version: 1, Purpose: "task_close_data", Context: c, NotBefore: before, NotAfter: after})
	require.NoError(t, err)
	hash := sha256.Sum256(ticket)
	digest := hex.EncodeToString(hash[:])
	session, err := destination.Open(ctx)
	require.NoError(t, err)
	defer session.Close()
	require.NoError(t, session.SendTaskClose(ctx, ct.TaskCloseEnvelope{Version: 1, Purpose: "task_close_data", Context: c, TicketDigest: digest, Ticket: ticket, IssuerCertificate: o.IssuerCertificate}))
	kind, wire, err := session.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, ct.EventReceipt, kind)
	_, err = destination.VerifyTaskCloseReceipt(verifier, wire, c, digest, before, after, time.Now().UTC())
	require.NoError(t, err, "real signed TaskCloseData produced authenticated current target gate closure")
	// A signed legacy receipt precedes the original bridge's process exit and
	// WebSocket terminal status. Consume that EOF before Session.Close aborts IO.
	kind, wire, err = session.Read(ctx)
	require.ErrorIs(t, err, io.EOF, "original TaskClose bridge completed its terminal status")
	require.Zero(t, kind, "TaskClose produced no extra event")
	require.Empty(t, wire)
	// TLS may finish before the finite adapter's process/status owner. Retain
	// that exact original raw connection locally until its terminal EOF too.
	require.NotNil(t, originalRaw)
	deadline, bounded := ctx.Deadline()
	require.True(t, bounded)
	require.NoError(t, originalRaw.SetReadDeadline(deadline))
	var extra [1]byte
	n, terminalErr := originalRaw.Read(extra[:])
	require.Zero(t, n, "original TaskClose adapter produced no trailing bytes")
	require.ErrorIs(t, terminalErr, io.EOF, "original TaskClose adapter/status reached successful EOF")
}
