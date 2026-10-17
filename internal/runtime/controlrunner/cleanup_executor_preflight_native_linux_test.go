//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// The budget is immutable from its first write. This genuine worker repeats
// an unsupported profile without replacing that budget or synthesizing a
// successful physical preflight. The expected refusal itself is not the RED:
// retaining any execution row or permanent slot after refusal is the defect.
func cleanupExecutorPreflightWorker(t *testing.T, ctx context.Context, s cleanupExecutorSetup, options state.Options, tlsConfig *tls.Config) {
	t.Helper()
	require.Equal(t, uint32(4096), options.TaskWorkspaceSyncRecyclingBudget.MaxIntentBytes)
	proxy := newCleanupExecutorEtcdProxy(t, tlsConfig, s.Task, "")
	defer proxy.close(t)
	options.Endpoints = []string{proxy.endpoint()}
	raw, err := clientv3.New(clientv3.Config{Endpoints: options.Endpoints, TLS: tlsConfig, DialTimeout: 2 * time.Second})
	require.NoError(t, err)
	defer raw.Close()
	original := cleanupExecutorPreflightHistory(t, ctx, s, raw)
	b, err := state.New(ctx, options)
	require.NoError(t, err)
	defer b.Close()
	shares := make([]uint16, 32)
	for i := range shares {
		shares[i] = uint16(i)
	}
	w, err := b.StartDueWorker(ctx, state.DueWorkerOptions{
		WorkerID: "cleanup-preflight-refusal", Partitions: []uint8{s.Task.Partition},
		RangeSlots: []uint16{0}, ClaimSlots: shares, RelistSlot: 0, StageTTL: 30 * time.Second,
	})
	require.NoError(t, err)
	defer w.Stop()
	before := proxy.snapshot()
	require.Len(t, before.GrantIDs, 1, "only the original worker session has a Lease")

	for attempt := 0; attempt < 2; attempt++ {
		result, executeErr := w.ExecuteCleanupLocal(ctx, s.Task)
		t.Logf("preflight-refusal actual Execute attempt=%d phase=%s retained=%t error=%v stats=%+v", attempt+1, result.Phase, result.Retained, executeErr, proxy.snapshot())
		require.ErrorIs(t, executeErr, state.ErrInvalidConfiguration)
		require.ErrorContains(t, executeErr, "workspace sync intent schema")
		require.NotEqual(t, state.OutcomeCommitted, result.Outcome)
		// The actual public call has returned and joined its job. Check all32
		// global cells by exact points; a namespace scan is not a capacity proof.
		cleanupExecutorPreflightAbsent(t, ctx, s, raw)
		require.Equal(t, original, cleanupExecutorPreflightHistory(t, ctx, s, raw), "Task birth, original receipt, intent and history remain exact")
		observed := proxy.snapshot()
		require.Equal(t, before.GrantIDs, observed.GrantIDs)
		cleanupExecutorPreflightNoEffects(t, observed)
	}

	stopErr := w.Stop()
	t.Logf("preflight-refusal original worker Stop=%v", stopErr)
	require.True(t, stopErr == nil || errors.Is(stopErr, state.ErrOutcomeUnknown) && errors.Is(stopErr, context.Canceled), "%v", stopErr)
	select {
	case <-w.Done():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.False(t, w.Status().Running)
	require.False(t, w.Status().Synchronized)
	require.NoError(t, b.Close())
	proxy.close(t)
	final := proxy.snapshot()
	require.Equal(t, before.GrantIDs, final.GrantIDs)
	cleanupExecutorPreflightNoEffects(t, final)
	t.Log("preflight-refusal public Execute repeated=2 E_absent=true slots_absent=32 claims=0 effects=0")
}

func cleanupExecutorPreflightNoEffects(t *testing.T, observed cleanupExecutorProxyStats) {
	t.Helper()
	require.False(t, observed.ObservationOverflow)
	require.Empty(t, observed.ClaimLeases)
	require.Empty(t, observed.QuiescenceGuards)
	require.Zero(t, observed.ClaimPuts)
	require.Zero(t, observed.ClosePuts)
	require.Zero(t, observed.QuiescencePuts)
	require.Zero(t, observed.ArchivePuts)
	require.Zero(t, observed.TerminationPuts)
	require.Zero(t, observed.ProviderPuts)
}

func cleanupExecutorPreflightAbsent(t *testing.T, parent context.Context, s cleanupExecutorSetup, raw *clientv3.Client) {
	t.Helper()
	prefix := fmt.Sprintf("%sp/%02x/tasks/%s/", s.Binding.Namespace, s.Task.Partition, s.Task.TaskID)
	ops := []clientv3.Op{clientv3.OpGet(prefix + "execution")}
	for slot := 0; slot < 32; slot++ {
		ops = append(ops, clientv3.OpGet(fmt.Sprintf("%scleanup-execution-slots/%02d", s.Binding.Namespace, slot)))
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	read, err := raw.Txn(ctx).Then(ops...).Commit()
	require.NoError(t, err)
	require.Len(t, read.Responses, 33)
	for i, point := range read.Responses {
		require.NotNil(t, point.GetResponseRange())
		require.Empty(t, point.GetResponseRange().Kvs, "execution/slot exact point index=%d must remain absent", i)
	}
}

// These raw snapshots are test observations, never cleanup authority. The
// initial immutable intent supplies only the exact receipt's diagnostic key.
// A bounded Task subtree additionally detects any new local effect history.
func cleanupExecutorPreflightHistory(t *testing.T, parent context.Context, s cleanupExecutorSetup, raw *clientv3.Client) [][]*mvccpb.KeyValue {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	partition := fmt.Sprintf("%sp/%02x/", s.Binding.Namespace, s.Task.Partition)
	taskKey := partition + "tasks/" + s.Task.TaskID
	intentKey := partition + "cleanup-intents/" + s.Task.TaskID
	intentRead, err := raw.Get(ctx, intentKey)
	require.NoError(t, err)
	require.Len(t, intentRead.Kvs, 1)
	var intent state.CleanupIntentRecord
	require.NoError(t, json.Unmarshal(intentRead.Kvs[0].Value, &intent))
	require.NoError(t, intent.Validate())
	receipt := fmt.Sprintf("%sstages/%s/%s/%s/receipt", partition, intent.Attempt.RequestID, intent.Attempt.StageID, intent.Attempt.AttemptID)
	read, err := raw.Txn(ctx).Then(
		clientv3.OpGet(taskKey), clientv3.OpGet(intentKey),
		clientv3.OpGet(partition+"sandboxes/"+s.Task.SandboxID+"/cleanup-task"),
		clientv3.OpGet(receipt), clientv3.OpGet(s.Binding.Namespace+"meta/workspace_sync_recycling_budget"),
		clientv3.OpGet(taskKey+"/", clientv3.WithPrefix(), clientv3.WithLimit(65)),
	).Commit()
	require.NoError(t, err)
	require.Len(t, read.Responses, 6)
	result := make([][]*mvccpb.KeyValue, 6)
	for i, point := range read.Responses {
		r := point.GetResponseRange()
		require.NotNil(t, r)
		require.False(t, r.More)
		if i < 5 {
			require.Len(t, r.Kvs, 1)
		} else {
			require.LessOrEqual(t, len(r.Kvs), 64)
		}
		result[i] = r.Kvs
	}
	require.Equal(t, result[0][0].CreateRevision, result[1][0].CreateRevision)
	require.Equal(t, result[0][0].CreateRevision, result[3][0].CreateRevision)
	budget := result[4][0]
	require.Equal(t, int64(1), budget.Version)
	require.Equal(t, budget.CreateRevision, budget.ModRevision)
	require.Zero(t, budget.Lease)
	var budgetRecord struct {
		MaxIntentBytes uint32 `json:"max_intent_bytes"`
	}
	require.NoError(t, json.Unmarshal(budget.Value, &budgetRecord))
	require.Equal(t, uint32(4096), budgetRecord.MaxIntentBytes)
	return result
}

// No observer termination is expected. The fixture owns the live Supervisor
// and must kill/wait it itself; that cleanup is not a business release proof.
func cleanupExecutorPreflightParent(t *testing.T, ctx context.Context, s cleanupExecutorSetup, admin *clientv3.Client, api *cleanupExecutorAPI, supervisor *exec.Cmd, waited *bool) {
	t.Helper()
	original := cleanupExecutorPreflightHistory(t, ctx, s, admin)
	path := filepath.Join(s.Directory, "worker-preflight.json")
	require.NoError(t, os.WriteFile(path, observerNativeJSON(t, s), 0600))
	worker := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCleanupExecutorWorkerChild$", "-test.v", "-test.timeout=110s")
	worker.Env = append(os.Environ(), "CLEANUP_EXECUTOR_CHILD_SETUP="+path)
	worker.Stdout, worker.Stderr = os.Stdout, os.Stderr
	require.NoError(t, worker.Start())
	workerErr := worker.Wait()
	t.Logf("preflight-refusal original worker actual Wait=%v", workerErr)
	require.NoError(t, workerErr)
	cleanupExecutorPreflightAbsent(t, ctx, s, admin)
	require.Equal(t, original, cleanupExecutorPreflightHistory(t, ctx, s, admin))
	api.mu.Lock()
	patches, deletes, upgrades, present := api.patches, api.deletes, api.upgrades, api.pod != nil
	api.mu.Unlock()
	require.Zero(t, patches)
	require.Zero(t, deletes)
	require.Zero(t, upgrades)
	require.True(t, present)
	require.NoError(t, supervisor.Process.Kill())
	supervisorErr := supervisor.Wait()
	*waited = true
	var exited *exec.ExitError
	require.ErrorAs(t, supervisorErr, &exited)
	require.Equal(t, syscall.SIGKILL, exited.Sys().(syscall.WaitStatus).Signal())
	t.Logf("preflight-refusal fixture-owned live supervisor actual Wait=%v; provider/bridge effects=0", supervisorErr)
}
