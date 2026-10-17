//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime/launcher"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
)

// cleanupWorkerRoleNative is called only by the separate pending native fixture
// after it has created an actual pending Task and stopped any original worker.
// The caller owns the real enrolled runtime, observer, provider and etcd fixture.
// This helper owns a signed-clock server, mounted files and two fresh command
// execs. Neither process receives a Task reference or an explicit Execute call.
func cleanupWorkerRoleNative(t *testing.T, ctx context.Context, s cleanupExecutorSetup, dir string, tlsConfig *tls.Config, ca, cert, key []byte, binaryPath string) {
	t.Helper()
	require.NotZero(t, os.Geteuid())
	require.True(t, filepath.IsAbs(binaryPath))
	info, err := os.Stat(binaryPath)
	require.NoError(t, err)
	require.True(t, info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0)
	stopClock := cleanupExecutorLoaderClock(t, ctx, s)
	defer stopClock()
	manifest := cleanupExecutorLoaderFiles(t, s, dir, ca, cert, key)
	wire, err := os.ReadFile(manifest)
	require.NoError(t, err)
	// Preserve uint64 identity bytes while editing only the worker fields.
	var mounted map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wire, &mounted))
	var worker map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(mounted["worker"], &worker))
	require.NotEmpty(t, worker)
	workerID := "cleanup-role-" + uuid.NewString()
	mounted["version"] = json.RawMessage("2")
	worker["automatic_cleanup"] = json.RawMessage("true")
	worker["worker_id"] = observerNativeJSON(t, workerID)
	mounted["worker"] = observerNativeJSON(t, worker)
	require.NoError(t, os.WriteFile(manifest, observerNativeJSON(t, mounted), 0600))

	raw, err := clientv3.New(clientv3.Config{
		Endpoints: []string{"https://127.0.0.1:2379"}, TLS: tlsConfig,
		DialTimeout: 2 * time.Second, Logger: zap.NewNop(),
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, raw.Close()) }()
	cleanupPendingAutomaticBacklog(t, ctx, raw, s)
	for _, mode := range []string{"signal", "fatal-session-fence"} {
		if !t.Run(mode, func(t *testing.T) {
			cleanupWorkerRoleNativeProcess(t, ctx, s, raw, manifest, workerID, binaryPath, mode)
		}) {
			return
		}
		if mode == "signal" {
			cleanupPendingAutomaticArchive(t, dir)
		}
	}
}

// The command has no success payload, so 64KiB is ample for bounded dependency
// diagnostics. Overflow fails its copy operation rather than growing the parent.
type cleanupWorkerRoleOutput struct{ buffer bytes.Buffer }

func (b *cleanupWorkerRoleOutput) Write(data []byte) (int, error) {
	if len(data) > 65536-b.buffer.Len() {
		return 0, errors.New("cleanup role output exceeds 64KiB")
	}
	return b.buffer.Write(data)
}

func (b *cleanupWorkerRoleOutput) String() string {
	return b.buffer.String()
}

func cleanupWorkerRoleNativeProcess(t *testing.T, ctx context.Context, s cleanupExecutorSetup, raw *clientv3.Client, manifest, workerID, binaryPath, mode string) {
	t.Helper()
	command := exec.Command(binaryPath, "serve", manifest)
	command.Env = []string{"GOMAXPROCS=2"}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr cleanupWorkerRoleOutput
	command.Stdout, command.Stderr = &stdout, &stderr
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
			// Failure cleanup owns only this fresh process group. It is an
			// actual force-stop/join, never evidence of a successful shutdown.
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			waitErr := <-waited
			t.Logf("cleanup role failure cleanup PID=%d actual Wait=%v", command.Process.Pid, waitErr)
		}
		t.Logf("cleanup role PID=%d mode=%s stdout=%q stderr=%q", command.Process.Pid, mode, stdout.String(), stderr.String())
	}()

	session := cleanupWorkerRoleSession(t, ctx, raw, s.Binding.Namespace, workerID, exited)
	if mode == "signal" {
		cleanupWorkerRoleSettled(t, ctx, raw, s, command.Process.Pid, exited)
		require.NoError(t, command.Process.Signal(syscall.SIGTERM))
	} else {
		cleanupWorkerRoleRenewed(t, ctx, raw, s, session, exited)
		// Delete only the exact observed original session. This is an
		// operator fault injection; the copied row grants no worker authority.
		fenced, err := raw.Txn(ctx).If(
			clientv3.Compare(clientv3.CreateRevision(string(session.Key)), "=", session.CreateRevision),
			clientv3.Compare(clientv3.ModRevision(string(session.Key)), "=", session.ModRevision),
			clientv3.Compare(clientv3.LeaseValue(string(session.Key)), "=", session.Lease),
			clientv3.Compare(clientv3.Value(string(session.Key)), "=", string(session.Value)),
		).Then(clientv3.OpDelete(string(session.Key))).Commit()
		require.NoError(t, err)
		require.True(t, fenced.Succeeded)
	}

	select {
	case err := <-waited:
		joined = true
		if mode == "signal" {
			require.NoError(t, err, "normal signal must join without unknown or other error leaves")
		} else {
			var exit *exec.ExitError
			require.ErrorAs(t, err, &exit)
			require.Equal(t, 1, exit.ExitCode())
			require.True(t, strings.HasSuffix(stderr.String(), "sandbox-cleanup-worker: failed\n"))
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(15 * time.Second):
		t.Fatal("cleanup role did not physically exit after signal/session loss")
	}
	require.Empty(t, stdout.String())
	require.NotContains(t, stderr.String(), "CLEANUP-LOADER-FIXTURE")
	remaining, err := raw.Get(ctx, string(session.Key))
	require.NoError(t, err)
	require.Empty(t, remaining.Kvs)
	ttl, err := raw.TimeToLive(ctx, clientv3.LeaseID(session.Lease))
	require.NoError(t, err)
	require.EqualValues(t, -1, ttl.TTL, "actual exit joins original lease release")
	t.Logf("cleanup role PID=%d mode=%s joined=true lease=%d revoked=true", command.Process.Pid, mode, session.Lease)
}

func cleanupWorkerRoleSession(t *testing.T, ctx context.Context, raw *clientv3.Client, namespace, workerID string, exited <-chan struct{}) *mvccpb.KeyValue {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		// The original registry is fixed at 32. This test observation reads
		// at most one extra row to reject an unexpectedly unbounded fixture.
		response, err := raw.Get(ctx, namespace+"workers/sessions/", clientv3.WithPrefix(), clientv3.WithLimit(33))
		require.NoError(t, err)
		require.LessOrEqual(t, len(response.Kvs), 32)
		var selected *mvccpb.KeyValue
		for _, kv := range response.Kvs {
			var row struct {
				WorkerID  string `json:"worker_id"`
				SessionID string `json:"session_id"`
				LeaseID   int64  `json:"lease_id"`
			}
			require.NoError(t, json.Unmarshal(kv.Value, &row))
			if row.WorkerID != workerID {
				continue
			}
			require.Nil(t, selected, "one command must own exactly one original session")
			require.Equal(t, namespace+"workers/sessions/"+row.SessionID, string(kv.Key))
			require.Positive(t, kv.Lease)
			require.Equal(t, row.LeaseID, kv.Lease)
			require.Equal(t, kv.CreateRevision, kv.ModRevision)
			selected = kv
		}
		if selected != nil {
			return selected
		}
		select {
		case <-exited:
			t.Fatal("cleanup role exited before its original session was observed")
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

func cleanupWorkerRoleSettled(t *testing.T, ctx context.Context, raw *clientv3.Client, s cleanupExecutorSetup, pid int, exited <-chan struct{}) {
	t.Helper()
	key := fmt.Sprintf("%sp/%02x/tasks/%s/execution", s.Binding.Namespace, s.Task.Partition, s.Task.TaskID)
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		response, err := raw.Get(ctx, key)
		require.NoError(t, err)
		require.LessOrEqual(t, len(response.Kvs), 1)
		if len(response.Kvs) == 1 {
			var row struct {
				Phase, Confirmed string
				Run              *struct {
					Worker launcher.WorkspaceSyncWorkerReference `json:"worker"`
					Joined bool                                  `json:"joined"`
				}
				Release *struct {
					CreateRevision int64 `json:"create_revision"`
				}
			}
			require.NoError(t, json.Unmarshal(response.Kvs[0].Value, &row))
			if row.Phase == "settled" {
				require.Equal(t, "settled", row.Confirmed)
				require.NotNil(t, row.Run)
				require.True(t, row.Run.Joined)
				require.NoError(t, row.Run.Worker.Validate())
				require.Equal(t, pid, row.Run.Worker.PID, "physical Run belongs to the actual command exec")
				require.EqualValues(t, os.Geteuid(), row.Run.Worker.UID)
				require.NotNil(t, row.Release)
				require.Positive(t, row.Release.CreateRevision)
				occupied, err := raw.Get(ctx, s.Binding.Namespace+"cleanup-execution-slots/", clientv3.WithPrefix(), clientv3.WithLimit(33))
				require.NoError(t, err)
				require.Empty(t, occupied.Kvs)
				t.Logf("cleanup role actual PID=%d automatically settled Task=%s RunJoined=true releaseRevision=%d", pid, s.Task.TaskID, row.Release.CreateRevision)
				return
			}
		}
		select {
		case <-exited:
			t.Fatal("cleanup role exited before actual automatic Task settlement")
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

// Observe a real renewal after the original partition exists before injecting
// session loss. A session row alone is published during startup and would also
// let a constructor refusal masquerade as a resident-worker fatal-exit test.
func cleanupWorkerRoleRenewed(t *testing.T, ctx context.Context, raw *clientv3.Client, s cleanupExecutorSetup, session *mvccpb.KeyValue, exited <-chan struct{}) {
	t.Helper()
	partitionKey := fmt.Sprintf("%sp/%02x/worker", s.Binding.Namespace, s.Task.Partition)
	minimum := int64(0)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		partition, err := raw.Get(ctx, partitionKey)
		require.NoError(t, err)
		require.LessOrEqual(t, len(partition.Kvs), 1)
		if len(partition.Kvs) == 1 {
			require.Equal(t, session.Lease, partition.Kvs[0].Lease)
			ttl, err := raw.TimeToLive(ctx, clientv3.LeaseID(session.Lease))
			require.NoError(t, err)
			require.Positive(t, ttl.TTL)
			if minimum != 0 && ttl.TTL > minimum+2 {
				t.Logf("cleanup role original lease=%d actual renewal observed TTL=%d minimum=%d", session.Lease, ttl.TTL, minimum)
				return
			}
			if minimum == 0 || ttl.TTL < minimum {
				minimum = ttl.TTL
			}
		}
		select {
		case <-exited:
			t.Fatal("cleanup role exited before resident session renewal")
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}
