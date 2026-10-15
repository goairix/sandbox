package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/goairix/sandbox/internal/nodeobserver"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	t "github.com/goairix/sandbox/internal/runtime/controltransport"
	"k8s.io/client-go/tools/clientcmd"
)

// This root-owned operator configuration has no management signer. Runtime
// enrollment and native command authorization remain separate protocol inputs.
type config struct {
	Version                  uint32              `json:"version"`
	Binding                  p.TrustBinding      `json:"binding"`
	Roots                    []ed25519.PublicKey `json:"roots"`
	NodeName                 string              `json:"node_name"`
	NodeUID                  string              `json:"node_uid"`
	Certificate              []byte              `json:"certificate"`
	PrivateKey               ed25519.PrivateKey  `json:"private_key"`
	ClockSocket              string              `json:"clock_socket"`
	ClockPublicKey           ed25519.PublicKey   `json:"clock_public_key"`
	ClockAudience            string              `json:"clock_audience"`
	EtcdEndpoints            []string            `json:"etcd_endpoints"`
	EtcdClusterID            uint64              `json:"etcd_cluster_id"`
	EtcdIdentity             []byte              `json:"etcd_identity"`
	EtcdCA                   []byte              `json:"etcd_ca"`
	EtcdClientCertificate    []byte              `json:"etcd_client_certificate"`
	EtcdClientKey            []byte              `json:"etcd_client_key"`
	Kubeconfig               string              `json:"kubeconfig"`
	KubernetesNamespace      string              `json:"kubernetes_namespace"`
	JournalDirectory         string              `json:"journal_directory"`
	EnrollmentSocket         string              `json:"enrollment_socket"`
	CommandAddress           string              `json:"command_address"`
	MaxSlots                 uint32              `json:"max_slots"`
	MaxActive                uint32              `json:"max_active"`
	RequestTimeoutMillis     uint32              `json:"request_timeout_millis"`
	TerminationTimeoutMillis uint32              `json:"termination_timeout_millis"`
}

func decodeConfig(wire []byte) (config, error) {
	var c config
	if len(wire) == 0 || len(wire) > 256*1024 {
		return c, fmt.Errorf("observer config exceeds bound")
	}
	d := json.NewDecoder(bytes.NewReader(wire))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, err
	}
	canonical, err := json.Marshal(c)
	if err != nil || !bytes.Equal(canonical, wire) || c.Version != 1 || c.RequestTimeoutMillis == 0 || c.RequestTimeoutMillis > 5000 || c.TerminationTimeoutMillis == 0 || c.TerminationTimeoutMillis > 30000 {
		return config{}, fmt.Errorf("invalid canonical observer config")
	}
	for _, path := range []string{c.ClockSocket, c.Kubeconfig, c.JournalDirectory, c.EnrollmentSocket} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return config{}, fmt.Errorf("observer config requires fixed absolute paths")
		}
	}
	return c, nil
}

func run(path string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("observer requires root")
	}
	if err := t.CheckProtectedPath(path, 0600); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	wire, readErr := io.ReadAll(io.LimitReader(f, 256*1024+1))
	err = errors.Join(readErr, f.Close())
	if err != nil {
		return err
	}
	defer clear(wire)
	c, err := decodeConfig(wire)
	if err != nil {
		return err
	}
	defer clear(c.PrivateKey)
	defer clear(c.EtcdClientKey)
	if err = t.CheckProtectedPath(c.Kubeconfig, 0600); err != nil {
		return err
	}
	kube, err := clientcmd.BuildConfigFromFlags("", c.Kubeconfig)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(c.EtcdCA) {
		return fmt.Errorf("invalid etcd CA")
	}
	certificate, err := tls.X509KeyPair(c.EtcdClientCertificate, c.EtcdClientKey)
	if err != nil {
		return err
	}
	clock, err := p.NewSignedClockClient(p.SignedClockClientOptions{Binding: c.Binding, Audience: c.ClockAudience, PublicKey: c.ClockPublicKey, Dial: func(ctx context.Context) (net.Conn, error) {
		if err := t.CheckProtectedPath(c.ClockSocket, os.ModeSocket|0600); err != nil {
			return nil, err
		}
		return (&net.Dialer{}).DialContext(ctx, "unix", c.ClockSocket)
	}})
	if err != nil {
		return err
	}
	observer, err := nodeobserver.New(nodeobserver.Options{Binding: c.Binding, Roots: c.Roots, NodeName: c.NodeName, NodeUID: c.NodeUID, Certificate: c.Certificate, PrivateKey: c.PrivateKey, Clock: clock, EtcdEndpoints: c.EtcdEndpoints, EtcdTLS: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{certificate}}, EtcdClusterID: c.EtcdClusterID, EtcdIdentity: c.EtcdIdentity, KubernetesConfig: kube, KubernetesNamespace: c.KubernetesNamespace, JournalDirectory: c.JournalDirectory, MaxSlots: c.MaxSlots, MaxActive: c.MaxActive, RequestTimeout: time.Duration(c.RequestTimeoutMillis) * time.Millisecond, TerminationTimeout: time.Duration(c.TerminationTimeoutMillis) * time.Millisecond})
	if err != nil {
		return err
	}
	defer observer.Close()
	if _, err = os.Lstat(c.EnrollmentSocket); !os.IsNotExist(err) {
		return fmt.Errorf("observer enrollment socket already exists")
	}
	uds, err := net.ListenUnix("unix", &net.UnixAddr{Name: c.EnrollmentSocket, Net: "unix"})
	if err != nil {
		return err
	}
	defer uds.Close()
	if err = os.Chmod(c.EnrollmentSocket, 0600); err != nil {
		return err
	}
	commands, err := net.Listen("tcp", c.CommandAddress)
	if err != nil {
		return err
	}
	defer commands.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	return observer.Serve(ctx, uds, commands)
}

func main() {
	path := flag.String("config", "/etc/sandbox-node-observer/config.json", "root-owned canonical observer configuration")
	flag.Parse()
	if err := run(*path); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
