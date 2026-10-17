//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/provisioning/deployment"
	"github.com/goairix/sandbox/internal/runtime/launcher"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
)

// Each invocation has its own real Task/runtime. The accepted TLS proxy loses
// only the original committed archive-bound ACK; physical work and subsequent
// automatic recovery run in one original worker, without an Execute submission.
func cleanupPendingResultNative(t *testing.T, ctx context.Context, s cleanupExecutorSetup, kernel *launcher.KernelBoundary, dir string, tlsConfig *tls.Config, ca, certificate, key []byte) {
	t.Helper()
	stopClock := cleanupExecutorLoaderClock(t, ctx, s)
	defer stopClock()
	raw, err := clientv3.New(clientv3.Config{
		Endpoints: []string{"https://127.0.0.1:2379"}, TLS: tlsConfig,
		DialTimeout: 2 * time.Second, Logger: zap.NewNop(),
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, raw.Close()) }()
	pending := cleanupPendingAutomaticBacklog(t, ctx, raw, s)
	require.LessOrEqual(t, len(pending.Value), 4096)
	proxy := newCleanupExecutorEtcdProxy(t, tlsConfig, s.Task, "bound-recovery")
	defer proxy.close(t)
	workerID := "cleanup-result-" + uuid.NewString()
	manifest := cleanupPendingResultManifest(t, s, dir, ca, certificate, key, proxy.endpoint(), workerID)
	var session *mvccpb.KeyValue

	if s.AutomaticMode == "automatic-result-role" {
		session = cleanupPendingResultRole(t, ctx, s, raw, proxy, pending, manifest, workerID, dir)
	} else {
		require.Equal(t, "automatic-result-mounted", s.AutomaticMode)
		d, err := deployment.OpenAutomaticLocalCleanup(ctx, manifest, kernel)
		require.NoError(t, err)
		defer d.Close()
		session = cleanupWorkerRoleSession(t, ctx, raw, s.Binding.Namespace, workerID, d.Done())
		cleanupPendingResultRecovered(t, ctx, s, raw, proxy, pending, session, os.Getpid(), d.Done(), dir)

		// Settlement is already observed. The explicit ledger diagnostic, not
		// merely an unknown actor cancellation during Close, proves job history
		// survived actual registry removal and successful automatic recovery.
		closeErr := d.Close()
		require.ErrorIs(t, closeErr, state.ErrOutcomeUnknown)
		require.Contains(t, closeErr.Error(), "automatic cleanup failures=1:")
		require.Equal(t, closeErr, d.Close())
		select {
		case <-d.Done():
		default:
			t.Fatal("automatic result Close returned before original worker completion")
		}
		waitErr := d.Wait(context.Background())
		require.ErrorIs(t, waitErr, state.ErrOutcomeUnknown)
		require.Contains(t, waitErr.Error(), "automatic cleanup failures=1:")
		require.NoError(t, kernel.ValidateCurrent())
		_, err = kernel.WorkspaceSyncWorkerReference()
		require.NoError(t, err)
		t.Logf("automatic job history after actual recovery: Close=%v Wait=%v borrowed_kernel_valid=true", closeErr, waitErr)
	}

	// This proxy owns remote handlers/pumps separately from the worker. Stop
	// and join them before asserting that every original granted lease is gone.
	proxy.close(t)
	stats := proxy.snapshot()
	require.False(t, stats.ObservationOverflow)
	// The real proxy also observes original Stage grants. This is a diagnostic
	// ceiling for this one-Task test, not a new production admission limit.
	require.LessOrEqual(t, len(stats.GrantIDs), 64)
	require.Contains(t, stats.GrantIDs, session.Lease)
	require.Len(t, stats.ClaimLeases, 2, "only the failed Run and its subsequent recovery acquire TaskClaims")
	sessions, err := raw.Get(ctx, s.Binding.Namespace+"workers/sessions/", clientv3.WithPrefix(), clientv3.WithLimit(33))
	require.NoError(t, err)
	require.Empty(t, sessions.Kvs)
	for _, id := range stats.GrantIDs {
		ttl, err := raw.TimeToLive(ctx, clientv3.LeaseID(id))
		require.NoError(t, err)
		require.EqualValues(t, -1, ttl.TTL, "actual original worker/Claim leases must be revoked")
	}
	t.Logf("automatic result mode=%s all worker/process/proxy joins complete, actual leases gone: %+v", s.AutomaticMode, stats)
}

func cleanupPendingResultManifest(t *testing.T, s cleanupExecutorSetup, dir string, ca, certificate, key []byte, endpoint, workerID string) string {
	t.Helper()
	manifest := cleanupExecutorLoaderFiles(t, s, dir, ca, certificate, key)
	wire, err := os.ReadFile(manifest)
	require.NoError(t, err)
	// Preserve the genuine uint64 ClusterID and all unmodified identity bytes.
	var mounted map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wire, &mounted))
	var worker, etcd map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(mounted["worker"], &worker))
	require.NoError(t, json.Unmarshal(mounted["etcd"], &etcd))
	require.NotEmpty(t, worker)
	require.NotEmpty(t, etcd)
	mounted["version"] = json.RawMessage("2")
	worker["automatic_cleanup"] = json.RawMessage("true")
	worker["worker_id"] = observerNativeJSON(t, workerID)
	etcd["endpoints"] = observerNativeJSON(t, []string{endpoint})
	mounted["worker"], mounted["etcd"] = observerNativeJSON(t, worker), observerNativeJSON(t, etcd)
	require.NoError(t, os.WriteFile(manifest, observerNativeJSON(t, mounted), 0600))
	return manifest
}

// Keep only the first bounded E plus fixed proxy observations, never a polling
// history. These copied coordinates are test diagnostics, not physical authority.
type cleanupPendingResultFirst struct {
	execution *mvccpb.KeyValue
	row       cleanupExecutorRecoveryRow
	stats     cleanupExecutorProxyStats
	at        time.Time
}

func cleanupPendingResultJoined(t *testing.T, ctx context.Context, s cleanupExecutorSetup, raw *clientv3.Client, proxy *cleanupExecutorEtcdProxy, pending *mvccpb.KeyValue, pid int, exited <-chan struct{}) cleanupPendingResultFirst {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		rpc, cancel := context.WithTimeout(ctx, 2*time.Second)
		response, err := raw.Get(rpc, proxy.prefix+"execution")
		cancel()
		require.NoError(t, err)
		require.LessOrEqual(t, len(response.Kvs), 1)
		if len(response.Kvs) == 1 {
			kv := response.Kvs[0]
			require.LessOrEqual(t, len(kv.Value), 4096)
			var row cleanupExecutorRecoveryRow
			require.NoError(t, json.Unmarshal(kv.Value, &row))
			require.NotEqual(t, "settled", row.Phase, "must observe the original failed Run before recovery")
			if row.Run != nil && row.Run.Joined && row.Confirmed == "archive-bound" {
				require.Equal(t, "occupied", row.Phase)
				require.Equal(t, s.Task, row.Task)
				require.Equal(t, pending.CreateRevision, row.TaskBirth)
				require.NoError(t, row.Run.Worker.Validate())
				require.Equal(t, pid, row.Run.Worker.PID)
				require.NotEmpty(t, row.Run.ID)
				require.NotNil(t, row.Archive)
				require.Nil(t, row.Pending)
				require.Equal(t, pending, cleanupExecutorRecoveryPoint(t, ctx, raw, string(pending.Key)))
				slot := cleanupExecutorRecoveryPoint(t, ctx, raw, fmt.Sprintf("%scleanup-execution-slots/%02d", s.Binding.Namespace, row.Slot))
				require.Zero(t, slot.Lease)
				require.LessOrEqual(t, len(slot.Value), 4096)
				stats := proxy.snapshot()
				require.LessOrEqual(t, len(stats.GrantIDs), 64)
				require.Equal(t, 1, stats.FaultMatched)
				require.Equal(t, 1, stats.FaultCommitted)
				require.Equal(t, 1, stats.FaultJoined)
				require.Zero(t, stats.FaultNotForwarded)
				require.Positive(t, stats.FaultRevision)
				require.Equal(t, 1, stats.ClaimPuts)
				require.Equal(t, 1, stats.ArchivePuts)
				require.False(t, stats.ObservationOverflow)
				t.Logf("automatic original job physically joined after committed archive ACK loss: PID=%d E_birth=%d Task_birth=%d Run=%s S=%d fault_revision=%d stats=%+v", pid, kv.CreateRevision, row.TaskBirth, row.Run.ID, row.Slot, stats.FaultRevision, stats)
				return cleanupPendingResultFirst{execution: kv, row: row, stats: stats, at: time.Now()}
			}
		}
		select {
		case <-exited:
			t.Fatal("automatic worker exited before original failed job joined")
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

// The same original worker must recover through normal automatic scheduling. No
// test wake, new session, Task submission or logical-clock advance drives it.
func cleanupPendingResultRecovered(t *testing.T, ctx context.Context, s cleanupExecutorSetup, raw *clientv3.Client, proxy *cleanupExecutorEtcdProxy, pending, session *mvccpb.KeyValue, pid int, exited <-chan struct{}, dir string) {
	t.Helper()
	first := cleanupPendingResultJoined(t, ctx, s, raw, proxy, pending, pid, exited)
	cleanupWorkerRoleSettled(t, ctx, raw, s, pid, exited)
	execution := cleanupExecutorRecoveryPoint(t, ctx, raw, proxy.prefix+"execution")
	require.LessOrEqual(t, len(execution.Value), 4096)
	var row cleanupExecutorRecoveryRow
	require.NoError(t, json.Unmarshal(execution.Value, &row))
	require.Equal(t, first.execution.CreateRevision, execution.CreateRevision)
	require.Equal(t, first.row.TaskBirth, row.TaskBirth)
	require.Equal(t, first.row.Incarnation, row.Incarnation)
	require.NotNil(t, row.Run)
	require.NotEqual(t, first.row.Run.ID, row.Run.ID)
	require.Equal(t, session, cleanupExecutorRecoveryPoint(t, ctx, raw, string(session.Key)), "recovery must keep the same original session")
	cleanupPendingAutomaticArchive(t, dir)
	stats := proxy.snapshot()
	require.Equal(t, 2, stats.ClaimPuts)
	require.Equal(t, first.stats.ArchivePuts, stats.ArchivePuts, "recovery may not prepare another archive command")
	require.Equal(t, first.stats.ClosePuts, stats.ClosePuts)
	require.Equal(t, first.stats.QuiescencePuts, stats.QuiescencePuts)
	require.Equal(t, 1, stats.FaultMatched)
	require.Equal(t, 1, stats.FaultCommitted)
	require.Equal(t, 1, stats.FaultJoined)
	require.False(t, stats.ObservationOverflow)
	t.Logf("automatic same-worker recovery actually settled: PID=%d same_session_lease=%d original_Run=%s recovered_Run=%s elapsed_after_join=%s stats=%+v", pid, session.Lease, first.row.Run.ID, row.Run.ID, time.Since(first.at), stats)
}

// This is a fresh actual command exec under the common zero-cap worker. It
// receives only mounted v2 configuration, not a Task or a result callback.
func cleanupPendingResultRole(t *testing.T, ctx context.Context, s cleanupExecutorSetup, raw *clientv3.Client, proxy *cleanupExecutorEtcdProxy, pending *mvccpb.KeyValue, manifest, workerID, dir string) *mvccpb.KeyValue {
	t.Helper()
	require.NotZero(t, os.Geteuid())
	command := exec.Command("/fixture/cleanup-worker", "serve", manifest)
	command.Env = []string{"GOMAXPROCS=2"}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr cleanupWorkerRoleOutput
	command.Stdout, command.Stderr = &stdout, &stderr
	startedAt := time.Now()
	require.NoError(t, command.Start())
	waited := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		waited <- command.Wait()
		close(exited)
	}()
	joined := false
	defer func() {
		if !joined {
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			t.Logf("automatic result role failure cleanup actual Wait=%v", <-waited)
		}
		t.Logf("automatic result role PID=%d stdout=%q stderr=%q", command.Process.Pid, stdout.String(), stderr.String())
	}()
	session := cleanupWorkerRoleSession(t, ctx, raw, s.Binding.Namespace, workerID, exited)
	cleanupPendingResultRecovered(t, ctx, s, raw, proxy, pending, session, command.Process.Pid, exited, dir)
	require.NoError(t, command.Process.Signal(syscall.SIGTERM))
	select {
	case err := <-waited:
		joined = true
		var exit *exec.ExitError
		require.ErrorAs(t, err, &exit)
		require.Equal(t, 1, exit.ExitCode(), "successful recovery must not conceal the original unknown job on shutdown")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(15 * time.Second):
		t.Fatal("automatic result role did not actually exit after SIGTERM")
	}
	require.Empty(t, stdout.String())
	require.NoError(t, cleanupPendingResultRoleStderr(stderr.String(), proxy.endpoint(), startedAt, time.Now(), []string{
		manifest, dir, s.Task.TaskID, s.Task.SandboxID, "-----BEGIN", "CLEANUP-LOADER-FIXTURE",
	}))
	t.Logf("automatic result role PID=%d same-worker recovery complete, SIGTERM actual Wait exit1 joined=true", command.Process.Pid)
	return session
}

// The command appends one fixed failure line; its existing etcd client also
// emits the one intentionally lost Txn ACK as a structured warning. Accept only
// that exact bounded warning from this process/proxy, never arbitrary stderr.
// Errors deliberately omit actual values and private rejection needles.
func cleanupPendingResultRoleStderr(text, endpoint string, started, finished time.Time, private []string) error {
	invalid := fmt.Errorf("unexpected automatic result role stderr")
	lines := strings.Split(text, "\n")
	if len(text) > 65536 || len(lines) != 3 || len(lines[0]) > 2048 || lines[1] != "sandbox-cleanup-worker: failed" || lines[2] != "" {
		return invalid
	}
	for _, needle := range private {
		if needle != "" && strings.Contains(text, needle) {
			return invalid
		}
	}

	expected := map[string]string{
		"level": "warn", "logger": "etcd-client",
		"caller": "v3@v3.6.14/retry_interceptor.go:68",
		"msg":    "retrying of unary invoker failed", "method": "/etcdserverpb.KV/Txn",
		"error": "rpc error: code = DeadlineExceeded desc = context deadline exceeded",
	}
	// Read keys before their scalar values so duplicates cannot be overwritten
	// by a later valid field. Exactly ten accepted keys also proves presence.
	decoder := json.NewDecoder(strings.NewReader(lines[0]))
	decoder.UseNumber()
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return invalid
	}
	fields := make(map[string]string, 10)
	for decoder.More() {
		token, err := decoder.Token()
		name, valid := token.(string)
		if err != nil || !valid {
			return invalid
		}
		if _, duplicate := fields[name]; duplicate {
			return invalid
		}
		fixed, known := expected[name]
		if !known && name != "ts" && name != "target" && name != "peer" && name != "attempt" {
			return invalid
		}
		value, err := decoder.Token()
		if err != nil {
			return invalid
		}
		if name == "attempt" {
			attempt, valid := value.(json.Number)
			if !valid || attempt.String() != "0" {
				return invalid
			}
			fields[name] = "0"
			continue
		}
		field, valid := value.(string)
		matches := !known || field == fixed
		if name == "error" {
			// The same actual deadline can win in either gRPC's context or
			// HTTP/2 RST handling. Accept only these two observed descriptions;
			// both retain DeadlineExceeded and neither includes upstream data.
			matches = field == fixed || field == "rpc error: code = DeadlineExceeded desc = stream terminated by RST_STREAM with error code: CANCEL"
		}
		if !valid || !matches {
			return invalid
		}
		fields[name] = field
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || len(fields) != 10 {
		return invalid
	}
	if _, err = decoder.Token(); err != io.EOF {
		return invalid
	}
	stamp, err := time.Parse(time.RFC3339Nano, fields["ts"])
	if err != nil || stamp.Before(started) || stamp.After(finished) {
		return invalid
	}

	// The client pointer varies, but only a uint64 hexadecimal address and
	// this fixture's exact TLS loopback endpoint may occupy the target field.
	target, present := strings.CutPrefix(fields["target"], "etcd-endpoints://0x")
	pointer, address, separated := strings.Cut(target, "/")
	if !present || !separated || len(pointer) == 0 || len(pointer) > 16 || endpoint != "https://"+address {
		return invalid
	}
	for _, digit := range pointer {
		if !(digit >= '0' && digit <= '9' || digit >= 'a' && digit <= 'f') {
			return invalid
		}
	}
	host, port, err := net.SplitHostPort(address)
	proxyPort, portErr := strconv.Atoi(port)
	if err != nil || host != "127.0.0.1" || portErr != nil || proxyPort < 1 || proxyPort > 65535 {
		return invalid
	}
	peer, present := strings.CutPrefix(fields["peer"], "Peer{Addr: '"+address+"', LocalAddr: '")
	local, terminated := strings.CutSuffix(peer, "', AuthInfo: 'tls'}")
	if !present || !terminated {
		return invalid
	}
	host, port, err = net.SplitHostPort(local)
	localPort, portErr := strconv.Atoi(port)
	if err != nil || host != "127.0.0.1" || portErr != nil || localPort < 1 || localPort > 65535 {
		return invalid
	}
	return nil
}
