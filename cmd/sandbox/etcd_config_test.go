package main

import (
	"crypto/x509"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/goairix/sandbox/internal/config"
	"github.com/stretchr/testify/require"
)

func TestEtcdOptionsFromConfigLoadsMutualTLS(t *testing.T) {
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	directory := t.TempDir()
	certificate := server.TLS.Certificates[0]
	privateKey, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	require.NoError(t, err)
	ca := filepath.Join(directory, "ca.pem")
	cert := filepath.Join(directory, "client.pem")
	key := filepath.Join(directory, "client-key.pem")
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}), 0600))
	require.NoError(t, os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}), 0600))
	require.NoError(t, os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKey}), 0600))

	options, err := etcdOptionsFromConfig(config.EtcdConfig{
		Endpoints: []string{"etcd-a:2379"}, Prefix: "/sandbox/cell-a/", TimeoutMS: 3000,
		CAFile: ca, CertFile: cert, KeyFile: key,
	})
	require.NoError(t, err)
	require.Equal(t, "/sandbox/cell-a/", options.Prefix)
	require.Equal(t, []string{"etcd-a:2379"}, options.Endpoints)
	require.NotNil(t, options.TLS)
	require.Len(t, options.TLS.Certificates, 1)
	parsed, err := x509.ParseCertificate(certificate.Certificate[0])
	require.NoError(t, err)
	_, err = parsed.Verify(x509.VerifyOptions{Roots: options.TLS.RootCAs, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(ca, []byte("invalid"), 0600))
	_, err = etcdOptionsFromConfig(config.EtcdConfig{CAFile: ca, CertFile: cert, KeyFile: key})
	require.ErrorContains(t, err, "CA")
}

func TestEtcdOptionsFromConfigAllowsNoTLS(t *testing.T) {
	options, err := etcdOptionsFromConfig(config.EtcdConfig{
		Endpoints: []string{"etcd-a:2379"}, Prefix: "/sandbox/cell-a/", TimeoutMS: 3000,
	})
	require.NoError(t, err)
	require.Nil(t, options.TLS)

	options, err = etcdOptionsFromConfig(config.EtcdConfig{
		Endpoints: []string{"etcd-a:2379"}, Prefix: "/sandbox/cell-a/", TimeoutMS: 3000,
		CAFile: "/missing/ca.pem",
	})
	require.ErrorContains(t, err, "load etcd CA")
	require.Nil(t, options.TLS)
}
