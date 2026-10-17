//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/provisioning/deployment"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/goairix/sandbox/internal/runtime/launcher"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func cleanupExecutorLoaderCA(certificate *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
}

// Only this case enters deployment through actual mounted files. The inherited
// worker boundary is genuine and borrowed; no direct cleanup Backend or signer
// can substitute for the loader under test.
func cleanupExecutorLoaderWorker(t *testing.T, ctx context.Context, s cleanupExecutorSetup, kernel *launcher.KernelBoundary, dir string, tlsConfig *tls.Config, ca, certificate, key []byte) {
	t.Helper()
	require.Equal(t, 1000, os.Geteuid())
	defer clear(key)
	defer clear(s.IssuerKey)
	defer clear(s.LoaderClockKey)
	stopClock := cleanupExecutorLoaderClock(t, ctx, s)
	defer stopClock()
	manifest := cleanupExecutorLoaderFiles(t, s, dir, ca, certificate, key)
	raw, err := clientv3.New(clientv3.Config{Endpoints: []string{"https://127.0.0.1:2379"}, TLS: tlsConfig, DialTimeout: 2 * time.Second})
	require.NoError(t, err)
	defer raw.Close()

	cleanupExecutorLoaderPartial(t, ctx, s, kernel, tlsConfig, raw, manifest)
	d, err := deployment.OpenLocalCleanup(ctx, manifest, kernel)
	require.NoError(t, err)
	defer d.Close()
	t.Log("mounted loader same shares reopened")

	prefix := fmt.Sprintf("%sp/%02x/tasks/%s/", s.Binding.Namespace, s.Task.Partition, s.Task.TaskID)
	watchCtx, cancelWatch := context.WithCancel(ctx)
	watch := raw.Watch(watchCtx, prefix, clientv3.WithPrefix(), clientv3.WithCreatedNotify())
	defer cancelWatch()
	select {
	case created := <-watch:
		require.NoError(t, created.Err())
		require.True(t, created.Created)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	traceDone := make(chan cleanupExecutorClaimTrace, 1)
	go func() { traceDone <- cleanupExecutorObserveClaims(watch, prefix, make(chan struct{})) }()
	joined := false
	defer func() {
		cancelWatch()
		if !joined {
			<-traceDone
		}
	}()

	result, err := d.Execute(ctx, s.Task)
	t.Logf("mounted loader Execute phase=%s outcome=%v retained=%t error=%v", result.Phase, result.Outcome, result.Retained, err)
	require.NoError(t, err)
	require.Equal(t, state.OutcomeCommitted, result.Outcome)
	require.Equal(t, "settled", string(result.Phase))
	require.False(t, result.Retained)
	require.NotNil(t, result.Slot)
	require.NotNil(t, result.Release)
	require.Nil(t, result.Pending)
	_, err = raw.Put(ctx, prefix+"test-observation-end", "complete")
	require.NoError(t, err)
	var trace cleanupExecutorClaimTrace
	select {
	case trace = <-traceDone:
		joined = true
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancelWatch()
	require.NoError(t, trace.err)
	require.Empty(t, trace.active)
	require.Len(t, trace.ids, 2)
	require.NotEqual(t, trace.ids[0], trace.ids[1])
	require.Equal(t, []string{"put", "delete", "put", "delete"}, trace.events)
	t.Logf("mounted loader actual original TaskClaim rollover: %v", trace.ids)

	content, err := os.ReadFile(filepath.Join(dir, "target", "cleanup-executor", "nested", "data"))
	require.NoError(t, err)
	require.Equal(t, cleanupExecutorPayload, string(content))
	for path, mode := range map[string]os.FileMode{"nested": 0750, "nested/data": 0640} {
		info, err := os.Stat(filepath.Join(dir, "target", "cleanup-executor", path))
		require.NoError(t, err)
		require.Equal(t, mode, info.Mode().Perm())
	}
	repeat, err := d.Execute(ctx, s.Task)
	require.NoError(t, err)
	require.Equal(t, result.Slot, repeat.Slot)
	require.Equal(t, result.Release, repeat.Release)
	t.Logf("mounted loader settled Task=%s slot=%+v release=%+v content/modes/repeat verified", s.Task.TaskID, result.Slot, result.Release)

	closeErr := d.Close()
	t.Logf("mounted loader Close=%v", closeErr)
	if closeErr != nil {
		require.ErrorIs(t, closeErr, state.ErrOutcomeUnknown)
		require.ErrorIs(t, closeErr, context.Canceled)
	}
	require.Equal(t, closeErr, d.Close(), "repeated Close returns the original actual join result")
	_, err = d.Execute(ctx, s.Task)
	require.Error(t, err)
	require.NoError(t, kernel.ValidateCurrent())
	_, err = kernel.WorkspaceSyncWorkerReference()
	require.NoError(t, err)
	t.Log("mounted loader borrowed kernel valid after Close")
}

// The clock key is independent of the root signing key. Its actual fixed UDS
// and signed frames are consumed by the original loader's SignedClockClient.
func cleanupExecutorLoaderClock(t *testing.T, ctx context.Context, s cleanupExecutorSetup) func() {
	t.Helper()
	require.NoError(t, os.Mkdir(filepath.Dir(ct.ClockSocket), 0700))
	listener, err := net.Listen("unix", ct.ClockSocket)
	require.NoError(t, err)
	clockCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- p.ServeSignedClock(clockCtx, listener, p.SignedClockServerOptions{Binding: s.Binding, Key: s.LoaderClockKey, Source: nativeFixtureClock{}, MaxConnections: 4})
	}()
	cleanup := func() {
		cancel()
		_ = listener.Close()
		serveErr := <-done
		t.Logf("mounted loader signed clock joined=%v", serveErr)
		require.True(t, serveErr == nil || errors.Is(serveErr, context.Canceled) || errors.Is(serveErr, net.ErrClosed), "%v", serveErr)
		require.NoError(t, os.RemoveAll(filepath.Dir(ct.ClockSocket)))
	}
	// Install cleanup before even the permissions assertion can fail.
	permissionsErr := os.Chmod(ct.ClockSocket, 0600)
	if permissionsErr != nil {
		cleanup()
		t.Fatal(permissionsErr)
	}
	return cleanup
}

func cleanupExecutorLoaderFiles(t *testing.T, s cleanupExecutorSetup, dir string, ca, certificate, key []byte) string {
	t.Helper()
	write := func(name string, wire []byte) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), wire, 0600))
	}
	der, err := x509.MarshalPKCS8PrivateKey(s.IssuerKey)
	require.NoError(t, err)
	defer clear(der)
	issuerPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	defer clear(issuerPEM)
	write("issuer-key.pem", issuerPEM)
	write("issuer.json", s.IssuerCertificate)
	write("launcher.json", s.LoaderConfig)
	write("ca.pem", ca)
	write("server.pem", certificate)
	write("server-key.pem", key)
	write("api-ca.pem", s.LoaderKubeCA)
	write("kubeconfig", []byte("apiVersion: v1\nkind: Config\ncurrent-context: selected\ncontexts:\n- name: selected\n  context:\n    cluster: cluster\n    user: user\nclusters:\n- name: cluster\n  cluster:\n    server: "+s.ProviderAddress+"\n    certificate-authority: api-ca.pem\nusers:\n- name: user\n  user:\n    token: CLEANUP-LOADER-FIXTURE\n"))
	shares := make([]uint16, 32)
	for i := range shares {
		shares[i] = uint16(i)
	}
	i := s.Identity
	manifest := map[string]any{
		"version": 1, "launcher_config_file": "launcher.json", "issuer_private_key_file": "issuer-key.pem", "issuer_certificate_file": "issuer.json",
		"etcd": map[string]any{
			"endpoints":    []string{"https://127.0.0.1:2379"},
			"identity":     map[string]any{"schema_version": i.SchemaVersion, "prefix": i.Prefix, "authority_id": i.AuthorityID, "cell": i.Cell, "cluster_id": i.ClusterID, "storage_id": i.StorageID, "runtime_id": i.RuntimeID, "restore_epoch": i.RestoreEpoch},
			"tls":          map[string]any{"ca_file": "ca.pem", "cert_file": "server.pem", "key_file": "server-key.pem", "server_name": ""},
			"dial_timeout": "2s", "request_timeout": "2s", "snapshot_cache_bytes": 262144,
		},
		"kubernetes": map[string]any{"kubeconfig_file": "kubeconfig", "namespace": "runtimes", "bridge_max_active": 16, "provider_max_active": 4, "request_timeout": "3s"},
		"worker":     map[string]any{"worker_id": "cleanup-loader", "partitions": []int{int(s.Task.Partition)}, "range_slots": []uint16{0}, "claim_slots": shares, "relist_slot": 0, "session_ttl": "60s", "stage_ttl": "30s"},
		"storage":    map[string]any{"identity": s.WorkspaceStorage, "bucket": s.WorkspaceBucket, "prefix_base": "", "directory": filepath.Join(dir, "target")},
		"nodes":      []map[string]any{{"name": "node-a", "uid": s.NodeUID, "boot_id": s.NodeBootID, "observer_address": s.ObserverAddress}},
		"cleanup":    map[string]any{"max_active": 32, "claim_ttl": "120s", "job_timeout": "90s", "archive_timeout": "60s", "release_timeout": "30s"},
		"archive":    map[string]any{"directory": filepath.Join(dir, "spool"), "jobs": 1, "entries": 16, "job_bytes": 1048576, "total_bytes": 1048576, "job_index_bytes": 1048576, "total_index_bytes": 1048576},
		"recycling":  map[string]any{"budget_id": s.BudgetID, "slots_per_partition": 1, "max_intent_bytes": 16384},
	}
	write("cleanup.json", observerNativeJSON(t, manifest))
	return filepath.Join(dir, "cleanup.json")
}

type cleanupExecutorLoaderStartupTrace struct {
	put, deleted *clientv3.Event
	err          error
}

// This blocker is a real original session/share owner. It forces failure only
// after the loader has opened its Backend and acquired its own Relist share.
// Observed leased rows grant this fixture no Task or physical effect authority.
func cleanupExecutorLoaderPartial(t *testing.T, ctx context.Context, s cleanupExecutorSetup, kernel *launcher.KernelBoundary, tlsConfig *tls.Config, raw *clientv3.Client, manifest string) {
	t.Helper()
	ns, err := state.NewNamespace(s.Identity.Prefix, s.Identity.AuthorityID, s.Identity.Cell)
	require.NoError(t, err)
	b, err := state.New(ctx, state.Options{Endpoints: []string{"https://127.0.0.1:2379"}, TLS: tlsConfig, Namespace: ns, Identity: s.Identity, RequestTimeout: 2 * time.Second, WorkerControl: &state.WorkerControlOptions{SessionTTL: time.Minute}})
	require.NoError(t, err)
	defer b.Close()
	session, err := b.OpenWorkerSession(ctx, "loader-blocker")
	require.NoError(t, err)
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = b.ReleaseWorkerSession(cleanupCtx, session)
	}()
	share, err := b.AcquireWorkerBudget(ctx, session, state.WorkerBudgetRange, 0)
	require.NoError(t, err)
	ref := share.Reference()
	rangeKey, relistKey := s.Binding.Namespace+"workers/budget/range/00", s.Binding.Namespace+"workers/budget/relist/00"
	before, err := raw.Get(ctx, rangeKey)
	require.NoError(t, err)
	require.Len(t, before.Kvs, 1)
	require.Equal(t, ref.CreateRevision, before.Kvs[0].CreateRevision)
	require.Equal(t, ref.Session.LeaseID, before.Kvs[0].Lease)

	watchCtx, cancel := context.WithCancel(ctx)
	watch := raw.Watch(watchCtx, relistKey, clientv3.WithCreatedNotify(), clientv3.WithPrevKV())
	defer cancel()
	select {
	case created := <-watch:
		require.NoError(t, created.Err())
		require.True(t, created.Created)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	done := make(chan cleanupExecutorLoaderStartupTrace, 1)
	go func() { done <- cleanupExecutorLoaderObserveStartup(watch) }()
	joined := false
	defer func() {
		cancel()
		if !joined {
			<-done
		}
	}()
	failed, openErr := deployment.OpenLocalCleanup(ctx, manifest, kernel)
	if failed != nil {
		defer failed.Close()
	}
	require.Nil(t, failed)
	require.ErrorIs(t, openErr, state.ErrConflict)
	var trace cleanupExecutorLoaderStartupTrace
	select {
	case trace = <-done:
		joined = true
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	require.NoError(t, trace.err)
	require.NotNil(t, trace.put)
	require.NotNil(t, trace.deleted)
	require.Equal(t, relistKey, string(trace.put.Kv.Key))
	require.True(t, trace.put.IsCreate())
	require.Equal(t, int64(1), trace.put.Kv.Version)
	require.Equal(t, trace.put.Kv.CreateRevision, trace.put.Kv.ModRevision)
	require.Positive(t, trace.put.Kv.Lease)
	require.Greater(t, trace.deleted.Kv.ModRevision, trace.put.Kv.ModRevision)
	require.Equal(t, trace.put.Kv, trace.deleted.PrevKv)
	var row struct {
		WorkerID  string `json:"worker_id"`
		SessionID string `json:"session_id"`
		LeaseID   int64  `json:"lease_id"`
		Kind      string `json:"kind"`
		Slot      uint16 `json:"slot"`
	}
	require.NoError(t, json.Unmarshal(trace.put.Kv.Value, &row))
	require.Equal(t, "cleanup-loader", row.WorkerID)
	require.Equal(t, "relist", row.Kind)
	require.Zero(t, row.Slot)
	id, err := uuid.Parse(row.SessionID)
	require.NoError(t, err)
	require.Equal(t, id.String(), row.SessionID)
	require.NotEqual(t, uuid.Nil, id)
	require.NotEqual(t, ref.Session.SessionID, row.SessionID)
	require.Equal(t, trace.put.Kv.Lease, row.LeaseID)
	sessionKey := s.Binding.Namespace + "workers/sessions/" + row.SessionID
	historical, err := raw.Get(ctx, sessionKey, clientv3.WithRev(trace.put.Kv.ModRevision))
	require.NoError(t, err)
	require.Len(t, historical.Kvs, 1)
	require.Less(t, historical.Kvs[0].CreateRevision, trace.put.Kv.CreateRevision)
	require.Equal(t, row.LeaseID, historical.Kvs[0].Lease)
	var historicalRow struct {
		WorkerID  string `json:"worker_id"`
		SessionID string `json:"session_id"`
	}
	require.NoError(t, json.Unmarshal(historical.Kvs[0].Value, &historicalRow))
	require.Equal(t, row.WorkerID, historicalRow.WorkerID)
	require.Equal(t, row.SessionID, historicalRow.SessionID)
	for _, key := range []string{relistKey, sessionKey} {
		after, err := raw.Get(ctx, key)
		require.NoError(t, err)
		require.Empty(t, after.Kvs)
	}
	after, err := raw.Get(ctx, rangeKey)
	require.NoError(t, err)
	require.Equal(t, before.Kvs, after.Kvs, "partial loader cleanup must not release another owner's share")
	t.Logf("mounted loader partial startup conflict=%v targetSession=%s relistPut=%d relistDelete=%d blockerLease=%d unchanged", openErr, row.SessionID, trace.put.Kv.ModRevision, trace.deleted.Kv.ModRevision, ref.Session.LeaseID)

	require.NoError(t, b.ReleaseWorkerBudget(ctx, share))
	require.NoError(t, b.ReleaseWorkerSession(ctx, session))
	require.NoError(t, b.Close())
}

func cleanupExecutorLoaderObserveStartup(watch clientv3.WatchChan) cleanupExecutorLoaderStartupTrace {
	var trace cleanupExecutorLoaderStartupTrace
	for response := range watch {
		if err := response.Err(); err != nil {
			trace.err = err
			return trace
		}
		for _, event := range response.Events {
			switch {
			case event.Type == mvccpb.PUT && trace.put == nil:
				trace.put = event
			case event.Type == mvccpb.DELETE && trace.put != nil && trace.deleted == nil:
				trace.deleted = event
				return trace
			default:
				trace.err = errors.New("unexpected original partial-startup relist event")
				return trace
			}
		}
	}
	trace.err = errors.New("partial-startup relist watch ended before actual put/delete")
	return trace
}
