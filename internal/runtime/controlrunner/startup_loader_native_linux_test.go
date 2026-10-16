//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/provisioning/deployment"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestStartupLoaderNative(t *testing.T) {
	if os.Getenv("SANDBOX_STARTUP_LOADER_NATIVE") != "1" {
		t.Skip("requires exact owned TLS etcd and executable startup fixture")
	}
	dir := t.TempDir()
	admin, observerTLS, clusterID := observerNativeEtcd(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	write := func(name string, wire []byte) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, wire, 0600))
		return path
	}
	key := func(name string) ed25519.PrivateKey {
		_, private, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		der, err := x509.MarshalPKCS8PrivateKey(private)
		require.NoError(t, err)
		write(name, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
		return private
	}
	root, issuer, clockKey := key("root-key.pem"), key("issuer-key.pem"), key("clock-key.pem")
	namespace, err := state.NewNamespace("/sandbox", "observer", "v1")
	require.NoError(t, err)
	identity := state.Identity{SchemaVersion: 1, Prefix: "/sandbox", AuthorityID: "observer", Cell: "v1", ClusterID: clusterID, StorageID: "storage", RuntimeID: "launcher", RestoreEpoch: "restore"}
	identityWire, err := json.Marshal(identity)
	require.NoError(t, err)
	_, err = admin.Put(ctx, namespace.Root()+"meta/identity", string(identityWire))
	require.NoError(t, err)
	_, err = admin.Put(ctx, namespace.Root()+"meta/restore_epoch", identity.RestoreEpoch)
	require.NoError(t, err)
	_, err = admin.UserAddWithOptions(ctx, "server", "", &clientv3.UserAddOptions{NoPassword: true})
	require.NoError(t, err)
	_, err = admin.RoleAdd(ctx, "startup-loader")
	require.NoError(t, err)
	_, err = admin.RoleGrantPermission(ctx, "startup-loader", namespace.Root(), clientv3.GetPrefixRangeEnd(namespace.Root()), clientv3.PermissionType(clientv3.PermReadWrite))
	require.NoError(t, err)
	_, err = admin.UserGrantRole(ctx, "server", "startup-loader")
	require.NoError(t, err)
	binding := p.TrustBinding{Namespace: namespace.Root(), AuthorityID: "launcher", Target: "target", RestoreEpoch: "restore"}
	launcher := launcherconfig.Config{Version: 2, Binding: launcherconfig.Binding(binding), Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}, UID: 1000, GID: 1000, ContractDigest: profileFixtureContract(t), ClockSocket: ct.ClockSocket, ClockPublicKey: clockKey.Public().(ed25519.PublicKey), ClockAudience: "launcher", MaxActive: 4, Enrollment: launcherconfig.Enrollment{NodeUID: uuid.NewString(), NodeBootID: uuid.NewString()}}
	writeLauncher := func() {
		wire, err := json.Marshal(launcher)
		require.NoError(t, err)
		write("launcher.json", wire)
	}
	writeLauncher()
	now := time.Now().UTC()
	issuerCert, err := p.SignCommandIssuerCertificate(root, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: issuer.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(5 * time.Minute)})
	require.NoError(t, err)
	write("issuer.json", issuerCert)
	require.NoError(t, os.MkdirAll(filepath.Dir(ct.ClockSocket), 0700))
	listener, err := net.Listen("unix", ct.ClockSocket)
	require.NoError(t, err)
	clockCtx, stopClock := context.WithCancel(ctx)
	clockDone := make(chan error, 1)
	go func() {
		clockDone <- p.ServeSignedClock(clockCtx, listener, p.SignedClockServerOptions{Binding: binding, Key: clockKey, Source: nativeFixtureClock{}, MaxConnections: 4})
	}()
	t.Cleanup(func() {
		stopClock()
		listener.Close()
		err := <-clockDone
		require.True(t, err == nil || errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed), "original signed clock joined: %v", err)
		t.Logf("original signed clock Serve joined=%v", err)
	})
	var requests, posts atomic.Int32
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		http.Error(w, "startup must not request Pods", http.StatusForbidden)
	}))
	t.Cleanup(api.Close)
	write("api-ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw}))
	write("kubeconfig", []byte("apiVersion: v1\nkind: Config\ncurrent-context: selected\ncontexts:\n- name: selected\n  context:\n    cluster: cluster\n    user: user\nclusters:\n- name: cluster\n  cluster:\n    server: "+api.URL+"\n    certificate-authority: api-ca.pem\nusers:\n- name: user\n  user:\n    token: STARTUP-TOKEN-SENTINEL\n"))
	var inventory state.PreparationInventoryOptions
	inventory.QuotaSlots[7] = 1
	resources := map[string]any{"cpu": "2", "memory": "512Mi", "disk": "1Mi", "tmp_disk": "50Mi"}
	m := map[string]any{
		"version":              1,
		"etcd":                 map[string]any{"endpoints": []string{"https://127.0.0.1:2379"}, "identity": map[string]any{"schema_version": 1, "prefix": "/sandbox", "authority_id": "observer", "cell": "v1", "cluster_id": clusterID, "storage_id": "storage", "runtime_id": "launcher", "restore_epoch": "restore"}, "tls": map[string]any{"ca_file": "ca.pem", "cert_file": "server.pem", "key_file": "server-key.pem"}, "dial_timeout": "2s", "request_timeout": "2s", "snapshot_cache_bytes": 262144},
		"preparation":          map[string]any{"quota_slots": inventory.QuotaSlots, "quota_partition": 7},
		"launcher_config_file": "launcher.json", "root_private_key_file": "root-key.pem", "issuer_private_key_file": "issuer-key.pem", "issuer_certificate_file": "issuer.json",
		"kubernetes": map[string]any{"kubeconfig_file": "kubeconfig", "namespace": "runtimes", "max_active": 4, "request_timeout": "1s"},
		"image":      enrolledProfileFixtureImage, "node_name": "node-a", "worker_id": "startup-loader", "resources": resources,
		"business": map[string]any{"principal": "native", "defaults": map[string]any{"timeout_seconds": 60, "resources": resources}},
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	writeManifest := func() {
		wire, err := json.Marshal(m)
		require.NoError(t, err)
		write("manifest.json", wire)
	}
	writeManifest()
	check := func(name string, want error, cli bool) {
		t.Helper()
		before, err := admin.Get(ctx, "\x00", clientv3.WithFromKey())
		require.NoError(t, err)
		leasesBefore, err := admin.Leases(ctx)
		require.NoError(t, err)
		if cli {
			cmd := exec.CommandContext(ctx, "/fixture/sandbox", "--check-etcd-startup", manifestPath)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			t.Logf("%s original sandbox Cmd.Wait=%v output=%s", name, err, out)
			if want == nil {
				require.NoError(t, err)
				require.Contains(t, string(out), "etcd startup check passed")
			} else {
				require.Error(t, err)
				require.Contains(t, string(out), "etcd startup check failed")
			}
			require.NotContains(t, string(out), "STARTUP-TOKEN-SENTINEL")
			require.NotContains(t, string(out), "BEGIN PRIVATE KEY")
		} else {
			d, err := deployment.Open(ctx, manifestPath)
			if want == nil {
				require.NoError(t, err)
				require.NotNil(t, d.Service())
				require.NotNil(t, d.Backend())
				require.NoError(t, d.Close())
				require.NoError(t, d.Close())
			} else {
				require.Error(t, err)
				require.Nil(t, d)
				if !errors.Is(want, errStartupLoaderAny) {
					require.ErrorIs(t, err, want)
				}
				require.NotContains(t, err.Error(), "STARTUP-TOKEN-SENTINEL")
			}
		}
		after, err := admin.Get(ctx, "\x00", clientv3.WithFromKey())
		require.NoError(t, err)
		leasesAfter, err := admin.Leases(ctx)
		require.NoError(t, err)
		require.Equal(t, before.Header.Revision, after.Header.Revision, name+" wrote KV revision")
		require.Equal(t, before.Kvs, after.Kvs, name+" mutated KV state")
		require.Equal(t, leasesBefore.Leases, leasesAfter.Leases, name+" granted a lease")
		require.Zero(t, posts.Load())
		require.Zero(t, requests.Load())
		t.Logf("%s readonly revision=%d keys=%d leases=%d Pod POST=%d", name, after.Header.Revision, len(after.Kvs), len(leasesAfter.Leases), posts.Load())
	}
	check("missing inventory partial failure", state.ErrPreparationNotReady, false)
	check("missing inventory executable", state.ErrPreparationNotReady, true)
	certificate, err := tls.LoadX509KeyPair(filepath.Join(dir, "server.pem"), filepath.Join(dir, "server-key.pem"))
	require.NoError(t, err)
	operator, err := state.New(ctx, state.Options{Endpoints: []string{"https://127.0.0.1:2379"}, Namespace: namespace, Identity: identity, TLS: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: observerTLS.RootCAs, Certificates: []tls.Certificate{certificate}}, RequestTimeout: 2 * time.Second, PreparationInventory: &inventory})
	require.NoError(t, err)
	for {
		progress, err := operator.InitializePreparationInventory(ctx)
		require.NoError(t, err)
		if progress.Ready {
			break
		}
	}
	require.NoError(t, operator.Close())
	_, err = admin.RoleRevokePermission(ctx, "startup-loader", namespace.Root(), clientv3.GetPrefixRangeEnd(namespace.Root()))
	require.NoError(t, err)
	_, err = admin.RoleGrantPermission(ctx, "startup-loader", namespace.Root(), clientv3.GetPrefixRangeEnd(namespace.Root()), clientv3.PermissionType(clientv3.PermRead))
	require.NoError(t, err)
	check("ready Open and repeated Close with read-only etcd user", nil, false)
	check("ready executable without legacy config or Redis", nil, true)
	id := m["etcd"].(map[string]any)["identity"].(map[string]any)
	id["storage_id"] = "wrong-storage"
	writeManifest()
	check("identity mismatch", state.ErrIdentityMismatch, false)
	check("identity mismatch executable", state.ErrIdentityMismatch, true)
	id["storage_id"] = "storage"
	writeManifest()
	_, wrongClock, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	launcher.ClockPublicKey = wrongClock.Public().(ed25519.PublicKey)
	writeLauncher()
	check("wrong signed clock key", errStartupLoaderAny, false)
	check("wrong signed clock executable", errStartupLoaderAny, true)
	launcher.ClockPublicKey = clockKey.Public().(ed25519.PublicKey)
	writeLauncher()
	write("issuer.json", []byte(strings.Replace(string(issuerCert), "command_issuer", "invalid_issuer", 1)))
	check("bad issuer", errStartupLoaderAny, false)
}

var errStartupLoaderAny = errors.New("any startup loader error")
