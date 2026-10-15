//go:build linux && (amd64 || arm64)

package nodeobserver

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	t "github.com/goairix/sandbox/internal/runtime/controltransport"
	clientv3 "go.etcd.io/etcd/client/v3"
	"golang.org/x/sys/unix"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type observerSlot struct {
	registration enrollmentRecord
	wire         []byte
	anchor       *processAnchor
	ownerDone    chan struct{}
	running      bool
	pending      []byte
}

type Observer struct {
	mu            sync.Mutex
	options       Options
	verifier      *p.ManagementVerifier
	identity      p.NodeObserverIdentity
	credential    *p.NodeObserverTLSCredential
	client        *clientv3.Client
	apiTransport  *http.Transport
	provider      providerGuard
	authority     authorityReader
	journal       *journal
	lock          *os.File
	slots         []observerSlot
	ctx           context.Context
	cancel        context.CancelFunc
	enroll        *net.UnixListener
	commands      net.Listener
	connections   map[net.Conn]struct{}
	rpcs          chan struct{}
	active        chan struct{}
	ioWG          sync.WaitGroup
	terminationWG sync.WaitGroup
	serveDone     chan struct{}
	served        bool
	stopping      bool
	closeOnce     sync.Once
	closeErr      error
}

func copyObserverTLS(config *tls.Config) (*tls.Config, error) {
	if config == nil || config.InsecureSkipVerify || config.RootCAs == nil || config.GetClientCertificate != nil || config.GetCertificate != nil || config.VerifyConnection != nil || config.VerifyPeerCertificate != nil || config.KeyLogWriter != nil {
		return nil, fmt.Errorf("observer etcd requires pinned concrete TLS credentials")
	}
	out := config.Clone()
	out.MinVersion, out.MaxVersion = tls.VersionTLS13, tls.VersionTLS13
	out.RootCAs = config.RootCAs.Clone()
	out.Certificates = make([]tls.Certificate, len(config.Certificates))
	if len(out.Certificates) != 1 {
		return nil, fmt.Errorf("observer etcd requires one client identity")
	}
	for i, certificate := range config.Certificates {
		out.Certificates[i] = certificate
		out.Certificates[i].Certificate = make([][]byte, len(certificate.Certificate))
		for j, der := range certificate.Certificate {
			if len(der) > 8192 {
				return nil, fmt.Errorf("observer etcd TLS certificate exceeds bound")
			}
			out.Certificates[i].Certificate[j] = bytes.Clone(der)
		}
		encoded, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
		if err != nil {
			return nil, fmt.Errorf("observer requires copyable concrete etcd TLS key")
		}
		key, err := x509.ParsePKCS8PrivateKey(encoded)
		clear(encoded)
		if err != nil {
			return nil, err
		}
		out.Certificates[i].PrivateKey = key
	}
	out.NextProtos = append([]string(nil), config.NextProtos...)
	out.SessionTicketsDisabled, out.ClientSessionCache = true, nil
	return out, nil
}

func New(options Options) (_ *Observer, result error) {
	if os.Geteuid() != 0 || options.MaxSlots == 0 || options.MaxSlots > 1024 || options.MaxActive == 0 || options.MaxActive > 4 || options.RequestTimeout <= 0 || options.RequestTimeout > 5*time.Second || options.TerminationTimeout <= 0 || options.TerminationTimeout > 30*time.Second || len(options.EtcdEndpoints) == 0 || len(options.EtcdEndpoints) > 4 || options.EtcdClusterID == 0 || len(options.EtcdIdentity) == 0 || len(options.EtcdIdentity) > 4096 || !json.Valid(options.EtcdIdentity) || len(options.Certificate) > 2048 || len(options.PrivateKey) != ed25519.PrivateKeySize || !bytes.Equal(ed25519.NewKeyFromSeed(options.PrivateKey[:ed25519.SeedSize]), options.PrivateKey) || len(validation.IsDNS1123Subdomain(options.NodeName)) != 0 || len(validation.IsDNS1123Label(options.KubernetesNamespace)) != 0 || !filepath.IsAbs(options.JournalDirectory) || filepath.Clean(options.JournalDirectory) != options.JournalDirectory || options.KubernetesConfig == nil || options.Clock == nil {
		return nil, fmt.Errorf("invalid node observer options")
	}
	clockValue := reflect.ValueOf(options.Clock)
	switch clockValue.Kind() {
	case reflect.Pointer, reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Slice:
		if clockValue.IsNil() {
			return nil, fmt.Errorf("nil node observer controlled clock")
		}
	}
	var limits unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limits); err != nil || limits.Cur < uint64(options.MaxSlots)*6+64 {
		return nil, fmt.Errorf("node observer descriptor budget unavailable")
	}
	for _, endpoint := range options.EtcdEndpoints {
		u, err := url.Parse(endpoint)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || len(endpoint) > 1024 {
			return nil, fmt.Errorf("observer requires fixed HTTPS etcd endpoints")
		}
	}
	tlsConfig, err := copyObserverTLS(options.EtcdTLS)
	if err != nil {
		return nil, err
	}
	verifier, err := p.NewManagementVerifier(options.Binding, options.Roots)
	if err != nil {
		return nil, err
	}
	boot, err := bootID()
	if err != nil {
		return nil, err
	}
	options.PrivateKey, options.Certificate = bytes.Clone(options.PrivateKey), bytes.Clone(options.Certificate)
	options.EtcdIdentity = bytes.Clone(options.EtcdIdentity)
	options.EtcdEndpoints = append([]string(nil), options.EtcdEndpoints...)
	options.EtcdTLS, options.Roots = tlsConfig, nil
	if len(options.KubernetesConfig.CAData)+len(options.KubernetesConfig.CertData)+len(options.KubernetesConfig.KeyData)+len(options.KubernetesConfig.BearerToken) > 128*1024 {
		return nil, fmt.Errorf("observer Kubernetes credentials exceed bound")
	}
	options.KubernetesConfig = rest.CopyConfig(options.KubernetesConfig)
	options.KubernetesConfig.CAData = bytes.Clone(options.KubernetesConfig.CAData)
	options.KubernetesConfig.CertData = bytes.Clone(options.KubernetesConfig.CertData)
	options.KubernetesConfig.KeyData = bytes.Clone(options.KubernetesConfig.KeyData)
	ctx, cancel := context.WithCancel(context.Background())
	o := &Observer{options: options, verifier: verifier, ctx: ctx, cancel: cancel, serveDone: make(chan struct{}), connections: make(map[net.Conn]struct{}), rpcs: make(chan struct{}, options.MaxActive), active: make(chan struct{}, options.MaxActive), slots: make([]observerSlot, options.MaxSlots)}
	defer func() {
		if result != nil {
			_ = o.Close()
		}
	}()
	clockCtx, clockCancel := context.WithTimeout(ctx, time.Second)
	observation, err := options.Clock.Observe(clockCtx)
	clockErr := clockCtx.Err()
	clockCancel()
	if err != nil || clockErr != nil || observation.UTC.IsZero() || observation.UTC.Location() != time.UTC || observation.Uncertainty < 0 || observation.Uncertainty > time.Second {
		return nil, fmt.Errorf("observer controlled clock unavailable")
	}
	o.identity, err = verifier.VerifyNodeObserverCertificate(options.Certificate, options.NodeUID, boot, observation.UTC)
	if err != nil || !bytes.Equal(o.identity.PublicKey(), options.PrivateKey[ed25519.SeedSize:]) {
		return nil, fmt.Errorf("observer current node certificate/key mismatch: %v", err)
	}
	o.credential, err = p.NewNodeObserverTLSCredential(options.PrivateKey, options.Certificate, true)
	if err != nil {
		return nil, err
	}
	config := options.KubernetesConfig
	if config.Transport != nil || config.WrapTransport != nil || config.ExecProvider != nil || config.AuthProvider != nil || len(config.CAData)+len(config.CertData)+len(config.KeyData)+len(config.BearerToken) > 128*1024 {
		return nil, fmt.Errorf("observer Kubernetes requires bounded concrete credentials")
	}
	apiTLS, err := rest.TLSConfigFor(config)
	if err != nil {
		return nil, err
	}
	o.apiTransport = &http.Transport{TLSClientConfig: apiTLS, MaxConnsPerHost: 2, MaxIdleConns: 2, MaxIdleConnsPerHost: 2, IdleConnTimeout: time.Second, TLSHandshakeTimeout: options.RequestTimeout, ResponseHeaderTimeout: options.RequestTimeout}
	wrapped, err := rest.HTTPWrappersForConfig(config, o.apiTransport)
	if err != nil {
		return nil, err
	}
	api, err := kubernetes.NewForConfigAndClient(config, &http.Client{Transport: wrapped, Timeout: options.RequestTimeout})
	if err != nil {
		return nil, err
	}
	o.provider = providerGuard{client: api, namespace: options.KubernetesNamespace, nodeName: options.NodeName, nodeUID: options.NodeUID, nodeBootID: boot}
	rpc, rpcCancel := context.WithTimeout(ctx, options.RequestTimeout)
	node, err := api.CoreV1().Nodes().Get(rpc, options.NodeName, metav1.GetOptions{})
	rpcErr := rpc.Err()
	rpcCancel()
	if err != nil || rpcErr != nil || node == nil || string(node.UID) != options.NodeUID || node.Status.NodeInfo.BootID != boot || node.DeletionTimestamp != nil {
		return nil, fmt.Errorf("observer exact current API node unavailable: %v", err)
	}
	o.client, err = clientv3.New(clientv3.Config{Endpoints: options.EtcdEndpoints, TLS: options.EtcdTLS, DialTimeout: options.RequestTimeout, MaxCallSendMsgSize: nativeWireLimit, MaxCallRecvMsgSize: nativeWireLimit})
	if err != nil {
		return nil, err
	}
	o.authority = authorityReader{client: o.client, verifier: verifier, clock: options.Clock, binding: options.Binding, identity: options.EtcdIdentity, clusterID: options.EtcdClusterID, timeout: options.RequestTimeout}
	var stat unix.Stat_t
	if err := unix.Lstat(options.JournalDirectory, &stat); err != nil || stat.Uid != 0 || stat.Mode&0777 != 0700 || stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return nil, fmt.Errorf("observer journal must be root-owned private directory")
	}
	lockFD, err := unix.Open(filepath.Join(options.JournalDirectory, "observer.lock"), unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	o.lock = os.NewFile(uintptr(lockFD), "observer-journal-lock")
	if err := unix.Flock(int(o.lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, fmt.Errorf("observer journal already owned: %w", err)
	}
	o.journal, err = openJournal(options.JournalDirectory, options.MaxSlots)
	if err != nil {
		return nil, err
	}
	if err := o.restoreSlots(); err != nil {
		return nil, err
	}
	return o, nil
}

func (o *Observer) stop() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.stopping = true
	if o.cancel != nil {
		o.cancel()
	}
	if o.enroll != nil {
		_ = o.enroll.Close()
	}
	if o.commands != nil {
		_ = o.commands.Close()
	}
	for conn := range o.connections {
		_ = conn.Close()
	}
}

func (o *Observer) Serve(ctx context.Context, enroll *net.UnixListener, commands net.Listener) error {
	if o == nil || ctx == nil || enroll == nil || commands == nil {
		if enroll != nil {
			_ = enroll.Close()
		}
		if commands != nil {
			_ = commands.Close()
		}
		return fmt.Errorf("missing observer listener/context")
	}
	path := enroll.Addr().String()
	var socket, parent unix.Stat_t
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || unix.Lstat(path, &socket) != nil || socket.Mode&unix.S_IFMT != unix.S_IFSOCK || socket.Mode&0777 != 0600 || socket.Uid != 0 || unix.Lstat(filepath.Dir(path), &parent) != nil || parent.Mode&unix.S_IFMT != unix.S_IFDIR || parent.Mode&0777 != 0700 || parent.Uid != 0 {
		_ = enroll.Close()
		_ = commands.Close()
		return fmt.Errorf("observer enrollment socket/parent must be root-owned 0600/0700")
	}
	o.mu.Lock()
	if o.served || o.stopping {
		o.mu.Unlock()
		_ = enroll.Close()
		_ = commands.Close()
		return fmt.Errorf("observer already served or closed")
	}
	o.served, o.enroll, o.commands = true, enroll, commands
	o.mu.Unlock()
	defer close(o.serveDone)
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(stopped); o.stop() })
	defer func() {
		o.stop()
		if !stop() {
			<-stopped
		}
		o.ioWG.Wait()
		o.terminationWG.Wait()
	}()
	config, err := p.NewNodeObserverServerTLSConfig(o.verifier, o.credential, o.options.Clock, o.options.NodeUID, o.identity.NodeBootID())
	if err != nil {
		return err
	}
	results := make(chan error, 2)
	go func() { results <- o.accept(enroll, nil) }()
	go func() { results <- o.accept(commands, config) }()
	first := <-results
	o.stop()
	second := <-results
	if ctx.Err() != nil {
		return nil
	}
	return errors.Join(first, second)
}

func (o *Observer) accept(listener net.Listener, config *tls.Config) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if o.ctx.Err() != nil {
				return nil
			}
			return err
		}
		o.mu.Lock()
		select {
		case o.rpcs <- struct{}{}:
			if o.stopping {
				<-o.rpcs
				o.mu.Unlock()
				_ = conn.Close()
				continue
			}
			o.connections[conn] = struct{}{}
			o.ioWG.Add(1)
		default:
			o.mu.Unlock()
			_ = conn.Close()
			continue
		}
		o.mu.Unlock()
		go func() {
			defer o.ioWG.Done()
			defer func() {
				_ = conn.Close()
				o.mu.Lock()
				delete(o.connections, conn)
				o.mu.Unlock()
				<-o.rpcs
			}()
			rpc, cancel := context.WithTimeout(o.ctx, o.options.RequestTimeout)
			defer cancel()
			deadline, _ := rpc.Deadline()
			if conn.SetDeadline(deadline) != nil {
				return
			}
			if config == nil {
				unixConn, ok := conn.(*net.UnixConn)
				if ok {
					if err := o.handleEnrollment(rpc, unixConn); err != nil {
						slog.Warn("node observer enrollment refused", "reason", err)
					}
				}
				return
			}
			secure := tls.Server(conn, config)
			if secure.HandshakeContext(rpc) != nil {
				return
			}
			peer, err := p.NodeObserverTLSPeerCertificate(secure.ConnectionState())
			if err != nil {
				return
			}
			wire, err := t.ReadNodeObserverFrame(secure)
			if err != nil {
				return
			}
			request, err := t.DecodeNodeTerminationRequest(wire)
			if err != nil {
				return
			}
			var result p.NodeTerminationResult
			if request.Purpose == "task_runtime_terminate_query" {
				result, err = o.query(*request.Query)
			} else if bytes.Equal(peer, request.IssuerCertificate) {
				result, err = o.terminate(rpc, request.Ticket, peer)
			} else {
				return
			}
			if err != nil {
				slog.Warn("node observer command refused", "reason", err)
			}
			if err == nil {
				if reply, err := p.EncodeNodeTerminationResult(result); err == nil {
					_ = t.WriteNodeObserverFrame(secure, reply)
				}
			}
		}()
	}
}

func (o *Observer) Close() error {
	if o == nil {
		return nil
	}
	o.closeOnce.Do(func() {
		o.stop()
		o.mu.Lock()
		served := o.served
		o.mu.Unlock()
		if served {
			<-o.serveDone
		}
		for i := range o.slots {
			o.closeErr = errors.Join(o.closeErr, o.slots[i].anchor.close())
		}
		if o.client != nil {
			o.closeErr = errors.Join(o.closeErr, o.client.Close())
		}
		if o.apiTransport != nil {
			o.apiTransport.CloseIdleConnections()
		}
		if o.lock != nil {
			o.closeErr = errors.Join(o.closeErr, o.lock.Close())
		}
		if o.journal != nil {
			o.closeErr = errors.Join(o.closeErr, o.journal.close())
		}
		o.closeErr = errors.Join(o.closeErr, o.credential.Close())
		clear(o.options.PrivateKey)
		o.options.PrivateKey = nil
	})
	return o.closeErr
}
