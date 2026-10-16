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
	once     sync.Once
	closeErr error
}

func (d *Deployment) Service() *provisioning.PlainColdService { return d.service }
func (d *Deployment) Backend() *state.Backend                 { return d.backend }

func (d *Deployment) Close() error {
	if d == nil {
		return nil
	}
	d.once.Do(func() {
		if d.service != nil {
			d.closeErr = d.service.Close()
		}
		if d.backend != nil {
			d.closeErr = errors.Join(d.closeErr, d.backend.Close())
		}
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
	base := filepath.Dir(path)
	launcherWire, err := boundedFile(relativeFile(base, m.LauncherConfigFile), launcherconfig.MaxBytes, "launcher configuration")
	if err != nil {
		return nil, err
	}
	launcher, err := launcherconfig.Decode(launcherWire)
	if err != nil {
		return nil, fileError("launcher configuration")
	}
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
	d := &Deployment{}
	d.backend, err = state.New(ctx, state.Options{Endpoints: m.Etcd.Endpoints, Namespace: namespace, Identity: state.Identity{SchemaVersion: i.SchemaVersion, Prefix: i.Prefix, AuthorityID: i.AuthorityID, Cell: i.Cell, ClusterID: i.ClusterID, StorageID: i.StorageID, RuntimeID: i.RuntimeID, RestoreEpoch: i.RestoreEpoch}, TLS: tlsConfig, DialTimeout: dialTimeout, RequestTimeout: requestTimeout, SnapshotCacheBytes: m.Etcd.SnapshotCacheBytes, PreparationInventory: &inventory, Clock: clock, PublicationTrust: &state.RuntimePublicationTrust{AuthorityID: launcher.Binding.AuthorityID, Target: launcher.Binding.Target, Roots: launcher.Roots}, EnrolledPublication: &state.EnrolledPublicationOptions{ContractDigest: launcher.ContractDigest, MaxActive: launcher.MaxActive}})
	if err != nil {
		return nil, fmt.Errorf("deployment backend: %w", err)
	}
	r, defaults := m.Resources, m.Business.Defaults
	dr := defaults.Resources
	d.service, err = provisioning.NewPlainColdService(ctx, provisioning.PlainColdOptions{Backend: d.backend, API: kube.EnrolledBridgeOptions{Config: api, Namespace: m.Kubernetes.Namespace, MaxActive: m.Kubernetes.MaxActive, RequestTimeout: apiTimeout}, Image: m.Image, NodeName: m.NodeName, NodeUID: launcher.Enrollment.NodeUID, NodeBootID: launcher.Enrollment.NodeBootID, WorkerID: m.WorkerID, CPU: r.CPU, Memory: r.Memory, Disk: r.Disk, TmpDisk: r.TmpDisk, QuotaPartition: m.Preparation.QuotaPartition, LauncherConfig: launcherWire, RootPrivateKey: root, IssuerPrivateKey: issuer, IssuerCertificate: issuerWire, Clock: clock, Business: &provisioning.PlainColdBusinessOptions{Principal: m.Business.Principal, Defaults: sandboxsnapshot.Defaults{TimeoutSeconds: defaults.TimeoutSeconds, Resources: types.ResourceLimits{CPU: dr.CPU, Memory: dr.Memory, Disk: dr.Disk, TmpDisk: dr.TmpDisk}}}})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("deployment service: %w", err), d.Close())
	}
	return d, nil
}
