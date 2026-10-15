package etcd

import (
	"context"
	"encoding/base64"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
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
	for _, mutate := range []func(*mvccpb.KeyValue){
		func(k *mvccpb.KeyValue) { k.Lease = 1 },
		func(k *mvccpb.KeyValue) { k.Value = append([]byte(" "), k.Value...) },
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
