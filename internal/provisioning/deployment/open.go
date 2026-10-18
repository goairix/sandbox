package deployment

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"time"

	"github.com/goairix/sandbox/internal/provisioning"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	kube "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	"github.com/goairix/sandbox/internal/sandboxsnapshot"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/goairix/sandbox/pkg/types"
)

// Deployment owns its Service and Backend. Borrowed accessors are valid until
// Close; callers must close Deployment rather than either borrowed resource.
type Deployment struct {
	service  *provisioning.PlainColdService
	backend  *state.Backend
	issuer   *ExecIssuer
	http     *HTTPSettings
	once     sync.Once
	closeErr error
}

func (d *Deployment) Service() *provisioning.PlainColdService { return d.service }
func (d *Deployment) Backend() *state.Backend                 { return d.backend }

// HTTPSettings returns a caller-owned copy; nil means HTTP was not configured.
// Accessors, including this one, must not race Deployment.Close.
func (d *Deployment) HTTPSettings() *HTTPSettings { return d.http.clone() }

func (d *Deployment) Close() error {
	if d == nil {
		return nil
	}
	d.once.Do(func() {
		defer d.http.Zero()
		if d.service != nil {
			d.closeErr = d.service.Close()
		}
		if d.backend != nil {
			d.closeErr = errors.Join(d.closeErr, d.backend.Close())
		}
		d.closeErr = errors.Join(d.closeErr, d.issuer.Close())
	})
	return d.closeErr
}

func positiveDuration(value string) (time.Duration, error) {
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, fileError("timeout")
	}
	return d, nil
}

// Open consumes existing operator-provisioned metadata and inventory only.
func Open(ctx context.Context, path string) (*Deployment, error) {
	return open(ctx, path, openService)
}

// OpenHTTP requires explicit HTTP settings before any credential or Backend
// initialization. Open preserves compatibility with read-only check manifests.
func OpenHTTP(ctx context.Context, path string) (*Deployment, error) {
	return open(ctx, path, openHTTP)
}

// InitializeInventory is an explicit operator action on the mounted deployment's
// original preparation profile. Identity and trust must already be provisioned.
// It never starts a Service, worker or runtime. Each original CAS creates at most
// eight slots; an error, including an unknown write result, ends this invocation
// without retry. A later explicit invocation resumes the durable progress.
// The 30-second total deadline bounds loading and initialization; all owned
// resources are physically closed before returning, including on cancellation.
func InitializeInventory(ctx context.Context, path string) (err error) {
	if ctx == nil {
		return errConfiguration
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	d, err := open(bounded, path, openInventory)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := d.Close()
		err = errors.Join(err, closeErr, bounded.Err())
	}()

	var maxCalls uint64
	for calls := uint64(0); ; calls++ {
		if err := bounded.Err(); err != nil {
			return err
		}
		progress, err := d.backend.InitializePreparationInventory(bounded)
		if err != nil {
			return err
		}
		if progress.Ready {
			return bounded.Err()
		}
		if calls == 0 {
			// Original initialization has one initial progress CAS, at most
			// ceil(Total/8) slot batches, and one final ready CAS. Total is
			// authenticated by the original fixed-profile progress decoder.
			maxCalls = 2 + (progress.Total+7)/8
		}
		if calls+1 >= maxCalls {
			return state.ErrPreparationNotReady
		}
	}
}

// The inventory path shares the complete mounted loader through Backend.New,
// then returns before constructing the Service that requires inventory ready.
// Neither ordinary loader mode can enter the operator's mutation loop.
type openMode uint8

const (
	openService openMode = iota
	openHTTP
	openInventory
)

func open(ctx context.Context, path string, mode openMode) (*Deployment, error) {
	if ctx == nil {
		return nil, errConfiguration
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	wire, err := boundedFile(path, 65536, "manifest")
	if err != nil {
		return nil, err
	}
	m, err := parseManifest(wire)
	if err != nil {
		return nil, err
	}
	if mode == openHTTP && m.HTTP == nil {
		return nil, fileError("HTTP settings required")
	}
	base := filepath.Dir(path)
	launcherWire, err := boundedFile(relativeFile(base, m.LauncherConfigFile), launcherconfig.MaxBytes, "launcher configuration")
	if err != nil {
		return nil, err
	}
	launcher, err := launcherconfig.Decode(launcherWire)
	if err != nil {
		return nil, fileError("launcher configuration")
	}
	httpSettings, err := loadHTTPSettings(base, m.HTTP, launcher)
	if err != nil {
		return nil, err
	}
	httpTransferred := false
	defer func() {
		if !httpTransferred {
			httpSettings.Zero()
		}
	}()
	root, err := signingPrivateKey(relativeFile(base, m.RootPrivateKeyFile), "root signing key")
	if err != nil {
		return nil, err
	}
	defer clear(root)
	issuer, err := signingPrivateKey(relativeFile(base, m.IssuerPrivateKeyFile), "issuer signing key")
	if err != nil {
		return nil, err
	}
	defer clear(issuer)
	issuerWire, err := boundedFile(relativeFile(base, m.IssuerCertificateFile), 8192, "issuer certificate")
	if err != nil {
		return nil, err
	}
	tlsConfig, err := mountedTLS(relativeFile(base, m.Etcd.TLS.CAFile), relativeFile(base, m.Etcd.TLS.CertFile), relativeFile(base, m.Etcd.TLS.KeyFile), m.Etcd.TLS.ServerName)
	if err != nil {
		return nil, err
	}
	api, err := loadKubeconfig(relativeFile(base, m.Kubernetes.KubeconfigFile))
	if err != nil {
		return nil, err
	}
	defer clear(api.KeyData)
	dialTimeout, err := positiveDuration(m.Etcd.DialTimeout)
	if err != nil {
		return nil, err
	}
	requestTimeout, err := positiveDuration(m.Etcd.RequestTimeout)
	if err != nil {
		return nil, err
	}
	apiTimeout, err := positiveDuration(m.Kubernetes.RequestTimeout)
	if err != nil {
		return nil, err
	}
	if len(m.Preparation.QuotaSlots) != 256 {
		return nil, fileError("preparation quota slots")
	}
	var inventory state.PreparationInventoryOptions
	copy(inventory.QuotaSlots[:], m.Preparation.QuotaSlots)
	i := m.Etcd.Identity
	namespace, err := state.NewNamespace(i.Prefix, i.AuthorityID, i.Cell)
	if err != nil {
		return nil, err
	}
	clock, err := p.NewSignedClockClient(p.SignedClockClientOptions{Binding: launcher.TrustBinding(), PublicKey: launcher.ClockPublicKey, Audience: launcher.ClockAudience, Dial: func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", launcher.ClockSocket)
	}})
	if err != nil {
		return nil, fileError("signed clock")
	}
	d := &Deployment{http: httpSettings}
	httpTransferred = true
	d.issuer, err = NewExecIssuer(ctx, issuer, issuerWire, launcher, clock)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("deployment issuer: %w", err), d.Close())
	}
	// The same digest-pinned image defines this deployment's only plain
	// producer and detached cleanup contract. Backend.New checks the pin against
	// enrolled publication before admission; the consumer remains a separately
	// configured automatic worker using that same image. No second image setting
	// or caller-supplied cleanup capability can bypass target authentication.
	options := state.Options{
		Endpoints: m.Etcd.Endpoints,
		Namespace: namespace,
		Identity: state.Identity{
			SchemaVersion: i.SchemaVersion, Prefix: i.Prefix,
			AuthorityID: i.AuthorityID, Cell: i.Cell, ClusterID: i.ClusterID,
			StorageID: i.StorageID, RuntimeID: i.RuntimeID, RestoreEpoch: i.RestoreEpoch,
		},

		TLS: tlsConfig, DialTimeout: dialTimeout, RequestTimeout: requestTimeout,
		SnapshotCacheBytes:   m.Etcd.SnapshotCacheBytes,
		PreparationInventory: &inventory,

		Clock: clock, ExecIssuer: d.issuer,
		PublicationTrust: &state.RuntimePublicationTrust{
			AuthorityID: launcher.Binding.AuthorityID, Target: launcher.Binding.Target, Roots: launcher.Roots,
		},
		EnrolledPublication: &state.EnrolledPublicationOptions{
			ContractDigest: launcher.ContractDigest, MaxActive: launcher.MaxActive,
		},
		DetachedCleanup: &state.DetachedCleanupOptions{PinnedPlainImage: m.Image},
	}
	d.backend, err = state.New(ctx, options)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("deployment backend: %w", err), d.Close())
	}
	if mode == openInventory {
		return d, nil
	}

	r, defaults := m.Resources, m.Business.Defaults
	dr := defaults.Resources
	d.service, err = provisioning.NewPlainColdService(ctx, provisioning.PlainColdOptions{Backend: d.backend, API: kube.EnrolledBridgeOptions{Config: api, Namespace: m.Kubernetes.Namespace, MaxActive: m.Kubernetes.MaxActive, RequestTimeout: apiTimeout}, Image: m.Image, NodeName: m.NodeName, NodeUID: launcher.Enrollment.NodeUID, NodeBootID: launcher.Enrollment.NodeBootID, WorkerID: m.WorkerID, CPU: r.CPU, Memory: r.Memory, Disk: r.Disk, TmpDisk: r.TmpDisk, QuotaPartition: m.Preparation.QuotaPartition, LauncherConfig: launcherWire, RootPrivateKey: root, IssuerPrivateKey: issuer, IssuerCertificate: issuerWire, Clock: clock, Business: &provisioning.PlainColdBusinessOptions{Principal: m.Business.Principal, Defaults: sandboxsnapshot.Defaults{TimeoutSeconds: defaults.TimeoutSeconds, Resources: types.ResourceLimits{CPU: dr.CPU, Memory: dr.Memory, Disk: dr.Disk, TmpDisk: dr.TmpDisk}}}})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("deployment service: %w", err), d.Close())
	}
	return d, nil
}
