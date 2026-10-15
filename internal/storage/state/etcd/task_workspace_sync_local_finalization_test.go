package etcd

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestTaskWorkspaceSyncLocalFinalizationAdmission(t *testing.T) {
	seed := recoveryTestSeed(t)
	t.Run("recovery-owner-before-original", func(t *testing.T) {
		s := &taskArchiveStore{limits: taskArchiveLimits{Jobs: 2, JobBytes: 4096, TotalBytes: 8192, TotalIndexBytes: 8192}, spools: make(map[*taskArchiveSpool]struct{}), recovery: &taskArchiveRecoveryStore{memoryBytes: 4096, gate: make(chan struct{}, 1)}}
		s.self = s
		d := taskArchiveRecoveryDiagnostic{Occupied: true, State: 3}
		copy(d.TaskID[:], seed.Task.TaskID)
		s.recovery.slots[0].diagnostic = d
		owner, err := s.registerRecovery(context.Background(), 0)
		require.NoError(t, err)
		defer func() { require.NoError(t, owner.Close()) }()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		spool, err := s.reserveRecovery(ctx, seed)
		require.Error(t, err)
		require.Nil(t, spool, "same-task exclusion must precede slot charge and initialization")
		require.Zero(t, s.jobs, "denied admission cannot consume quota")
		require.False(t, s.recovery.slots[1].diagnostic.Occupied)
	})
	t.Run("quarantined-original-before-recovery", func(t *testing.T) {
		s := &taskArchiveStore{limits: taskArchiveLimits{Jobs: 2}, spools: make(map[*taskArchiveSpool]struct{}), recovery: &taskArchiveRecoveryStore{}}
		s.self = s
		d := taskArchiveRecoveryDiagnostic{Occupied: true, State: 3}
		copy(d.TaskID[:], seed.Task.TaskID)
		s.recovery.slots[0].diagnostic = d
		s.recovery.slots[1].diagnostic = taskArchiveRecoveryDiagnostic{Occupied: true, Quarantined: true}
		original := &taskArchiveSpool{store: s, recovery: &taskArchiveRecoveryOwner{slot: 1}, recoverySeedTask: seed.Task}
		s.spools[original] = struct{}{}
		_, err := s.registerRecovery(context.Background(), 0)
		require.ErrorIs(t, err, ErrConflict, "admission uses the immutable seed before manifest initialization")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		spool, err := s.reserveRecovery(ctx, seed)
		require.Error(t, err)
		require.Nil(t, spool)
		require.Zero(t, s.jobs)
	})
}

func TestTaskWorkspaceSyncLocalFinalizationRecords(t *testing.T) {
	seed := recoveryTestSeed(t)
	d := strings.Repeat("f", 64)
	claim := taskCloseClaimWire{seed.Task, seed.ClaimID, seed.WorkerID, seed.ClaimRevision, seed.LeaseID}
	p := taskWorkspaceSyncLocalProgress{Version: 1, Task: seed.Task, TaskBirth: 1, Binding: d, Claim: claim, Phase: "regular"}
	wire, err := encodeWorkspaceSyncLocalProgress(p)
	require.NoError(t, err)
	kv := &mvccpb.KeyValue{Key: []byte("progress"), Value: []byte(wire), CreateRevision: 2, ModRevision: 2, Version: 1}
	_, err = decodeWorkspaceSyncLocalProgress(kv)
	require.NoError(t, err)
	exhausted := &taskWorkspaceSyncLocalFinalizer{progressKV: &mvccpb.KeyValue{Version: math.MaxInt64}}
	require.ErrorIs(t, exhausted.saveProgress(context.Background(), p), ErrConflict)
	require.ErrorIs(t, exhausted.settleMetadata(context.Background(), strings.Repeat("f", 64)), ErrConflict)
	require.ErrorIs(t, exhausted.publishCompletion(context.Background()), ErrConflict)
	for _, mutate := range []func(*mvccpb.KeyValue){
		func(k *mvccpb.KeyValue) { k.Lease = 1 },
		func(k *mvccpb.KeyValue) { k.Value = append([]byte(" "), k.Value...) },
		func(k *mvccpb.KeyValue) { k.Version, k.ModRevision = math.MaxInt64, 3 },
		func(k *mvccpb.KeyValue) {
			k.Value = []byte(strings.Replace(wire, `"phase":"regular"`, `"phase":"bogus"`, 1))
		},
	} {
		bad := *kv
		mutate(&bad)
		_, err := decodeWorkspaceSyncLocalProgress(&bad)
		require.Error(t, err)
	}
	p.Phase, p.CheckpointRevision, p.CheckpointDigest = "metadata", 3, d
	p.IntentID, p.IntentBirth = seed.ArchiveCommand, 4
	path := strings.Repeat(strings.Repeat("<", 31)+"/", 127) + strings.Repeat("<", 32)
	require.Len(t, path, 4096)
	p.Ledger.Path = base64.StdEncoding.EncodeToString([]byte(path))
	for i := 0; i < 128; i++ {
		end := uint64((i+1)*32 - 1)
		if i == 127 {
			end = 4096
		}
		p.Ledger.Frames = append(p.Ledger.Frames, [3]uint64{end, math.MaxUint64, 0777})
	}
	wire, err = encodeWorkspaceSyncLocalProgress(p)
	require.NoError(t, err)
	require.LessOrEqual(t, len(wire), 13657)
	require.Len(t, p.Ledger.Path, 5464)
	p.Ledger.Frames = append(p.Ledger.Frames, [3]uint64{4096, 1, 0700})
	_, err = encodeWorkspaceSyncLocalProgress(p)
	require.Error(t, err)
	complete := taskWorkspaceSyncLocalComplete{Version: 1, Domain: "sandbox.workspace-sync-local-complete.v1", Task: seed.Task, TaskBirth: 1, Binding: d, Claim: claim, ProgressDigest: d, EvidenceDigest: d, CheckpointRevision: 3, CheckpointDigest: d, RootDigest: d}
	value, err := encodeOperationMetadata(complete, 8192)
	require.NoError(t, err)
	ckv := &mvccpb.KeyValue{Value: []byte(value), CreateRevision: 5, ModRevision: 5, Version: 1}
	_, err = decodeWorkspaceSyncLocalComplete(ckv)
	require.NoError(t, err)
	ckv.ModRevision++
	_, err = decodeWorkspaceSyncLocalComplete(ckv)
	require.Error(t, err, "completion is immutable")
}

func TestTaskWorkspaceSyncLocalFinalizationArchiveOrder(t *testing.T) {
	var order taskWorkspaceSyncLocalArchiveOrder
	for _, e := range []workspaceSyncArchiveEntry{{workspaceSyncLocalFile: workspaceSyncLocalFile{path: "a"}, kind: '5'}, {workspaceSyncLocalFile: workspaceSyncLocalFile{path: "a/x"}, kind: '0'}, {workspaceSyncLocalFile: workspaceSyncLocalFile{path: "b"}, kind: '5'}} {
		require.NoError(t, order.accept(&e))
	}
	require.Error(t, order.accept(&workspaceSyncArchiveEntry{workspaceSyncLocalFile: workspaceSyncLocalFile{path: "a/y"}, kind: '0'}), "a closed producer subtree cannot reopen")
}

func TestTaskWorkspaceSyncLocalFinalizationBounds(t *testing.T) {
	b, d := recyclingLocalDraftFixture(t)
	prefix, err := b.taskOperationsPrefix(d.record.Reference.Task)
	require.NoError(t, err)
	checkpointKey, err := b.namespace.taskCheckpointKey(d.record.Reference.Task)
	require.NoError(t, err)
	c := &TaskClaim{reference: d.record.Claim, checkpointKey: checkpointKey}
	a := &taskArchiveRecoveryAuthority{backend: b, claim: c, prefix: prefix}
	for i := 0; i < 50; i++ {
		a.comparisons = append(a.comparisons, clientv3.Compare(clientv3.Value(b.namespace.Root()+fmt.Sprintf("history/%02d", i)), "=", strings.Repeat("v", 512)))
	}
	s := &taskWorkspaceSyncRecoverySource{authority: a, manifest: taskArchiveRecoveryManifest{Seed: taskArchiveRecoverySeed{ArchiveCommand: d.record.Reference.IntentID}}}
	progress, complete, err := b.namespace.workspaceSyncLocalKeys(c.reference.Task)
	require.NoError(t, err)
	f := &taskWorkspaceSyncLocalFinalizer{source: s, progressKey: progress, completeKey: complete, progressKV: &mvccpb.KeyValue{ModRevision: 9, Value: []byte(strings.Repeat("p", 16384))}}
	require.NoError(t, f.preflight())
	f.charge = &taskWorkspaceSyncLocalMetadataCharge{intentKey: d.intentKey, slotKey: d.slotKey, value: strings.Repeat("i", int(b.workspaceSyncBudget.options.MaxIntentBytes)), birth: 8, generation: taskWorkspaceSyncRecyclingGeneration{create: 1, mod: 8, version: 2, value: strings.Repeat("s", 512)}}
	cmps := f.metadataComparisons(false)
	require.Len(t, cmps, 61)
	writes := []Write{{Key: d.slotKey, Value: []byte(strings.Repeat("s", 512))}, {Key: d.intentKey, Delete: true}, {Key: progress, Value: []byte(strings.Repeat("p", 16384))}}
	request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: taskProtoWrites(writes)}
	require.Equal(t, 64, len(request.Compare)+len(request.Success)+len(request.Failure))
	require.NoError(t, taskCheckWire(b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{recyclingGCReply(), taskProtoResponse(nil, nil)}))
	require.LessOrEqual(t, request.Size(), 256*1024)
	require.Len(t, f.metadataComparisons(true), 62)
	request.Success = append(request.Success, taskProtoRange(b.identityKey))
	require.Error(t, taskCheckWire(b.namespace, []*pb.TxnRequest{request}, nil))
}

func TestTaskWorkspaceSyncLocalFinalizationMetadataGate(t *testing.T) {
	store := &taskArchiveStore{limits: taskArchiveLimits{Jobs: 1}, recovery: &taskArchiveRecoveryStore{gate: make(chan struct{}, 1)}}
	store.self = store
	store.recovery.slots[0].diagnostic = taskArchiveRecoveryDiagnostic{Occupied: true, State: 3}
	s, err := store.registerRecovery(context.Background(), 0)
	require.NoError(t, err)
	require.NoError(t, s.begin(context.Background()))
	f := &taskWorkspaceSyncLocalFinalizer{source: s}
	// Another metadata owner holds the only token. Waiting is still a real
	// source operation, so Close cancels and joins it before clearing admission.
	finished := make(chan error, 1)
	go func() { _, err := f.metadataGate(s.ctx); s.end(); finished <- err }()
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	require.Error(t, <-finished)
	require.NoError(t, <-closed)
	require.Nil(t, store.recovery.slots[0].owner)
}
