package etcd

import (
	"bytes"
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// Options binds the backend to existing operator-provisioned state. Plain HTTP
// requires explicit opt-in and every endpoint must be on the loopback interface.
// New copies TLS configuration data. PrivateKey signers and callback/interface
// dependencies remain shared: callers must keep them immutable and safe for
// concurrent use for the backend lifetime, and must not mutate Options during New.
// Certificates already added to RootCAs or ClientCAs must also remain immutable.
type Options struct {
	Endpoints             []string
	Namespace             Namespace
	Identity              Identity
	DialTimeout           time.Duration
	RequestTimeout        time.Duration
	TLS                   *tls.Config
	AllowInsecureLoopback bool
}

type Backend struct {
	client         *clientv3.Client
	namespace      Namespace
	identityKey    string
	restoreKey     string
	identityValue  string
	restoreEpoch   string
	clusterID      uint64
	requestTimeout time.Duration
}

func validateOptions(o Options) error {
	invalid := func(message string) error { return fmt.Errorf("%w: %s", ErrInvalidConfiguration, message) }
	if _, err := NewNamespace(o.Namespace.prefix, o.Namespace.scope, o.Namespace.cell); err != nil {
		return err
	}
	if !o.Identity.valid(o.Namespace) || !validSegment(o.Identity.RestoreEpoch) || len(o.Identity.RestoreEpoch) > maxIdentityFieldBytes {
		return invalid("expected identity or restore epoch is invalid")
	}
	if o.DialTimeout < 0 || o.RequestTimeout < 0 {
		return invalid("timeouts must be positive")
	}
	if len(o.Endpoints) == 0 {
		return invalid("endpoints are required")
	}
	for _, endpoint := range o.Endpoints {
		u, err := url.Parse(endpoint)
		if err != nil || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
			return invalid("endpoint must contain only scheme and host")
		}
		switch u.Scheme {
		case "https":
			if o.TLS == nil {
				return invalid("HTTPS requires mutual TLS")
			}
		case "http":
			ip := net.ParseIP(u.Hostname())
			if !o.AllowInsecureLoopback || o.TLS != nil || !(strings.EqualFold(u.Hostname(), "localhost") || ip != nil && ip.IsLoopback()) {
				return invalid("HTTP requires explicit loopback opt-in without TLS")
			}
		default:
			return invalid("endpoint requires explicit http or https scheme")
		}
	}
	if o.TLS != nil {
		if o.TLS.GetClientCertificate != nil {
			return invalid("TLS requires static client certificates without a selection callback")
		}
		if o.TLS.MinVersion != 0 && o.TLS.MinVersion < tls.VersionTLS12 {
			return invalid("TLS minimum version must be TLS 1.2 or newer")
		}
		if o.TLS.InsecureSkipVerify || o.TLS.RootCAs == nil || len(o.TLS.RootCAs.Subjects()) == 0 || len(o.TLS.Certificates) == 0 {
			return invalid("TLS requires trusted roots and a client certificate with verification enabled")
		}
		for _, certificate := range o.TLS.Certificates {
			if len(certificate.Certificate) == 0 || certificate.PrivateKey == nil {
				return invalid("invalid TLS client certificate")
			}
			leaf, err := x509.ParseCertificate(certificate.Certificate[0])
			if err != nil {
				return invalid("invalid TLS client certificate")
			}
			now := time.Now()
			if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
				return invalid("TLS client certificate is outside its validity period")
			}
			signer, ok := certificate.PrivateKey.(crypto.Signer)
			if !ok {
				return invalid("TLS client private key must be a signer")
			}
			public, err := x509.MarshalPKIXPublicKey(signer.Public())
			if err != nil {
				return invalid("invalid TLS client private key")
			}
			expected, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
			if err != nil || !bytes.Equal(public, expected) {
				return invalid("TLS client certificate and private key do not match")
			}
		}
	}
	return nil
}

// New opens an existing namespace and fails closed if any identity binding is
// missing or inconsistent. It never writes operator metadata. Every endpoint
// must belong to the expected cluster. Production endpoints require verified
// mutual TLS with static client certificates and TLS 1.2 or newer; plain HTTP
// is limited to explicitly permitted loopback endpoints.
func New(ctx context.Context, o Options) (*Backend, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is nil", ErrInvalidConfiguration)
	}
	if err := validateOptions(o); err != nil {
		return nil, err
	}
	if o.DialTimeout == 0 {
		o.DialTimeout = 5 * time.Second
	}
	if o.RequestTimeout == 0 {
		o.RequestTimeout = 3 * time.Second
	}
	tlsConfig, err := cloneTLSConfig(o.TLS)
	if err != nil {
		return nil, fmt.Errorf("%w: clone TLS certificate: %v", ErrInvalidConfiguration, err)
	}
	if tlsConfig != nil {
		if tlsConfig.MinVersion == 0 {
			tlsConfig.MinVersion = tls.VersionTLS12
		}
	}
	client, err := clientv3.New(clientv3.Config{Endpoints: append([]string(nil), o.Endpoints...), DialTimeout: o.DialTimeout, TLS: tlsConfig})
	if err != nil {
		return nil, fmt.Errorf("etcd state: create client: %w", err)
	}
	b := &Backend{client: client, namespace: o.Namespace, clusterID: o.Identity.ClusterID, restoreEpoch: o.Identity.RestoreEpoch, requestTimeout: o.RequestTimeout}
	b.identityKey, _ = o.Namespace.Key("meta", "identity")
	b.restoreKey, _ = o.Namespace.Key("meta", "restore_epoch")
	ready := false
	defer func() {
		if !ready {
			_ = client.Close()
		}
	}()
	for _, endpoint := range o.Endpoints {
		reqCtx, cancel := b.requestContext(ctx)
		status, err := client.Status(reqCtx, endpoint)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("etcd state: endpoint status: %w", err)
		}
		if status.Header == nil || status.Header.ClusterId != b.clusterID {
			return nil, fmt.Errorf("%w: endpoint belongs to another cluster", ErrIdentityMismatch)
		}
	}
	reqCtx, cancel := b.requestContext(ctx)
	response, err := client.Txn(reqCtx).Then(clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey)).Commit()
	cancel()
	if err != nil {
		return nil, fmt.Errorf("etcd state: read identity: %w", err)
	}
	if response.Header == nil || response.Header.ClusterId != b.clusterID || len(response.Responses) != 2 {
		return nil, fmt.Errorf("%w: cluster or metadata response mismatch", ErrIdentityMismatch)
	}
	identity := response.Responses[0].GetResponseRange()
	restore := response.Responses[1].GetResponseRange()
	if identity == nil || restore == nil || len(identity.Kvs) != 1 || len(restore.Kvs) != 1 {
		return nil, fmt.Errorf("%w: operator metadata is missing", ErrIdentityMismatch)
	}
	if identity.Kvs[0].Lease != 0 || restore.Kvs[0].Lease != 0 {
		return nil, fmt.Errorf("%w: operator metadata must be permanent", ErrIdentityMismatch)
	}
	actual, err := decodeIdentity(string(identity.Kvs[0].Value), o.Namespace)
	if err != nil {
		return nil, err
	}
	expected := o.Identity
	expected.RestoreEpoch = ""
	if actual != expected || string(restore.Kvs[0].Value) != o.Identity.RestoreEpoch {
		return nil, fmt.Errorf("%w: operator identity or restore epoch differs", ErrIdentityMismatch)
	}
	b.identityValue = string(identity.Kvs[0].Value)
	ready = true
	return b, nil
}

// cloneTLSConfig gives the backend ownership of mutable TLS configuration data.
// PrivateKey signers cannot be cloned generically (including HSM signers) and
// retain the immutable, concurrency-safe ownership contract documented in Options.
func cloneTLSConfig(source *tls.Config) (*tls.Config, error) {
	if source == nil {
		return nil, nil
	}
	owned := source.Clone()
	if source.RootCAs != nil {
		owned.RootCAs = source.RootCAs.Clone()
	}
	if source.ClientCAs != nil {
		owned.ClientCAs = source.ClientCAs.Clone()
	}
	owned.NextProtos = slices.Clone(source.NextProtos)
	owned.CipherSuites = slices.Clone(source.CipherSuites)
	owned.CurvePreferences = slices.Clone(source.CurvePreferences)
	owned.EncryptedClientHelloConfigList = slices.Clone(source.EncryptedClientHelloConfigList)
	owned.EncryptedClientHelloKeys = slices.Clone(source.EncryptedClientHelloKeys)
	for i := range owned.EncryptedClientHelloKeys {
		owned.EncryptedClientHelloKeys[i].Config = slices.Clone(source.EncryptedClientHelloKeys[i].Config)
		owned.EncryptedClientHelloKeys[i].PrivateKey = slices.Clone(source.EncryptedClientHelloKeys[i].PrivateKey)
	}
	owned.Certificates = make([]tls.Certificate, len(source.Certificates))
	for i, cert := range source.Certificates {
		cloned, err := cloneTLSCertificate(cert)
		if err != nil {
			return nil, err
		}
		owned.Certificates[i] = cloned
	}
	if source.NameToCertificate != nil {
		owned.NameToCertificate = make(map[string]*tls.Certificate, len(source.NameToCertificate))
		for name, cert := range source.NameToCertificate {
			if cert == nil {
				owned.NameToCertificate[name] = nil
				continue
			}
			cloned, err := cloneTLSCertificate(*cert)
			if err != nil {
				return nil, err
			}
			owned.NameToCertificate[name] = &cloned
		}
	}
	return owned, nil
}

func cloneTLSCertificate(source tls.Certificate) (tls.Certificate, error) {
	owned := source
	owned.Certificate = cloneByteSlices(source.Certificate)
	owned.OCSPStaple = slices.Clone(source.OCSPStaple)
	owned.SignedCertificateTimestamps = cloneByteSlices(source.SignedCertificateTimestamps)
	owned.SupportedSignatureAlgorithms = slices.Clone(source.SupportedSignatureAlgorithms)
	owned.Leaf = nil
	if len(owned.Certificate) > 0 {
		leaf, err := x509.ParseCertificate(owned.Certificate[0])
		if err != nil {
			return tls.Certificate{}, err
		}
		owned.Leaf = leaf
	}
	return owned, nil
}

func cloneByteSlices(source [][]byte) [][]byte {
	if source == nil {
		return nil
	}
	owned := make([][]byte, len(source))
	for i := range source {
		owned[i] = slices.Clone(source[i])
	}
	return owned
}

func (b *Backend) requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, b.requestTimeout)
}
func (b *Backend) baseComparisons() []clientv3.Cmp {
	return []clientv3.Cmp{
		clientv3.Compare(clientv3.Value(b.identityKey), "=", b.identityValue),
		clientv3.Compare(clientv3.LeaseValue(b.identityKey), "=", 0),
		clientv3.Compare(clientv3.Value(b.restoreKey), "=", b.restoreEpoch),
		clientv3.Compare(clientv3.LeaseValue(b.restoreKey), "=", 0),
	}
}
func (b *Backend) Close() error { return b.client.Close() }
