//go:build linux

package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	k "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"k8s.io/client-go/rest"
)

// A blocked configured callback must remain part of the original registered
// preparation goroutine even when its borrowed context has been canceled.
type managedPreparationIssuer struct {
	*taskCloseProvider
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	calls   atomic.Int64
}

func (i *managedPreparationIssuer) Certificate(ctx context.Context) ([]byte, error) {
	i.calls.Add(1)
	if i.entered != nil {
		i.once.Do(func() { close(i.entered) })
		<-i.release
	}
	return bytes.Clone(i.wire), ctx.Err()
}

// The original metadata producers and registry actor use the bounded gRPC
// server, while New validates a genuine native Kernel and complete deployment
// configuration. Kubernetes clients are real configured clients, with zero
// requests expected: this component fixture does not enable cold quota return.
// Preparation must finish and the consumer must reject it before E/Grant. This
// proves preparation ownership, not physical cleanup or managed Claim admission.
func managedPreparationFixture(t *testing.T, blocked bool) (detachedCleanupSourceFixture, *managedPreparationIssuer) {
	t.Helper()
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("requires original SANDBOX_WORKSPACE_SYNC_LOCAL_NATIVE Kernel bootstrap")
	}
	var issuer *managedPreparationIssuer
	f := newDetachedCleanupSourceFixture(t, "detached", &DetachedCleanupOptions{PinnedPlainImage: detachedCleanupPlainImageForTest}, func(o *Options) {
		o.WorkerControl.SessionTTL = 60 * time.Second
		o.RequestTimeout = time.Second
		root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{8}, 32))
		now := time.Now().UTC()
		wire, err := p.SignCommandIssuerCertificate(root, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: o.Namespace.Root(), AuthorityID: o.PublicationTrust.AuthorityID, Target: o.PublicationTrust.Target, RestoreEpoch: o.Identity.RestoreEpoch, PublicKey: key.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)})
		require.NoError(t, err)
		verifier, err := p.NewManagementVerifier(p.TrustBinding{Namespace: o.Namespace.Root(), AuthorityID: o.PublicationTrust.AuthorityID, Target: o.PublicationTrust.Target, RestoreEpoch: o.Identity.RestoreEpoch}, o.PublicationTrust.Roots)
		require.NoError(t, err)
		identity, err := verifier.VerifyCommandIssuerCertificate(wire, now)
		require.NoError(t, err)
		old := &taskCloseProvider{wire: wire, key: key, identity: identity}
		issuer = &managedPreparationIssuer{taskCloseProvider: old}
		if blocked {
			issuer.entered, issuer.release = make(chan struct{}), make(chan struct{})
		}
		credential, err := p.NewManagementTLSCredential(key, wire, "command_issuer")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, credential.Close()) })
		observer, err := p.NewNodeObserverTLSCredential(key, wire, false)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, observer.Close()) })
		config := &rest.Config{Host: "http://127.0.0.1:1"}
		bridge, err := k.NewEnrolledBridgeClient(k.EnrolledBridgeOptions{Config: config, Namespace: "runtimes", MaxActive: 1, RequestTimeout: time.Second})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, bridge.Close()) })
		provider, err := k.NewTaskRuntimeCleanupClient(k.TaskRuntimeCleanupOptions{Config: config, Namespace: "runtimes", MaxActive: 1, RequestTimeout: time.Second})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, provider.Close()) })
		dir := t.TempDir()
		for _, name := range []string{"spool", "target"} {
			require.NoError(t, os.Mkdir(filepath.Join(dir, name), 0700))
		}
		recovery := recoveryTestOptions(filepath.Join(dir, "spool"), 1)
		o.TaskIssuer, o.TaskQuiescenceIssuer = issuer, &quiescenceProvider{old: old}
		o.TaskWorkspaceArchiveIssuer = &archiveNativeIssuer{old: old}
		o.TaskRuntimeTerminationIssuer = &detachedTerminationFixtureIssuer{key: key, wire: wire, identity: identity}
		o.TaskWorkspaceArchiveRecovery = &recovery
		o.TaskWorkspaceSyncRecyclingBudget = &TaskWorkspaceSyncBudgetOptions{BudgetID: uuid.NewString(), SlotsPerPartition: 1, MaxIntentBytes: 16384}
		// Provision the original immutable deployment budget before New pins it;
		// neither this configuration point nor its options grants a Task claim.
		budgetKey, err := o.Namespace.Key("meta", "workspace_sync_recycling_budget")
		require.NoError(t, err)
		budgetValue, err := encodeOperationMetadata(taskWorkspaceSyncRecyclingBudgetRecord{2, *o.TaskWorkspaceSyncRecyclingBudget}, 1024)
		require.NoError(t, err)
		raw, err := clientv3.New(clientv3.Config{Endpoints: o.Endpoints, TLS: o.TLS, DialTimeout: time.Second})
		require.NoError(t, err)
		_, putErr := raw.Txn(context.Background()).Then(clientv3.OpPut(budgetKey, budgetValue)).Commit()
		closeErr := raw.Close()
		require.NoError(t, putErr)
		require.NoError(t, closeErr)
		o.LocalRuntimeRelease = &LocalRuntimeReleaseOptions{MaxActive: 1, Timeout: 30 * time.Second, Provider: provider}
		o.LocalCleanup = &LocalCleanupOptions{Worker: workspaceSyncLocalNativeWorker, Bridge: bridge, Credential: credential, ObserverCredential: observer, Nodes: []LocalCleanupNode{{Name: "node-a", UID: uuid.NewString(), BootID: uuid.NewString(), ObserverAddress: "127.0.0.1:1"}}, Storage: LocalCleanupStorage{Identity: o.Identity.StorageID, Bucket: "bucket", Directory: filepath.Join(dir, "target")}, ClaimTTL: 60 * time.Second, JobTimeout: time.Minute, ArchiveTimeout: 30 * time.Second, MaxActive: 32}
	})
	require.NoError(t, f.b.cleanupPhysicalProfile(), "formal New must own the complete deployment configuration")
	require.NoError(t, f.b.cleanupDetachedPreparationProfile())
	return f, issuer
}

func managedPreparationWorker(t *testing.T, f detachedCleanupSourceFixture, partitions ...uint8) (*DueWorker, context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	// The original model profile gives each Claim share a one-token burst and
	// one token/second. Observe, bundle, birth and snapshot are four separately
	// charged actor turns. Acquire four real shares at startup so this ownership
	// test can reach preparation inside the unchanged three-second join bound;
	// no rate, bucket, native owner or actor transition is patched by the test.
	w, err := f.b.StartDueWorker(ctx, DueWorkerOptions{WorkerID: "detached-preparation", Partitions: partitions, RangeSlots: []uint16{0}, ClaimSlots: []uint16{0, 1, 2, 3}, RelistSlot: 0, StageTTL: 30 * time.Second})
	require.NoError(t, err)
	require.Equal(t, int64(60), w.session.ttlSeconds)
	require.Len(t, w.claims, 4)
	t.Cleanup(func() {
		cancel()
		_ = w.Stop()
		workerTestReceive(t, w.Done())
	})
	return w, ctx, cancel
}

// Keep the original timeout. On failure expose actual protected workspace
// state and goroutine stacks instead of mistaking admission backoff for a
// callback/ownership hang. No diagnostic observation advances the actor.
func managedPreparationReceive(t *testing.T, w *DueWorker, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
		return
	case <-time.After(3 * time.Second):
	}
	w.backend.archiveLife.mu.Lock()
	workspace, preparing := w.managedSource != nil, false
	if workspace {
		preparing = w.managedSource.preparing
	}
	reserved, keys := w.cache.managedSource, w.cache.managedSourceKeys
	w.backend.archiveLife.mu.Unlock()
	stacks := make([]byte, 64<<10)
	n := runtime.Stack(stacks, true)
	t.Fatalf("original owner did not return: status=%+v workspace=%t preparing=%t reserved=%t keys=%d\n%s", w.Status(), workspace, preparing, reserved, keys, stacks[:n])
}

func TestCleanupDetachedPreparationNativeMixedDispatcher(t *testing.T) {
	f, issuer := managedPreparationFixture(t, false)
	w, ctx, _ := managedPreparationWorker(t, f, f.task.Reference.Partition)
	// The public local-only entry sees this same genuine Task but cannot select
	// the private mixed profile, even under complete deployment configuration.
	local, err := w.ExecuteCleanupLocal(ctx, f.task.Reference)
	require.ErrorIs(t, err, ErrInvalidConfiguration)
	require.NotEqual(t, "detached-wire-verified", local.Reason)
	require.Zero(t, issuer.calls.Load())
	j, err := w.enqueueCleanupLocal(ctx, f.task.Reference)
	require.NoError(t, err)
	managedPreparationReceive(t, w, j.done)
	require.ErrorIs(t, j.err, ErrInvalidConfiguration, "a consumer without cold quota return cannot admit E/Grant")
	require.Equal(t, "detached-wire-verified", j.result.Reason)
	require.NotNil(t, j.detachedTarget)
	require.True(t, j.detachedTarget.report.complete)
	require.Positive(t, j.detachedTarget.report.variants)
	require.LessOrEqual(t, j.detachedTarget.report.retainedKeys, 84)
	require.Positive(t, issuer.calls.Load())
	require.Nil(t, j.claim)
	require.Less(t, w.Status().OwnedBytes, uint64(48<<20))
}

func TestCleanupDetachedPreparationNativeCancelJoinsCallback(t *testing.T) {
	f, issuer := managedPreparationFixture(t, true)
	w, ctx, cancel := managedPreparationWorker(t, f, f.task.Reference.Partition)
	var release sync.Once
	unblock := func() { release.Do(func() { close(issuer.release) }) }
	t.Cleanup(unblock)
	j, err := w.enqueueCleanupLocal(ctx, f.task.Reference)
	require.NoError(t, err)
	managedPreparationReceive(t, w, issuer.entered)
	cancel()
	stopped := make(chan struct{})
	go func() {
		_ = w.Stop()
		close(stopped)
	}()
	// Status and the original cache use one lock/formula. Reservation remains
	// 24MiB and84 keys while the original callback physically owns the frame.
	f.b.archiveLife.mu.Lock()
	source := w.managedSource
	reserved, reserveKeys := w.cache.managedSource, w.cache.managedSourceKeys
	keys, bytes := w.cache.limitsLocked(w.cache.automatic, w.cache.coldQuota)
	baseKeys, baseBytes := dueCacheLimits(w.cache.automatic, w.cache.coldQuota)
	f.b.archiveLife.mu.Unlock()
	require.NotNil(t, source)
	require.Same(t, j, source.job)
	require.True(t, source.preparing)
	require.True(t, reserved)
	require.Equal(t, 84, reserveKeys)
	require.Equal(t, baseKeys-84, keys)
	require.Equal(t, baseBytes-uint64(24<<20), bytes)
	require.GreaterOrEqual(t, w.Status().OwnedBytes, uint64(48<<20))
	select {
	case <-j.done:
		t.Fatal("job detached its blocked Certificate callback")
	case <-w.Done():
		t.Fatal("worker stopped before Certificate physically returned")
	case <-stopped:
		t.Fatal("Stop refunded a still-running preparation frame")
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	workerTestReceive(t, stopped)
	workerTestReceive(t, j.done)
	workerTestReceive(t, w.Done())
	require.Less(t, w.Status().OwnedBytes, uint64(48<<20))
	f.b.archiveLife.mu.Lock()
	source, reserved, reserveKeys = w.managedSource, w.cache.managedSource, w.cache.managedSourceKeys
	f.b.archiveLife.mu.Unlock()
	require.Nil(t, source)
	require.False(t, reserved)
	require.Zero(t, reserveKeys)
}

func TestCleanupDetachedPreparationNativeSecondJobCannotSteal(t *testing.T) {
	f, issuer := managedPreparationFixture(t, true)
	second := detachedCleanupSourceOnPublication(t, duePublicationSourceOnBackend(t, f.b, f.server, "detached", f.now))
	partitions := []uint8{f.task.Reference.Partition}
	if second.task.Reference.Partition != partitions[0] {
		partitions = append(partitions, second.task.Reference.Partition)
	}
	_, secondSnapshot, err := f.b.namespace.sandboxKeys(second.task.Reference.Partition, second.task.Reference.SandboxID, second.task.Snapshot.Version)
	require.NoError(t, err)
	var secondSourceReads atomic.Int64
	f.b.client.KV = &faultKV{KV: f.b.client.KV, match: func(ops []clientv3.Op) bool {
		for _, op := range ops {
			if op.IsGet() && string(op.KeyBytes()) == secondSnapshot {
				secondSourceReads.Add(1)
			}
		}
		return false
	}}
	w, ctx, _ := managedPreparationWorker(t, f, partitions...)
	var release sync.Once
	unblock := func() { release.Do(func() { close(issuer.release) }) }
	t.Cleanup(unblock)
	first, err := w.enqueueCleanupLocal(ctx, f.task.Reference)
	require.NoError(t, err)
	managedPreparationReceive(t, w, issuer.entered)
	next, err := w.enqueueCleanupLocal(ctx, second.task.Reference)
	require.NoError(t, err)
	// Let the original actor service other lanes while preparation is held;
	// the second registered genuine Task cannot acquire/replace its workspace.
	select {
	case <-next.done:
		t.Fatal("second job must wait for the shared preparation workspace")
	case <-time.After(200 * time.Millisecond):
	}
	f.b.archiveLife.mu.Lock()
	source, reserveKeys := w.managedSource, w.cache.managedSourceKeys
	f.b.archiveLife.mu.Unlock()
	require.NotNil(t, source)
	require.Same(t, first, source.job)
	require.True(t, source.preparing)
	require.Equal(t, 84, reserveKeys)
	require.Zero(t, secondSourceReads.Load())
	require.Equal(t, int64(1), issuer.calls.Load())
	unblock()
	workerTestReceive(t, first.done)
	workerTestReceive(t, next.done)
	require.Equal(t, "detached-wire-verified", first.result.Reason)
	require.Equal(t, "detached-wire-verified", next.result.Reason)
	require.Less(t, w.Status().OwnedBytes, uint64(48<<20))
}
