//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/provisioning/deployment"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/launcher"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
)

// The private manifest/ACK layouts match the second test ELF. They are test
// process coordinates only; no exported API can consume them as authority.
type cleanupPendingCapacityInput struct {
	Binding, ConfigBinding     p.TrustBinding
	Identity                   state.Identity
	Root                       ed25519.PrivateKey
	RootPublic                 ed25519.PublicKey
	PhysicalTask               state.TaskReference
	Contract, Directory, Nonce string
}

type cleanupPendingCapacityACK struct {
	Nonce, Phase           string
	Slot                   uint32
	Rows, Turns, Conflicts int
}

func TestCleanupPendingPersistentCapacityNative(t *testing.T) {
	if os.Getenv("SANDBOX_CLEANUP_CAPACITY_NATIVE") != "1" {
		t.Skip("explicit owned two-ELF persistent-capacity Linux fixture required")
	}
	cleanupExecutorNativeCase(t, "automatic-persistent-capacity")
}

// Diagnostic reads retain at most 66 E/S rows for the fixed 33-Task case.
// These observations do not fund admission, release a slot or create a Run.
func cleanupPendingCapacityRows(t *testing.T, ctx context.Context, raw *clientv3.Client, root string) map[string]*mvccpb.KeyValue {
	t.Helper()
	response, err := raw.Get(ctx, root, clientv3.WithPrefix())
	require.NoError(t, err)
	rows := make(map[string]*mvccpb.KeyValue)
	for _, kv := range response.Kvs {
		key := string(kv.Key)
		if strings.HasPrefix(key, root+"cleanup-execution-slots/") || strings.Contains(key, "/tasks/") && strings.HasSuffix(key, "/execution") {
			require.Less(t, len(rows), 66)
			require.LessOrEqual(t, len(kv.Value), 4096)
			rows[key] = kv
		}
	}
	return rows
}

// Parent owns both OS children and all pipe ends. A single bounded reader
// carries four <=4096-byte ACKs; failures close inputs so the metadata owner
// joins itself, with a 2*q+1s cleanup ceiling before the original PID is killed.
// Only its one Wait goroutine reaps that PID, including every failure path.
func cleanupPendingCapacityParent(t *testing.T, ctx context.Context, s cleanupExecutorSetup, root ed25519.PrivateKey, admin *clientv3.Client, api *cleanupExecutorAPI, supervisor *exec.Cmd, supervisorWaited *bool) {
	t.Helper()
	require.NotEqual(t, uint8(0xaa), s.Task.Partition)
	var config launcherconfig.Config
	require.NoError(t, json.Unmarshal(s.LoaderConfig, &config))
	binding := p.TrustBinding{Namespace: config.Binding.Namespace, AuthorityID: config.Binding.AuthorityID, Target: config.Binding.Target, RestoreEpoch: config.Binding.RestoreEpoch}
	require.Equal(t, s.Binding, binding)
	require.Contains(t, config.Roots, root.Public().(ed25519.PublicKey))
	input := cleanupPendingCapacityInput{Binding: s.Binding, ConfigBinding: binding, Identity: s.Identity, Root: root, RootPublic: s.Root, PhysicalTask: s.Task, Contract: s.Contract, Directory: s.Directory, Nonce: uuid.NewString()}
	wire := observerNativeJSON(t, input)
	defer clear(wire)
	require.LessOrEqual(t, len(wire), 65536)
	inputPath := filepath.Join(s.Directory, "capacity-private.json")
	require.NoError(t, os.WriteFile(inputPath, wire, 0600))
	defer os.Remove(inputPath)
	commands, commandWriter, err := os.Pipe()
	require.NoError(t, err)
	defer commands.Close()
	ackReader, replies, err := os.Pipe()
	require.NoError(t, err)
	defer replies.Close()
	defer commandWriter.Close()
	defer ackReader.Close()
	child := exec.CommandContext(ctx, "/fixture/capacity-metadata", "-test.run=^TestCleanupPersistentCapacityChild$", "-test.v", "-test.timeout=110s")
	child.Env = append(os.Environ(), "CLEANUP_CAPACITY_CHILD_INPUT="+inputPath)
	child.ExtraFiles = []*os.File{commands, replies}
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	require.NoError(t, child.Start())
	require.NoError(t, commands.Close())
	require.NoError(t, replies.Close())
	waited := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		waited <- child.Wait()
		close(exited)
	}()
	type reply struct {
		ack cleanupPendingCapacityACK
		err error
	}
	acks := make(chan reply, 1)
	readerCtx, stopReader := context.WithCancel(ctx)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		reader := bufio.NewReaderSize(ackReader, 4096)
		for count := 0; count < 4; count++ {
			line, err := reader.ReadSlice('\n')
			var result reply
			result.err = err
			if err == nil {
				result.err = json.Unmarshal(line, &result.ack)
			}
			select {
			case acks <- result:
			case <-readerCtx.Done():
				return
			}
			if result.err != nil {
				return
			}
		}
	}()
	joined := false
	defer func() {
		_ = commandWriter.Close()
		stopReader()
		_ = ackReader.Close()
		<-readerDone
		if !joined {
			select {
			case err := <-waited:
				t.Logf("metadata failure cleanup original PID=%d Wait=%v", child.Process.Pid, err)
			case <-time.After(5 * time.Second):
				_ = child.Process.Kill()
				t.Logf("metadata bounded cleanup original PID=%d Wait=%v", child.Process.Pid, <-waited)
			}
		}
	}()
	receive := func(phase string) cleanupPendingCapacityACK {
		var result reply
		select {
		case result = <-acks:
		case <-exited:
			select {
			case result = <-acks:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		require.NoError(t, result.err)
		require.Equal(t, input.Nonce, result.ack.Nonce)
		require.Equal(t, phase, result.ack.Phase)
		return result.ack
	}
	send := func(command byte, phase string) cleanupPendingCapacityACK {
		_, err := commandWriter.Write([]byte{command})
		require.NoError(t, err)
		return receive(phase)
	}
	ready := receive("READY31")
	require.Equal(t, 63, ready.Rows)
	// The root private key is already resident in the metadata child. The
	// ordinary physical worker receives only its original setup, never this file.
	require.NoError(t, os.Remove(inputPath))
	workerPath := filepath.Join(s.Directory, "worker-capacity.json")
	require.NoError(t, os.WriteFile(workerPath, observerNativeJSON(t, s), 0600))
	worker := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCleanupExecutorWorkerChild$", "-test.v", "-test.timeout=110s")
	worker.Env = append(os.Environ(), "CLEANUP_EXECUTOR_CHILD_SETUP="+workerPath)
	worker.Stdout, worker.Stderr = os.Stdout, os.Stderr
	require.NoError(t, worker.Start())
	workerDone := make(chan error, 1)
	workerExited := make(chan struct{})
	go func() {
		workerDone <- worker.Wait()
		close(workerExited)
	}()
	workerJoined := false
	defer func() {
		if !workerJoined {
			_ = worker.Process.Kill()
			t.Logf("capacity physical failure original PID=%d Wait=%v", worker.Process.Pid, <-workerDone)
		}
	}()
	first := cleanupPendingCapacityPhysical(t, ctx, s, admin, worker.Process.Pid, workerExited, "archive-bound")
	full := cleanupPendingCapacityRows(t, ctx, admin, s.Binding.Namespace)
	require.Len(t, full, 65)
	selecting := 0
	for key, kv := range full {
		if !strings.HasSuffix(key, "/execution") {
			continue
		}
		var row cleanupExecutorRecoveryRow
		require.NoError(t, json.Unmarshal(kv.Value, &row))
		if row.Phase == "selecting" {
			selecting++
			require.Equal(t, uint8(0xaa), row.Task.Partition)
			delete(full, key)
		}
	}
	require.Equal(t, 1, selecting)
	require.Len(t, full, 64)
	checked := send(1, "CHECKED33")
	require.Equal(t, first.Slot, checked.Slot)
	require.Equal(t, 65, checked.Rows)
	require.Equal(t, 32, checked.Conflicts-ready.Conflicts)
	afterCheck := cleanupPendingCapacityRows(t, ctx, admin, s.Binding.Namespace)
	for key, before := range full {
		require.Equal(t, before, afterCheck[key], "same physical stable window is a prerequisite: %s", key)
	}
	settled := cleanupPendingCapacityPhysical(t, ctx, s, admin, worker.Process.Pid, workerExited, "settled")
	require.Equal(t, first.Incarnation, settled.Incarnation)
	require.NotEqual(t, first.Run.ID, settled.Run.ID)
	// Join the physical observer before the metadata retry can legitimately
	// occupy that same S; its own exact-S-absence check must already be done.
	workerErr := <-workerDone
	workerJoined = true
	require.NoError(t, workerErr)
	retried := send(2, "RETRIED33")
	require.Equal(t, first.Slot, retried.Slot)
	require.Equal(t, 65, retried.Rows)
	stopped := send(3, "STOPPED")
	require.Equal(t, 65, stopped.Rows)
	metadataErr := <-waited
	joined = true
	require.NoError(t, metadataErr)
	childErr := supervisor.Wait()
	*supervisorWaited = true
	var exit *exec.ExitError
	require.ErrorAs(t, childErr, &exit)
	require.Equal(t, syscall.SIGKILL, exit.Sys().(syscall.WaitStatus).Signal())
	occupied, err := admin.Get(ctx, s.Binding.Namespace+"cleanup-execution-slots/", clientv3.WithPrefix(), clientv3.WithLimit(33))
	require.NoError(t, err)
	require.Len(t, occupied.Kvs, 32, "STOP leaves 31 original metadata occupations and the real retry33")
	for _, kv := range occupied.Kvs {
		require.Zero(t, kv.Lease)
	}
	api.mu.Lock()
	patches, deletes, upgrades, absent := api.patches, api.deletes, api.upgrades, api.pod == nil
	api.mu.Unlock()
	require.Equal(t, 1, patches)
	require.Equal(t, 1, deletes)
	require.Positive(t, upgrades)
	require.True(t, absent)
	t.Logf("persistent capacity original 31 metadata plus one physical filled 32; CHECK33 refused32 without altering original64; true physical release freed slot=%d and retry33 reserved it; seed turns=%d check turns=%d retry turns=%d total conflicts=%d; both original PIDs joined", first.Slot, ready.Turns, checked.Turns-ready.Turns, retried.Turns-checked.Turns, stopped.Conflicts)
}

// Only the exact real physical E/Run and its own S are considered here. The
// accepted one-Task helper checks global S-empty, which is intentionally false
// for this experiment; its assertions and all old selectors stay unchanged.
func cleanupPendingCapacityPhysical(t *testing.T, ctx context.Context, s cleanupExecutorSetup, raw *clientv3.Client, pid int, exited <-chan struct{}, wanted string) cleanupExecutorRecoveryRow {
	t.Helper()
	prefix := fmt.Sprintf("%sp/%02x/tasks/%s/", s.Binding.Namespace, s.Task.Partition, s.Task.TaskID)
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	finalRead := false
	for {
		readCtx := ctx
		if finalRead {
			var cancel context.CancelFunc
			readCtx, cancel = context.WithTimeout(ctx, 2*time.Second)
			defer cancel() // At most one final observation, then return or Fatal.
		}
		response, err := raw.Get(readCtx, prefix+"execution")
		require.NoError(t, err)
		require.LessOrEqual(t, len(response.Kvs), 1)
		if len(response.Kvs) == 1 {
			var row cleanupExecutorRecoveryRow
			require.NoError(t, json.Unmarshal(response.Kvs[0].Value, &row))
			if row.Confirmed == wanted && row.Run != nil && row.Run.Joined {
				require.Equal(t, s.Task, row.Task)
				require.NoError(t, row.Run.Worker.Validate())
				require.Equal(t, pid, row.Run.Worker.PID)
				slot := fmt.Sprintf("%scleanup-execution-slots/%02d", s.Binding.Namespace, row.Slot)
				occupied, err := raw.Get(readCtx, slot)
				require.NoError(t, err)
				if wanted == "settled" {
					require.Equal(t, "settled", row.Phase)
					require.NotNil(t, row.Release)
					require.Empty(t, occupied.Kvs)
				} else {
					require.Equal(t, "occupied", row.Phase)
					require.Len(t, occupied.Kvs, 1)
					require.Zero(t, occupied.Kvs[0].Lease)
				}
				return row
			}
			if wanted == "archive-bound" {
				require.NotEqual(t, "settled", row.Phase, "normal recovery raced the full-capacity observation")
			}
		}
		if finalRead {
			t.Fatal("physical worker exited without required original Run state in final observation")
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-exited:
			// The preceding Get may have observed the pre-settlement E before
			// this process completed and exited. Exit proves neither success nor
			// failure: make one bounded fresh observation with all checks intact.
			finalRead = true
		case <-ticker.C:
		}
	}
}

func cleanupPendingCapacityWorker(t *testing.T, ctx context.Context, s cleanupExecutorSetup, kernel *launcher.KernelBoundary, dir string, tlsConfig *tls.Config, ca, certificate, key []byte) {
	t.Helper()
	stopClock := cleanupExecutorLoaderClock(t, ctx, s)
	defer stopClock()
	raw, err := clientv3.New(clientv3.Config{Endpoints: []string{"https://127.0.0.1:2379"}, TLS: tlsConfig, DialTimeout: 2 * time.Second, Logger: zap.NewNop()})
	require.NoError(t, err)
	defer raw.Close()
	pending := cleanupPendingAutomaticBacklog(t, ctx, raw, s)
	proxy := newCleanupExecutorEtcdProxy(t, tlsConfig, s.Task, "bound-recovery")
	defer proxy.close(t)
	workerID := "cleanup-capacity-physical"
	manifest := cleanupPendingResultManifest(t, s, dir, ca, certificate, key, proxy.endpoint(), workerID)
	d, err := deployment.OpenAutomaticLocalCleanup(ctx, manifest, kernel)
	require.NoError(t, err)
	defer d.Close()
	session := cleanupWorkerRoleSession(t, ctx, raw, s.Binding.Namespace, workerID, d.Done())
	first := cleanupPendingResultJoined(t, ctx, s, raw, proxy, pending, os.Getpid(), d.Done())
	settled := cleanupPendingCapacityPhysical(t, ctx, s, raw, os.Getpid(), d.Done(), "settled")
	require.Equal(t, first.row.Incarnation, settled.Incarnation)
	require.NotEqual(t, first.row.Run.ID, settled.Run.ID)
	require.Equal(t, session, cleanupExecutorRecoveryPoint(t, ctx, raw, string(session.Key)))
	cleanupPendingAutomaticArchive(t, dir)
	stats := proxy.snapshot()
	require.Equal(t, 2, stats.ClaimPuts)
	require.Equal(t, first.stats.ArchivePuts, stats.ArchivePuts)
	require.Equal(t, first.stats.ClosePuts, stats.ClosePuts)
	require.Equal(t, first.stats.QuiescencePuts, stats.QuiescencePuts)
	require.Equal(t, 1, stats.FaultMatched)
	require.Equal(t, 1, stats.FaultCommitted)
	require.Equal(t, 1, stats.FaultJoined)
	require.False(t, stats.ObservationOverflow)
	closeErr := d.Close()
	require.ErrorIs(t, closeErr, state.ErrOutcomeUnknown)
	require.Contains(t, closeErr.Error(), "automatic cleanup failures=1:")
	require.Equal(t, closeErr, d.Close())
	<-d.Done()
	waitErr := d.Wait(context.Background())
	require.ErrorIs(t, waitErr, state.ErrOutcomeUnknown)
	require.Contains(t, waitErr.Error(), "automatic cleanup failures=1:")
	require.NoError(t, kernel.ValidateCurrent())
	_, err = kernel.WorkspaceSyncWorkerReference()
	require.NoError(t, err)
	proxy.close(t)
	for _, id := range stats.GrantIDs {
		ttl, err := raw.TimeToLive(ctx, clientv3.LeaseID(id))
		require.NoError(t, err)
		require.EqualValues(t, -1, ttl.TTL)
	}
	t.Logf("capacity physical actual PID=%d same session=%d recovered Run=%s signed release=%d; original actor, jobs, RPCs, proxy joined", os.Getpid(), session.Lease, settled.Run.ID, settled.Release.CreateRevision)
}
