package etcd

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

func TestTaskWorkspaceSyncCheckpointV2Wire(t *testing.T) {
	_, _, _, initial := taskTestRecords()
	payload := `{"version":1,"final_sync_attempt_id":"` + taskTestID + `","plan_digest":"` + strings.Repeat("a", 64) + `","archive_sha256":"` + strings.Repeat("b", 64) + `","archive_bytes":2048,"archive_entries":2,"ticket_digest":"` + strings.Repeat("c", 64) + `","target_digest":"` + strings.Repeat("d", 64) + `","entry_ordinal":2,"body_end":1029,"path_digest":"` + strings.Repeat("e", 64) + `","body_sha256":"` + strings.Repeat("f", 64) + `","intent_id":"` + taskClaimTestID + `","intent_birth":12,"evidence_digest":"` + strings.Repeat("1", 64) + `"}`
	initial.ClaimID, initial.DetailDigest = taskClaimTestID, workspaceSyncIntentDigest(payload)
	wire, err := encodeTaskCheckpointRecord(initial)
	require.NoError(t, err)
	wire = strings.Replace(wire, `"version":1`, `"version":2`, 1)
	wire = strings.TrimSuffix(wire, "}") + `,"workspace_sync":` + payload + "}"
	var got TaskCheckpointRecord
	require.NoError(t, decodeTaskCheckpointRecord(&mvccpb.KeyValue{Value: []byte(wire), CreateRevision: 11, ModRevision: 13, Version: 2}, &got), "strict v2 checkpoint retains durable workspace progress")
	require.EqualValues(t, 2, got.Version)
}

func workspaceSyncCheckpointRecordFixture() TaskCheckpointRecord {
	_, _, _, r := taskTestRecords()
	p := &TaskWorkspaceSyncCheckpoint{Version: 1, FinalSyncAttemptID: taskTestID, PlanDigest: strings.Repeat("a", 64), ArchiveSHA256: strings.Repeat("b", 64), ArchiveBytes: 2048, ArchiveEntries: 2, TicketDigest: strings.Repeat("c", 64), TargetDigest: strings.Repeat("d", 64), EntryOrdinal: 2, BodyEnd: 1029, PathDigest: strings.Repeat("e", 64), BodySHA256: strings.Repeat("f", 64), IntentID: taskClaimTestID, IntentBirth: 12, EvidenceDigest: strings.Repeat("1", 64)}
	wire, _ := encodeOperationMetadata(*p, 4096)
	r.Version, r.ClaimID, r.DetailDigest, r.WorkspaceSync = 2, taskClaimTestID, workspaceSyncIntentDigest(wire), p
	return r
}

func TestTaskWorkspaceSyncCheckpointStrict(t *testing.T) {
	r := workspaceSyncCheckpointRecordFixture()
	wire, err := encodeTaskCheckpointRecord(r)
	require.NoError(t, err)
	kv := &mvccpb.KeyValue{Value: []byte(wire), CreateRevision: 11, ModRevision: 13, Version: 2}
	var got TaskCheckpointRecord
	require.NoError(t, decodeTaskCheckpointRecord(kv, &got))
	require.Equal(t, r, got)
	require.NotSame(t, r.WorkspaceSync, got.WorkspaceSync)
	badWire := func(wire []byte) {
		t.Helper()
		bad, dst := *kv, got
		bad.Value = wire
		require.ErrorIs(t, decodeTaskCheckpointRecord(&bad, &dst), ErrCorruptRecord)
		require.Equal(t, got, dst, "malformed checkpoint must not partially replace diagnostic destination")
	}
	taskBadFields(t, kv.Value, badWire, "")
	badWire(append([]byte(" "), kv.Value...))
	badWire(append(bytes.Clone(kv.Value), []byte(" {}")...))
	badWire(append(bytes.Clone(kv.Value), bytes.Repeat([]byte(" "), 4097-len(kv.Value))...))
	badWire([]byte(strings.Replace(wire, `"version":2`, `"version":2.0`, 1)))
	for _, mutate := range []func(*TaskWorkspaceSyncCheckpoint){
		func(p *TaskWorkspaceSyncCheckpoint) { p.Version++ },
		func(p *TaskWorkspaceSyncCheckpoint) { p.FinalSyncAttemptID = "bad" },
		func(p *TaskWorkspaceSyncCheckpoint) { p.IntentID = strings.ToUpper(taskClaimTestID) },
		func(p *TaskWorkspaceSyncCheckpoint) { p.ArchiveBytes = 1535 },
		func(p *TaskWorkspaceSyncCheckpoint) { p.ArchiveBytes = math.MaxUint64 },
		func(p *TaskWorkspaceSyncCheckpoint) { p.ArchiveEntries = 0 },
		func(p *TaskWorkspaceSyncCheckpoint) { p.EntryOrdinal = 0 },
		func(p *TaskWorkspaceSyncCheckpoint) { p.EntryOrdinal++ },
		func(p *TaskWorkspaceSyncCheckpoint) { p.BodyEnd = 0 },
		func(p *TaskWorkspaceSyncCheckpoint) { p.BodyEnd = p.ArchiveBytes + 1 },
		func(p *TaskWorkspaceSyncCheckpoint) { p.IntentBirth = 0 },
		func(p *TaskWorkspaceSyncCheckpoint) { p.IntentBirth = math.MaxInt64 },
		func(p *TaskWorkspaceSyncCheckpoint) { p.PlanDigest = strings.Repeat("A", 64) },
		func(p *TaskWorkspaceSyncCheckpoint) { p.ArchiveSHA256 = "bad" },
		func(p *TaskWorkspaceSyncCheckpoint) { p.TicketDigest = "" },
		func(p *TaskWorkspaceSyncCheckpoint) { p.TargetDigest = "bad" },
		func(p *TaskWorkspaceSyncCheckpoint) { p.PathDigest = "bad" },
		func(p *TaskWorkspaceSyncCheckpoint) { p.BodySHA256 = "bad" },
		func(p *TaskWorkspaceSyncCheckpoint) { p.EvidenceDigest = "bad" },
	} {
		bad := workspaceSyncCheckpointRecordFixture()
		mutate(bad.WorkspaceSync)
		require.ErrorIs(t, bad.Validate(), ErrInvalidRecord)
		_, err := encodeTaskCheckpointRecord(bad)
		require.ErrorIs(t, err, ErrInvalidRecord)
		wire, err := json.Marshal(bad)
		require.NoError(t, err)
		badWire(wire)
	}
	for _, mutate := range []func(*TaskCheckpointRecord){
		func(p *TaskCheckpointRecord) { p.Version = 1 },
		func(p *TaskCheckpointRecord) { p.WorkspaceSync = nil },
		func(p *TaskCheckpointRecord) { p.State = TaskCheckpointNeedsReconciliation },
		func(p *TaskCheckpointRecord) { p.DetailDigest = strings.Repeat("0", 64) },
	} {
		bad := workspaceSyncCheckpointRecordFixture()
		mutate(&bad)
		require.ErrorIs(t, bad.Validate(), ErrInvalidRecord)
	}
	for _, version := range []int64{0, -1, math.MaxInt64} {
		bad := *kv
		bad.Version = version
		require.ErrorIs(t, decodeTaskCheckpointRecord(&bad, &got), ErrCorruptRecord)
	}
	bad := *kv
	bad.ModRevision = math.MaxInt64
	require.ErrorIs(t, decodeTaskCheckpointRecord(&bad, &got), ErrCorruptRecord)
	owned, err := decodeTaskCheckpointAt(kv, r.Reference, 11, r.Attempt)
	require.NoError(t, err)
	owned.WorkspaceSync.BodyEnd++
	require.Equal(t, uint64(1029), got.WorkspaceSync.BodyEnd)
	initial := r.Attempt
	initial.StageID = "foreign"
	_, err = decodeTaskCheckpointAt(kv, r.Reference, 11, initial)
	require.ErrorIs(t, err, ErrCorruptRecord)
	clear(kv.Value)
	require.Equal(t, r, got, "diagnostic decode owns input bytes and nested payload")
}

func TestTaskWorkspaceSyncCheckpointV1Compatibility(t *testing.T) {
	_, _, _, r := taskTestRecords()
	golden := `{"version":1,"reference":{"namespace":"/codex-test/tasks/cell/","restore_epoch":"epoch","task_id":"8a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172","sandbox_id":"sandbox","partition":171},"claim_id":"","detail_digest":"","state":"pending","attempt":{"namespace":"/codex-test/tasks/cell/","partition":171,"request_id":"request","stage_id":"cleanup","attempt_id":"9a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172","restore_epoch":"epoch"}}`
	wire, err := encodeTaskCheckpointRecord(r)
	require.NoError(t, err)
	require.Equal(t, golden, wire)
	var got TaskCheckpointRecord
	require.NoError(t, decodeTaskCheckpointRecord(&mvccpb.KeyValue{CreateRevision: 11, ModRevision: 11, Value: []byte(wire + " ")}, &got), "v1 retains its original whitespace acceptance")
	require.Equal(t, r, got)
}
