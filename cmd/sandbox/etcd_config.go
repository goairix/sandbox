package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"time"

	"github.com/goairix/sandbox/internal/config"
	etcdstate "github.com/goairix/sandbox/internal/storage/state/etcd"
)

// etcdOptionsFromConfig loads the new deployment's client identity before
// opening any state connection. The configured CA authenticates etcd, while
// the certificate and key identify this API replica to the server.
func etcdOptionsFromConfig(c config.EtcdConfig) (etcdstate.Options, error) {
	caPEM, err := os.ReadFile(c.CAFile)
	if err != nil {
		return etcdstate.Options{}, fmt.Errorf("load etcd CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return etcdstate.Options{}, fmt.Errorf("load etcd CA: no certificates")
	}
	identity, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return etcdstate.Options{}, fmt.Errorf("load etcd client identity: %w", err)
	}
	return etcdstate.Options{
		Endpoints: append([]string(nil), c.Endpoints...),
		Prefix:    c.Prefix,
		Timeout:   time.Duration(c.TimeoutMS) * time.Millisecond,
		TLS: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			RootCAs:      roots,
			Certificates: []tls.Certificate{identity},
		},
	}, nil
}
