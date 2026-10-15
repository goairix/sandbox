//go:build linux && (amd64 || arm64)

package nodeobserver

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"io"
	"math/big"
	"net/http"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type credentialTestClock struct{}

func (credentialTestClock) Observe(ctx context.Context) (p.ClockObservation, error) {
	return p.ClockObservation{UTC: time.Now().UTC()}, ctx.Err()
}

func TestNodeObserverCredentialsCloseOwnership(t *testing.T) {
	public, callerKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	callerTLS := &tls.Config{RootCAs: x509.NewCertPool(), Certificates: []tls.Certificate{{Certificate: [][]byte{[]byte("bounded-public-cert")}, PrivateKey: callerKey}}}
	ownedTLS, err := copyObserverTLS(callerTLS)
	require.NoError(t, err)
	signer := ownedTLS.Certificates[0].PrivateKey.(*observerTLSKey)
	ownedKey := signer.key.(ed25519.PrivateKey)
	callerKube := []byte("owned-kube-private-key")
	ownedKube := bytes.Clone(callerKube)
	config := &rest.Config{BearerToken: "borrowed-immutable-token", TLSClientConfig: rest.TLSClientConfig{KeyData: ownedKube}}
	transport := &http.Transport{TLSClientConfig: &tls.Config{}}
	observer := &Observer{options: Options{EtcdTLS: ownedTLS, KubernetesConfig: config, PrivateKey: bytes.Clone(callerKey)}, apiTransport: transport, provider: providerGuard{client: &kubernetes.Clientset{}}}
	require.NoError(t, observer.Close())
	require.True(t, bytes.Equal(make([]byte, len(ownedKey)), ownedKey), "producer-owned TLS key bytes survive Close")
	require.True(t, bytes.Equal(make([]byte, len(ownedKube)), ownedKube), "producer-owned Kubernetes KeyData survives Close")
	_, err = signer.Sign(rand.Reader, []byte("closed"), crypto.Hash(0))
	require.Error(t, err)
	require.Empty(t, config.BearerToken)
	require.Nil(t, observer.options.EtcdTLS)
	require.Nil(t, observer.options.KubernetesConfig)
	require.Nil(t, observer.provider.client)
	require.Nil(t, observer.apiTransport)
	require.Nil(t, transport.TLSClientConfig)
	message := []byte("caller remains usable")
	require.True(t, ed25519.Verify(public, message, ed25519.Sign(callerKey, message)))
	require.Equal(t, []byte("owned-kube-private-key"), callerKube)
}

func TestNodeObserverCredentialsFailurePreservesCaller(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	config := &tls.Config{RootCAs: x509.NewCertPool(), Certificates: []tls.Certificate{{Certificate: [][]byte{[]byte("prefix"), bytes.Repeat([]byte{1}, 8193)}, PrivateKey: key}}}
	_, err = copyObserverTLS(config)
	require.Error(t, err)
	message := []byte("failed-copy-caller")
	require.True(t, ed25519.Verify(public, message, ed25519.Sign(key, message)))
	config.Certificates[0].Certificate = [][]byte{[]byte("bounded")}
	observer, err := New(Options{MaxSlots: 1, MaxActive: 1, RequestTimeout: time.Second, TerminationTimeout: time.Second, EtcdEndpoints: []string{"https://127.0.0.1:2379"}, EtcdClusterID: 1, EtcdIdentity: []byte(`{"version":1}`), PrivateKey: key, NodeName: "node", KubernetesNamespace: "runtimes", JournalDirectory: "/tmp/observer-credential-failure", KubernetesConfig: &rest.Config{}, Clock: credentialTestClock{}, EtcdTLS: config})
	require.Error(t, err)
	require.Nil(t, observer)
	require.True(t, ed25519.Verify(public, message, ed25519.Sign(key, message)))
}

type blockedCredentialSigner struct {
	key              ed25519.PrivateKey
	started, release chan struct{}
}

func (s *blockedCredentialSigner) Public() crypto.PublicKey { return s.key.Public() }
func (s *blockedCredentialSigner) Sign(_ io.Reader, message []byte, _ crypto.SignerOpts) ([]byte, error) {
	close(s.started)
	<-s.release
	return ed25519.Sign(s.key, message), nil
}

func TestNodeObserverCredentialsSignerCloseJoins(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	blocked := &blockedCredentialSigner{key: key, started: make(chan struct{}), release: make(chan struct{})}
	owned := &observerTLSKey{key: blocked, public: blocked.Public()}
	signed := make(chan error, 1)
	go func() { _, err := owned.Sign(rand.Reader, []byte("original handshake"), crypto.Hash(0)); signed <- err }()
	<-blocked.started
	closed := make(chan struct{})
	go func() { owned.close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("key closed before actual original Sign returned")
	case <-time.After(10 * time.Millisecond):
	}
	close(blocked.release)
	require.NoError(t, <-signed)
	<-closed
	_, err = owned.Sign(rand.Reader, []byte("late handshake"), crypto.Hash(0))
	require.Error(t, err)
}

func TestNodeObserverCredentialsConcreteKeys(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	for name, caller := range map[string]crypto.Signer{"ECDSA": ec, "RSA": rsaKey} {
		t.Run(name, func(t *testing.T) {
			out, err := copyObserverTLS(&tls.Config{RootCAs: x509.NewCertPool(), Certificates: []tls.Certificate{{Certificate: [][]byte{[]byte("bounded")}, PrivateKey: caller}}})
			require.NoError(t, err)
			wrapper := out.Certificates[0].PrivateKey.(*observerTLSKey)
			var private *big.Int
			switch key := wrapper.key.(type) {
			case *ecdsa.PrivateKey:
				private = key.D
			case *rsa.PrivateKey:
				private = key.D
			}
			clearOwnedTLS(out)
			require.Zero(t, private.Sign())
			require.Nil(t, wrapper.key)
			message := sha256.Sum256([]byte("caller after owned wipe"))
			_, err = caller.Sign(rand.Reader, message[:], crypto.SHA256)
			require.NoError(t, err)
		})
	}
}
