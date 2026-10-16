package provisioning

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"sync"
	"time"

	"github.com/goairix/sandbox/internal/kubecontract"
	"github.com/goairix/sandbox/internal/runtime"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	kube "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"k8s.io/client-go/rest"
)

type PlainColdOptions struct {
	Backend                                        *state.Backend
	API                                            kube.EnrolledBridgeOptions
	Image, NodeName, NodeUID, NodeBootID, WorkerID string
	CPU, Memory, Disk, TmpDisk                     string
	QuotaPartition                                 uint8
	LauncherConfig                                 []byte
	RootPrivateKey, IssuerPrivateKey               ed25519.PrivateKey
	IssuerCertificate                              []byte
	Clock                                          p.AuthorityClock
	Business                                       *PlainColdBusinessOptions
}

type PlainColdState string

const (
	PlainColdPending          PlainColdState = "pending"
	PlainColdCompleted        PlainColdState = "completed"
	PlainColdHistorical       PlainColdState = "historical-completion"
	PlainColdCapacity         PlainColdState = "capacity"
	PlainColdRetryable        PlainColdState = "retryable"
	PlainColdRecoveryRequired PlainColdState = "recovery-required"
)

// PlainColdResult reports creation attribution and current metadata only.
// Completed is not a reusable runtime-health or execution capability.
type PlainColdResult struct {
	State               PlainColdState
	IntentID, SandboxID string
	RequestID           string
	Stage               state.StageReference
	Attempt             *state.ColdCreateAttemptRecord
	// Runtime and Attempt are copied diagnostics, never native authority. Early
	// Resume failures may retain unverified caller copies; actions use Backend
	// records and refresh these fields as their original evidence is loaded.
	Runtime     p.RuntimeReference
	Publication *state.RuntimeEnrolledPublicationEntry
}

// PlainColdService owns one deployment's bounded active calls and transports.
// There is no resident per-sandbox owner, map, goroutine or claim lease.
type PlainColdService struct {
	self                                 *PlainColdService
	backend                              *state.Backend
	driver                               *kube.EnrolledCreateClient
	bridge                               *kube.EnrolledBridgeClient
	config                               launcherconfig.Config
	configWire                           []byte
	spec                                 runtime.SandboxSpec
	nodeName, worker, namespace, profile string
	quota                                uint8
	root, issuer                         ed25519.PrivateKey
	issuerWire                           []byte
	clock                                p.AuthorityClock
	verifier                             *p.ManagementVerifier
	business                             *PlainColdBusinessOptions
	mu                                   sync.Mutex
	closed                               bool
	limit                                int
	active                               map[*plainColdCall]struct{}
	wg                                   sync.WaitGroup
	closeOnce                            sync.Once
	closeDone                            chan struct{}
	closeErr                             error
}

type plainColdCall struct {
	context context.Context
	cancel  context.CancelFunc
}

func NewPlainColdService(ctx context.Context, o PlainColdOptions) (*PlainColdService, error) {
	if ctx == nil || o.Backend == nil || coldNil(o.Clock) || o.API.Config == nil || len(o.RootPrivateKey) != ed25519.PrivateKeySize || len(o.IssuerPrivateKey) != ed25519.PrivateKeySize || o.WorkerID == "" || len(o.WorkerID) > 128 || len(o.IssuerCertificate) > 8192 || len(o.LauncherConfig) > launcherconfig.MaxBytes {
		return nil, ErrPlainColdProfile
	}
	// Capture caller-owned backing before the first clock or Backend operation.
	o.RootPrivateKey, o.IssuerPrivateKey = bytes.Clone(o.RootPrivateKey), bytes.Clone(o.IssuerPrivateKey)
	defer clear(o.RootPrivateKey)
	defer clear(o.IssuerPrivateKey)
	o.IssuerCertificate, o.LauncherConfig = bytes.Clone(o.IssuerCertificate), bytes.Clone(o.LauncherConfig)
	o.API.Config = rest.CopyConfig(o.API.Config)
	o.API.Config.CAData, o.API.Config.CertData, o.API.Config.KeyData = bytes.Clone(o.API.Config.CAData), bytes.Clone(o.API.Config.CertData), bytes.Clone(o.API.Config.KeyData)
	cfg, err := launcherconfig.Decode(o.LauncherConfig)
	if err != nil {
		return nil, err
	}
	business, err := ownColdBusiness(o.Backend, o.Business, cfg, o.CPU, o.Memory, o.Disk, o.TmpDisk)
	if err != nil {
		return nil, err
	}
	contract, err := kubecontract.EnrolledPlainContractDigest(o.Image)
	if err != nil || contract != cfg.ContractDigest || o.NodeUID != cfg.Enrollment.NodeUID || o.NodeBootID != cfg.Enrollment.NodeBootID {
		return nil, ErrPlainColdProfile
	}
	rootFound := false
	for _, root := range cfg.Roots {
		rootFound = rootFound || bytes.Equal(root, o.RootPrivateKey.Public().(ed25519.PublicKey))
	}
	if !rootFound || bytes.Equal(o.RootPrivateKey.Public().(ed25519.PublicKey), o.IssuerPrivateKey.Public().(ed25519.PublicKey)) {
		return nil, ErrPlainColdProfile
	}
	credential, err := p.NewManagementTLSCredential(o.IssuerPrivateKey, o.IssuerCertificate, "command_issuer")
	if err != nil {
		return nil, err
	}
	credential.Close()
	verifier, err := p.NewManagementVerifier(cfg.TrustBinding(), cfg.Roots)
	if err != nil {
		return nil, err
	}
	now, err := coldNow(ctx, o.Clock)
	if err != nil {
		return nil, err
	}
	issuer, err := verifier.VerifyCommandIssuerCertificate(o.IssuerCertificate, now)
	if err != nil {
		return nil, err
	}
	// Re-sign the same public issuer claims as a local deployment proof. The
	// Backend must recognize the explicitly supplied root before any quota/POST.
	binding := cfg.TrustBinding()
	rootProof, err := p.SignCommandIssuerCertificate(o.RootPrivateKey, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: issuer.CertificateID(), Role: "command_issuer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: issuer.PublicKey(), NotBefore: issuer.NotBefore(), NotAfter: issuer.NotAfter()})
	if err != nil {
		return nil, err
	}
	if err := o.Backend.CheckColdCreateRootSigner(ctx, rootProof); err != nil {
		return nil, err
	}
	inventory, err := o.Backend.CheckColdCreateDeployment(ctx, binding, cfg.ContractDigest, o.IssuerCertificate, o.QuotaPartition)
	if err != nil {
		return nil, err
	}
	spec := runtime.SandboxSpec{ID: "profile-check", Image: o.Image, CPU: o.CPU, Memory: o.Memory, Disk: o.Disk, TmpDisk: o.TmpDisk, NetworkEnabled: cfg.NetworkAllowed, ReadOnlyRootFS: true, RunAsUser: int64(cfg.UID), SeccompProfile: "RuntimeDefault"}
	probe := kube.EnrolledPlainPodRequest{Spec: spec, NodeName: o.NodeName, NodeUID: o.NodeUID, NodeBootID: o.NodeBootID, CreationIntentID: "00000000-0000-4000-8000-000000000001", Generation: 1, Config: o.LauncherConfig}
	if _, err := kube.BuildEnrolledPlainPod(o.API.Namespace, probe); err != nil {
		return nil, err
	}
	driver, err := kube.NewEnrolledCreateClient(kube.EnrolledCreateOptions{API: o.API, Image: o.Image, NodeName: o.NodeName, NodeUID: o.NodeUID, NodeBootID: o.NodeBootID})
	if err != nil {
		return nil, err
	}
	bridge, err := kube.NewEnrolledBridgeClient(o.API)
	if err != nil {
		driver.Close()
		return nil, err
	}
	publicProfile, err := json.Marshal(struct {
		Domain, Inventory, APIHost, Namespace, NodeName, ServerName, CA, ClientCertificate string
		Spec                                                                               runtime.SandboxSpec
		Config                                                                             json.RawMessage
		Root, Issuer                                                                       []byte
		Quota                                                                              uint8
		IssuerCertificate                                                                  string
	}{"sandbox.plain-cold.service.v1", inventory, o.API.Config.Host, o.API.Namespace, o.NodeName, o.API.Config.ServerName, coldHash(o.API.Config.CAData), coldHash(o.API.Config.CertData), spec, o.LauncherConfig, o.RootPrivateKey.Public().(ed25519.PublicKey), o.IssuerPrivateKey.Public().(ed25519.PublicKey), o.QuotaPartition, coldHash(o.IssuerCertificate)})
	if err != nil {
		driver.Close()
		bridge.Close()
		return nil, err
	}
	s := &PlainColdService{backend: o.Backend, driver: driver, bridge: bridge, config: cfg, configWire: bytes.Clone(o.LauncherConfig), spec: spec, nodeName: o.NodeName, worker: o.WorkerID, namespace: o.API.Namespace, profile: coldHash(publicProfile), quota: o.QuotaPartition, root: bytes.Clone(o.RootPrivateKey), issuer: bytes.Clone(o.IssuerPrivateKey), issuerWire: bytes.Clone(o.IssuerCertificate), clock: o.Clock, verifier: verifier, business: business, limit: o.API.MaxActive, active: make(map[*plainColdCall]struct{}), closeDone: make(chan struct{})}
	s.self = s
	return s, nil
}

func coldNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	}
	return false
}

func coldHash(wire []byte) string {
	digest := sha256.Sum256(wire)
	return hex.EncodeToString(digest[:])
}

func (s *PlainColdService) enter(ctx context.Context) (*plainColdCall, error) {
	if s == nil || s.self != s || ctx == nil {
		return nil, ErrPlainColdProfile
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || len(s.active) >= s.limit {
		return nil, state.ErrConflict
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	call := &plainColdCall{}
	call.context, call.cancel = context.WithTimeout(ctx, 20*time.Second)
	s.active[call] = struct{}{}
	s.wg.Add(1)
	return call, nil
}

func (s *PlainColdService) leave(call *plainColdCall) {
	call.cancel()
	s.mu.Lock()
	delete(s.active, call)
	s.mu.Unlock()
	s.wg.Done()
}

func (s *PlainColdService) Close() error {
	if s == nil || s.self != s {
		return ErrPlainColdProfile
	}
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		for call := range s.active {
			call.cancel()
		}
		s.mu.Unlock()
		s.closeErr = errors.Join(s.driver.Close(), s.bridge.Close())
		s.wg.Wait()
		clear(s.root)
		clear(s.issuer)
		close(s.closeDone)
	})
	<-s.closeDone
	return s.closeErr
}

func (s *PlainColdService) now(ctx context.Context) (time.Time, error) {
	return coldNow(ctx, s.clock)
}

func coldNow(ctx context.Context, clock p.AuthorityClock) (time.Time, error) {
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	start := time.Now()
	o, err := clock.Observe(bounded)
	elapsed := time.Since(start)
	if err != nil {
		return time.Time{}, err
	}
	if bounded.Err() != nil || o.UTC.IsZero() || o.UTC.Location() != time.UTC || o.Uncertainty < 0 || o.Uncertainty > time.Second || elapsed < 0 || elapsed > time.Second-o.Uncertainty {
		return time.Time{}, ErrPlainColdProfile
	}
	return o.UTC.Add(elapsed), nil
}

func (s *PlainColdService) ready(ctx context.Context) error {
	_, err := s.backend.CheckColdCreateDeployment(ctx, s.config.TrustBinding(), s.config.ContractDigest, s.issuerWire, s.quota)
	return err
}

// Bootstrap has a physical connection owner and joins its cancellation callback.
func (s *PlainColdService) bootstrap(ctx context.Context, target kube.EnrolledBridgeTarget, request ct.BootstrapRequest) (ct.BootstrapResponse, error) {
	var response ct.BootstrapResponse
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := s.bridge.Dial(bounded, target)
	if err != nil {
		return response, err
	}
	defer conn.Close()
	done := make(chan struct{})
	stop := context.AfterFunc(bounded, func() { defer close(done); conn.Close() })
	defer func() {
		if !stop() {
			<-done
		}
	}()
	deadline, _ := bounded.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return response, err
	}
	if err := ct.WriteBootstrap(conn, request); err != nil {
		return response, err
	}
	half, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		return response, ErrPlainColdProfile
	}
	if err := half.CloseWrite(); err != nil {
		return response, err
	}
	if err := ct.ReadBootstrap(conn, &response); err != nil {
		return response, err
	}
	expected := "closed_hello"
	if request.Purpose == "activate" {
		expected = "activated"
	}
	if response.Version != 1 || response.Purpose != expected || bounded.Err() != nil {
		return response, ErrPlainColdProfile
	}
	return response, nil
}

func (s *PlainColdService) destination(target kube.EnrolledBridgeTarget, m state.ColdBootstrapMaterial, identity p.RuntimeIdentityContext) (*ct.Destination, *p.ManagementTLSCredential, error) {
	credential, err := p.NewManagementTLSCredential(s.issuer, m.IssuerCertificate, "command_issuer")
	if err != nil {
		return nil, nil, err
	}
	destination, err := ct.NewDestination(ct.DestinationOptions{Identity: identity, RuntimeCertificate: m.RuntimeCertificate, Credential: credential, Verifier: s.verifier, Clock: s.clock, Dial: func(ctx context.Context) (net.Conn, error) { return s.bridge.Dial(ctx, target) }})
	if err != nil {
		credential.Close()
		return nil, nil, err
	}
	return destination, credential, nil
}
