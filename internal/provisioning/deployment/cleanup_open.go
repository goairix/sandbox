package deployment

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sync"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	k "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/goairix/sandbox/internal/runtime/launcher"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
)

// LocalCleanupDeployment owns one configured original DueWorker and its
// dependencies. The v1 constructor executes explicit Task references; v2 also
// discovers permanent pending tasks. Neither provisions runtimes. The genuine
// worker KernelBoundary remains borrowed.
type LocalCleanupDeployment struct {
	mu       sync.Mutex
	closed   bool
	calls    sync.WaitGroup
	once     sync.Once
	closeErr error

	worker             *state.DueWorker
	backend            *state.Backend
	provider           *k.TaskRuntimeCleanupClient
	bridge             *k.EnrolledBridgeClient
	credential         *p.ManagementTLSCredential
	observerCredential *p.NodeObserverTLSCredential
	issuer             *CleanupIssuer
}

// Execute submits one known Task to the original worker mailbox. Cancellation
// does not turn uncertain work into completion or release its permanent slot.
// Close seals admission and joins accepted calls before closing dependencies.
func (d *LocalCleanupDeployment) Execute(ctx context.Context, task state.TaskReference) (state.CleanupLocalResult, error) {
	refused := state.CleanupLocalResult{Task: task, Outcome: state.OutcomeUnknown}
	if d == nil || ctx == nil {
		return refused, errConfiguration
	}
	if err := ctx.Err(); err != nil {
		return refused, err
	}
	d.mu.Lock()
	if d.closed || d.worker == nil {
		d.mu.Unlock()
		return refused, errConfiguration
	}
	w := d.worker
	d.calls.Add(1)
	d.mu.Unlock()
	defer d.calls.Done()

	return w.ExecuteCleanupLocal(ctx, task)
}

// Close seals new Execute calls, joins the worker and Backend, then closes
// owned dependencies in reverse construction order. Concurrent callers wait
// for the same completed close; errors, including unknown outcomes, are retained.
// It does not close or otherwise invalidate the caller's KernelBoundary.
func (d *LocalCleanupDeployment) Close() error {
	if d == nil {
		return nil
	}
	d.once.Do(func() {
		d.mu.Lock()
		d.closed = true
		d.mu.Unlock()

		// Do not hold mu or wait for Execute before Stop: the worker must first
		// cancel and join its accepted continuations and physical jobs.
		if d.worker != nil {
			d.closeErr = errors.Join(d.closeErr, d.worker.Stop())
		}
		if d.backend != nil {
			d.closeErr = errors.Join(d.closeErr, d.backend.Close())
		}
		d.calls.Wait()

		if d.provider != nil {
			d.closeErr = errors.Join(d.closeErr, d.provider.Close())
		}
		if d.bridge != nil {
			d.closeErr = errors.Join(d.closeErr, d.bridge.Close())
		}
		if d.observerCredential != nil {
			d.closeErr = errors.Join(d.closeErr, d.observerCredential.Close())
		}
		if d.credential != nil {
			d.closeErr = errors.Join(d.closeErr, d.credential.Close())
		}
		d.closeErr = errors.Join(d.closeErr, d.issuer.Close())
	})
	return d.closeErr
}

// OpenLocalCleanup loads the strict mounted v1 cleanup manifest and existing
// credentials. worker must be the caller's genuine, current workspace-sync
// worker boundary, retained until Close completes. No metadata is provisioned.
// ctx governs the DueWorker lifetime; canceling it stops the worker, but callers
// must still call Close to join work and release the owned dependencies.
// Relative configuration/credential paths resolve beside the manifest; data
// and archive roots are explicit absolute paths. Partial construction is joined
// and closed before an error is returned.
func OpenLocalCleanup(ctx context.Context, path string, worker *launcher.KernelBoundary) (*LocalCleanupDeployment, error) {
	return openLocalCleanup(ctx, path, worker, 1)
}

// OpenAutomaticLocalCleanup opens the strict mounted v2 automatic worker. The
// manifest must explicitly set worker.automatic_cleanup=true; v1 is refused.
// It borrows the caller's genuine workspace-sync boundary until Close joins,
// and has the same credential, configuration and partial-close ownership rules
// as OpenLocalCleanup. It discovers pending tasks without provisioning state.
func OpenAutomaticLocalCleanup(ctx context.Context, path string, worker *launcher.KernelBoundary) (*LocalCleanupDeployment, error) {
	return openLocalCleanup(ctx, path, worker, 2)
}

func openLocalCleanup(ctx context.Context, path string, worker *launcher.KernelBoundary, version int) (*LocalCleanupDeployment, error) {
	if ctx == nil {
		return nil, errConfiguration
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if worker == nil || worker.ValidateCurrent() != nil {
		return nil, errConfiguration
	}
	if _, err := worker.WorkspaceSyncWorkerReference(); err != nil {
		return nil, errConfiguration
	}

	wire, err := boundedFile(path, 65536, "cleanup manifest")
	if err != nil {
		return nil, err
	}
	m, err := parseCleanupManifestVersion(wire, version)
	if err != nil {
		return nil, err
	}
	base := filepath.Dir(path)
	launcherWire, err := boundedFile(relativeFile(base, m.LauncherConfigFile), launcherconfig.MaxBytes, "launcher configuration")
	if err != nil {
		return nil, err
	}
	config, err := launcherconfig.Decode(launcherWire)
	if err != nil {
		return nil, fileError("launcher configuration")
	}
	i := m.Etcd.Identity
	namespace, err := state.NewNamespace(i.Prefix, i.AuthorityID, i.Cell)
	if err != nil || config.Binding.Namespace != namespace.Root() || config.Binding.AuthorityID != i.RuntimeID || config.Binding.RestoreEpoch != i.RestoreEpoch || config.Binding.Target != "kubernetes" {
		return nil, fileError("cleanup launcher binding")
	}
	clock, err := p.NewSignedClockClient(p.SignedClockClientOptions{
		Binding: config.TrustBinding(), PublicKey: config.ClockPublicKey, Audience: config.ClockAudience,
		Dial: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", config.ClockSocket)
		},
	})
	if err != nil {
		return nil, fileError("signed clock")
	}

	key, err := signingPrivateKey(relativeFile(base, m.IssuerPrivateKeyFile), "issuer signing key")
	if err != nil {
		return nil, err
	}
	defer clear(key)
	certificate, err := boundedFile(relativeFile(base, m.IssuerCertificateFile), 8192, "issuer certificate")
	if err != nil {
		return nil, err
	}
	d := &LocalCleanupDeployment{}
	opened := false
	// All subsequent failures use this same ownership graph. Backend.Close
	// precedes credentials even when StartDueWorker fails halfway through.
	defer func() {
		if !opened {
			// Each failing branch joins Close into its returned error below.
			_ = d.Close()
		}
	}()
	fail := func(stage string, cause error) (*LocalCleanupDeployment, error) {
		return nil, errors.Join(fmt.Errorf("cleanup deployment %s: %w", stage, cause), d.Close())
	}
	d.issuer, err = NewCleanupIssuer(ctx, key, certificate, config, clock)
	if err != nil {
		return fail("issuer", err)
	}
	d.credential, err = p.NewManagementTLSCredential(key, certificate, "command_issuer")
	if err != nil {
		return fail("management credential", err)
	}
	d.observerCredential, err = p.NewNodeObserverTLSCredential(key, certificate, false)
	if err != nil {
		return fail("observer credential", err)
	}
	tlsConfig, err := mountedTLS(relativeFile(base, m.Etcd.TLS.CAFile), relativeFile(base, m.Etcd.TLS.CertFile), relativeFile(base, m.Etcd.TLS.KeyFile), m.Etcd.TLS.ServerName)
	if err != nil {
		return fail("etcd TLS", err)
	}
	api, err := loadKubeconfig(relativeFile(base, m.Kubernetes.KubeconfigFile))
	if err != nil {
		return fail("Kubernetes configuration", err)
	}
	defer clear(api.KeyData)
	apiTimeout := cleanupDuration(m.Kubernetes.RequestTimeout)
	d.bridge, err = k.NewEnrolledBridgeClient(k.EnrolledBridgeOptions{Config: api, Namespace: m.Kubernetes.Namespace, MaxActive: m.Kubernetes.BridgeMaxActive, RequestTimeout: apiTimeout})
	if err != nil {
		return fail("bridge", err)
	}
	d.provider, err = k.NewTaskRuntimeCleanupClient(k.TaskRuntimeCleanupOptions{Config: api, Namespace: m.Kubernetes.Namespace, MaxActive: m.Kubernetes.ProviderMaxActive, RequestTimeout: apiTimeout})
	if err != nil {
		return fail("provider", err)
	}

	options := m.backendOptions(namespace, config, clock, worker, d)
	options.TLS = tlsConfig
	d.backend, err = state.New(ctx, options)
	if err != nil {
		return fail("backend", err)
	}
	d.worker, err = d.backend.StartDueWorker(ctx, state.DueWorkerOptions{
		WorkerID: m.Worker.WorkerID, Partitions: m.Worker.Partitions,
		RangeSlots: m.Worker.RangeSlots, ClaimSlots: m.Worker.ClaimSlots, RelistSlot: m.Worker.RelistSlot,
		StageTTL: cleanupDuration(m.Worker.StageTTL),
		// The entry fixes the version and the parser requires v2's explicit
		// true field. No optional caller flag can reinterpret a v1 manifest.
		AutomaticCleanup: m.Version == 2,
	})
	if err != nil {
		return fail("worker", err)
	}
	if err := ctx.Err(); err != nil {
		return fail("startup canceled", err)
	}
	opened = true
	return d, nil
}

// Only concrete existing dependencies cross into etcd. The deployment layer
// interprets mounted files; it exports neither storage permissions nor signers
// to arbitrary callers, and etcd does not import deployment.
func (m cleanupManifest) backendOptions(namespace state.Namespace, config launcherconfig.Config, clock p.AuthorityClock, worker *launcher.KernelBoundary, d *LocalCleanupDeployment) state.Options {
	i, a, r := m.Etcd.Identity, m.Archive, m.Recycling
	nodes := make([]state.LocalCleanupNode, len(m.Nodes))
	for n, node := range m.Nodes {
		nodes[n] = state.LocalCleanupNode{Name: node.Name, UID: node.UID, BootID: node.BootID, ObserverAddress: node.ObserverAddress}
	}
	return state.Options{
		Endpoints: m.Etcd.Endpoints, Namespace: namespace,
		Identity:    state.Identity{SchemaVersion: i.SchemaVersion, Prefix: i.Prefix, AuthorityID: i.AuthorityID, Cell: i.Cell, ClusterID: i.ClusterID, StorageID: i.StorageID, RuntimeID: i.RuntimeID, RestoreEpoch: i.RestoreEpoch},
		DialTimeout: cleanupDuration(m.Etcd.DialTimeout), RequestTimeout: cleanupDuration(m.Etcd.RequestTimeout), SnapshotCacheBytes: m.Etcd.SnapshotCacheBytes,
		Clock: clock, PublicationTrust: &state.RuntimePublicationTrust{AuthorityID: config.Binding.AuthorityID, Target: config.Binding.Target, Roots: config.Roots},
		EnrolledPublication: &state.EnrolledPublicationOptions{ContractDigest: config.ContractDigest, MaxActive: config.MaxActive},
		TaskIssuer:          d.issuer, TaskQuiescenceIssuer: d.issuer, TaskWorkspaceArchiveIssuer: d.issuer, TaskRuntimeTerminationIssuer: d.issuer,
		WorkerControl:       &state.WorkerControlOptions{SessionTTL: cleanupDuration(m.Worker.SessionTTL)},
		LocalRuntimeRelease: &state.LocalRuntimeReleaseOptions{MaxActive: m.Cleanup.MaxActive, Timeout: cleanupDuration(m.Cleanup.ReleaseTimeout), Provider: d.provider},
		TaskWorkspaceArchiveRecovery: &state.TaskWorkspaceArchiveRecoveryOptions{TaskWorkspaceArchiveSpoolOptions: state.TaskWorkspaceArchiveSpoolOptions{
			Directory: a.Directory, Jobs: a.Jobs, Entries: a.Entries, JobBytes: a.JobBytes, TotalBytes: a.TotalBytes, JobIndexBytes: a.JobIndexBytes, TotalIndexBytes: a.TotalIndexBytes,
		}},
		TaskWorkspaceSyncRecyclingBudget: &state.TaskWorkspaceSyncBudgetOptions{BudgetID: r.BudgetID, SlotsPerPartition: r.SlotsPerPartition, MaxIntentBytes: r.MaxIntentBytes},
		LocalCleanup: &state.LocalCleanupOptions{
			Worker: worker, Bridge: d.bridge, Credential: d.credential, ObserverCredential: d.observerCredential,
			Nodes: nodes, Storage: state.LocalCleanupStorage{Identity: m.Storage.Identity, Bucket: m.Storage.Bucket, PrefixBase: m.Storage.PrefixBase, Directory: m.Storage.Directory},
			ClaimTTL: cleanupDuration(m.Cleanup.ClaimTTL), JobTimeout: cleanupDuration(m.Cleanup.JobTimeout), ArchiveTimeout: cleanupDuration(m.Cleanup.ArchiveTimeout), MaxActive: m.Cleanup.MaxActive,
		},
	}
}
