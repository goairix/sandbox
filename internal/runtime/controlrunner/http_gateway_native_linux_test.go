//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/api/etcdgateway"
	"github.com/goairix/sandbox/internal/provisioning"
	"github.com/goairix/sandbox/internal/provisioning/deployment"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/goairix/sandbox/pkg/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	grpcstatus "google.golang.org/grpc/status"
)

func TestHTTPGatewayNative(t *testing.T) {
	if os.Getenv("SANDBOX_HTTP_GATEWAY_NATIVE") != "1" {
		t.Skip("requires exact owned HTTP gateway Linux fixture")
	}
	runColdCreateServiceFixtureNative(t, coldServiceNativeVariant{httpGateway: true, detached: true})
}

func TestHTTPGatewayInventoryNative(t *testing.T) {
	if os.Getenv("SANDBOX_HTTP_INVENTORY_NATIVE") != "1" {
		t.Skip("requires exact owned mounted inventory and HTTP detached destroy Linux fixture")
	}
	runColdCreateServiceFixtureNative(t, coldServiceNativeVariant{inventoryGateway: true, detached: true, destroyDetached: true, destroyTerminal: true})
}

// Build the same mounted inputs consumed by production, retaining the fixture's
// original root/issuer, signed clock, TLS-etcd and Kubernetes bridge. Only this
// variant leaves inventory absent before calling the actual operator API.
func initializeHTTPInventoryNative(t *testing.T, ctx context.Context, f httpDestroyNativeContext) *deployment.Deployment {
	t.Helper()
	directory := filepath.Join(f.directory, "inventory-mounted")
	require.NoError(t, os.Mkdir(directory, 0700))
	write := func(name string, wire []byte) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(directory, name), wire, 0600))
	}
	for _, key := range []struct {
		name  string
		value any
	}{{"root-key.pem", f.options.RootPrivateKey}, {"issuer-key.pem", f.options.IssuerPrivateKey}} {
		der, err := x509.MarshalPKCS8PrivateKey(key.value)
		require.NoError(t, err)
		wire := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		write(key.name, wire)
		clear(der)
		clear(wire)
	}
	for _, name := range []string{"ca.pem", "server.pem", "server-key.pem"} {
		wire, err := os.ReadFile(filepath.Join(f.directory, name))
		require.NoError(t, err)
		write(name, wire)
		clear(wire)
	}
	write("launcher.json", f.options.LauncherConfig)
	write("issuer.json", f.options.IssuerCertificate)
	write("api-ca.pem", f.options.API.Config.CAData)
	write("api-key", []byte("native-http-key"))
	write("kubeconfig", []byte("apiVersion: v1\nkind: Config\ncurrent-context: selected\ncontexts:\n- name: selected\n  context:\n    cluster: cluster\n    user: user\nclusters:\n- name: cluster\n  cluster:\n    server: "+f.options.API.Config.Host+"\n    certificate-authority: api-ca.pem\nusers:\n- name: user\n  user:\n    token: INVENTORY-NATIVE\n"))
	var inventory state.PreparationInventoryOptions
	inventory.QuotaSlots[7] = 1
	resources := map[string]any{"cpu": f.options.CPU, "memory": f.options.Memory, "disk": f.options.Disk, "tmp_disk": f.options.TmpDisk}
	i := f.identity
	manifest := map[string]any{
		"version": 1,
		"etcd": map[string]any{
			"endpoints":    []string{"https://127.0.0.1:2379"},
			"identity":     map[string]any{"schema_version": i.SchemaVersion, "prefix": i.Prefix, "authority_id": i.AuthorityID, "cell": i.Cell, "cluster_id": i.ClusterID, "storage_id": i.StorageID, "runtime_id": i.RuntimeID, "restore_epoch": i.RestoreEpoch},
			"tls":          map[string]any{"ca_file": "ca.pem", "cert_file": "server.pem", "key_file": "server-key.pem"},
			"dial_timeout": "2s", "request_timeout": "2s", "snapshot_cache_bytes": 262144,
		},
		"preparation":          map[string]any{"quota_slots": inventory.QuotaSlots, "quota_partition": f.options.QuotaPartition},
		"launcher_config_file": "launcher.json", "root_private_key_file": "root-key.pem", "issuer_private_key_file": "issuer-key.pem", "issuer_certificate_file": "issuer.json",
		"kubernetes": map[string]any{"kubeconfig_file": "kubeconfig", "namespace": f.options.API.Namespace, "max_active": f.options.API.MaxActive, "request_timeout": "5s"},
		"image":      f.options.Image, "node_name": f.options.NodeName, "worker_id": f.options.WorkerID, "resources": resources,
		"business": map[string]any{"principal": f.options.Business.Principal, "defaults": map[string]any{"timeout_seconds": 60, "resources": resources}},
		"http":     map[string]any{"host": "127.0.0.1", "port": 8080, "api_key_file": "api-key", "env": map[string]string{"PATH": "/usr/bin:/bin"}, "exec_timeout_seconds": 5, "max_exec_timeout_seconds": 15, "max_active": 4, "requests_per_second": 100},
	}
	write("deployment.json", observerNativeJSON(t, manifest))
	path := filepath.Join(directory, "deployment.json")
	actualNamespace, err := state.NewNamespace(i.Prefix, i.AuthorityID, i.Cell)
	require.NoError(t, err)
	namespace := actualNamespace.Root()
	read := func() *clientv3.GetResponse {
		t.Helper()
		response, err := f.raw.Get(ctx, namespace, clientv3.WithPrefix(), clientv3.WithLimit(512))
		require.NoError(t, err)
		require.False(t, response.More)
		require.EqualValues(t, len(response.Kvs), response.Count)
		require.Equal(t, i.ClusterID, response.Header.ClusterId)
		var bytes int
		for _, kv := range response.Kvs {
			bytes += len(kv.Key) + len(kv.Value)
		}
		require.LessOrEqual(t, bytes, 1<<20, "a no-write proof must cover all original rows, not a truncated snapshot")
		return response
	}
	// Observe the original server counter as well as retained leases: a
	// Grant/Revoke pair could otherwise disappear between two lease listings.
	ca, err := os.ReadFile(filepath.Join(directory, "ca.pem"))
	require.NoError(t, err)
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(ca))
	certificate, err := tls.LoadX509KeyPair(filepath.Join(directory, "server.pem"), filepath.Join(directory, "server-key.pem"))
	require.NoError(t, err)
	metrics := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{certificate}}}}
	defer metrics.CloseIdleConnections()
	grants := func() uint64 {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://127.0.0.1:2379/metrics", nil)
		require.NoError(t, err)
		response, err := metrics.Do(request)
		require.NoError(t, err)
		wire, readErr := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
		closeErr := response.Body.Close()
		require.NoError(t, readErr)
		require.NoError(t, closeErr)
		require.Equal(t, http.StatusOK, response.StatusCode)
		require.LessOrEqual(t, len(wire), 2<<20)
		for _, line := range strings.Split(string(wire), "\n") {
			if strings.HasPrefix(line, "grpc_server_started_total{") && strings.Contains(line, `grpc_method="LeaseGrant"`) && strings.Contains(line, `grpc_service="etcdserverpb.Lease"`) {
				fields := strings.Fields(line)
				require.Len(t, fields, 2)
				value, err := strconv.ParseUint(fields[1], 10, 64)
				require.NoError(t, err)
				return value
			}
		}
		t.Fatal("original etcd LeaseGrant counter missing; cannot establish no-Grant startup")
		return 0
	}
	before := read()
	grantsBefore := grants()
	leasesBefore, err := f.raw.Leases(ctx)
	require.NoError(t, err)
	refused, err := deployment.OpenHTTP(ctx, path)
	require.ErrorIs(t, err, state.ErrPreparationNotReady)
	require.Nil(t, refused)
	afterRefusal := read()
	require.Equal(t, before.Kvs, afterRefusal.Kvs, "failed OpenHTTP cannot create or rewrite original metadata")
	require.Equal(t, before.Header.Revision, afterRefusal.Header.Revision)
	require.Equal(t, grantsBefore, grants(), "not-ready OpenHTTP cannot dispatch a transient Grant")
	leasesAfter, err := f.raw.Leases(ctx)
	require.NoError(t, err)
	require.ElementsMatch(t, leasesBefore.Leases, leasesAfter.Leases, "failed startup retains the exact original lease set")

	require.NoError(t, deployment.InitializeInventory(ctx, path))
	ready, err := f.backend.ReadPreparationInventory(ctx)
	require.NoError(t, err)
	require.True(t, ready.Ready)
	require.EqualValues(t, 33, ready.Total, "32 original preparation slots plus one configured quota slot")
	require.Equal(t, ready.Total, ready.Next)
	initialized := read()
	require.Greater(t, initialized.Header.Revision, before.Header.Revision)
	grantsInitialized := grants()
	require.NoError(t, deployment.InitializeInventory(ctx, path))
	repeated := read()
	require.Equal(t, initialized.Kvs, repeated.Kvs, "repeated ready initialization preserves Value/Create/Mod/Lease of every row")
	require.Equal(t, initialized.Header.Revision, repeated.Header.Revision, "ready re-entry performs no hidden state write")
	require.Equal(t, grantsInitialized, grants(), "ready re-entry cannot dispatch and revoke a hidden lease")
	leasesAfter, err = f.raw.Leases(ctx)
	require.NoError(t, err)
	require.ElementsMatch(t, leasesBefore.Leases, leasesAfter.Leases)

	owner, err := deployment.OpenHTTP(ctx, path)
	require.NoError(t, err)
	require.NotNil(t, owner)
	require.NotNil(t, owner.Service())
	t.Cleanup(func() { require.NoError(t, owner.Close()) })
	opened := read()
	require.Equal(t, repeated.Kvs, opened.Kvs)
	require.Equal(t, repeated.Header.Revision, opened.Header.Revision)
	t.Logf("actual mounted operator initialized Total=%d; repeated Ready retained exact rows and revision=%d; formal OpenHTTP succeeded", ready.Total, repeated.Header.Revision)
	return owner
}

func exerciseHTTPGatewayInventoryNative(t *testing.T, ctx context.Context, owner *deployment.Deployment, f httpDestroyNativeContext, bridgeCount func() int) {
	t.Helper()
	settings := owner.HTTPSettings()
	require.NotNil(t, settings)
	service := &httpNativeService{PlainColdService: owner.Service(), results: make(chan execServiceNativeAnswer, 1)}
	gateway, err := etcdgateway.New(service, settings)
	settings.Zero()
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serverCtx, stopServer := context.WithCancel(ctx)
	serverDone := make(chan error, 1)
	go func() { serverDone <- etcdgateway.Serve(serverCtx, listener, gateway) }()
	defer func() {
		stopServer()
		require.NoError(t, <-serverDone, "mounted Gateway sockets and handlers physically joined before Deployment.Close")
	}()
	transport := &http.Transport{MaxIdleConns: 2, MaxIdleConnsPerHost: 2}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 15 * time.Second, Transport: transport}
	origin := "http://" + listener.Addr().String()
	httpExec := false
	beforeDestroy := func(path string) {
		require.True(t, strings.HasPrefix(path, "/api/v1/sandboxes/"))
		id := strings.TrimPrefix(path, "/api/v1/sandboxes/")
		before, err := f.backend.ReadSandboxSnapshot(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, before)
		bridges := bridgeCount()
		var executed types.ExecResponse
		httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodPost, path+"/exec", "", types.ExecRequest{Language: "bash", Code: "printf 'inventory-http-out\\n'; printf 'inventory-http-err\\n' >&2; exit 7"}, true), 200, &executed)
		require.Equal(t, 7, executed.ExitCode)
		require.Equal(t, "inventory-http-out\n", executed.Stdout)
		require.Equal(t, "inventory-http-err\n", executed.Stderr)
		terminal := requireHTTPNativeExec(t, ctx, service, true)
		require.Equal(t, "local_terminal", terminal.Receipt.State())
		originalControl := before.View.Records().Control
		require.NotNil(t, originalControl)
		require.NotNil(t, originalControl.Runtime)
		require.Equal(t, uint32(7<<8), terminal.Receipt.RootWaitStatus())
		require.Equal(t, p.RuntimeReference(*originalControl.Runtime), terminal.Receipt.Context().Runtime)
		require.Equal(t, originalControl.Generation, terminal.Receipt.Context().Generation)
		require.Equal(t, originalControl.DataGateEpoch, terminal.Receipt.Context().DataGateEpoch)
		require.Equal(t, bridges+1, bridgeCount(), "the added HTTP operation owns exactly one actual bridge; the fixture's original cap is unchanged")
		after, err := f.backend.ReadSandboxSnapshot(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, after)
		require.Equal(t, before.View.Records().Control, after.View.Records().Control, "HTTP Exec retains the same runtime/generation/data gate before original DELETE")
		httpExec = true
	}
	httpDestroyNativeDetachedTerminal(t, ctx, f, client, origin, beforeDestroy)
	require.True(t, httpExec, "the complete destroy path must include the mounted Gateway's actual HTTP Exec route")
	t.Log("mounted operator→OpenHTTP→original HTTP Create/Exec/DELETE→production worker quota refund→GET404")
}

// Failure diagnostics are read only after the original production command's
// Wait. Fixed known Task points and scalar phase fields keep logs bounded and
// cannot mint recovery authority or turn the original failed run into success.
func cleanupHTTPInventoryNativeFailure(t *testing.T, raw *clientv3.Client, setup cleanupExecutorSetup, lease clientv3.LeaseID, directory string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if lease != 0 {
		ttl, err := raw.TimeToLive(ctx, lease, clientv3.WithAttachedKeys())
		if err != nil {
			t.Logf("joined cleanup failure original session TTL grpc_code=%s", grpcstatus.Code(err))
		} else {
			t.Logf("joined cleanup failure original session lease=%d TTL_seconds=%d attached_guards=%d", lease, ttl.TTL, len(ttl.Keys))
		}
	}
	base := fmt.Sprintf("%sp/%02x/tasks/%s", setup.Binding.Namespace, setup.Task.Partition, setup.Task.TaskID)
	for _, suffix := range []string{"", "/execution", "/checkpoint", "/claim", "/runtime-quiescence", "/runtime-termination", "/runtime-provider-cleanup", "/owner-release"} {
		response, err := raw.Get(ctx, base+suffix, clientv3.WithLimit(1))
		if err != nil {
			t.Logf("joined cleanup failure native point=%s grpc_code=%s", suffix, grpcstatus.Code(err))
			continue
		}
		if len(response.Kvs) == 0 {
			t.Logf("joined cleanup failure native point=%s absent header=%d", suffix, response.Header.Revision)
			continue
		}
		kv := response.Kvs[0]
		if len(kv.Value) > 256<<10 {
			t.Logf("joined cleanup failure native point=%s value exceeds diagnostic bound", suffix)
			continue
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(kv.Value, &fields) != nil {
			t.Logf("joined cleanup failure native point=%s non-object bytes=%d birth=%d mod=%d lease=%d", suffix, len(kv.Value), kv.CreateRevision, kv.ModRevision, kv.Lease)
			continue
		}
		var phase, confirmed, state string
		_ = json.Unmarshal(fields["phase"], &phase)
		_ = json.Unmarshal(fields["confirmed"], &confirmed)
		_ = json.Unmarshal(fields["state"], &state)
		var run struct {
			Joined bool `json:"joined"`
			Worker struct {
				PID int `json:"pid"`
			} `json:"worker"`
		}
		_ = json.Unmarshal(fields["run"], &run)
		t.Logf("joined cleanup failure native point=%s phase=%.64s confirmed=%.64s state=%.64s run_joined=%t worker_pid=%d bytes=%d birth=%d mod=%d lease=%d", suffix, phase, confirmed, state, run.Joined, run.Worker.PID, len(kv.Value), kv.CreateRevision, kv.ModRevision, kv.Lease)
	}
	config, err := launcherconfig.Decode(setup.LoaderConfig)
	if err == nil {
		clock, clockErr := p.NewSignedClockClient(p.SignedClockClientOptions{
			Binding: config.TrustBinding(), PublicKey: config.ClockPublicKey, Audience: config.ClockAudience,
			Dial: func(ctx context.Context) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", config.ClockSocket)
			},
		})
		if clockErr == nil {
			start := time.Now()
			observed, observeErr := clock.Observe(ctx)
			t.Logf("joined cleanup failure original signed clock audience=%.64s UTC=%s uncertainty_ns=%d elapsed_ns=%d failed=%t", config.ClockAudience, observed.UTC.Format(time.RFC3339Nano), observed.Uncertainty.Nanoseconds(), time.Since(start).Nanoseconds(), observeErr != nil)
		}
	}

	ca, err := os.ReadFile(filepath.Join(directory, "ca.pem"))
	if err != nil {
		t.Log("joined cleanup failure metrics CA unavailable")
		return
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Log("joined cleanup failure metrics CA invalid")
		return
	}
	certificate, err := tls.LoadX509KeyPair(filepath.Join(directory, "server.pem"), filepath.Join(directory, "server-key.pem"))
	if err != nil {
		t.Log("joined cleanup failure metrics credential unavailable")
		return
	}
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{certificate}}}}
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://127.0.0.1:2379/metrics", nil)
	if err != nil {
		t.Log("joined cleanup failure metrics request invalid")
		return
	}
	response, err := client.Do(request)
	if err != nil {
		t.Log("joined cleanup failure metrics transport failed")
		return
	}
	wire, readErr := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || response.StatusCode != http.StatusOK || len(wire) > 2<<20 {
		t.Logf("joined cleanup failure metrics refused status=%d bytes=%d read_failed=%t close_failed=%t", response.StatusCode, len(wire), readErr != nil, closeErr != nil)
		return
	}
	logged := 0
	for _, line := range strings.Split(string(wire), "\n") {
		if !strings.HasPrefix(line, "grpc_server_handled_total{") || strings.Contains(line, `grpc_code="OK"`) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		count, err := strconv.ParseUint(fields[1], 10, 64)
		if err == nil && count > 0 && len(line) <= 512 && logged < 16 {
			t.Logf("joined cleanup failure native RPC %s", line)
			logged++
		}
	}
}

func TestHTTPGatewayTTLNative(t *testing.T) {
	if os.Getenv("SANDBOX_HTTP_TTL_NATIVE") != "1" {
		t.Skip("requires exact owned HTTP TTL renewal Linux fixture")
	}
	runColdCreateServiceFixtureNative(t, coldServiceNativeVariant{ttlGateway: true, detached: true})
}

func TestHTTPGatewayTTLDestroyNative(t *testing.T) {
	if os.Getenv("SANDBOX_HTTP_TTL_DESTROY_NATIVE") != "1" {
		t.Skip("requires exact owned HTTP TTL renewal and detached destroy Linux fixture")
	}
	runColdCreateServiceFixtureNative(t, coldServiceNativeVariant{ttlGateway: true, detached: true, destroyDetached: true, destroyTerminal: true})
}

func TestHTTPGatewayTTLRecoveryNative(t *testing.T) {
	if os.Getenv("SANDBOX_HTTP_TTL_RECOVERY_NATIVE") != "1" {
		t.Skip("requires exact owned HTTP TTL completion-loss and cold-recovery Linux fixture")
	}
	runColdCreateServiceFixtureNative(t, coldServiceNativeVariant{ttlGateway: true, ttlRecovery: true, detached: true})
}

func TestHTTPGatewayTTLRotationRecoveryNative(t *testing.T) {
	if os.Getenv("SANDBOX_HTTP_TTL_ROTATION_RECOVERY_NATIVE") != "1" {
		t.Skip("requires exact owned HTTP TTL receipt-leaf rotation and cold-recovery Linux fixture")
	}
	// This proves an actual replacement leaf and fixed-candidate recovery, not
	// automatic fifteen-minute maintenance or a thirty-minute lifetime run.
	runColdCreateServiceFixtureNative(t, coldServiceNativeVariant{ttlGateway: true, ttlRecovery: true, ttlRotation: true, detached: true})
}

// Observe the real service return without replacing any runtime, bridge, Wait,
// credential, admission or writer operation. Each bounded scenario consumes it.
type httpNativeService struct {
	*provisioning.PlainColdService
	results chan execServiceNativeAnswer
}

func (s *httpNativeService) Exec(ctx context.Context, sandboxID, requestID string, request p.ExecutionRequest, stdout, stderr io.Writer) (provisioning.PlainExecResult, error) {
	result, err := s.PlainColdService.Exec(ctx, sandboxID, requestID, request, stdout, stderr)
	s.results <- execServiceNativeAnswer{result: result, err: err}
	return result, err
}

func requireHTTPNativeExec(t *testing.T, ctx context.Context, service *httpNativeService, terminal bool) provisioning.PlainExecResult {
	t.Helper()
	select {
	case answer := <-service.results:
		if terminal {
			require.NoError(t, answer.err)
			require.Equal(t, provisioning.PlainExecLocalTerminal, answer.result.State)
			require.Equal(t, "local_terminal", answer.result.Receipt.State())
			require.True(t, answer.result.Receipt.DrainConfirmed())
		} else {
			require.Error(t, answer.err)
			require.Equal(t, provisioning.PlainExecUnknown, answer.result.State)
			require.NotEqual(t, "local_terminal", answer.result.Receipt.State())
		}
		require.NoError(t, answer.result.Operation.Validate())
		require.NoError(t, answer.result.Effect.Validate())
		return answer.result
	case <-ctx.Done():
		t.Fatal("original HTTP Exec/renewal/Wait/IO owner did not join")
		return provisioning.PlainExecResult{}
	}
}

func httpNativeRequest(t *testing.T, client *http.Client, origin, method, path, key string, body any, authenticated bool) *http.Response {
	t.Helper()
	wire, err := json.Marshal(body)
	require.NoError(t, err)
	r, err := http.NewRequest(method, origin+path, bytes.NewReader(wire))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	if authenticated {
		r.Header.Set("Authorization", "Bearer native-http-key")
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	response, err := client.Do(r)
	require.NoError(t, err)
	return response
}

func httpNativeRead(t *testing.T, response *http.Response, status int, output any) []byte {
	t.Helper()
	defer response.Body.Close()
	wire, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	require.NoError(t, err)
	require.LessOrEqual(t, len(wire), 2<<20)
	require.Equal(t, status, response.StatusCode, "%s", wire)
	if output != nil {
		require.NoError(t, json.Unmarshal(wire, output))
	}
	return wire
}

type httpNativeEvent struct {
	event string
	data  json.RawMessage
}

func httpNativeNextEvent(t *testing.T, reader *bufio.Reader) httpNativeEvent {
	t.Helper()
	var event httpNativeEvent
	for {
		line, err := reader.ReadString('\n')
		require.NoError(t, err, "original HTTP stream ended before an expected event")
		require.LessOrEqual(t, len(line), 128<<10)
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			if event.event != "" {
				return event
			}
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			event.event = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			event.data = json.RawMessage(strings.TrimPrefix(line, "data: "))
		}
	}
}

func exerciseHTTPGatewayNative(t *testing.T, ctx context.Context, realService *provisioning.PlainColdService, hooks *execServiceNativeHooks, zeroPod func(), onePod func()) {
	t.Helper()
	service := &httpNativeService{PlainColdService: realService, results: make(chan execServiceNativeAnswer, 8)}
	settings := &deployment.HTTPSettings{APIKey: []byte("native-http-key"), Env: map[string]string{"PATH": "/usr/bin:/bin"}, ExecTimeoutSeconds: 5, MaxExecTimeoutSeconds: 15, MaxActive: 4, RequestsPerSecond: 100, UID: 1000, GID: 1000}
	gateway, err := etcdgateway.New(service, settings)
	require.NoError(t, err)
	settings.Zero()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serverCtx, cancelServer := context.WithCancel(ctx)
	serverDone := make(chan error, 1)
	go func() { serverDone <- etcdgateway.Serve(serverCtx, listener, gateway) }()
	serverJoined := false
	t.Cleanup(func() {
		cancelServer()
		if !serverJoined {
			require.NoError(t, <-serverDone)
		}
	})
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{MaxIdleConns: 2, MaxIdleConnsPerHost: 2}}
	t.Cleanup(client.CloseIdleConnections)
	origin := "http://" + listener.Addr().String()
	create := types.CreateSandboxRequest{Mode: "persistent"}
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodPost, "/api/v1/sandboxes", "", create, false), 401, nil)
	zeroPod()
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodGet, "/ready", "", nil, false), 200, nil)
	key := uuid.NewString()
	response := httpNativeRequest(t, client, origin, http.MethodPost, "/api/v1/sandboxes", key, create, true)
	require.Equal(t, key, response.Header.Get("Idempotency-Key"))
	var first types.CreateSandboxResponse
	httpNativeRead(t, response, 201, &first)
	require.False(t, first.Reused)
	require.Equal(t, "running", first.State)
	require.Equal(t, "persistent", first.Mode)
	require.Equal(t, 60, first.Timeout)
	require.NotZero(t, first.CreatedAt)
	require.NotNil(t, first.ExpiresAt)
	onePod()
	var replay types.CreateSandboxResponse
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodPost, "/api/v1/sandboxes", key, create, true), 201, &replay)
	require.Equal(t, first.ID, replay.ID)
	require.True(t, replay.Reused)
	require.Equal(t, first.CreatedAt, replay.CreatedAt)
	conflict := create
	conflict.Timeout = 61
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodPost, "/api/v1/sandboxes", key, conflict, true), 409, nil)
	var current types.SandboxResponse
	path := "/api/v1/sandboxes/" + first.ID
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodGet, path, "", nil, true), 200, &current)
	require.Equal(t, first.SandboxResponse, current)
	unsupported := httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodPut, path, "", nil, true), 501, nil)
	require.Contains(t, string(unsupported), `"code":"unsupported"`)
	execPath := path + "/exec"
	command := types.ExecRequest{Language: "bash", Code: "printf 'http-stdout\\n'; printf 'http-stderr\\n' >&2; exit 7"}
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodPost, execPath, "not-exec-dedup", command, true), 400, nil)
	var syncResult types.ExecResponse
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodPost, execPath, "", command, true), 200, &syncResult)
	require.Equal(t, 7, syncResult.ExitCode)
	require.Equal(t, "http-stdout\n", syncResult.Stdout)
	require.Equal(t, "http-stderr\n", syncResult.Stderr)
	terminal := requireHTTPNativeExec(t, ctx, service, true)
	require.Equal(t, uint32(7<<8), terminal.Receipt.RootWaitStatus())
	response = httpNativeRequest(t, client, origin, http.MethodPost, execPath+"/stream", "", command, true)
	require.Equal(t, 200, response.StatusCode)
	require.Contains(t, response.Header.Get("Content-Type"), "text/event-stream")
	reader := bufio.NewReader(response.Body)
	var stdout, stderr strings.Builder
	for {
		event := httpNativeNextEvent(t, reader)
		if event.event == "done" {
			var done types.SSEDoneData
			require.NoError(t, json.Unmarshal(event.data, &done))
			require.Equal(t, 7, done.ExitCode)
			break
		}
		require.NotEqual(t, "error", event.event, "%s", event.data)
		var output types.SSEStdoutData
		require.NoError(t, json.Unmarshal(event.data, &output))
		if event.event == "stdout" {
			stdout.WriteString(output.Content)
		} else if event.event == "stderr" {
			stderr.WriteString(output.Content)
		}
	}
	response.Body.Close()
	require.Equal(t, "http-stdout\n", stdout.String())
	require.Equal(t, "http-stderr\n", stderr.String())
	requireHTTPNativeExec(t, ctx, service, true)

	short := types.ExecRequest{Language: "bash", Code: "printf 'http-disconnect-started\\n'; /bin/sleep 2", Timeout: 5}
	hooks.failNext.Store(true)
	response = httpNativeRequest(t, client, origin, http.MethodPost, execPath+"/stream", "", short, true)
	reader = bufio.NewReader(response.Body)
	var disconnectOutput strings.Builder
	for i := 0; i < 16 && !strings.Contains(disconnectOutput.String(), "http-disconnect-started"); i++ {
		event := httpNativeNextEvent(t, reader)
		require.NotEqual(t, "error", event.event, "%s", event.data)
		require.NotEqual(t, "done", event.event, "command finished before the actual disconnect proof")
		if event.event == "stdout" {
			var output types.SSEStdoutData
			require.NoError(t, json.Unmarshal(event.data, &output))
			require.LessOrEqual(t, disconnectOutput.Len()+len(output.Content), 64<<10)
			disconnectOutput.WriteString(output.Content)
		}
	}
	require.Contains(t, disconnectOutput.String(), "http-disconnect-started")
	response.Body.Close()
	requireHTTPNativeExec(t, ctx, service, false)

	// Physically lose one marked original bridge after actual command output.
	// The authenticated HTTP error must retain that same operation/effect.
	hooks.mu.Lock()
	previous := make(map[*startupBridgeFixture]bool, len(hooks.failed))
	for bridge := range hooks.failed {
		previous[bridge] = true
	}
	hooks.mu.Unlock()
	hooks.failNext.Store(true)
	response = httpNativeRequest(t, client, origin, http.MethodPost, execPath+"/stream", "", short, true)
	reader = bufio.NewReader(response.Body)
	require.Equal(t, "stdout", httpNativeNextEvent(t, reader).event)
	hooks.mu.Lock()
	var lost *startupBridgeFixture
	for bridge := range hooks.failed {
		if !previous[bridge] {
			lost = bridge
		}
	}
	hooks.mu.Unlock()
	require.NotNil(t, lost)
	lost.Close()
	event := httpNativeNextEvent(t, reader)
	require.Equal(t, "error", event.event, "bridge loss must never produce done")
	require.Contains(t, string(event.data), "exec_unknown")
	unknown := requireHTTPNativeExec(t, ctx, service, false)
	require.Contains(t, string(event.data), unknown.Operation.OperationID)
	require.Contains(t, string(event.data), unknown.Effect.CommandID)
	response.Body.Close()

	hooks.failNext.Store(true)
	response = httpNativeRequest(t, client, origin, http.MethodPost, execPath+"/stream", "", short, true)
	require.Equal(t, "stdout", httpNativeNextEvent(t, bufio.NewReader(response.Body)).event)
	cancelServer()
	select {
	case err := <-serverDone:
		serverJoined = true
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("HTTP server shutdown did not physically join handlers")
	}
	response.Body.Close()
	requireHTTPNativeExec(t, ctx, service, false)
	require.NoError(t, realService.Close())
	onePod()
	require.Empty(t, service.results)
	t.Log("HTTP actual TLS-etcd/formal-launcher create/replay/conflict/current health/sync exit7/SSE terminal/disconnect/bridge unknown/shutdown joined")
}

// A metadata-only expiry update cannot satisfy this test: the real launcher,
// mTLS credentials, activation and signed Exec path must survive the old TTL.
// Waiting uses the actual clock shared with the signed clock server; the test
// never injects a replacement activation or advances a fixture-only clock.
type httpNativeTTLRecovery struct {
	arm            func()
	restart        func() (*provisioning.PlainColdService, *state.Backend)
	rotation       bool
	checkOriginal  func(*state.SandboxReadView)
	checkInstalled func(*state.SandboxTTLPending)
}

func exerciseHTTPGatewayTTLNative(t *testing.T, ctx context.Context, realService *provisioning.PlainColdService, backend *state.Backend, onePod func(), destroy *httpDestroyNativeContext, recovery *httpNativeTTLRecovery) {
	t.Helper()
	service := &httpNativeService{PlainColdService: realService, results: make(chan execServiceNativeAnswer, 1)}
	startServer := func() (string, func()) {
		// Serve permanently closes its Gateway on return. A cold restart owns
		// a new Gateway and fresh copied settings, never a reopened old gate.
		settings := &deployment.HTTPSettings{APIKey: []byte("native-http-key"), Env: map[string]string{"PATH": "/usr/bin:/bin"}, ExecTimeoutSeconds: 5, MaxExecTimeoutSeconds: 15, MaxActive: 4, RequestsPerSecond: 100, UID: 1000, GID: 1000}
		gateway, err := etcdgateway.New(service, settings)
		require.NoError(t, err)
		settings.Zero()
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		serverCtx, cancelServer := context.WithCancel(ctx)
		serverDone := make(chan error, 1)
		go func() { serverDone <- etcdgateway.Serve(serverCtx, listener, gateway) }()
		var once sync.Once
		return "http://" + listener.Addr().String(), func() {
			once.Do(func() {
				cancelServer()
				require.NoError(t, <-serverDone, "original HTTP handler owners joined")
			})
		}
	}
	origin, stopServer := startServer()
	t.Cleanup(func() { stopServer() })
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{MaxIdleConns: 2, MaxIdleConnsPerHost: 2}}
	t.Cleanup(client.CloseIdleConnections)

	// Every variant crosses the original ten-second expiry. Cold recovery and
	// destroy use ninety seconds for the new TTL; their fixture contexts remain
	// sixty and 120 seconds respectively. Rotation requests thirty minutes so
	// the new activation extends beyond the original fifteen-minute leaf and
	// production must issue a replacement. It still waits only the old ten seconds.
	renewalSeconds := 30
	if destroy != nil || recovery != nil {
		renewalSeconds = 90
	}
	if recovery != nil && recovery.rotation {
		renewalSeconds = 1800
	}
	create := types.CreateSandboxRequest{Mode: "persistent", Timeout: 10}
	key := uuid.NewString()
	var first types.CreateSandboxResponse
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodPost, "/api/v1/sandboxes", key, create, true), http.StatusCreated, &first)
	require.Equal(t, 10, first.Timeout)
	require.NotNil(t, first.ExpiresAt)
	originalExpiry := *first.ExpiresAt
	path := "/api/v1/sandboxes/" + first.ID
	before, err := backend.ReadSandboxView(ctx, first.ID)
	require.NoError(t, err)
	require.NotNil(t, before)
	original := before.Records().Control
	require.NotNil(t, original.Runtime)
	onePod()

	started := time.Now().UTC()
	var renewed types.UpdateTTLResponse
	var candidate *state.SandboxTTLPending
	if recovery != nil {
		if recovery.checkOriginal != nil {
			recovery.checkOriginal(before)
		}
		recovery.arm()
		var unknown map[string]any
		httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodPut, path+"/ttl", "", types.UpdateTTLRequest{Timeout: renewalSeconds}, true), http.StatusServiceUnavailable, &unknown)
		require.Equal(t, "outcome_unknown", unknown["code"])
		pendingView, err := backend.ReadSandboxView(ctx, first.ID)
		require.NoError(t, err)
		require.True(t, pendingView.Records().Control.ExpiresAt.Equal(originalExpiry), "missing completion must not silently commit old metadata")
		candidate, err = backend.ReadSandboxTTLPending(ctx, pendingView)
		require.NoError(t, err)
		require.NotNil(t, candidate)
		require.Equal(t, renewalSeconds, candidate.Timeout)
		recovery.checkInstalled(candidate)

		// Both service and Backend objects are replaced. Let actual old business
		// authority expire before retry: only the already-installed candidate may
		// authorize narrow metadata convergence; the old capability stays dead.
		// Stop and join every old HTTP handler before replacing the service
		// pointer. Closing only the business service would not synchronize the
		// Gateway's promoted methods with this cold restart.
		stopServer()
		service.PlainColdService, backend = recovery.restart()
		origin, stopServer = startServer()
		wait := time.Until(originalExpiry.Add(2 * time.Second))
		require.Positive(t, wait)
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			t.Fatal("original TTL did not elapse before cold recovery")
		}
		timer.Stop()
		onePod()
	}
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodPut, path+"/ttl", "", types.UpdateTTLRequest{Timeout: renewalSeconds}, true), http.StatusOK, &renewed)
	if candidate != nil {
		require.True(t, renewed.ExpiresAt.Equal(candidate.ExpiresAt))
		renewedView, err := backend.ReadSandboxView(ctx, first.ID)
		require.NoError(t, err)
		pending, err := backend.ReadSandboxTTLPending(ctx, renewedView)
		require.NoError(t, err)
		require.Nil(t, pending)
		current, err := backend.ReadSandboxTTLCurrent(ctx, renewedView)
		require.NoError(t, err)
		require.NotNil(t, current)
		require.Equal(t, candidate.Activation.Digest(), current.Activation.Digest(), "cold recovery cannot sign or select a replacement candidate")
		require.Equal(t, candidate.Baseline, current.Baseline)
		recovery.checkInstalled(candidate)
	}
	finished := time.Now().UTC()
	require.Equal(t, renewalSeconds, renewed.Timeout)
	require.True(t, renewed.ExpiresAt.After(originalExpiry))
	var current types.SandboxResponse
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodGet, path, "", nil, true), http.StatusOK, &current)
	require.Equal(t, first.ID, current.ID)
	require.Equal(t, renewalSeconds, current.Timeout)
	require.NotNil(t, current.ExpiresAt)
	require.True(t, current.ExpiresAt.Equal(renewed.ExpiresAt))
	require.True(t, current.CreatedAt.Add(time.Duration(renewalSeconds)*time.Second).Equal(renewed.ExpiresAt), "old API resets its TTL baseline")
	require.False(t, current.CreatedAt.Before(started))
	require.False(t, current.CreatedAt.After(finished))
	var replay types.CreateSandboxResponse
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodPost, "/api/v1/sandboxes", key, create, true), http.StatusCreated, &replay)
	require.True(t, replay.Reused)
	require.Equal(t, current, replay.SandboxResponse, "Create replay displays renewed TTL without changing original request identity")

	// Two seconds exceed the protocol's one-second authority margin. Every
	// subsequent health check, handshake and command runs after the old bound.
	wait := time.Until(originalExpiry.Add(2 * time.Second))
	if recovery == nil {
		require.Positive(t, wait, "the successful renewal must precede the old expiry")
	}
	if wait < 0 {
		wait = 0
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal("original TTL did not elapse within the fixture lifetime")
	}
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodGet, path, "", nil, true), http.StatusOK, &current)
	require.Equal(t, renewalSeconds, current.Timeout)
	command := types.ExecRequest{Language: "bash", Code: "printf 'ttl-renewed-stdout\\n'; printf 'ttl-renewed-stderr\\n' >&2; exit 7"}
	var executed types.ExecResponse
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodPost, path+"/exec", "", command, true), http.StatusOK, &executed)
	require.Equal(t, 7, executed.ExitCode)
	require.Equal(t, "ttl-renewed-stdout\n", executed.Stdout)
	require.Equal(t, "ttl-renewed-stderr\n", executed.Stderr)
	terminal := requireHTTPNativeExec(t, ctx, service, true)
	require.Equal(t, uint32(7<<8), terminal.Receipt.RootWaitStatus())
	require.Equal(t, p.RuntimeReference(*original.Runtime), terminal.Receipt.Context().Runtime)
	after, err := backend.ReadSandboxView(ctx, first.ID)
	require.NoError(t, err)
	require.NotNil(t, after)
	updated := after.Records().Control
	require.Equal(t, *original.Runtime, *updated.Runtime)
	require.Equal(t, original.Generation, updated.Generation)
	require.Equal(t, original.DataGateEpoch, updated.DataGateEpoch)
	require.True(t, updated.ExpiresAt.Equal(renewed.ExpiresAt))
	onePod()
	require.Empty(t, service.results)
	t.Log("HTTP actual PUT/GET/Create replay crossed original TTL; same runtime signed Exec exited7 with original Wait/IO drain")
	if destroy != nil {
		httpGatewayTTLDestroyNative(t, ctx, *destroy, client, origin, key, first, updated)
		onePod()
	}
}

// Continue only the sandbox created, renewed and executed above. Ordinary
// fixture inputs locate its actual source; the existing production worker must
// authenticate everything itself. Its original quota/P watch verifies refund
// before retirement, without a test-only Settle call or replacement sandbox.
func httpGatewayTTLDestroyNative(t *testing.T, ctx context.Context, f httpDestroyNativeContext, client *http.Client, origin, idempotency string, created types.CreateSandboxResponse, current *state.SandboxControlRecord) {
	t.Helper()
	var err error
	f.workspace, err = f.backend.DetachedIdentity(f.options.Business.Principal, idempotency)
	require.NoError(t, err)
	require.Equal(t, f.workspace.Hash(), current.WorkspaceHash)
	f.request.IntentID, f.request.SandboxID = current.IntentID, created.ID
	path := "/api/v1/sandboxes/" + created.ID
	var accepted httpDestroyNativeResponse
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodDelete, path, "", nil, true), http.StatusAccepted, &accepted)
	require.Equal(t, "destroy_pending", accepted.Code)
	require.Equal(t, provisioning.PlainColdDestroyPending, accepted.State)
	require.Equal(t, created.ID, accepted.SandboxID)
	require.Equal(t, current.Generation, accepted.Generation)
	require.Equal(t, p.RuntimeReference(*current.Runtime), accepted.Runtime)
	require.NoError(t, accepted.Task.Validate())
	httpDestroyNativeBirth(t, ctx, f, accepted)
	httpDestroyNativeAutomatic(t, ctx, f, accepted, nil)

	owner, err := f.backend.LoadOwner(ctx, f.workspace)
	require.NoError(t, err)
	require.Nil(t, owner)
	var absent types.ErrorResponse
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodGet, path, "", nil, true), http.StatusNotFound, &absent)
	require.Equal(t, "not_found", absent.Code)
	proof, err := f.backend.ReadSandboxReleased(ctx, created.ID)
	require.NoError(t, err)
	require.NotNil(t, proof, "the original release reader must authenticate cleanup of the renewed runtime")
	require.Positive(t, f.provider.patches.Load())
	require.EqualValues(t, 1, f.provider.deletes.Load())
	t.Logf("HTTP same sandbox=%s old TTL crossed→signed Exec→DELETE202→original managed cleanup/refund/Pgone→GET404; runtime UID=%s BootID=%s", created.ID, current.Runtime.UID, current.Runtime.BootID)
}
