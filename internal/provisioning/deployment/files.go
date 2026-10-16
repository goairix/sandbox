package deployment

import (
	"bytes"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func fileError(role string) error {
	return fmt.Errorf("%w: %s", errConfiguration, role)
}

func boundedFile(path string, limit int64, role string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit {
		return nil, fileError(role)
	}
	// Following projected Secret links is intentional. Nonblocking open and a
	// descriptor check prevent a replacement FIFO from hanging startup.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fileError(role)
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit {
		return nil, fileError(role)
	}
	wire, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || len(wire) == 0 || int64(len(wire)) > limit {
		clear(wire)
		return nil, fileError(role)
	}
	return wire, nil
}

func relativeFile(base, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(base, path)
}

func signingPrivateKey(path, role string) (ed25519.PrivateKey, error) {
	wire, err := boundedFile(path, 4096, role)
	if err != nil {
		return nil, err
	}
	defer clear(wire)
	block, rest := pem.Decode(wire)
	if block == nil || block.Type != "PRIVATE KEY" || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 || !bytes.HasPrefix(bytes.TrimSpace(wire), []byte("-----BEGIN PRIVATE KEY-----")) {
		return nil, fileError(role)
	}
	defer clear(block.Bytes)
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	private, ok := key.(ed25519.PrivateKey)
	if err != nil || !ok || len(private) != ed25519.PrivateKeySize {
		return nil, fileError(role)
	}
	return private, nil
}

func mountedTLS(caPath, certPath, keyPath, serverName string) (*tls.Config, error) {
	ca, err := boundedFile(caPath, 65536, "etcd CA")
	if err != nil {
		return nil, err
	}
	cert, err := boundedFile(certPath, 65536, "etcd client certificate")
	if err != nil {
		return nil, err
	}
	key, err := boundedFile(keyPath, 65536, "etcd client key")
	if err != nil {
		return nil, err
	}
	defer clear(key)
	roots := x509.NewCertPool()
	certificate, err := tls.X509KeyPair(cert, key)
	if err != nil || !roots.AppendCertsFromPEM(ca) {
		return nil, fileError("etcd TLS")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{certificate}, ServerName: serverName}, nil
}

func loadKubeconfig(path string) (*rest.Config, error) {
	wire, err := boundedFile(path, 65536, "kubeconfig")
	if err != nil {
		return nil, err
	}
	defer clear(wire)
	raw, err := clientcmd.Load(wire)
	if err != nil {
		return nil, fileError("kubeconfig")
	}
	for _, user := range raw.AuthInfos {
		if user == nil || user.Exec != nil || user.AuthProvider != nil || user.Impersonate != "" || user.ImpersonateUID != "" || len(user.ImpersonateGroups) != 0 || len(user.ImpersonateUserExtra) != 0 || len(user.Extensions) != 0 {
			return nil, fileError("kubeconfig authentication")
		}
	}
	current := raw.Contexts[raw.CurrentContext]
	if current == nil || len(raw.Extensions) != 0 {
		return nil, fileError("kubeconfig context")
	}
	cluster, user := raw.Clusters[current.Cluster], raw.AuthInfos[current.AuthInfo]
	if cluster == nil || user == nil || cluster.InsecureSkipTLSVerify || cluster.ProxyURL != "" || len(cluster.Extensions) != 0 {
		return nil, fileError("kubeconfig static HTTPS profile")
	}
	origin, err := url.Parse(cluster.Server)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || origin.Path != "" && origin.Path != "/" {
		return nil, fileError("kubeconfig HTTPS origin")
	}
	base := filepath.Dir(path)
	for _, field := range []struct {
		path *string
		data *[]byte
		role string
	}{
		{&cluster.CertificateAuthority, &cluster.CertificateAuthorityData, "Kubernetes CA"},
		{&user.ClientCertificate, &user.ClientCertificateData, "Kubernetes certificate"},
		{&user.ClientKey, &user.ClientKeyData, "Kubernetes key"},
	} {
		if len(*field.data) > 65536 {
			return nil, fileError(field.role)
		}
		if len(*field.data) == 0 && *field.path != "" {
			*field.data, err = boundedFile(relativeFile(base, *field.path), 65536, field.role)
			if err != nil {
				return nil, err
			}
		}
		*field.path = ""
	}
	defer clear(user.ClientKeyData)
	if user.Token == "" && user.TokenFile != "" {
		token, err := boundedFile(relativeFile(base, user.TokenFile), 16384, "Kubernetes token")
		if err != nil {
			return nil, err
		}
		user.Token = strings.TrimSpace(string(token))
		clear(token)
	}
	user.TokenFile = ""
	if len(user.Token) > 16384 {
		return nil, fileError("Kubernetes token")
	}
	config, err := clientcmd.NewNonInteractiveClientConfig(*raw, raw.CurrentContext, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err != nil {
		return nil, fileError("kubeconfig static configuration")
	}
	config.KeyData = bytes.Clone(config.KeyData)
	return config, nil
}
