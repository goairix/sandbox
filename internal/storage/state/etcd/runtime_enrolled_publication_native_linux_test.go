//go:build linux && (amd64 || arm64)

package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type enrolledNativePublicationFixture struct {
	b          *Backend
	raw        *clientv3.Client
	claim      *CreationClaim
	options    Options
	proof      []byte
	enrollment p.EnrolledRuntimeEvidence
}

type enrolledNativeTraceKV struct {
	clientv3.KV
	puts, deletes atomic.Int64
}

type enrolledNativeTraceTxn struct {
	clientv3.Txn
	owner *enrolledNativeTraceKV
}

func (k *enrolledNativeTraceKV) Txn(ctx context.Context) clientv3.Txn {
	return &enrolledNativeTraceTxn{Txn: k.KV.Txn(ctx), owner: k}
}
func (x *enrolledNativeTraceTxn) If(c ...clientv3.Cmp) clientv3.Txn { x.Txn = x.Txn.If(c...); return x }
func (x *enrolledNativeTraceTxn) count(ops []clientv3.Op) {
	for _, op := range ops {
		if op.IsPut() {
			x.owner.puts.Add(1)
		}
		if op.IsDelete() {
			x.owner.deletes.Add(1)
		}
	}
}
func (x *enrolledNativeTraceTxn) Then(ops ...clientv3.Op) clientv3.Txn {
	x.count(ops)
	x.Txn = x.Txn.Then(ops...)
	return x
}
func (x *enrolledNativeTraceTxn) Else(ops ...clientv3.Op) clientv3.Txn {
	x.count(ops)
	x.Txn = x.Txn.Else(ops...)
	return x
}

// This test ELF covers actual native metadata CAS/reply loss. Its test-issued
// crypto material does not claim a kernel observation. The separate runner ELF
// supplies the actual Supervisor + observer + publication chain in this fixture.
func newEnrolledNativePublicationFixture(t *testing.T, maximum bool, coldPreparation ...bool) enrolledNativePublicationFixture {
	t.Helper()
	cold := len(coldPreparation) == 1 && coldPreparation[0]
	enabled, directory := "SANDBOX_ENROLLED_PUBLICATION_NATIVE", "ENROLLED_NATIVE_TLS_DIRECTORY"
	if cold {
		enabled, directory = "SANDBOX_COLD_CREATE_NATIVE", "COLD_CREATE_TLS_DIRECTORY"
	}
	if os.Getenv(enabled) != "1" {
		t.Skip("requires exact owned enrolled TLS fixture")
	}
	dir := os.Getenv(directory)
	require.NotEmpty(t, dir)
	certificate, err := tls.LoadX509KeyPair(filepath.Join(dir, "server.pem"), filepath.Join(dir, "server-key.pem"))
	require.NoError(t, err)
	ca, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	require.NoError(t, err)
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(ca))
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{certificate}}
	raw, err := clientv3.New(clientv3.Config{Endpoints: []string{"https://127.0.0.1:2379"}, TLS: tlsConfig, DialTimeout: time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	ctx := context.Background()
	status, err := raw.Status(ctx, "https://127.0.0.1:2379")
	require.NoError(t, err)
	prefix, scope, cell := "/sandbox/enrolled-native", "fault", uuid.NewString()
	if cold {
		prefix = "/sandbox/cold-create-native"
	}
	if maximum {
		prefix, scope, cell = "/sandbox/enrolled-native/"+strings.Repeat("p", 253-len("/sandbox/enrolled-native/")), strings.Repeat("s", 128), strings.Repeat("c", 128)
	}
	n, err := NewNamespace(prefix, scope, cell)
	require.NoError(t, err)
	if maximum {
		require.Len(t, n.Root(), 512)
	}
	identity := Identity{SchemaVersion: 1, Prefix: prefix, AuthorityID: scope, Cell: cell, ClusterID: status.Header.ClusterId, StorageID: "enrolled-storage", RuntimeID: "enrolled-runtime", RestoreEpoch: "restore"}
	identityWire, err := json.Marshal(identity)
	require.NoError(t, err)
	ik, _ := n.Key("meta", "identity")
	rk, _ := n.Key("meta", "restore_epoch")
	_, err = raw.Put(ctx, ik, string(identityWire))
	require.NoError(t, err)
	_, err = raw.Put(ctx, rk, identity.RestoreEpoch)
	require.NoError(t, err)
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	options := Options{Endpoints: raw.Endpoints(), Namespace: n, Identity: identity, TLS: tlsConfig, RequestTimeout: time.Second, PublicationTrust: &RuntimePublicationTrust{AuthorityID: identity.RuntimeID, Target: "kubernetes", Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}, Clock: goodAuthorityClock(), EnrolledPublication: &EnrolledPublicationOptions{ContractDigest: strings.Repeat("b", 64), MaxActive: 4}}
	if cold {
		options.PreparationInventory = &PreparationInventoryOptions{}
		options.PreparationInventory.QuotaSlots[7], options.PreparationInventory.QuotaSlots[8] = 1, 1
	}
	b, err := New(ctx, options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	if cold {
		coldNativeInitialize(t, b)
	}
	w, err := NewWorkspaceIdentity("s3", identity.StorageID, "bucket", cell+"/")
	require.NoError(t, err)
	input := AcquireIntentInput{Workspace: w, Principal: "native", IdempotencyKey: uuid.NewString(), RequestID: uuid.NewString(), IntentID: uuid.NewString(), SandboxID: uuid.NewString(), SnapshotVersion: uuid.NewString(), Payload: json.RawMessage(`{"html":"<>&中>"}`), Now: time.Now().UTC(), TTL: time.Hour}
	acquired, err := b.AcquireIntent(ctx, input)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, acquired.Outcome)
	c, err := b.ClaimCreation(ctx, w, input.IntentID, "native-worker", 30*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.ReleaseCreationClaim(context.Background(), c) })
	if cold {
		reserved, err := b.ReserveColdPreparation(ctx, c, 7)
		require.NoError(t, err)
		require.Equal(t, OutcomeCommitted, reserved.Outcome)
	}
	dispatch, err := b.DeclareRuntimeDispatch(ctx, c, RuntimeDispatchInput{Kind: RuntimeDispatchCreate, Target: "kubernetes", Payload: json.RawMessage(`{"html":"<>&中>"}`)})
	require.NoError(t, err)
	runtimeKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32))
	runtime := RuntimeReference{ID: "pod", UID: uuid.NewString(), BootID: uuid.NewString()}
	if cold {
		attempt, err := b.BeginColdCreateAttempt(ctx, c)
		require.NoError(t, err)
		require.NotNil(t, attempt.Permit)
		_, err = b.RecordColdCreateObservation(ctx, c, runtime.UID)
		require.NoError(t, err)
	}
	cert := preparationCertificate(t, b, dispatch.Entry.Record, root, "plain", runtime)
	_, err = b.BindRuntime(ctx, c, cert)
	require.NoError(t, err)
	_, err = b.ConsumeRuntimeMount(ctx, c)
	require.NoError(t, err)
	now := time.Now().UTC()
	nb, na := now.Add(-30*time.Second), now.Add(5*time.Minute)
	binding := p.TrustBinding{Namespace: n.Root(), AuthorityID: identity.RuntimeID, Target: "kubernetes", RestoreEpoch: identity.RestoreEpoch}
	verifier, err := p.NewManagementVerifier(binding, []ed25519.PublicKey{root.Public().(ed25519.PublicKey)})
	require.NoError(t, err)
	runtimeContext := p.RuntimeIdentityContext{SandboxID: c.reference.SandboxID, WorkspaceHash: w.Hash(), Generation: c.reference.Generation, Runtime: p.RuntimeReference(runtime)}
	runtimeCertificate, err := p.SignRuntimeIdentityCertificate(root, p.RuntimeIdentityCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "runtime_receipt", Namespace: n.Root(), AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: runtimeKey.Public().(ed25519.PublicKey), NotBefore: nb, NotAfter: na, SandboxID: runtimeContext.SandboxID, WorkspaceHash: runtimeContext.WorkspaceHash, Generation: runtimeContext.Generation, Runtime: runtimeContext.Runtime})
	require.NoError(t, err)
	issuerKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{8}, 32))
	issuerCertificate, err := p.SignCommandIssuerCertificate(root, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: n.Root(), AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: issuerKey.Public().(ed25519.PublicKey), NotBefore: nb, NotAfter: na})
	require.NoError(t, err)
	rd, _ := snapshotDigest(runtimeCertificate)
	id, _ := snapshotDigest(issuerCertificate)
	activation, err := p.SignTargetActivation(root, p.TargetActivationClaims{Version: 1, Role: "target_activation", ActivationID: uuid.NewString(), Binding: binding, Identity: runtimeContext, DataGateEpoch: c.control.DataGateEpoch, UID: 1000, GID: 1000, ContractDigest: options.EnrolledPublication.ContractDigest, RuntimeCertificateDigest: rd, IssuerCertificateDigest: id, NotBefore: nb, NotAfter: na})
	require.NoError(t, err)
	observerKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{6}, 32))
	node, boot := uuid.NewString(), uuid.NewString()
	observerCertificate, err := p.SignNodeObserverCertificate(root, p.NodeObserverCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "node_observer", Namespace: n.Root(), AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, NodeUID: node, NodeBootID: boot, PublicKey: observerKey.Public().(ed25519.PublicKey), NotBefore: nb, NotAfter: na})
	require.NoError(t, err)
	observer, err := verifier.VerifyNodeObserverCertificate(observerCertificate, node, boot, now)
	require.NoError(t, err)
	ad, _ := snapshotDigest(activation)
	receipt, err := p.SignNodeEnrollmentReceipt(observerKey, observer, p.NodeEnrollmentReceiptClaims{Version: 1, Purpose: "runtime_node_enrolled", EnrollmentDigest: strings.Repeat("e", 64), ObserverCertificateDigest: observer.Digest(), NodeUID: node, NodeBootID: boot, ActivationDigest: ad, Runtime: runtimeContext.Runtime, SandboxID: runtimeContext.SandboxID, WorkspaceHash: runtimeContext.WorkspaceHash, Generation: runtimeContext.Generation, CreationIntentID: c.reference.IntentID, ObservedAt: now})
	require.NoError(t, err)
	material := p.EnrolledRuntimeEvidence{Request: p.NodeEnrollmentRequest{Version: 1, Purpose: "runtime_node_enrollment", RuntimeCertificate: runtimeCertificate, Activation: activation, IssuerCertificate: issuerCertificate}, ObserverCertificate: observerCertificate, Receipt: receipt.Wire()}
	cd, _ := snapshotDigest(cert)
	ready := p.EnrolledReadyReceiptClaims{ReadyReceiptClaims: p.ReadyReceiptClaims{Version: 2, CertificateDigest: cd, Claim: p.ClaimReference{ClaimID: c.reference.ClaimID, CreateRevision: c.reference.CreateRevision, LeaseID: c.reference.LeaseID}, DataGateEpoch: c.control.DataGateEpoch, GateState: "open", ObservedAt: now, ValidUntil: now.Add(5 * time.Second)}, EnrollmentDigest: receipt.Claims.EnrollmentDigest, ObserverCertificateDigest: observer.Digest(), ActivationDigest: ad}
	proof, err := p.SignEnrolledReadyReceipt(runtimeKey, cert, ready)
	require.NoError(t, err)
	return enrolledNativePublicationFixture{b: b, raw: raw, claim: c, options: options, proof: proof, enrollment: material}
}

func TestEnrolledPublicationNative(t *testing.T) {
	t.Run("lost-ACK-frozen-readonly-cold", func(t *testing.T) {
		f := newEnrolledNativePublicationFixture(t, false)
		b, c := f.b, f.claim
		jk, _, err := b.namespace.runtimePublicationKeys(c.reference.Partition, c.reference.IntentID)
		require.NoError(t, err)
		lease := &creationFaultLease{Lease: b.client.Lease}
		b.client.Lease = lease
		var commits atomic.Int32
		b.client.KV = &faultKV{KV: b.client.KV, match: putsKey(jk), after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
			require.NoError(t, err)
			require.True(t, r.Succeeded)
			commits.Add(1)
			return nil, context.DeadlineExceeded
		}}
		trace := &enrolledNativeTraceKV{KV: b.client.KV}
		b.client.KV = trace
		first, err := b.PublishRuntimeEnrolled(context.Background(), c, f.proof, f.enrollment)
		require.ErrorIs(t, err, ErrOutcomeUnknown)
		require.Equal(t, OutcomeUnknown, first.Outcome)
		require.NotEmpty(t, first.Reference.Digest)
		require.NoError(t, first.GuardCleanupError)
		grants, revokes := lease.grants.Load(), lease.revokes.Load()
		puts, deletes := trace.puts.Load(), trace.deletes.Load()
		retry, err := b.PublishRuntimeEnrolled(context.Background(), c, f.proof, f.enrollment)
		require.NoError(t, err)
		require.Equal(t, OutcomeCommitted, retry.Outcome)
		require.Equal(t, PublishReplay, retry.Disposition)
		require.Equal(t, first.Reference, retry.Reference)
		require.Equal(t, int32(1), commits.Load())
		require.Equal(t, grants, lease.grants.Load())
		require.Equal(t, revokes, lease.revokes.Load())
		require.Equal(t, puts, trace.puts.Load())
		require.Equal(t, deletes, trace.deletes.Load())
		require.Zero(t, lease.keeps.Load())
		cold, err := New(context.Background(), f.options)
		require.NoError(t, err)
		defer cold.Close()
		cold.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
			t.Error("historical lookup observed current clock")
			return ClockObservation{}, context.Canceled
		})
		coldTrace := &enrolledNativeTraceKV{KV: cold.client.KV}
		cold.client.KV = coldTrace
		entry, err := cold.LoadRuntimePublicationEnrolled(context.Background(), c.workspace, c.reference.IntentID)
		require.NoError(t, err)
		require.Equal(t, retry.Entry, entry)
		outcome, err := cold.ReadRuntimeEnrolledPublicationAttempt(context.Background(), first.Reference)
		require.NoError(t, err)
		require.Equal(t, OutcomeCommitted, outcome)
		require.Zero(t, coldTrace.puts.Load())
		require.Zero(t, coldTrace.deletes.Load())
		t.Log("real native commit ACK dropped; original reference retained; replay Grant/Put/Delete/KeepAlive zero; independent Backend cold validates same receipt without current clock")
	})
	t.Run("lost-guard-ACK-original-attempt-only", func(t *testing.T) {
		f := newEnrolledNativePublicationFixture(t, false)
		lease := &creationFaultLease{Lease: f.b.client.Lease}
		f.b.client.Lease = lease
		f.b.client.KV = &faultKV{KV: f.b.client.KV, match: func(ops []clientv3.Op) bool {
			return len(ops) == 1 && ops[0].IsPut() && strings.Contains(string(ops[0].KeyBytes()), "/attempts/")
		}, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
			require.NoError(t, err)
			require.True(t, r.Succeeded)
			return nil, context.DeadlineExceeded
		}}
		trace := &enrolledNativeTraceKV{KV: f.b.client.KV}
		f.b.client.KV = trace
		first, err := f.b.PublishRuntimeEnrolled(context.Background(), f.claim, f.proof, f.enrollment)
		require.ErrorIs(t, err, ErrOutcomeUnknown)
		require.NoError(t, first.GuardCleanupError)
		require.NotEmpty(t, first.Reference.Digest)
		grants, puts, deletes := lease.grants.Load(), trace.puts.Load(), trace.deletes.Load()
		retry, err := f.b.PublishRuntimeEnrolled(context.Background(), f.claim, f.proof, f.enrollment)
		require.NoError(t, err)
		require.Equal(t, OutcomeUnknown, retry.Outcome)
		require.Equal(t, first.Reference, retry.Reference)
		require.Equal(t, grants, lease.grants.Load())
		require.Equal(t, puts, trace.puts.Load())
		require.Equal(t, deletes, trace.deletes.Load())
		require.Zero(t, lease.keeps.Load())
	})
	for _, fault := range []string{"partial", "leased", "mutated", "foreign-header", "restore"} {
		t.Run("cold-"+fault, func(t *testing.T) {
			f := newEnrolledNativePublicationFixture(t, false)
			published, err := f.b.PublishRuntimeEnrolled(context.Background(), f.claim, f.proof, f.enrollment)
			require.NoError(t, err)
			require.Equal(t, OutcomeCommitted, published.Outcome)
			jk, _, _ := f.b.namespace.runtimePublicationKeys(f.claim.reference.Partition, f.claim.reference.IntentID)
			point, err := f.raw.Get(context.Background(), jk)
			require.NoError(t, err)
			switch fault {
			case "partial":
				_, err = f.raw.Delete(context.Background(), jk)
			case "mutated":
				_, err = f.raw.Put(context.Background(), jk, string(point.Kvs[0].Value))
			case "leased":
				lease, e := f.raw.Grant(context.Background(), 5)
				require.NoError(t, e)
				_, err = f.raw.Put(context.Background(), jk, string(point.Kvs[0].Value), clientv3.WithLease(lease.ID))
			case "restore":
				_, err = f.raw.Put(context.Background(), f.b.restoreKey, "different-restore")
			case "foreign-header":
				f.b.client.KV = &faultKV{KV: f.b.client.KV, match: func([]clientv3.Op) bool { return true }, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
					require.NoError(t, err)
					r.Header.ClusterId++
					return r, nil
				}}
			}
			require.NoError(t, err)
			entry, err := f.b.LoadRuntimePublicationEnrolled(context.Background(), f.claim.workspace, f.claim.reference.IntentID)
			require.Error(t, err)
			require.Nil(t, entry)
		})
	}
	for pin := 0; pin < 9; pin++ {
		t.Run("history-pin-"+string(rune('0'+pin)), func(t *testing.T) {
			f := newEnrolledNativePublicationFixture(t, false)
			bundle, err := f.b.loadEnrolledPreparation(context.Background(), f.claim.workspace, f.claim.reference.IntentID)
			require.NoError(t, err)
			all := append(append(bundle.DispatchKVs, bundle.BindingKVs...), bundle.MountKVs...)
			target := all[pin]
			jk, _, _ := f.b.namespace.runtimePublicationKeys(f.claim.reference.Partition, f.claim.reference.IntentID)
			f.b.client.KV = &faultKV{KV: f.b.client.KV, match: putsKey(jk), before: func() {
				_, err := f.raw.Put(context.Background(), string(target.Key), string(target.Value))
				require.NoError(t, err)
			}}
			result, err := f.b.PublishRuntimeEnrolled(context.Background(), f.claim, f.proof, f.enrollment)
			require.NoError(t, err)
			require.Equal(t, OutcomeUnknown, result.Outcome)
			require.Nil(t, result.Entry)
			control, err := f.b.LoadControl(context.Background(), f.claim.reference.Partition, f.claim.reference.SandboxID)
			require.NoError(t, err)
			require.NotEqual(t, PhaseActive, control.Phase)
		})
	}
	t.Run("native-current-claim-loss", func(t *testing.T) {
		f := newEnrolledNativePublicationFixture(t, false)
		jk, _, _ := f.b.namespace.runtimePublicationKeys(f.claim.reference.Partition, f.claim.reference.IntentID)
		f.b.client.KV = &faultKV{KV: f.b.client.KV, match: putsKey(jk), before: func() {
			_, err := f.raw.Revoke(context.Background(), clientv3.LeaseID(f.claim.reference.LeaseID))
			require.NoError(t, err)
		}}
		result, err := f.b.PublishRuntimeEnrolled(context.Background(), f.claim, f.proof, f.enrollment)
		require.NoError(t, err)
		require.Equal(t, OutcomeUnknown, result.Outcome)
		require.Nil(t, result.Entry)
		control, err := f.b.LoadControl(context.Background(), f.claim.reference.Partition, f.claim.reference.SandboxID)
		require.NoError(t, err)
		require.NotEqual(t, PhaseActive, control.Phase)
	})
	t.Run("maximum-namespace-real64nodes", func(t *testing.T) {
		f := newEnrolledNativePublicationFixture(t, true)
		jk, _, _ := f.b.namespace.runtimePublicationKeys(f.claim.reference.Partition, f.claim.reference.IntentID)
		observed := false
		f.b.client.KV = &publicationCaptureKV{KV: f.b.client.KV, capture: func(c []clientv3.Cmp, ops []clientv3.Op) {
			if putsKey(jk)(ops) {
				observed = true
				require.Len(t, c, 54)
				require.Len(t, ops, 9)
				ref := f.claim.enrolledAttempt.reference
				_, receiptKey, err := f.b.stageKeys(ref)
				require.NoError(t, err)
				request := &pb.TxnRequest{Compare: taskProtoComparisons(c), Failure: taskProtoPoints([]string{receiptKey})}
				for _, op := range ops {
					request.Success = append(request.Success, taskProtoPut(string(op.KeyBytes()), string(op.ValueBytes()), 0))
				}
				receipt, err := encodeReceipt(ref, OutcomeCommitted)
				require.NoError(t, err)
				success, failure := taskProtoPutResponse(9), taskProtoResponse([]string{receiptKey}, []string{receipt})
				require.LessOrEqual(t, request.Size(), 262144)
				require.LessOrEqual(t, success.Size(), 262144)
				require.LessOrEqual(t, failure.Size(), 262144)
				t.Logf("native maximum namespace512 commit54+9+1 request=%d conservative success=%d failure=%d bytes; native branch headers/scalars independently bounded", request.Size(), success.Size(), failure.Size())
			}
		}}
		result, err := f.b.PublishRuntimeEnrolled(context.Background(), f.claim, f.proof, f.enrollment)
		require.NoError(t, err)
		require.Equal(t, OutcomeCommitted, result.Outcome)
		require.True(t, observed)
		entry, err := f.b.LoadRuntimePublicationEnrolled(context.Background(), f.claim.workspace, f.claim.reference.IntentID)
		require.NoError(t, err)
		require.Equal(t, result.Entry, entry)
	})
}
