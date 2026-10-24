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

// etcdOptionsFromConfig prepares the selected connection mode. With no TLS
// files it uses a plaintext client; a CA authenticates a TLS server, and an
// optional certificate/key pair supplies the client identity.
func etcdOptionsFromConfig(c config.EtcdConfig) (etcdstate.Options, error) {
	options := etcdstate.Options{
		Endpoints: append([]string(nil), c.Endpoints...),
		Prefix:    c.Prefix,
		Timeout:   time.Duration(c.TimeoutMS) * time.Millisecond,
	}
	if (c.CertFile == "") != (c.KeyFile == "") {
		return etcdstate.Options{}, fmt.Errorf("load etcd client identity: certificate and key must be configured together")
	}
	if c.CAFile == "" && c.CertFile == "" {
		return options, nil
	}
	connectionTLS := &tls.Config{MinVersion: tls.VersionTLS13}
	if c.CAFile != "" {
		caPEM, err := os.ReadFile(c.CAFile)
		if err != nil {
			return etcdstate.Options{}, fmt.Errorf("load etcd CA: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(caPEM) {
			return etcdstate.Options{}, fmt.Errorf("load etcd CA: no certificates")
		}
		connectionTLS.RootCAs = roots
	}
	if c.CertFile != "" {
		identity, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return etcdstate.Options{}, fmt.Errorf("load etcd client identity: %w", err)
		}
		connectionTLS.Certificates = []tls.Certificate{identity}
	}
	options.TLS = connectionTLS
	return options, nil
}
