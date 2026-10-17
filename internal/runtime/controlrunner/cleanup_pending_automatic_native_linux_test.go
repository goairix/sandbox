//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/provisioning/deployment"
	"github.com/goairix/sandbox/internal/runtime/launcher"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
)

// Each mode receives its own committed Task and actual enrolled runtime from
// the original fixture. Routing precedes the explicit v1 loader, so no original
// worker or scheduling share can overlap either automatic composition.
func cleanupPendingAutomaticWorker(t *testing.T, ctx context.Context, s cleanupExecutorSetup, kernel *launcher.KernelBoundary, dir string, tlsConfig *tls.Config, ca, certificate, key []byte) {
	t.Helper()
	defer clear(key)
	defer clear(s.IssuerKey)
	defer clear(s.LoaderClockKey)
	switch s.AutomaticMode {
	case "automatic-mounted-backlog":
		cleanupPendingAutomaticMounted(t, ctx, s, kernel, dir, tlsConfig, ca, certificate, key)
	case "automatic-worker-role":
		cleanupWorkerRoleNative(t, ctx, s, dir, tlsConfig, ca, certificate, key, "/fixture/cleanup-worker")
	case "automatic-pending-aba", "automatic-pending-ack":
		cleanupPendingFaultMounted(t, ctx, s, kernel, dir, tlsConfig, ca, certificate, key)
	case "automatic-result-mounted", "automatic-result-role":
		cleanupPendingResultNative(t, ctx, s, kernel, dir, tlsConfig, ca, certificate, key)
	default:
		t.Fatal("unknown automatic cleanup native mode")
	}
}

func cleanupPendingAutomaticMounted(t *testing.T, ctx context.Context, s cleanupExecutorSetup, kernel *launcher.KernelBoundary, dir string, tlsConfig *tls.Config, ca, certificate, key []byte) {
	t.Helper()
	stopClock := cleanupExecutorLoaderClock(t, ctx, s)
	defer stopClock()
	manifest := cleanupExecutorLoaderFiles(t, s, dir, ca, certificate, key)
	wire, err := os.ReadFile(manifest)
	require.NoError(t, err)
	// Preserve uint64 identity bytes; decoding numbers into any would round
	// the real etcd ClusterID through float64 before the mounted loader sees it.
	var mounted map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wire, &mounted))
	var worker map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(mounted["worker"], &worker))
	require.NotEmpty(t, worker)
	mounted["version"] = json.RawMessage("2")
	worker["automatic_cleanup"] = json.RawMessage("true")
	mounted["worker"] = observerNativeJSON(t, worker)
	require.NoError(t, os.WriteFile(manifest, observerNativeJSON(t, mounted), 0600))

	raw, err := clientv3.New(clientv3.Config{
		Endpoints: []string{"https://127.0.0.1:2379"}, TLS: tlsConfig,
		DialTimeout: 2 * time.Second, Logger: zap.NewNop(),
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, raw.Close()) }()
	pending := cleanupPendingAutomaticBacklog(t, ctx, raw, s)
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
	traceJoined := false
	defer func() {
		cancelWatch()
		if !traceJoined {
			<-traceDone
		}
	}()

	// Only this public mounted entry starts work. The Task/P already exist;
	// the test never submits a candidate to the explicit execution mailbox.
	d, err := deployment.OpenAutomaticLocalCleanup(ctx, manifest, kernel)
	require.NoError(t, err)
	defer d.Close()
	cleanupWorkerRoleSettled(t, ctx, raw, s, os.Getpid(), d.Done())
	settledAt := time.Now()
	_, err = raw.Put(ctx, prefix+"test-observation-end", "complete")
	require.NoError(t, err)
	var trace cleanupExecutorClaimTrace
	select {
	case trace = <-traceDone:
		traceJoined = true
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancelWatch()
	require.NoError(t, trace.err)
	require.Empty(t, trace.active)
	require.Len(t, trace.ids, 2)
	require.NotEqual(t, trace.ids[0], trace.ids[1])
	require.Equal(t, []string{"put", "delete", "put", "delete"}, trace.events)
	t.Logf("automatic mounted worker actual original TaskClaim rollover: %v", trace.ids)
	cleanupPendingAutomaticArchive(t, dir)
	cleanupPendingAutomaticSweep(t, ctx, raw, pending, d, settledAt)

	closeErr := d.Close()
	t.Logf("automatic mounted worker actual Close=%v", closeErr)
	cleanupPendingAutomaticCloseResult(t, closeErr)
	require.Equal(t, closeErr, d.Close(), "repeated Close retains the original physical join result")
	select {
	case <-d.Done():
	default:
		t.Fatal("automatic deployment Close returned before original worker completion")
	}
	waitErr := d.Wait(context.Background())
	t.Logf("automatic mounted worker actual terminal Wait=%v", waitErr)
	cleanupPendingAutomaticCloseResult(t, waitErr)
	require.NoError(t, kernel.ValidateCurrent())
	_, err = kernel.WorkspaceSyncWorkerReference()
	require.NoError(t, err)
	t.Log("automatic mounted backlog complete: actual archive, settled release, normal sweep P deletion, physical Close/Wait and borrowed kernel valid")
}

// This is a test-side observation of the original producer transaction, not an
// adoption of Task metadata as physical authority. Actual work still traverses
// the enrolled runtime, signed archive and observer/provider release protocols.
func cleanupPendingAutomaticBacklog(t *testing.T, ctx context.Context, raw *clientv3.Client, s cleanupExecutorSetup) *mvccpb.KeyValue {
	t.Helper()
	partition := fmt.Sprintf("%sp/%02x/", s.Binding.Namespace, s.Task.Partition)
	task, err := raw.Get(ctx, partition+"tasks/"+s.Task.TaskID)
	require.NoError(t, err)
	require.Len(t, task.Kvs, 1)
	pending, err := raw.Get(ctx, partition+"cleanup-pending/"+s.Task.TaskID)
	require.NoError(t, err)
	require.Len(t, pending.Kvs, 1)
	row := pending.Kvs[0]
	require.Zero(t, row.Lease)
	require.EqualValues(t, 1, row.Version)
	require.Equal(t, row.CreateRevision, row.ModRevision)
	require.Equal(t, task.Kvs[0].CreateRevision, row.CreateRevision, "backlog discovery evidence is born with the original Task")
	var record struct {
		Version    uint32              `json:"version"`
		Reference  state.TaskReference `json:"reference"`
		TaskDigest string              `json:"task_digest"`
	}
	require.NoError(t, json.Unmarshal(row.Value, &record))
	require.EqualValues(t, 1, record.Version)
	require.Equal(t, s.Task, record.Reference)
	digest := sha256.Sum256(task.Kvs[0].Value)
	require.Equal(t, fmt.Sprintf("%x", digest), record.TaskDigest)
	execution, err := raw.Get(ctx, partition+"tasks/"+s.Task.TaskID+"/execution")
	require.NoError(t, err)
	require.Empty(t, execution.Kvs, "fresh backlog must not have been explicitly executed before automatic startup")
	t.Logf("automatic startup backlog Task=%s P_birth=%d permanent=true execution_absent=true", s.Task.TaskID, row.CreateRevision)
	return row
}

func cleanupPendingAutomaticArchive(t *testing.T, dir string) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(dir, "target", "cleanup-executor", "nested", "data"))
	require.NoError(t, err)
	require.Equal(t, cleanupExecutorPayload, string(content))
	for path, mode := range map[string]os.FileMode{"nested": 0750, "nested/data": 0640} {
		info, err := os.Stat(filepath.Join(dir, "target", "cleanup-executor", path))
		require.NoError(t, err)
		require.Equal(t, mode, info.Mode().Perm())
	}
}

// Give the production actor its normal sweep interval; no test wake, index
// mutation or explicit execution drives retirement. Waiting at least 30s after
// observed settlement avoids mistaking an immediate test-side read for a sweep.
// The deletion may legitimately precede that minimum observation wait, because
// production measures 30s from the prior finite round's end, not settlement.
func cleanupPendingAutomaticSweep(t *testing.T, ctx context.Context, raw *clientv3.Client, pending *mvccpb.KeyValue, d *deployment.LocalCleanupDeployment, settledAt time.Time) {
	t.Helper()
	minimum := time.NewTimer(max(30*time.Second-time.Since(settledAt), 0))
	defer minimum.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	minimumElapsed, absent := false, false
	for {
		response, err := raw.Get(ctx, string(pending.Key))
		require.NoError(t, err)
		require.LessOrEqual(t, len(response.Kvs), 1)
		if len(response.Kvs) == 0 {
			absent = true
		} else {
			require.False(t, absent, "settled pending evidence must not reappear after its sweep deletion")
			require.Equal(t, pending, response.Kvs[0], "retained P must preserve its exact original occupation until retirement")
		}
		if minimumElapsed && absent {
			t.Logf("automatic normal sweep retired P=%s after observed settlement elapsed=%s revision=%d", pending.Key, time.Since(settledAt), response.Header.Revision)
			return
		}
		select {
		case <-d.Done():
			t.Fatalf("automatic worker stopped before normal sweep completed: %v", d.Wait(context.Background()))
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-minimum.C:
			minimumElapsed = true
		case <-ticker.C:
		}
	}
}

func cleanupPendingAutomaticCloseResult(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		// Preserve the existing native executor's conservative unknown-on-
		// canceled-stage contract; this is joined, not silently successful.
		require.ErrorIs(t, err, state.ErrOutcomeUnknown)
		require.ErrorIs(t, err, context.Canceled)
		require.True(t, cleanupPendingAutomaticCloseLeaves(err), "unexpected dependency error mixed with canceled unknown close")
	}
}

func cleanupPendingAutomaticCloseLeaves(err error) bool {
	if err == nil {
		return false
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		children := wrapped.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !cleanupPendingAutomaticCloseLeaves(child) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return cleanupPendingAutomaticCloseLeaves(wrapped.Unwrap())
	default:
		return err == state.ErrOutcomeUnknown || err == context.Canceled
	}
}
