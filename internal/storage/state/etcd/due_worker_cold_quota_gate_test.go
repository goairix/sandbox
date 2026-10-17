package etcd

import (
	"context"
	"fmt"
	"testing"

	"github.com/goairix/sandbox/internal/runtime/launcher"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func cleanupColdQuotaBirthRelease(t *testing.T, f *cleanupColdQuotaFixture) (*cleanupBirthEvidence, *cleanupBirthReleaseEvidence) {
	t.Helper()
	f.fund()
	birth, err := f.b.readCleanupBirth(f.w.ctx, f.part, f.ranges, f.task.TaskID)
	require.NoError(t, err)
	f.fund()
	release, err := f.b.readCleanupBirthRelease(f.w.ctx, f.part, f.ranges, birth)
	require.NoError(t, err)
	require.NotNil(t, release)
	return birth, release
}

// Even the original signed release and common-birth T/I/receipt cannot delete
// P in cold mode before the registered original job has verified returned quota.
func TestDueWorkerColdQuotaPendingDeleteGate(t *testing.T) {
	f := newCleanupColdQuotaFixture(t)
	birth, release := cleanupColdQuotaBirthRelease(t, f)
	prior := f.server.point(f.birthSeed.keys[5])
	require.NotNil(t, prior)
	before := len(f.requests())
	f.fund()
	err := f.b.deleteCleanupPending(f.w.ctx, f.part, f.claims, birth, release)
	require.Error(t, err)
	err = f.b.deleteCleanupPending(f.w.ctx, f.part, f.claims, birth, release, f.job)
	require.Error(t, err)
	require.Len(t, f.requests(), before)
	require.Equal(t, prior, f.server.point(f.birthSeed.keys[5]))
	f.b.archiveLife.mu.Lock()
	claimTokens := f.claims.bucket.tokens
	f.b.archiveLife.mu.Unlock()
	require.Equal(t, uint64(1), claimTokens, "private quota gate is checked before paid deletion")
	f.verify(t)
	f.fund()
	err = f.b.deleteCleanupPending(f.w.ctx, f.part, f.claims, birth, release, f.job)
	require.NoError(t, err)
	require.Nil(t, f.server.point(f.birthSeed.keys[5]))
	request := f.requests()[len(f.requests())-1]
	for _, fence := range []taskFence{birth.task, birth.intent, birth.receipt, *birth.release, *birth.pending} {
		for _, comparison := range taskProtoComparisons(fence.comparisons()) {
			require.Contains(t, request.Compare, comparison, "quota gating cannot weaken the original P CAS")
		}
	}
}

// A quota proof copied into another job cannot open the physical-settlement
// gate, even if every ordinary public coordinate still matches the original.
func TestDueWorkerColdQuotaPendingDeleteRejectsCopiedVerifiedJob(t *testing.T) {
	f := newCleanupColdQuotaFixture(t)
	birth, release := cleanupColdQuotaBirthRelease(t, f)
	f.verify(t)
	copyJob := *f.job
	f.fund()
	before := len(f.requests())
	err := f.b.deleteCleanupPending(f.w.ctx, f.part, f.claims, birth, release, &copyJob)
	require.Error(t, err)
	require.Len(t, f.requests(), before)
	require.NotNil(t, f.server.point(f.birthSeed.keys[5]))
}

// The actual pending-delete actor branch must enqueue quota-only work in its
// original registry, then return to due/renewal work. Missing E/S cannot start a
// physical Run or cause reads of ownership records that signed release deleted.
func TestDueWorkerColdQuotaPendingReleaseUsesOriginalRegistry(t *testing.T) {
	f := newCleanupColdQuotaFixture(t)
	birth, release := cleanupColdQuotaBirthRelease(t, f)
	f.b.finishCleanupJob(f.job, nil)
	f.w.automatic = &dueWorkerAutomatic{active: 0, pendingPart: -1, phase: "pending-delete",
		partitions: []dueWorkerAutomaticPartition{{scan: f.w.scans[0]}}, birth: birth, release: release,
		candidate: cleanupPendingRecord{Version: 1, Reference: f.task, TaskDigest: workerIdentityDigest(birth.task.value)}}
	grants, _, _ := f.server.counts()
	f.fund()
	before := len(f.requests())
	_, err := f.w.automaticPendingTurn(f.w.ctx)
	require.NoError(t, err)
	require.Len(t, f.requests(), before, "enqueue is local and cannot spend the pending deletion Claim")
	require.NotNil(t, f.server.point(f.birthSeed.keys[5]))
	var queued *cleanupLocalJob
	for _, job := range f.w.cleanupJobs {
		if job != nil {
			require.Nil(t, queued, "only the original registry may retain the quota continuation")
			queued = job
		}
	}
	require.NotNil(t, queued)
	require.Equal(t, f.task, queued.task)
	require.False(t, queued.started)
	require.Empty(t, queued.runID)
	f.job = queued
	for turn := 0; turn < 40 && f.server.point(f.birthSeed.keys[5]) != nil; turn++ {
		f.turn(t)
	}
	require.Nil(t, f.server.point(f.birthSeed.keys[5]))
	require.NotNil(t, f.server.point(f.returnKey))
	require.Nil(t, f.server.point(f.birthSeed.keys[4]), "quota-only work cannot initialize E")
	gotGrants, _, _ := f.server.counts()
	require.Equal(t, grants+1, gotGrants, "only the original quota Stage is allowed")
	control, _, err := f.b.namespace.sandboxKeys(f.task.Partition, f.task.SandboxID, "read")
	require.NoError(t, err)
	owner, err := f.b.namespace.Key("p", fmt.Sprintf("%02x", f.task.Partition), "workspaces", f.workspace.Hash(), "owner")
	require.NoError(t, err)
	index, err := f.b.namespace.runtimeIndexKey(f.history.publication.entry.Record.Runtime.UID)
	require.NoError(t, err)
	for _, request := range f.requests()[before:] {
		for _, op := range request.Success {
			if get := op.GetRequestRange(); get != nil {
				for _, deletedOwnershipKey := range []string{control, owner, index} {
					require.NotEqual(t, deletedOwnershipKey, string(get.Key))
				}
			}
		}
	}
}

// Physical completion was sufficient to remove discovery P in old modes.
// Cold mode must preserve/create P until its same-registry quota job finishes.
func TestDueWorkerColdQuotaBackfillPreservesPhysicalTerminal(t *testing.T) {
	for _, cold := range []bool{false, true} {
		for _, pending := range []bool{false, true} {
			t.Run(fmt.Sprintf("cold-%t-pending-%t", cold, pending), func(t *testing.T) {
				f := newCleanupColdQuotaFixture(t)
				f.b.localCleanup.ColdRuntimeQuotaReturn = cold
				if !pending {
					// The host transport implements actual Txn deletes, not the
					// separate optional DeleteRange service method.
					_, err := f.b.client.Txn(context.Background()).Then(clientv3.OpDelete(f.birthSeed.keys[5])).Commit()
					require.NoError(t, err)
				}
				cleanupColdQuotaLegacyView(t, f)
				base := &backfillBootstrapFixture{b: f.b, server: f.server, session: f.session,
					partition: f.part, ranges: f.ranges, claims: f.claims}
				state := backfillBootstrapCreate(t, base)
				f.fund()
				page, err := f.b.readCleanupLegacyPage(f.w.ctx, f.part, f.ranges, state, nil)
				require.NoError(t, err)
				birth, release := cleanupColdQuotaBirthRelease(t, f)
				f.fund()
				next, err := f.b.advanceCleanupBackfill(f.w.ctx, f.part, f.claims, page, birth, release)
				require.NoError(t, err)
				require.Equal(t, "complete", next.record.Phase)
				if cold {
					require.NotNil(t, f.server.point(f.birthSeed.keys[5]))
				} else {
					require.Nil(t, f.server.point(f.birthSeed.keys[5]))
				}
			})
		}
	}
}

// Reuse the existing historical Range model. Capture the actual guarded B
// observation before its CAS, then honor fixed-revision keys-only range reads;
// the base point-only worker model intentionally cannot supply legacy rows.
func cleanupColdQuotaLegacyView(t *testing.T, f *cleanupColdQuotaFixture) {
	t.Helper()
	key, err := f.b.namespace.cleanupBackfillKey(f.task.Partition)
	require.NoError(t, err)
	pages := &duePageFixture{server: f.server, snapshots: make(map[int64][]*mvccpb.KeyValue)}
	f.server.mu.Lock()
	f.server.txnReply = func(request *pb.TxnRequest, response *pb.TxnResponse) {
		if !response.Succeeded || len(request.Success) != 1 {
			return
		}
		q := request.Success[0].GetRequestRange()
		if q == nil {
			return
		}
		if string(q.Key) == key && len(q.RangeEnd) == 0 && q.Revision == 0 {
			var snapshot []*mvccpb.KeyValue
			for _, point := range f.server.values {
				snapshot = append(snapshot, workerTestKV(point))
			}
			pages.snapshots[response.Header.Revision] = snapshot
		}
		if len(q.RangeEnd) != 0 {
			pages.rangeReply(request, response)
			if q.KeysOnly {
				for _, point := range response.Responses[0].GetResponseRange().Kvs {
					point.Value = nil
				}
			}
		}
	}
	f.server.mu.Unlock()
}

// These schema-valid Run coordinates are refusal inputs only: no worker,
// physical effects or successful native join is asserted by this host test.
func TestDueWorkerColdQuotaWaitsForOriginalRunJoin(t *testing.T) {
	for _, fault := range []string{"run-not-joined", "effects-not-joined"} {
		t.Run(fault, func(t *testing.T) {
			f := newCleanupColdQuotaFixture(t)
			r := cleanupBirthTestExecution(f.birthSeed)
			r.Phase, r.Confirmed = "occupied", "owner-released"
			r.Run = &cleanupExecutionRun{ID: "44444444-4444-4444-8444-444444444444",
				Worker: launcher.WorkspaceSyncWorkerReference{Incarnation: "55555555-5555-4555-8555-555555555555",
					BootID: f.history.publication.entry.Record.Runtime.BootID, PID: 7, StartTicks: 1, UID: 1, GID: 1,
					PIDNamespaceDev: 1, PIDNamespaceIno: 2, MountNamespaceDev: 1, MountNamespaceIno: 3, ExecutableDev: 1, ExecutableIno: 4}}
			require.NoError(t, r.Validate())
			f.job.state = &cleanupSlotState{record: r}
			if fault == "effects-not-joined" {
				r.Run.Joined = true
				f.job.started = true
				f.job.runID = r.Run.ID
			}
			before := len(f.requests())
			err := f.w.beginCleanupColdQuota(f.job)
			f.job.started = false // The refusal input owns no physical goroutine.
			require.Error(t, err)
			require.Nil(t, f.w.coldQuota)
			require.Len(t, f.requests(), before)
			require.NotNil(t, f.server.point(f.slotKey))
		})
	}
}

// Any mutation of a previously captured physical origin must close the gate
// before wire preflight, E initialization, S reservation or new Run admission.
func TestDueWorkerColdQuotaOriginPrecedesExecutionMutations(t *testing.T) {
	for _, phase := range []string{"wire-preflight", "initialize", "reserve", "run"} {
		t.Run(phase, func(t *testing.T) {
			f := newCleanupColdQuotaFixture(t)
			f.job.step = phase
			f.job.bundle.task.DataGateEpoch++
			f.fund()
			before := len(f.requests())
			_, err := f.w.cleanupTurn()
			require.NoError(t, err)
			require.Error(t, f.job.err)
			require.Len(t, f.requests(), before)
			require.Nil(t, f.server.point(f.birthSeed.keys[4]))
			require.Nil(t, f.server.point(f.returnKey))
		})
	}
}

// A charged typed slot conflict is terminal for this job. Treating it as a
// budget yield would pin the sole workspace and starve every other job.
func TestDueWorkerColdQuotaTypedConflictReleasesWorkspace(t *testing.T) {
	f := newCleanupColdQuotaFixture(t)
	birth, release := cleanupColdQuotaBirthRelease(t, f)
	for turn := 0; turn < 24; turn++ {
		if s := f.w.coldQuota; s != nil && s.prepare != nil && s.prepare.phase == coldQuotaWorkerPrepareWantSlot {
			break
		}
		f.turn(t)
	}
	require.NotNil(t, f.w.coldQuota)
	require.NotNil(t, f.w.coldQuota.prepare)
	require.Equal(t, coldQuotaWorkerPrepareWantSlot, f.w.coldQuota.prepare.phase)
	var slot coldSlotRecord
	require.NoError(t, coldDecode(f.server.point(f.slotKey), &slot))
	slot.Owner.IntentID = "another-runtime-owner"
	wire, err := coldEncode(slot)
	require.NoError(t, err)
	f.server.put(f.slotKey, wire, 0)
	f.fund()
	_, err = f.w.cleanupTurn()
	require.NoError(t, err)
	require.ErrorIs(t, f.job.err, ErrConflict)
	require.Nil(t, f.w.coldQuota)
	workerTestReceive(t, f.job.done)
	f.b.archiveLife.mu.Lock()
	liveErr := f.session.live()
	f.b.archiveLife.mu.Unlock()
	require.NoError(t, liveErr)
	replacement, err := f.w.enqueueCleanupColdQuota(birth, release)
	require.NoError(t, err)
	require.NoError(t, f.w.beginCleanupColdQuota(replacement))
	require.Same(t, replacement, f.w.coldQuota.job)
}
