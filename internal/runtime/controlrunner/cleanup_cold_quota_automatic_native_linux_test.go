//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/provisioning/deployment"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/launcher"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
)

// TestCleanupColdQuotaAutomaticNative owns two fresh public cold creations.
// Each automatic worker discovers a committed backlog without an Execute call;
// the shared fixture retains its real archive, observer, provider and PID1 joins.
func TestCleanupColdQuotaAutomaticNative(t *testing.T) {
	if os.Getenv("SANDBOX_AUTOMATIC_COLD_QUOTA_NATIVE") != "1" {
		t.Skip("requires exact owned automatic cold quota native fixture")
	}
	for _, mode := range []string{"automatic-cold-quota-mounted", "automatic-cold-quota-role"} {
		if !t.Run(mode, func(t *testing.T) { cleanupExecutorNativeCase(t, mode) }) {
			return
		}
	}
}

func cleanupColdQuotaAutomaticMode(mode string) bool {
	return mode == "automatic-cold-quota-mounted" || mode == "automatic-cold-quota-role"
}

// Only the fixture operator initializes inventory before any runtime exists.
// Runtime workers receive the ready profile but cannot synthesize readiness.
func cleanupColdQuotaAutomaticInventory(t *testing.T, ctx context.Context, b *state.Backend) {
	t.Helper()
	for count := 0; count < 8; count++ {
		progress, err := b.InitializePreparationInventory(ctx)
		require.NoError(t, err)
		if progress.Ready {
			break
		}
	}
	inventory, err := b.ReadPreparationInventory(ctx)
	require.NoError(t, err)
	require.True(t, inventory.Ready)
}

func cleanupColdQuotaAutomaticCreate(t *testing.T, ctx context.Context, b *state.Backend, claim *state.CreationClaim) {
	t.Helper()
	attempt, err := b.BeginColdCreateAttempt(ctx, claim)
	require.NoError(t, err)
	require.NoError(t, b.ConsumeColdCreatePermit(claim, attempt.Permit))
}

// This observation retains original MVCC envelopes only for assertions. None
// of these copied rows can authorize archive, termination, release or refund.
type cleanupColdQuotaAutomaticHistory struct {
	namespace                             state.Namespace
	pending, task, reservation, quotaSlot *mvccpb.KeyValue
	preparation                           state.ColdPreparationRecord
}

func cleanupColdQuotaAutomaticBefore(t *testing.T, ctx context.Context, raw *clientv3.Client, s cleanupExecutorSetup) cleanupColdQuotaAutomaticHistory {
	t.Helper()
	ns, err := state.NewNamespace("/sandbox", "observer", "v1")
	require.NoError(t, err)
	prefix := fmt.Sprintf("%sp/%02x/", s.Binding.Namespace, s.Task.Partition)
	h := cleanupColdQuotaAutomaticHistory{namespace: ns, pending: cleanupPendingAutomaticBacklog(t, ctx, raw, s)}
	h.task = coldQuotaReadOne(t, ctx, raw, prefix+"tasks/"+s.Task.TaskID)
	h.reservation = coldQuotaReadOne(t, ctx, raw, prefix+"intents/"+s.IntentID+"/cold-preparation")
	require.NoError(t, json.Unmarshal(h.reservation.Value, &h.preparation))
	require.Equal(t, "settled", h.preparation.Phase)
	require.Equal(t, s.IntentID, h.preparation.Owner.IntentID)
	require.Equal(t, s.SandboxID, h.preparation.Owner.SandboxID)
	require.Equal(t, s.Generation, h.preparation.Owner.Generation)
	require.Equal(t, s.Task.Partition, h.preparation.Owner.Partition)
	require.EqualValues(t, 7, h.preparation.Quota.Partition)
	coldQuotaAssertHeld(t, ctx, raw, ns, h.preparation)
	slotKey, _ := coldQuotaSlotKeys(t, ns, h.preparation.Quota)
	h.quotaSlot = coldQuotaReadOne(t, ctx, raw, slotKey)

	// Independent preparation payment must already be committed before the
	// automatic actor starts; the quota remains occupied by the same owner.
	paidKey := coldQuotaReceiptKey(t, ns, h.preparation.Settlement)
	paid := coldQuotaReadOne(t, ctx, raw, paidKey)
	var receipt struct {
		state.StageReference
		Outcome state.Outcome `json:"outcome"`
	}
	require.NoError(t, json.Unmarshal(paid.Value, &receipt))
	require.Equal(t, state.OutcomeCommitted, receipt.Outcome)
	require.Equal(t, "cold_preparation_settle", receipt.StageID)
	require.Equal(t, h.reservation.ModRevision, paid.CreateRevision)
	require.Less(t, paid.CreateRevision, h.pending.CreateRevision)
	t.Logf("actual cold backlog Task=%s TaskBirth=%d quota=%+v owner=%+v paid=%d", s.Task.TaskID, h.task.CreateRevision, h.preparation.Quota, h.preparation.Owner, paid.CreateRevision)
	return h
}

// The watch is exact-key and bounded to the fixture's one original P. Its real
// DELETE revision proves quota payment preceded retirement, even if scheduling
// makes polling observe both completed operations for the first time together.
type cleanupColdQuotaAutomaticRetirement struct {
	event *clientv3.Event
	err   error
}

func cleanupColdQuotaAutomaticWatch(t *testing.T, ctx context.Context, raw *clientv3.Client, pending *mvccpb.KeyValue) (<-chan cleanupColdQuotaAutomaticRetirement, context.CancelFunc) {
	t.Helper()
	watchCtx, cancel := context.WithCancel(ctx)
	watch := raw.Watch(watchCtx, string(pending.Key), clientv3.WithRev(pending.CreateRevision+1), clientv3.WithPrevKV(), clientv3.WithCreatedNotify())
	select {
	case created := <-watch:
		if created.Err() != nil || !created.Created {
			cancel()
			for range watch {
			}
		}
		require.NoError(t, created.Err())
		require.True(t, created.Created)
	case <-ctx.Done():
		cancel()
		for range watch {
		}
		t.Fatal(ctx.Err())
	}
	done := make(chan cleanupColdQuotaAutomaticRetirement, 1)
	go func() {
		defer close(done)
		// Closing the wrapper channel certifies both the observer and its
		// underlying client watch have joined, including after an early result.
		defer func() {
			cancel()
			for range watch {
			}
		}()
		for response := range watch {
			if err := response.Err(); err != nil {
				done <- cleanupColdQuotaAutomaticRetirement{err: err}
				return
			}
			for _, event := range response.Events {
				if event.Type != mvccpb.DELETE {
					done <- cleanupColdQuotaAutomaticRetirement{err: fmt.Errorf("original pending row mutated before retirement")}
					return
				}
				done <- cleanupColdQuotaAutomaticRetirement{event: event}
				return
			}
		}
		done <- cleanupColdQuotaAutomaticRetirement{err: watchCtx.Err()}
	}()
	return done, cancel
}

func cleanupColdQuotaAutomaticManifest(t *testing.T, s cleanupExecutorSetup, dir string, ca, certificate, key []byte) string {
	t.Helper()
	manifest := cleanupExecutorLoaderFiles(t, s, dir, ca, certificate, key)
	wire, err := os.ReadFile(manifest)
	require.NoError(t, err)
	// RawMessage preserves the real uint64 cluster identity without float64.
	var mounted, worker map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wire, &mounted))
	require.NoError(t, json.Unmarshal(mounted["worker"], &worker))
	mounted["version"] = json.RawMessage("3")
	worker["automatic_cleanup"] = json.RawMessage("true")
	worker["worker_id"] = observerNativeJSON(t, "cleanup-cold-quota")
	mounted["worker"] = observerNativeJSON(t, worker)
	var profile state.Options
	coldQuotaReturnNativeProfile(&profile)
	mounted["preparation"] = observerNativeJSON(t, profile.PreparationInventory)
	require.NoError(t, os.WriteFile(manifest, observerNativeJSON(t, mounted), 0600))
	return manifest
}

func cleanupColdQuotaAutomaticWorker(t *testing.T, ctx context.Context, s cleanupExecutorSetup, kernel *launcher.KernelBoundary, dir string, tlsConfig *tls.Config, ca, certificate, key []byte) {
	t.Helper()
	defer clear(key)
	defer clear(s.IssuerKey)
	defer clear(s.LoaderClockKey)
	stopClock := cleanupExecutorLoaderClock(t, ctx, s)
	defer stopClock()
	manifest := cleanupColdQuotaAutomaticManifest(t, s, dir, ca, certificate, key)
	raw, err := clientv3.New(clientv3.Config{Endpoints: []string{"https://127.0.0.1:2379"}, TLS: tlsConfig, DialTimeout: 2 * time.Second, Logger: zap.NewNop()})
	require.NoError(t, err)
	defer func() { require.NoError(t, raw.Close()) }()
	history := cleanupColdQuotaAutomaticBefore(t, ctx, raw, s)
	retired, cancelWatch := cleanupColdQuotaAutomaticWatch(t, ctx, raw, history.pending)
	defer func() {
		cancelWatch()
		// A buffered result can arrive before the observer's deferred close.
		// Draining through channel closure joins that original goroutine on
		// both success and failure paths before the raw client is closed.
		for range retired {
		}
	}()
	observe := func(exited <-chan struct{}) {
		select {
		case result := <-retired:
			cancelWatch()
			for range retired {
			}
			require.NoError(t, result.err)
			require.NotNil(t, result.event)
			cleanupColdQuotaAutomaticReturned(t, ctx, raw, s, history, result.event)
		case <-exited:
			t.Fatal("automatic cold quota actor exited before original P retirement")
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}

	if s.AutomaticMode == "automatic-cold-quota-role" {
		cleanupColdQuotaAutomaticCommand(t, ctx, raw, s, manifest, observe)
	} else {
		d, err := deployment.OpenColdQuotaAutomaticLocalCleanup(ctx, manifest, kernel)
		require.NoError(t, err)
		defer d.Close()
		cleanupWorkerRoleSettled(t, ctx, raw, s, os.Getpid(), d.Done())
		observe(d.Done())
		closeErr := d.Close()
		cleanupPendingAutomaticCloseResult(t, closeErr)
		require.Equal(t, closeErr, d.Close())
		select {
		case <-d.Done():
		default:
			t.Fatal("cold quota Close returned before original worker completion")
		}
		waitErr := d.Wait(context.Background())
		cleanupPendingAutomaticCloseResult(t, waitErr)
		t.Logf("actual mounted v3 Close=%v Wait=%v original worker joined=true", closeErr, waitErr)
	}
	cleanupPendingAutomaticArchive(t, dir)
	require.NoError(t, kernel.ValidateCurrent())
	_, err = kernel.WorkspaceSyncWorkerReference()
	require.NoError(t, err)
}

// The independent role receives only mounted configuration. Signal shutdown
// must exit successfully and revoke its actual original session lease.
func cleanupColdQuotaAutomaticCommand(t *testing.T, ctx context.Context, raw *clientv3.Client, s cleanupExecutorSetup, manifest string, observe func(<-chan struct{})) {
	t.Helper()
	command := exec.Command("/fixture/cleanup-worker", "serve-cold-quota", manifest)
	command.Env = []string{"GOMAXPROCS=2"}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr cleanupWorkerRoleOutput
	command.Stdout, command.Stderr = &stdout, &stderr
	require.NoError(t, command.Start())
	waited, exited := make(chan error, 1), make(chan struct{})
	go func() {
		waited <- command.Wait()
		close(exited)
	}()
	joined := false
	defer func() {
		if !joined {
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			t.Logf("cold quota command failure cleanup actual Wait=%v", <-waited)
		}
		t.Logf("actual cold quota role PID=%d stdout=%q stderr=%q", command.Process.Pid, stdout.String(), stderr.String())
	}()
	session := cleanupWorkerRoleSession(t, ctx, raw, s.Binding.Namespace, "cleanup-cold-quota", exited)
	cleanupWorkerRoleSettled(t, ctx, raw, s, command.Process.Pid, exited)
	observe(exited)
	require.NoError(t, command.Process.Signal(syscall.SIGTERM))
	select {
	case err := <-waited:
		joined = true
		require.NoError(t, err, "normal signal shutdown must physically join without dependency errors")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(15 * time.Second):
		t.Fatal("cold quota role did not physically exit after SIGTERM")
	}
	require.Empty(t, stdout.String())
	remaining, err := raw.Get(ctx, string(session.Key))
	require.NoError(t, err)
	require.Empty(t, remaining.Kvs)
	ttl, err := raw.TimeToLive(ctx, clientv3.LeaseID(session.Lease))
	require.NoError(t, err)
	require.EqualValues(t, -1, ttl.TTL)
	t.Logf("actual cold quota role PID=%d original session=%s lease=%d revoked=true joined=true", command.Process.Pid, session.Key, session.Lease)
}

func cleanupColdQuotaAutomaticReturned(t *testing.T, ctx context.Context, raw *clientv3.Client, s cleanupExecutorSetup, h cleanupColdQuotaAutomaticHistory, event *clientv3.Event) {
	t.Helper()
	require.Equal(t, h.pending, event.PrevKv, "normal sweep deletes the exact original P occupation")
	require.Equal(t, string(h.pending.Key), string(event.Kv.Key))
	deletedAt := event.Kv.ModRevision
	require.Greater(t, deletedAt, h.pending.CreateRevision)
	prefix := fmt.Sprintf("%sp/%02x/intents/%s/", s.Binding.Namespace, s.Task.Partition, s.IntentID)
	journal := coldQuotaReadOne(t, ctx, raw, prefix+"cold-runtime-quota-attempt")
	ref, err := coldQuotaJournalNativeRecord(journal)
	require.NoError(t, err)
	require.Zero(t, journal.Lease)
	require.EqualValues(t, 1, journal.Version)
	require.Equal(t, journal.CreateRevision, journal.ModRevision)
	require.Greater(t, journal.CreateRevision, h.task.CreateRevision)
	require.Equal(t, "cold_runtime_quota_return", ref.StageID)
	require.Equal(t, h.namespace.Root(), ref.Namespace)
	require.Equal(t, s.Binding.RestoreEpoch, ref.RestoreEpoch)
	require.Equal(t, h.preparation.Owner.Partition, ref.Partition)
	require.Equal(t, h.preparation.Owner.RequestID, ref.RequestID)
	locator := state.StageAttemptLocator{Namespace: ref.Namespace, Partition: ref.Partition, RequestID: ref.RequestID, StageID: ref.StageID, AttemptID: ref.AttemptID, RestoreEpoch: ref.RestoreEpoch}
	require.NoError(t, locator.Validate(), "actual journal reference must retain a canonical original attempt identity")
	digest, err := hex.DecodeString(ref.Digest)
	require.NoError(t, err)
	require.Len(t, digest, sha256.Size)
	require.Equal(t, hex.EncodeToString(digest), ref.Digest, "stage digest must use canonical lowercase SHA256 encoding")
	var attribution struct {
		Version       int                     `json:"version"`
		Domain        string                  `json:"domain"`
		Namespace     string                  `json:"namespace"`
		RestoreEpoch  string                  `json:"restore_epoch"`
		ProfileDigest string                  `json:"profile_digest"`
		Owner         state.ColdSlotOwner     `json:"owner"`
		Quota         state.ColdSlotReference `json:"quota"`
		Task          state.TaskReference     `json:"task"`
		TaskBirth     int64                   `json:"task_birth"`
		ReleaseStamp  p.RecordStamp           `json:"release_stamp"`
	}
	require.NoError(t, json.Unmarshal(journal.Value, &attribution))
	require.Equal(t, 1, attribution.Version)
	require.Equal(t, "sandbox.cold-runtime-quota-attempt.v1", attribution.Domain)
	require.Equal(t, h.namespace.Root(), attribution.Namespace)
	require.Equal(t, s.Binding.RestoreEpoch, attribution.RestoreEpoch)
	require.Equal(t, h.preparation.ProfileDigest, attribution.ProfileDigest)
	require.Equal(t, h.preparation.Owner, attribution.Owner)
	require.Equal(t, h.preparation.Quota, attribution.Quota)
	require.Equal(t, s.Task, attribution.Task)
	require.Equal(t, h.task.CreateRevision, attribution.TaskBirth)
	require.Equal(t, attribution.ReleaseStamp.CreateRevision, attribution.ReleaseStamp.ModRevision)
	require.Greater(t, attribution.ReleaseStamp.CreateRevision, h.task.CreateRevision)
	require.Len(t, attribution.ReleaseStamp.SHA256, 64)
	require.Greater(t, journal.CreateRevision, attribution.ReleaseStamp.ModRevision)
	returned := coldQuotaReadOne(t, ctx, raw, prefix+"cold-runtime-quota-return")
	var proof struct {
		Attempt      state.StageAttemptLocator `json:"attempt"`
		ReleaseStamp p.RecordStamp             `json:"release_stamp"`
		Owner        state.ColdSlotOwner       `json:"owner"`
		Quota        state.ColdSlotReference   `json:"quota"`
		Runtime      state.RuntimeReference    `json:"runtime"`
		NodeUID      string                    `json:"node_uid"`
		NodeBootID   string                    `json:"node_boot_id"`
		Release      struct {
			Task      state.TaskReference `json:"task"`
			TaskBirth int64               `json:"task_birth"`
		} `json:"release"`
	}
	require.NoError(t, json.Unmarshal(returned.Value, &proof))
	require.Equal(t, locator, proof.Attempt)
	require.Equal(t, attribution.ReleaseStamp, proof.ReleaseStamp)
	require.Equal(t, h.preparation.Owner, proof.Owner)
	require.Equal(t, h.preparation.Quota, proof.Quota)
	require.Equal(t, state.RuntimeReference(s.Runtime), proof.Runtime)
	require.Equal(t, s.NodeUID, proof.NodeUID)
	require.Equal(t, s.NodeBootID, proof.NodeBootID)
	require.Equal(t, s.Task, proof.Release.Task)
	require.Equal(t, h.task.CreateRevision, proof.Release.TaskBirth)
	require.Zero(t, returned.Lease)
	require.EqualValues(t, 1, returned.Version)
	require.Equal(t, returned.CreateRevision, returned.ModRevision)
	require.Greater(t, returned.CreateRevision, journal.ModRevision)
	require.Less(t, returned.CreateRevision, deletedAt, "quota receipt must precede the real P DELETE transaction")
	receipt := coldQuotaJournalNativeReceipt(t, ctx, raw, h.namespace, ref, state.OutcomeCommitted)
	require.Equal(t, returned.CreateRevision, receipt.CreateRevision)

	// Read at the actual deletion revision: later inspection alone could hide
	// an invalid early retirement followed by a subsequent unrelated refund.
	slotKey, freeKey := coldQuotaSlotKeys(t, h.namespace, h.preparation.Quota)
	var slot, free *mvccpb.KeyValue
	for index, key := range []string{slotKey, freeKey} {
		response, err := raw.Get(ctx, key, clientv3.WithRev(deletedAt))
		require.NoError(t, err)
		require.Len(t, response.Kvs, 1)
		if index == 0 {
			slot = response.Kvs[0]
		} else {
			free = response.Kvs[0]
		}
	}
	var released struct {
		Occupied bool                    `json:"occupied"`
		Owner    state.ColdSlotOwner     `json:"owner"`
		Slot     state.ColdSlotReference `json:"slot"`
	}
	require.NoError(t, json.Unmarshal(slot.Value, &released))
	require.False(t, released.Occupied)
	require.Equal(t, state.ColdSlotOwner{}, released.Owner)
	require.Equal(t, h.preparation.Quota, released.Slot)
	require.Equal(t, h.quotaSlot.CreateRevision, slot.CreateRevision)
	require.Zero(t, slot.Lease)
	require.Greater(t, slot.Version, h.quotaSlot.Version)
	require.Equal(t, slot.Value, free.Value)
	require.Equal(t, returned.CreateRevision, slot.ModRevision)
	require.Zero(t, free.Lease)
	require.EqualValues(t, 1, free.Version)
	require.Equal(t, returned.CreateRevision, free.CreateRevision)
	require.Equal(t, free.CreateRevision, free.ModRevision)
	require.Equal(t, h.reservation, coldQuotaReadOne(t, ctx, raw, string(h.reservation.Key)))
	require.Equal(t, h.task, coldQuotaReadOne(t, ctx, raw, string(h.task.Key)))
	absent, err := raw.Get(ctx, string(h.pending.Key))
	require.NoError(t, err)
	require.Empty(t, absent.Kvs)
	t.Logf("actual automatic quota returned TaskBirth=%d quotaSlotBirth=%d journal=%d receipt=%d PDelete=%d same-owner=true same-quota-epoch=true", h.task.CreateRevision, slot.CreateRevision, journal.ModRevision, receipt.CreateRevision, deletedAt)
}
