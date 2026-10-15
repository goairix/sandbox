package etcd

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestTaskWorkspaceSyncRecoveryOwner(t *testing.T) {
	s := &taskArchiveStore{limits: taskArchiveLimits{Jobs: 2}, recovery: &taskArchiveRecoveryStore{}}
	s.self = s
	s.recovery.slots[0].diagnostic = taskArchiveRecoveryDiagnostic{Occupied: true, State: 3}
	r, err := s.registerRecovery(context.Background(), 0)
	require.NoError(t, err)
	require.Nil(t, r.index, "a registered waiter allocates no archive index or metadata buffers")
	_, err = s.registerRecovery(context.Background(), 0)
	require.ErrorIs(t, err, ErrConflict)
	s.recovery.slots[1].diagnostic = s.recovery.slots[0].diagnostic
	_, err = s.registerRecovery(context.Background(), 1)
	require.ErrorIs(t, err, ErrConflict, "duplicate cold slots cannot own concurrent writers for one task")
	require.NoError(t, r.begin(context.Background()))
	joined := make(chan error, 1)
	go func() { joined <- r.Close() }()
	select {
	case <-joined:
		t.Fatal("Close crossed a real operation registration")
	case <-time.After(20 * time.Millisecond):
	}
	r.end()
	require.NoError(t, <-joined)
	require.Nil(t, s.recovery.slots[0].owner)
	require.True(t, s.recovery.slots[0].diagnostic.Occupied)
}

func TestTaskWorkspaceSyncRecoveryMonitorAdmission(t *testing.T) {
	store := &taskArchiveStore{limits: taskArchiveLimits{Jobs: 1}, recovery: &taskArchiveRecoveryStore{}}
	store.self = store
	store.recovery.slots[0].diagnostic = taskArchiveRecoveryDiagnostic{Occupied: true, State: 3}
	s, err := store.registerRecovery(context.Background(), 0)
	require.NoError(t, err)
	require.True(t, s.beginMonitor())
	foreground := make(chan error, 1)
	go func() { foreground <- s.begin(context.Background()) }()
	select {
	case err := <-foreground:
		s.endMonitor()
		t.Fatalf("foreground crossed admitted monitor: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	s.endMonitor()
	require.NoError(t, <-foreground)
	s.end()
	require.True(t, s.beginMonitor())
	go func() { foreground <- s.begin(context.Background()) }()
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	require.Error(t, <-foreground, "owner cancellation wakes a waiting foreground admission")
	select {
	case err := <-closed:
		s.endMonitor()
		t.Fatalf("Close crossed monitor registration: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	s.endMonitor()
	require.NoError(t, <-closed)
}

func TestTaskWorkspaceSyncRecoveryCheckpointClaimHistory(t *testing.T) {
	r := workspaceSyncCheckpointRecordFixture()
	p := *r.WorkspaceSync
	b := &Backend{}
	c := &TaskClaim{origin: b, reference: TaskClaimReference{Task: r.Reference, ClaimID: uuid.NewString(), CreateRevision: 20}, birth: 11, initial: r.Attempt, checkpointKey: "checkpoint"}
	c.self = c
	s := &taskWorkspaceSyncRecoverySource{authority: &taskArchiveRecoveryAuthority{backend: b, claim: c}, plan: TaskWorkspaceSyncPlan{digest: p.PlanDigest}, manifest: taskArchiveRecoveryManifest{Seed: taskArchiveRecoverySeed{FinalSyncAttempt: p.FinalSyncAttemptID, ArchiveRevision: 10, TicketDigest: p.TicketDigest}, Received: &taskArchiveRecoveryReceived{Bytes: p.ArchiveBytes, Entries: p.ArchiveEntries, SHA256: p.ArchiveSHA256}, Binding: &taskArchiveRecoveryBinding{TargetDigest: p.TargetDigest}}}
	value, err := encodeTaskCheckpointRecord(r)
	require.NoError(t, err)
	kv := &mvccpb.KeyValue{Key: []byte(c.checkpointKey), Value: []byte(value), CreateRevision: 11, ModRevision: 13, Version: 2}
	_, err = s.decodeCheckpoint(kv)
	require.NoError(t, err, "strictly earlier claim checkpoint is validated history")
	_, err = captureWorkspaceSyncCheckpoint(kv, c, p)
	require.Error(t, err, "original checkpoint builder remains strict across claims")
	for _, mod := range []int64{20, 21} {
		bad := *kv
		bad.ModRevision = mod
		_, err := s.decodeCheckpoint(&bad)
		require.Error(t, err, "old claim checkpoint cannot be contemporaneous or later than fresh claim")
	}
	changed := r
	copy := *r.WorkspaceSync
	copy.TargetDigest = strings.Repeat("9", 64)
	changed.WorkspaceSync = &copy
	payload, err := encodeOperationMetadata(copy, 4096)
	require.NoError(t, err)
	changed.DetailDigest = workspaceSyncIntentDigest(payload)
	value, err = encodeTaskCheckpointRecord(changed)
	require.NoError(t, err)
	bad := *kv
	bad.Value = []byte(value)
	_, err = s.decodeCheckpoint(&bad)
	require.Error(t, err, "frozen old TargetDigest is never replaced with current worker digest")
}

func TestTaskWorkspaceSyncRecoveryWireBounds(t *testing.T) {
	b, d := recyclingLocalDraftFixture(t)
	d.record.RelativePath = strings.Repeat("p", workspaceSyncMaxPathBytes)
	d.record.Local.Root = "/" + strings.Repeat("r", 4095)
	require.NoError(t, b.prepareRecyclingIntent(d))
	prefix, err := b.taskOperationsPrefix(d.record.Reference.Task)
	require.NoError(t, err)
	a := &taskArchiveRecoveryAuthority{backend: b, claim: &TaskClaim{reference: d.record.Claim}, prefix: prefix}
	for i := 0; i < 50; i++ {
		key := b.namespace.Root() + fmt.Sprintf("history/%02d/", i)
		key += strings.Repeat("k", 1024-len(key))
		a.comparisons = append(a.comparisons, clientv3.Compare(clientv3.Value(key), "=", strings.Repeat("v", 512)))
	}
	s := &taskWorkspaceSyncRecoverySource{authority: a, target: &taskWorkspaceSyncLocalTarget{pin: *d.record.Local}}
	charge := &taskWorkspaceSyncRecoveryCharge{source: s, record: d.record, intentKey: d.intentKey, slotKey: d.slotKey, value: d.value, birth: 7}
	q := &taskWorkspaceSyncRecoveryCompletion{charge: charge, physical: workspaceSyncLocalIOResult{d.record.Size, d.record.BodySHA256, d.record.Local.Dev, 22}}
	terminal, err := s.recoveryTerminalValue(q)
	require.NoError(t, err)
	require.LessOrEqual(t, len(terminal), 512)
	checkpointKey, err := b.namespace.taskCheckpointKey(d.record.Reference.Task)
	require.NoError(t, err)
	cmps := append(s.comparisons(charge), clientv3.Compare(clientv3.ModRevision(charge.slotKey), "=", 8), clientv3.Compare(clientv3.Value(charge.slotKey), "=", terminal), clientv3.Compare(clientv3.ModRevision(checkpointKey), "=", 6), clientv3.Compare(clientv3.Value(checkpointKey), "=", strings.Repeat("c", 4096)))
	request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: taskProtoWrites([]Write{{Key: charge.slotKey, Value: []byte(terminal)}, {Key: charge.intentKey, Delete: true}, {Key: checkpointKey, Value: []byte(strings.Repeat("c", 4096))}})}
	require.Len(t, cmps, 61)
	require.Equal(t, 64, len(request.Compare)+len(request.Success)+len(request.Failure))
	require.NoError(t, taskCheckWire(b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{recyclingGCReply(), taskProtoResponse(nil, nil)}))
	require.LessOrEqual(t, request.Size(), 256*1024)
	request.Success = append(request.Success, taskProtoRange(b.identityKey))
	require.ErrorIs(t, taskCheckWire(b.namespace, []*pb.TxnRequest{request}, nil), ErrInvalidMutation)
}

func TestTaskWorkspaceSyncRecoveryRejectedProfiles(t *testing.T) {
	for _, b := range []*Backend{nil, {}, {archiveStore: &taskArchiveStore{}}} {
		r, err := b.RecoverTaskWorkspaceSyncLocal(context.Background(), nil, 0, nil, nil)
		require.Error(t, err)
		require.False(t, r.RegularFilesComplete)
		require.Zero(t, r.CompletedFiles)
	}
}

func TestTaskWorkspaceSyncRecoveryMetadataOwnsDecodedValues(t *testing.T) {
	m := recoveryTestManifest(t, "bound")
	var input, output [taskArchiveRecoveryMetadataBytes]byte
	wire, err := encodeTaskArchiveRecovery(m, input[:])
	require.NoError(t, err)
	decoded, err := decodeTaskArchiveRecovery(wire, output[:])
	require.NoError(t, err)
	clear(input[:])
	clear(output[:])
	require.Equal(t, m, decoded, "manifest and exclusions own decoded strings; neither metadata scratch array escapes")
}
